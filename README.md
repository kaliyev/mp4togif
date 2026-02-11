# MP4 to GIF Converter

A Go-based service that converts MP4 videos to high-quality GIFs using FFmpeg. It employs a two-pass encoding process with palette generation to ensure optimal visual quality.

## Features
- High-quality GIF conversion using FFmpeg `palettegen` and `paletteuse` filters.
- Lanczos scaling for superior image resizing.
- Configurable FPS, width, start time, and duration.
- Dockerized for easy deployment.
- Modern Go 1.25 idioms.

## Prerequisites
- **Go 1.25+**
- **FFmpeg** and **ffprobe** (must be in system PATH)

## Project Structure
```text
.
├── Dockerfile          # Multi-stage Docker build
├── go.mod              # Go module definition
├── main.go             # Main application logic and HTTP server
└── tmp/videos/         # Default directory for videos and generated GIFs
```

## Configuration
The application is configured via environment variables:

| Variable | Description | Default |
| :--- | :--- | :--- |
| `ADDR` | Server listen address | `:8180` |
| `VIDEOS_DIR` | Directory containing source videos and where GIFs are saved | `tmp/videos/` |
| `MAX_BODY_BYTES` | Maximum allowed request body size | `209715200` (200MB) |
| `FFMPEG_TIMEOUT` | Maximum time allowed for FFmpeg conversion | `2m` |

## Setup and Run

### Running Locally
1. Ensure `ffmpeg` and `ffprobe` are installed:
   ```bash
   ffmpeg -version
   ffprobe -version
   ```
2. Start the server:
   ```bash
   go run main.go
   ```

### Using Docker
1. Build the image:
   ```bash
   docker build -t mp4togif .
   ```
2. Run the container:
   ```bash
   docker run -p 8180:8080 -v $(pwd)/tmp/videos:/tmp/videos mp4togif
   ```
   *Note: The Dockerfile defaults `ADDR` to `:8080` internally.*

## API Usage

### POST `/convert`
Converts an MP4 file to GIF.

**Request Body:**
```json
{
  "filepath": "input.mp4",
  "fps": 10,
  "width": 480,
  "start": 0,
  "duration": 30
}
```
- `filepath`: (Required) Relative path to the file inside `VIDEOS_DIR`.
- `fps`: Frames per second (1-30, default: 10).
- `width`: GIF width (64-1920, default: 480).
- `start`: Start time in seconds (default: 0).
- `duration`: Duration in seconds. If omitted, defaults to 30s or the remaining length of the video.

**Response:**
Returns the filename of the generated GIF on success, or FFmpeg error output on failure.

## Development

### FFmpeg Implementation Details
The conversion uses a two-pass approach:
1. **Palette Generation**: `palettegen` filter creates a custom 256-color palette for the specific video clip.
2. **GIF Creation**: `paletteuse` filter applies the generated palette with dither.
3. Both passes use `lanczos` scaling.

### Go 1.25 Idioms
- Uses `cmp.Or` for default value handling.
- Uses `time.Since` for duration logging.
- Uses `context.WithTimeout` for external process management.
- Uses `http.NewServeMux()` for routing.
