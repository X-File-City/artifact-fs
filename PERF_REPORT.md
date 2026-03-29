# Performance Report

This report summarizes clone and cold full-hydration benchmarks run through the E2E test harness against four repositories:

- `empty`
- `workers-sdk`
- `workerd`
- `next.js`

The final 10x statistics use local `file://` mirrors for the non-empty repositories. That isolates ArtifactFS behavior from GitHub WAN latency and makes the measurements repeatable.

## Method

Benchmarks were run with the E2E benchmark harness in `e2e_bench_test.go`.

For each run, the harness measured:

- blobless clone time
- time to cold-hydrate every unique blob reachable from the repo HEAD
- hydrated objects per second

The benchmark was run 10 times per repo. Percentiles are computed from 10 samples, so `p95` and `p99` are effectively near-max order statistics rather than stable tail estimates.

Representative command:

```sh
AFS_RUN_E2E_BENCH=1 \
AFS_E2E_BENCH_RUNS=10 \
AFS_E2E_BENCH_WORKERS_SDK_URL=file:///tmp/artifact-fs-bench-mirrors/workers-sdk.git \
AFS_E2E_BENCH_WORKERD_URL=file:///tmp/artifact-fs-bench-mirrors/workerd.git \
AFS_E2E_BENCH_NEXTJS_URL=file:///tmp/artifact-fs-bench-mirrors/next.js.git \
go test -v -run TestE2EBenchmarkRepos -count=1 -timeout 8h
```

## Results

### Clone Time (seconds)

| Repo | Objects | Median | p90 | p95 | p99 | Max |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| `empty` | 0 | 0.365 | 0.370 | 0.421 | 0.421 | 0.421 |
| `workers-sdk` | 3,984 | 2.757 | 2.803 | 2.809 | 2.809 | 2.809 |
| `workerd` | 2,445 | 1.490 | 1.522 | 1.551 | 1.551 | 1.551 |
| `next.js` | 22,531 | 27.896 | 28.111 | 28.163 | 28.163 | 28.163 |

### Full Hydration Time (seconds)

| Repo | Median | p90 | p95 | p99 | Max |
| --- | ---: | ---: | ---: | ---: | ---: |
| `empty` | 0.000 | 0.000 | 0.000 | 0.000 | 0.000 |
| `workers-sdk` | 18.016 | 18.501 | 18.553 | 18.553 | 18.553 |
| `workerd` | 10.805 | 11.096 | 11.354 | 11.354 | 11.354 |
| `next.js` | 106.688 | 107.522 | 107.832 | 107.832 | 107.832 |

### Hydration Throughput (objects/sec)

| Repo | Median | p90 | p95 | p99 | Max |
| --- | ---: | ---: | ---: | ---: | ---: |
| `empty` | 0.0 | 0.0 | 0.0 | 0.0 | 0.0 |
| `workers-sdk` | 220.7 | 226.7 | 228.0 | 228.0 | 228.0 |
| `workerd` | 226.2 | 227.6 | 230.9 | 230.9 | 230.9 |
| `next.js` | 211.2 | 213.7 | 214.6 | 214.6 | 214.6 |

### Hydration Throughput (MiB/sec)

| Repo | Median | p90 | p95 | p99 | Max |
| --- | ---: | ---: | ---: | ---: | ---: |
| `empty` | 0.00 | 0.00 | 0.00 | 0.00 | 0.00 |
| `workers-sdk` | 1.22 | 1.25 | 1.26 | 1.26 | 1.26 |
| `workerd` | 1.88 | 1.89 | 1.92 | 1.92 | 1.92 |
| `next.js` | 1.28 | 1.30 | 1.30 | 1.30 | 1.30 |

## Findings

- **Hydration dominates cold startup** for non-empty repositories. Median `clone + full hydration` time is about `20.8s` for `workers-sdk`, `12.3s` for `workerd`, and `134.6s` for `next.js`.
- **Throughput is roughly constant across repo sizes** at about `211-226 objects/sec`. That suggests runtime scales close to linearly with unique blob count.
- **Per-object overhead appears to be the main limiter.** `next.js` is far larger than `workerd`, but hydration throughput is only modestly lower.
- **Variance is low** in the mirror-backed runs. That means these numbers are stable enough to use as a baseline when testing changes.
- **Mirror-backed runs reported `unknown_sizes=0` for all objects.** In earlier remote sanity checks against GitHub, true lazy remote hydration was much slower, which points to a separate network-fetch problem not visible in the mirror-backed baseline.

## Likely Hot Spots

The current code suggests a few clear candidates.

- `internal/gitstore/gitstore.go`: `batchCatFile.fetchToFile()` calls `f.Sync()` for every hydrated blob before renaming it into the cache.
- `internal/gitstore/gitstore.go`: each blob is fetched, written, synced, and renamed independently, which adds fixed per-object filesystem overhead.
- `internal/hydrator/hydrator.go`: hydration is object-oriented and worker-limited. The default concurrency is 4 workers, which may be conservative for local disk-backed clones.
- `internal/daemon/daemon.go` and `internal/snapshot/store.go`: size backfill happens only after hydration succeeds. On true blobless remotes, this interacts with the `size=0 until hydrated` path and can make mounted reads less representative.

## Improvement Plan

### 1. Reduce local cache write cost

Goal: increase local full-hydration throughput without changing semantics of Git data access.

- Measure the impact of removing or relaxing per-blob `f.Sync()` for the blob cache.
- If full durability is not required for cache files, treat the cache as reconstructible and prefer throughput over per-file sync guarantees.
- Re-run this benchmark after the change and compare against the current baseline.

Why this is first: the benchmark is dominated by a repeated per-object operation, and `f.Sync()` is one of the most expensive operations in that path.

### 2. Tune hydration concurrency

Goal: find the knee in the curve for local blob hydration.

- Benchmark `--hydration-concurrency` at values like `4`, `8`, `16`, and `32`.
- Track both wall-clock hydration time and objects/sec.
- Stop increasing concurrency once storage contention or context-switching flattens the curve.

Why this is next: the current throughput is stable and low enough that a simple concurrency increase may yield an immediate improvement.

### 3. Reduce fixed per-object overhead

Goal: make hydration cost less sensitive to object count.

- Review whether cache file creation, rename, and metadata work can be reduced or batched.
- Consider whether the cache layout can avoid some directory and file churn for very large repos.
- Profile `next.js` hydration specifically, since it is the clearest stress case for fixed per-object cost.

Why this matters: the current scaling pattern looks linear in unique blob count, which is usually a sign that fixed per-item work is dominating.

### 4. Separate local and remote performance work

Goal: avoid conflating local ArtifactFS overhead with remote promisor latency.

- Keep the local-mirror benchmark as the control for ArtifactFS internals.
- Add a smaller remote benchmark mode for a few representative samples against GitHub.
- Compare remote objects/sec against mirror objects/sec to isolate network and Git lazy-fetch cost.

Why this matters: earlier remote checks were dramatically slower than the mirror baseline, so remote hydration likely needs a different optimization strategy.

### 5. Improve true remote blobless hydration behavior

Goal: make real cold reads over blobless clones viable, not just fast on local mirrors.

- Investigate whether we can fetch blobs in larger batches instead of relying on one lazy object fetch at a time.
- Review the interaction between unknown file sizes and the mounted read path so a cold read cannot silently short-circuit through a `size=0` result.
- Add a targeted regression benchmark for remote blobless hydration once a candidate fix exists.

Why this matters: users experience remote cold hydration, not mirror-backed hydration. The local benchmark is necessary, but not sufficient.

## Suggested Next Experiments

- Remove or gate cache `f.Sync()` behind a durability option and re-run the 10x mirror benchmark.
- Benchmark hydration concurrency at `4`, `8`, `16`, and `32` on `workers-sdk` and `next.js`.
- Capture a CPU and syscall profile during `next.js` hydration to confirm whether the dominant cost is sync/write/rename overhead.
- Add a small remote benchmark variant, even if it only runs 1-3 times, to quantify the gap between local and GitHub-backed hydration.

## Bottom Line

The local benchmark baseline is now stable. ArtifactFS currently hydrates large repositories at roughly `211-226 objects/sec`, and hydration time dominates cold-start cost. The most defensible first optimization is to reduce per-object cache write overhead, then retest concurrency. After that, remote blobless fetch behavior should be treated as a separate performance project.
