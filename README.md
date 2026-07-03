# Defqon Stream Recorder

Docker-based automatic stream recorder with a dark Tailwind WebUI. It is built
for multi-user recording, but it can record any Twitch,
YouTube, Streamlink-compatible, or raw HTTP/HLS source.

## Features

- Multiple sources with per-source enable, auto-record, audio-only, transcode, quality, container, and colour settings.
- Twitch and YouTube recording through `streamlink`, piped into FFmpeg.
- Raw HTTP/HLS/DASH recording directly with FFmpeg.
- One finished subdirectory per source/stage.
- Separate temporary, finished, and log directories.
- Editable DEFQON.1 timetable from the WebUI.
- Optional FFmpeg transcoding with hardware acceleration presets for CUDA/NVENC, Quick Sync, and VAAPI.
- Optional `.nfo` files beside completed recordings.
- Disk free-space guard that pauses recording below 1 GB free and warns earlier.
- SMTP and Discord webhook notifications.
- Optional `rclone` backups for Dropbox, Google Drive, S3-compatible storage, and many other remotes.
- Basic player interface for audio and video streams, with optional WaveSurfer.js waveform rendering.
- Live stage switching from the WebUI when the browser can play the resolved stream URL.
- Custom app name, logo URL, colour scheme, accent colour, and custom CSS.

## Quick Start

```bash
docker compose up -d
```

Open:

```text
http://localhost:8080
```

On first start the app creates:

```text
data/
  config/config.json
  incomplete/
  logs/
  recordings/
```

The bundled `dq-timetable.json` is used to seed DEFQON.1 stages and timetable
entries. Auto-recording starts disabled by default so you can review sources,
storage, and backup settings before recording.

## Docker Compose

```yaml
services:
  recorder:
    image: ghcr.io/givinghawk/mutlirec:latest
    restart: unless-stopped
    ports:
      - "8080:8080"
    environment:
      TZ: Europe/Amsterdam
      CONFIG_PATH: /data/config/config.json
      FINISHED_DIR: /data/recordings
      TEMP_DIR: /data/incomplete
      LOG_DIR: /data/logs
    volumes:
      - ./data:/data
```

For local development:

```bash
make docker
docker compose up -d
```

## Sources

Source types:

- `youtube`: recorded with Streamlink.
- `twitch`: recorded with Streamlink.
- `http`: recorded directly with FFmpeg.

Each source can be configured with:

- URL and preferred Streamlink quality.
- Output container such as `mkv`, `mp4`, `m4a`, or `ts`.
- Audio-only recording.
- Stream copy or transcode.
- Extra Streamlink and FFmpeg arguments.
- Optional hardware acceleration.
- Optional NFO note.

Finished files are written to:

```text
data/recordings/<SOURCE_NAME>/
```

Active partial files are kept in:

```text
data/incomplete/<SOURCE_NAME>/
```

Recording logs are kept in:

```text
data/logs/
```

## Timetable

The timetable is editable in the WebUI as JSON. Entries use RFC3339 timestamps:

```json
[
  {
    "stage": "RED",
    "url": "https://www.youtube.com/@qdance/live",
    "sets": [
      {
        "start": "2026-06-26T13:00:00+02:00",
        "end": "2026-06-26T14:00:00+02:00",
        "name": "Opening Ceremony"
      }
    ]
  }
]
```

## Storage

The recorder checks free space before starting automatic recordings. Defaults:

- Warning threshold: 5 GB free.
- Stop threshold: 1 GB free.

For SMB or NFS storage, mount the share on the Docker host and bind it into the
container:

```yaml
volumes:
  - /mnt/defqon-recordings:/data/recordings
```

Host mounting is the most predictable approach across Linux, macOS, Windows,
NAS systems, and Docker Desktop.

## Backups

The image includes `rclone`. Mount an `rclone.conf`, enable backups in the UI,
and set a remote path such as:

```text
gdrive:defqon-recordings
s3:my-bucket/defqon
dropbox:defqon
```

Example Compose mount:

```yaml
volumes:
  - ./data:/data
  - ~/.config/rclone/rclone.conf:/home/app/.config/rclone/rclone.conf:ro
```

Backups run after each completed recording when enabled.

## Notifications

The WebUI supports:

- Discord webhook notifications.
- SMTP notifications.

Notifications are sent when recordings finish. Backup failures are written to
the app event log.

## Hardware Transcoding

Leave transcoding disabled for the safest default. Stream copy avoids quality
loss and uses less CPU.

If you enable transcoding, choose one of:

- `cuda` for NVIDIA/NVENC.
- `qsv` for Intel Quick Sync.
- `vaapi` for VAAPI.

You must also pass the matching device/runtime into Docker. For example, VAAPI
usually needs `/dev/dri`, and NVIDIA needs NVIDIA Container Toolkit.

## Development

```bash
make build
make run
make check
```

Environment variables:

| Variable | Default | Purpose |
| --- | --- | --- |
| `HTTP_ADDR` | `:8080` | Web server listen address |
| `CONFIG_PATH` | `/data/config/config.json` | Persistent config path |
| `FINISHED_DIR` | `/data/recordings` | Finished recordings |
| `TEMP_DIR` | `/data/incomplete` | Active partial recordings |
| `LOG_DIR` | `/data/logs` | Per-recording logs |

## Disclaimer

This project is not affiliated with or endorsed by Q-dance, DEFQON.1, Twitch,
YouTube, or any stream provider. Use it only where you have the right to record
and store the content.
