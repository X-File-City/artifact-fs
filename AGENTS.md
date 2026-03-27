# AGENTS.md

Guide for AI coding agents working in this repo. Read this before making changes.

## Critical rules

These are non-negotiable. Violations cause security issues, data corruption, or multi-minute performance regressions.

- **Credentials never appear in CLI args.** Use env-based credential helpers (`credentialEnv()` in gitstore.go) so tokens don't show in `ps` output. All log output passes through `auth.RedactString`.
- **Blob content is never converted to `string`.** `BlobToCache` streams `git cat-file --batch` stdout to a temp file via `io.CopyN`. String conversion corrupts binary files silently.
- **`GIT_NO_LAZY_FETCH=1` must be set on `cat-file --batch-check`** during `batchResolveSizes`. Without it, every blob OID triggers a network round-trip on blobless clones -- size resolution takes minutes instead of milliseconds.
- **`model.CleanPath()` is the only path normalization function.** It strips leading slashes, cleans dot-segments, defaults empty/root to ".". Adding local `cleanPath` wrappers causes inconsistent path matching at overlay/snapshot boundaries.

## Build, test, lint

- `go build ./cmd/artifact-fs` -- build the binary
- `go test ./...` -- run all tests (unit + integration stubs)
- `go test -run TestFoo ./internal/foo` -- run a single test
- `go vet ./...` -- static analysis (no linter beyond vet)
- `AFS_RUN_BENCH=1 go test -run TestBenchRepos -v` -- benchmarks (clones real repos; needs network)
- `AFS_RUN_E2E_TESTS=1 go test -run TestE2E -v` -- e2e tests (needs FUSE: macFUSE on darwin, `/dev/fuse` on linux)
- Tests live in `*_test.go` alongside source. `bench_test.go` and `e2e_test.go` are in the root package.

## Running the daemon

`add-repo` and `daemon` are separate commands with very different lifecycles. Do not confuse them.

- **`add-repo`** -- one-shot. Clones the repo (blobless), builds the initial snapshot in SQLite, registers it, then exits. Does NOT mount FUSE or start goroutines.
- **`daemon`** -- long-running. Mounts all registered repos via FUSE, starts background goroutines (watcher, hydrator, refresh loop), blocks on `<-ctx.Done()`.

```sh
export ARTIFACT_FS_ROOT=/tmp/artifact-fs-test
go build -o artifact-fs ./cmd/artifact-fs
./artifact-fs add-repo --name myrepo --remote https://github.com/org/repo.git --branch main --mount-root /tmp
./artifact-fs daemon --root /tmp --hydration-concurrency 8 &
DAEMON_PID=$!
# test against /tmp/myrepo/
kill $DAEMON_PID
```

- `--hydration-concurrency N` controls parallel blob-fetch workers (default 4). Each worker gets a dedicated `git cat-file --batch` process from the batch pool.
- Daemon logs JSON to stderr. Capture with `2>/tmp/daemon.log`.
- After killing the daemon, clean stale mounts with `umount /tmp/myrepo`.
- macFUSE must be installed on macOS (`/Library/Filesystems/macfuse.fs` must exist).

## Architecture

Data flow: `git clone --filter=blob:none` -> `ls-tree` + `cat-file --batch-check` -> SQLite snapshot -> FUSE mount -> on-demand hydration via `cat-file --batch`.

### Subsystems

- **Resolver** (`fusefs/merged.go`) -- merges snapshot (base git tree) + overlay (local writes) into a unified view. Reads the current generation atomically so FUSE ops see new trees without locks.

- **Engine** (`fusefs/ops.go`) -- handles writes by promoting base files to the overlay via `ensureOverlay()` (hydrate blob, copy-on-write). `PrefetchDir()` enqueues file children for speculative hydration on `OpenDir`.

- **Snapshot** (`snapshot/store.go`) -- SQLite-backed store of `base_nodes` rows keyed by `(generation, path)`. `PublishGeneration` does a bulk insert in a transaction. `UpdateSize` backfills file sizes after hydration. Old generations are pruned.

- **Overlay** (`overlay/store.go`) -- persistent writable layer. SQLite metadata + `upper/` directory for backing files. Whiteouts (kind="delete") hide base entries.

- **Hydrator** (`hydrator/hydrator.go`) -- priority queue (heap) blob fetcher with deduped waiters. Workers block on a `workReady` channel and drain the queue on each wake. `EnsureHydrated` blocks until the blob is cached; `Enqueue` is fire-and-forget for prefetch. `OnHydrated` callback backfills file sizes in the snapshot.

- **GitStore batch pool** (`gitstore/gitstore.go`) -- a pool of persistent `git cat-file --batch` processes (one pool per gitDir). `BlobToCache` acquires a process, streams the blob to a temp file, then atomic-renames it. On error, the process is discarded and retried once.

- **Watcher** (`watcher/watcher.go`) -- polls git HEAD/refs/index mtimes at 500ms. On HEAD change, the daemon re-indexes the tree and atomically updates the resolver's generation.

- **Refresh loop** -- periodic `git fetch` with exponential backoff (capped at 10min), reset on success. Separate from the watcher: watcher detects local HEAD changes; refresh loop pulls from remote.

## Conventions

- **Interfaces** -- `model.OverlayStore` is the single canonical interface for overlay operations. Do not create subset interfaces in fusefs. Same for `model.Hydrator`, `model.SnapshotStore`, `model.GitStore`.
- **Readdir** -- `Readdir()` is a thin wrapper around `ReaddirTyped()`. Add overlay merge logic only in `ReaddirTyped`; `Readdir` delegates.
- **SQLite** -- WAL mode + `busy_timeout=5000` (see `meta.OpenDB`). Use `modernc.org/sqlite` (pure Go, no CGo).
- **FUSE .git gitfile** -- the root directory synthesizes a `.git` file so git commands work inside the mount. Content is computed once and stored on `ArtifactFuse`.
- **Inodes** -- monotonically allocated at runtime (root = 1), NOT persisted. The kernel re-looks-up paths via `LookUpInode` after `ReadDir`.
- **Snapshot generations** -- atomic int64 on the Resolver. FUSE ops read it without locks.

## Do not

| Don't | Do instead | Why |
|-------|-----------|-----|
| Add `cleanPath` wrappers | `model.CleanPath` | One normalization function prevents path mismatch bugs |
| Create subset interfaces (e.g. `OverlayWriter`) | Use the canonical `model.*` interfaces | Subset interfaces drift and fragment call sites |
| Use `git ls-tree -l` on blobless clones | `ls-tree` + `cat-file --batch-check` with `GIT_NO_LAZY_FETCH=1` | `-l` fetches every blob size from remote and hangs |
| Pass credentials in git CLI args | `credentialEnv()` env-based helpers | CLI args visible in `ps` output |
| Call `fuse.Mount` outside `daemon` | Only `daemon` mounts FUSE | `add-repo` is one-shot; mounting there leaks resources |
| Convert blob bytes to `string` | Stream via `io.CopyN` to a file | String conversion silently corrupts binary content |
| Use a ticker in the hydrator | Workers wake via `workReady` channel | Ticker adds up to 20ms latency per item with no benefit |
| Spawn `git cat-file -p` per blob | Use the batch pool via `BlobToCache` | One-shot processes pay ~300ms connection overhead each |
