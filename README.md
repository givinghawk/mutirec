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
- Visual timetable editor (day tabs, click-to-edit/add sets), with raw JSON editing still available.
- Optional timetable lookup/import from [timetable.lol](https://timetable.lol) for hundreds of festivals, or build your own by hand - entirely optional either way.
- Per-source "timetable stage" linking, for when a recording source's name doesn't match its stage name in the timetable.
- Star any timetable set to get a Discord/SMTP reminder a configurable number of minutes before it starts.
- Optional FFmpeg transcoding with hardware acceleration presets for CUDA/NVENC, Quick Sync, and VAAPI.
- Optional `.nfo` files beside completed recordings.
- Disk free-space guard that pauses recording below 1 GB free and warns earlier.
- SMTP and Discord webhook notifications.
- Optional `rclone` backups for Dropbox, Google Drive, S3-compatible storage, and many other remotes.
- Basic player interface for audio and video streams, with optional WaveSurfer.js waveform rendering.
- Live stage switching from the WebUI when the browser can play the resolved stream URL.
- Optional live rewind: scrub backward within an in-progress recording using a rolling HLS buffer.
- Custom app name, logo URL, colour scheme, accent colour, and custom CSS.
- Recordings library view with search/filter across all finished files.
- One-click stream test/resolve before saving a source, to catch bad URLs or qualities early.
- Delete and duplicate buttons for sources, plus validation on required fields.
- Toast notifications surface API/server errors directly in the WebUI.
- Session-based login page in front of the whole app (WebUI and API), with a one-time setup wizard on first run - no environment variables required unless you want them.

## Quick Start

```bash
docker compose up -d
```

Open:

```text
http://localhost:8080
```

You'll land on a one-time setup page to choose a username and password -
nothing to configure by hand first. You can change these credentials any
time from Settings → Account. See [Authentication](#authentication) below if
you'd rather manage credentials via environment variables instead.

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

The Timetable tab has a visual editor: pick a day, click a set to edit or
delete it, or click "+ Set" on a stage row to add a new one. Raw JSON editing
is still available behind "Show raw JSON" for bulk edits or scripting. Entries
use RFC3339 timestamps:

```json
[
  {
    "stage": "RED",
    "url": "https://www.youtube.com/@qdance/live",
    "sets": [
      {
        "id": "opt-stable-id",
        "start": "2026-06-26T13:00:00+02:00",
        "end": "2026-06-26T14:00:00+02:00",
        "name": "Opening Ceremony"
      }
    ]
  }
]
```

`id` is optional on manual entries - one is assigned automatically on save if
missing, and it's what favoriting/reminders key off internally.

### Importing from timetable.lol (optional)

The Timetable tab can look up a festival on
[timetable.lol](https://timetable.lol) and import its schedule instead of
building one by hand. This is entirely optional - your own hand-built or
JSON-edited timetable works exactly the same either way, and importing is
just a shortcut. Timetable data is provided by timetable.lol; importing
replaces the current timetable and remembers which event it came from so you
can re-sync or unlink later from the same panel. Existing per-stage stream
URLs are preserved by matching on stage name across a re-sync.

If a recording source's name doesn't match the stage name from an imported
(or hand-built) timetable, set "Timetable stage" on that source in the
Source Manager to point it at the right stage for Now/Next lookups.

### Reminders

Star any set in the visual timetable to get a reminder before it starts, sent
through whichever of Discord webhook/SMTP is configured in Settings. The lead
time (default 15 minutes) is configurable under Settings → Notifications.
Reminders are tracked in memory only, so a restart shortly before a starred
set starts may re-send its reminder once.

## Live Rewind

Enable "Live rewind" on a source to let viewers scrub backward while it is
actively recording, instead of only watching the live edge. While recording,
the app additionally transcodes the stream to H.264/AAC and segments it into
a rolling HLS playlist (default: last 30 minutes, configurable via the
"Live rewind window" setting); the WebUI plays that back through hls.js so
you get DVR-style seeking. Once the recording finishes, the HLS buffer is
deleted — the archival file (in its original codec/container) is the
permanent copy, and normal playback resumes from `/media/`.

This costs extra CPU per rewind-enabled source (it runs a second transcode
alongside the archival copy) and a bounded amount of temp disk space for the
rolling window, so it's opt-in per source rather than global.

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

### Versioning

The build version is `v<VERSION file>+<short commit sha>` (e.g. `v1.0.0+9838481`),
computed by `make build`/`make docker` and by CI, and shown in the WebUI
sidebar and Help tab. Bump the `VERSION` file for a new release; the commit
suffix is automatic. Run `make version` to print the computed string.

Environment variables:

| Variable | Default | Purpose |
| --- | --- | --- |
| `HTTP_ADDR` | `:8080` | Web server listen address |
| `CONFIG_PATH` | `/data/config/config.json` (Docker) or `./config/config.json` (native) | Persistent config path |
| `FINISHED_DIR` | `/data/recordings` (Docker) or `./data/recordings` (native) | Finished recordings |
| `TEMP_DIR` | `/data/incomplete` (Docker) or `./data/incomplete` (native) | Active partial recordings |
| `LOG_DIR` | `/data/logs` (Docker) or `./data/logs` (native) | Per-recording logs |
| `AUTH_USERNAME` | *(none — see Authentication)* | Login username, optional |
| `AUTH_PASSWORD` | *(none — see Authentication)* | Login password, optional |

Outside of Docker (e.g. `go run ./cmd/web` or a native binary on Windows/macOS/Linux),
the `/data` and `/app` defaults above are automatically swapped for paths
relative to the working directory, so the app has somewhere to write without
any of these variables being set.

## Authentication

The entire WebUI and API sit behind a login page (`/login`) backed by a
session cookie. There are two ways to manage credentials, and neither
requires editing files or environment variables unless you want to:

- **Setup wizard (default).** On first run, with no `AUTH_USERNAME`/
  `AUTH_PASSWORD` set, the app redirects to a one-time `/setup` page where you
  choose a username and password directly in the browser. They're hashed
  with bcrypt and saved to `auth.json` next to your config file. Change them
  any time from **Settings → Account** — no restart or redeploy needed.
- **Environment variables (for automated/Docker deployments).** Set
  `AUTH_USERNAME` and `AUTH_PASSWORD` explicitly (for example in
  `docker-compose.yml`) if you'd rather pin credentials externally:

  ```yaml
  environment:
    AUTH_USERNAME: yourname
    AUTH_PASSWORD: a-long-random-password
  ```

  When these are set, they always take priority over any saved credentials,
  and the Account settings form becomes read-only (change the environment
  variables and restart instead).

Either way, don't expose the port beyond localhost until credentials are in
place — the setup wizard is only reachable until you complete it once, so
there's no window where the app runs with a default or guessable password.

Source `streamlinkArgs`/`ffmpegArgs` are passed straight to those tools, so
treat WebUI access as equivalent to shell access to `streamlink`/`ffmpeg` on
the host — only share credentials with people you'd trust with that.

## Disclaimer

This project is not affiliated with or endorsed by Q-dance, DEFQON.1, Twitch,
YouTube, or any stream provider. Use it only where you have the right to record
and store the content.
