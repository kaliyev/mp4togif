# mp4togif (MP4 transcoder)

A small Go HTTP service that re-encodes MP4 videos with FFmpeg using S3-compatible storage (MinIO). It has two jobs:

- **Incident ingest** (`POST /incidents`): accepts incident data plus a video, stores the original in S3, inserts a row into Postgres, then converts the video and notifies Telegram in the background. This replaces the n8n "SchoolIncidents" workflow.
- **Conversion** (`POST /convert`, `GET /file`): converts an object already in S3 and serves objects from the bucket.

> **Note:** despite the repository name, the service currently produces **MP4**, not GIF. GIF output was replaced by MP4 re-encoding (`-crf 28 -preset medium`).

## Features
- Incident ingest with basic auth, Postgres insert, background conversion through a bounded worker pool, and Telegram notifications (text, then the converted video).
- Graceful shutdown: running conversions get `SHUTDOWN_TIMEOUT` to finish; queued ones are logged as dropped (their originals stay in S3).
- Input and output via S3-compatible storage (MinIO). S3 is required; there is no local-file mode.
- Optional trimming by start time and duration.
- Default clip: 30 seconds from the middle of the video (duration detected with `ffprobe`).
- Configurable output frame rate.
- FFmpeg run with a timeout; stderr tail is returned on failure.
- Dockerized; CI builds and pushes an image to GitHub Container Registry.

## Prerequisites
- **Go 1.25+**
- **FFmpeg** and **ffprobe** in `PATH`
- An S3-compatible endpoint (e.g. MinIO)
- PostgreSQL with the `core_incident` table (for `/incidents` only)

## Project Structure
```text
.
├── Dockerfile               # Multi-stage build, alpine + ffmpeg runtime
├── .github/workflows/ci.yml # go build + docker build/push to ghcr.io
├── go.mod / go.sum          # Module: minio-go, pgx, google/uuid
├── main.go                  # Config, server wiring, shutdown, /file
├── convert.go               # /convert and the FFmpeg helpers
├── incidents.go             # /incidents ingest, DB insert, conversion workers
└── telegram.go              # Minimal Telegram Bot API client
```

## Configuration
Environment variables:

| Variable | Description | Default |
| :--- | :--- | :--- |
| `ADDR` | Server listen address | `:8180` |
| `MAX_BODY_BYTES` | Maximum allowed request body size | `209715200` (200MB) |
| `FFMPEG_TIMEOUT` | Maximum time allowed for an FFmpeg run (Go duration, e.g. `2m`) | `2m` |
| `S3_ENDPOINT` | S3 endpoint, e.g. `localhost:9000`. **Required** for `/convert` and `/file` | (empty) |
| `S3_ACCESS_KEY` | S3 access key | (empty) |
| `S3_SECRET_KEY` | S3 secret key | (empty) |
| `S3_BUCKET` | S3 bucket used for both input and output | `videos` |
| `S3_USE_SSL` | Use TLS for S3 (`true`/`false`) | `false` |
| `VIDEOS_DIR` | Read into config but currently unused | `tmp/videos/` |
| `SHUTDOWN_TIMEOUT` | On SIGTERM, how long to wait for in-flight requests and running conversions | `25s` |
| `DATABASE_URL` | Postgres DSN, e.g. `postgres://user:pass@db:5432/app`. **Required** for `/incidents` | (empty) |
| `SCHOOL_ID` | UUID written to `core_incident.school_id`. **Required** for `/incidents` | (empty) |
| `INGEST_PATH` | Path(s) of the ingest endpoint, comma-separated, e.g. `/incidents,/webhook/<n8n-id>` to keep the old n8n webhook path during migration | `/incidents` |
| `INGEST_USER` / `INGEST_PASSWORD` | Basic auth credentials for the ingest endpoint. **Required** for `/incidents` | (empty) |
| `INGEST_WORKERS` | Number of conversions running in parallel | `2` |
| `INGEST_QUEUE_SIZE` | Conversions waiting for a worker; when full, ingest requests wait | `100` |
| `TELEGRAM_BOT_TOKEN` / `TELEGRAM_CHAT_ID` | Telegram notifications; disabled if either is empty | (empty) |
| `TELEGRAM_HEADER` | First line of the text notification | `Что-то произошло в школе` |
| `TELEGRAM_API_URL` | Bot API base URL; point it at a local Bot API server to lift the 50 MB upload limit | `https://api.telegram.org` |

If `S3_ENDPOINT` is empty, the server starts but `/convert` and `/file` return `500 s3 client not configured`.
The `/incidents` endpoint is registered only when `DATABASE_URL`, `SCHOOL_ID`, `INGEST_USER`, `INGEST_PASSWORD` and `S3_ENDPOINT` are all set; otherwise the startup log says which are missing.

## Setup and Run

### Locally
```bash
S3_ENDPOINT=localhost:9000 S3_ACCESS_KEY=minio S3_SECRET_KEY=minio123 go run .
```

### Docker
```bash
docker build -t mp4togif .
docker run -p 8180:8080 \
  -e S3_ENDPOINT=host.docker.internal:9000 \
  -e S3_ACCESS_KEY=minio -e S3_SECRET_KEY=minio123 \
  mp4togif
```
The Dockerfile sets `ADDR=:8080` inside the container. Docker sends SIGKILL 10 seconds after SIGTERM by default; to let conversions finish, raise it above `SHUTDOWN_TIMEOUT` (`docker run --stop-timeout 30`, or `stop_grace_period: 30s` in Compose).

## API

### `GET /healthz`
Returns `200 OK`.

### `POST /incidents`
Accepts an incident from a camera/analytics client. Requires basic auth (`INGEST_USER` / `INGEST_PASSWORD`).

**Request:** `multipart/form-data` with the video file in `video` and the incident as text fields.

Required fields: `location`, `date` (`DD.MM.YYYY`), `time` (e.g. `08:15:00`). Fields used in the Telegram message: `classification`, `description`, `confidence_percentage`, `bullying_score`, `fight_score`. All text fields are stored in `core_incident.data` as a JSON object of strings, as n8n did.

```bash
curl -u cam:secret \
  -F location='Corridor 2' -F date=13.10.2026 -F time=08:15:00 \
  -F classification=fight -F fight_score=0.9 \
  -F 'video=@clip.mp4;type=video/mp4' \
  http://localhost:8180/incidents
```

**Processing:**
1. The original is uploaded to `S3_BUCKET` as `<incident uuid><ext>` (the client's file name is only logged).
2. A row is inserted: `INSERT INTO core_incident (id, location, datetime, data, video, school_id, created_at)`. `datetime` is `date` + `time` cast to `timestamptz` by Postgres in the session time zone; `video` is the S3 key of the original. If the insert fails, the uploaded original is removed.
3. The response is sent and a Telegram text message goes out.
4. In the background a worker converts the video (first 30 s, 5 fps, same FFmpeg settings as `/convert`), uploads `<incident uuid>_converted.mp4`, and sends it to Telegram.

**Responses:**
- `200 {"result":"success","id":"<uuid>"}`
- `400 {"result":"error","message":"fields missing: location, video"}`, or not multipart
- `401` wrong or missing credentials
- `413` body larger than `MAX_BODY_BYTES`
- `500 {"result":"error","message":"something wrong"}`: S3 upload or DB insert failed (details in the log), including an unparseable `date`/`time`

### `POST /convert`
Downloads `s3_input_key` from the bucket, re-encodes it, and uploads the result to the same bucket.

**Request body:**
```json
{
  "s3_input_key": "raw/input.mp4",
  "fps": 10,
  "start": 0,
  "duration": 30
}
```
- `s3_input_key` (required): object key of the source MP4.
- `fps`: output frame rate, clamped to 1–30. Default 10.
- `start`: start offset in seconds. Used only when `duration` > 0.
- `duration`: clip length in seconds. If omitted or 0, the service takes 30 seconds from the middle of the video, or the whole video if it is shorter than 30 seconds.
- `width`: accepted in the JSON but currently ignored.

**Output key:** `<basename of s3_input_key without extension>_converted.mp4`, placed at the bucket root (the source prefix is not preserved). Example: `raw/input.mp4` → `input_converted.mp4`. Re-running on the same input overwrites the object.

**Response (200):**
```json
{"success":"ok","path":"input_converted.mp4","hash":"3f2a9c1b7d4e"}
```
`hash` is the first 12 hex chars of SHA-256 of `s3_input_key`.

**Errors:**
- `400` invalid JSON or missing `s3_input_key`
- `405` method other than POST
- `422` FFmpeg failed or timed out (message includes the tail of FFmpeg output)
- `500` S3 not configured, download/upload failure, temp dir error

### `GET /file?key=...`
Streams an object from the bucket with `Content-Type`, `Content-Length` and `Content-Disposition: attachment`.
- `400` missing `key`
- `404` object does not exist
- `500` S3 not configured or other S3 error

## Development

### FFmpeg pipeline
Single pass:
```
ffmpeg -y [-ss START] [-t DURATION] -i input.mp4 -r FPS -preset medium -crf 28 output.mp4
```
Duration is detected beforehand with:
```
ffprobe -v error -show_entries format=duration -of default=noprint_wrappers=1:nokey=1 input.mp4
```
Each request works in its own temp directory (`os.MkdirTemp`). For `/convert` it is removed after the request; for `/incidents` it is handed to the worker and removed after conversion.

### Go idioms used
- `cmp.Or` for defaults, `context.WithTimeout` for the FFmpeg process, `http.NewServeMux` for routing, `http.MaxBytesReader` for body limits.
- A logging middleware prints method, path and elapsed time per request.

### Known gaps
- No tests; CI only runs `go build`.
- Conversions are kept in memory only: if the process dies or the shutdown timeout runs out, queued and running conversions are lost (logged as dropped). The row and the original stay; re-run with `/convert` using the original key.
- The JSON response is built by string concatenation; a key containing `"` would produce invalid JSON.
- If `ffprobe` fails and `duration` is not given, the clip length becomes 0.
