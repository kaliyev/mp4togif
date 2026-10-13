# CLAUDE.md

Go HTTP service that re-encodes MP4 files stored in S3-compatible storage (MinIO) via FFmpeg, and ingests school incidents (`/incidents`: Postgres row + video in S3 + Telegram), replacing an n8n workflow. Despite the repo name, output is **MP4**, not GIF; GIF support was removed in commit `39b45e9`. Full API and config reference lives in [README.md](README.md). Keep README and `.junie/guidelines.md` in sync with the code when behaviour changes; both have drifted before.

## Layout
- Flat `package main`, one file per concern. The Dockerfile builds with `COPY *.go`, so a subpackage in a subdirectory also needs a Dockerfile change; CI's `go build ./...` would not catch the omission.
- `.junie/guidelines.md` is a copy of the project notes for the Junie agent; it is gitignored, so edits to it do not show in `git status`.
- `tmp/` holds local sample videos and is gitignored. The `mp4togif` binary in the repo root is a stale build artifact, also gitignored.

## Verify
- Build: `go build ./...` (what CI runs; there are no tests).
- Running locally needs `ffmpeg`, `ffprobe` and a reachable S3 endpoint via `S3_ENDPOINT`, `S3_ACCESS_KEY`, `S3_SECRET_KEY`. Without `S3_ENDPOINT` the server starts but `/convert` and `/file` return 500. `/incidents` also needs Postgres and more env; README lists it, and the startup log names what is missing.
- Throwaway S3 for tests: `minio/minio` and `quay.io/minio/minio` images no longer pull. `chrislusf/seaweedfs server -s3 -s3.config=<identities json>` works (without identities it rejects signed requests). Under SeaweedFS `/file` returns headers with an empty body (412 on minio-go's ranged re-read); that is a SeaweedFS quirk, not a regression.
- `TELEGRAM_API_URL` can point at a local fake server to check notifications without a real bot.
- CI (`.github/workflows/ci.yml`) builds and pushes a Docker image to ghcr.io on every push to `main`, so a broken build on `main` ships a broken image.

## Conventions
- Config comes only from environment variables through the `env*` helpers in `main.go`; add new settings there and document them in README.
- Output object key is `<basename>_converted.mp4` at the bucket root. Changing it changes what callers download via `/file`, so treat it as a public contract.
- `/incidents` contracts carried over from n8n: response JSON `{"result":"success"|"error", ...}` (the camera client reads it), and the `core_incident` columns, which another app reads; `video` holds the S3 key of the original. The table looks like a Django model of that app, so schema changes belong there, not here.
- `ConvertRequest` still carries `width`, and `cfg` still carries `videosDir`, but neither is used. Either wire them up or remove them; do not document them as working.
