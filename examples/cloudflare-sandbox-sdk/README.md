# Cloudflare Sandbox SDK + ArtifactFS

This example shows the smallest useful integration between Cloudflare Sandbox SDK and ArtifactFS.
It extends the standard Sandbox image, adds the `artifact-fs` binary, and mounts one Git remote inside the sandbox at `/workspace/mnt/<repo-name>`.

The default remote is `https://github.com/cloudflare/sandbox-sdk.git`, but the Worker can override it per request with `MOUNT_GIT_REMOTE` behavior through the bootstrap command. The repo name is inferred from the remote URL, so `https://github.com/cloudflare/sandbox-sdk.git` mounts at `/workspace/mnt/sandbox-sdk`.

The example requires a bearer token for both `POST /mount` and `GET /status`. Set `SANDBOX_API_TOKEN` in local dev or as a Worker secret before using the API.

## What This Example Covers

- A custom Sandbox image based on `cloudflare/sandbox`
- A small bootstrap script that registers a repo with ArtifactFS and starts the daemon on demand
- A Worker API that creates a sandbox, passes mount env vars, and returns repo status

## Files

- `Dockerfile`: builds `artifact-fs` from this repo and adds it to the Sandbox image
- `container_src/mount-artifact-fs-repo.sh`: idempotent mount helper used inside the sandbox
- `src/index.ts`: minimal Worker API with `POST /mount` and `GET /status`
- `wrangler.jsonc`: container and Durable Object binding configuration

## Run Locally

```bash
cd examples/cloudflare-sandbox-sdk
npm install
cp .dev.vars.example .dev.vars
npm run dev
```

The first run builds the Docker image, so it will be slower.

If your environment uses TLS interception or a private CA, pass the PEM at image build time with Wrangler `image_vars` as `ARTIFACT_FS_EXTRA_CA_PEM`.

## Mount the Default Repo

```bash
curl -X POST http://localhost:8787/mount \
  -H 'authorization: Bearer local-dev-token' \
  -H 'content-type: application/json' \
  -d '{"sandboxId":"demo"}'
```

Example response:

```json
{
  "sandboxId": "demo",
  "remote": "https://github.com/cloudflare/sandbox-sdk.git",
  "branch": "main",
  "repoName": "sandbox-sdk",
  "mountPath": "/workspace/mnt/sandbox-sdk",
  "head": "...",
  "gitStatus": "## main...origin/main",
  "artifactFsStatus": "repo=sandbox-sdk state=mounted ..."
}
```

## Mount a Different Repo

Use an HTTPS or SSH remote. The example accepts either form and infers the repo name from the last path segment. For HTTPS remotes, do not embed credentials in the URL.

```bash
curl -X POST http://localhost:8787/mount \
  -H 'authorization: Bearer local-dev-token' \
  -H 'content-type: application/json' \
  -d '{
    "sandboxId": "artifact-fs-demo",
    "remote": "git@github.com:cloudflare/artifact-fs.git",
    "branch": "main"
  }'
```

## Check Status Later

```bash
curl \
  -H 'authorization: Bearer local-dev-token' \
  "http://localhost:8787/status?sandboxId=artifact-fs-demo&remote=git@github.com:cloudflare/artifact-fs.git&branch=main"
```

## Notes

- This example is intentionally narrow. It focuses on mounting a repo and proving the mount worked.
- The example supports exactly one mounted repo per sandbox. Reuse the same `sandboxId` only when you want the same remote and branch.
- Public repos work out of the box. Private repos need auth wiring in the sandbox image or Worker environment, which is deliberately left to downstream builds.
- The bootstrap script uses `nohup` so the ArtifactFS daemon survives the setup command and remains available for later requests.
