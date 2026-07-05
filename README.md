# MutiRec

### *Muti-* as in **Mutual** — record together, not alone.

[![Build](https://github.com/givinghawk/mutirec/actions/workflows/container.yml/badge.svg)](https://github.com/givinghawk/mutirec/actions/workflows/container.yml)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)
[![Go](https://img.shields.io/badge/go-1.26-00ADD8?logo=go&logoColor=white)](go.mod)
[![Container](https://img.shields.io/badge/container-ghcr.io-2496ED?logo=docker&logoColor=white)](https://github.com/givinghawk/mutirec/pkgs/container/mutirec)

> **Why "MutiRec"?** It started as a typo — *multirec* missing its "l". We
> kept it, because the shorter name fits better: **Mutual Recorder**. Not one
> person babysitting one stream, but a shared setup — multiple sources,
> multiple people with their own logins, one library everyone organizes
> together. The typo turned out to be the better name.

**MutiRec** is a self-hosted, Docker-based stream recorder with a dark
Tailwind WebUI. Point it at Twitch, YouTube, or any Streamlink/raw HTTP/HLS
source, hand out accounts to the people helping you record, and let it run
unattended through an entire festival weekend.

It ships tuned for hardstyle festival weekends (DEFQON.1's stages are the
default source list, and a handful of well-known DJs/streamers are one click
away as Preset Packs) but there's nothing festival-specific about the
recorder itself — add any source you like.

---

## Contents

- [Features](#features)
- [Quick Start](#quick-start)
- [Docker Compose](#docker-compose)
- [Sources](#sources)
- [Timetable](#timetable)
- [Live Rewind](#live-rewind)
- [Preset Packs](#preset-packs)
- [Auto-Reconnect](#auto-reconnect)
- [Progressive Web App](#progressive-web-app)
- [Storage](#storage)
- [Backups](#backups)
- [Notifications](#notifications)
- [Hardware Transcoding](#hardware-transcoding)
- [Development](#development)
- [Authentication and Users](#authentication-and-users)
- [Disclaimer](#disclaimer)

## Features

**Recording**

- Twitch and YouTube via `streamlink`, piped into FFmpeg; raw HTTP/HLS/DASH recorded directly with FFmpeg.
- Per-source enable, auto-record, audio-only, transcode, quality, container, and colour settings.
- Optional hardware-accelerated transcoding (CUDA/NVENC, Quick Sync, VAAPI) and single-pass loudness normalization (EBU R128).
- Auto-reconnect with exponential backoff for a source that drops mid-stream, silent until it's actually been live.
- Optional live rewind: scrub backward within an in-progress recording via a rolling HLS buffer.
- Disk free-space guard that pauses recording below 1 GB free and warns earlier.

**Organizing & discovery**

- Visual timetable editor (day tabs, click-to-edit/add sets), with raw JSON editing still available.
- Optional timetable lookup/import from [timetable.lol](https://timetable.lol) for hundreds of festivals.
- Recordings library with search/filter, plus Smart Match to suggest which archived set an unsorted recording belongs to.
- Events tab: Organisations → Festivals → yearly editions, so old recordings stay tied to the right franchise across years.
- Preset Packs: bundled, ready-to-add sources for well-known DJs/streamers/events — one click, no URLs to hand-type.

**Accounts & security**

- Multi-user accounts with admin/viewer roles, managed from Settings → Users.
- Optional Discord OAuth as a faster login for an *existing* account — it never creates one on its own.
- Session-based login in front of the whole app (WebUI and API), with a one-time setup wizard on first run.

**Notifications & backups**

- Star any timetable set for a Discord/SMTP reminder before it starts.
- SMTP and Discord webhook notifications when recordings finish.
- Optional `rclone` backups to Dropbox, Google Drive, S3-compatible storage, and more.

**Nice touches**

- Installable as a PWA for quick access to the dashboard and Watch tab from a phone or desktop.
- Custom app name, logo, colour scheme, and CSS.
- One-click stream test/resolve before saving a source, to catch bad URLs or qualities early.
- Optional `.nfo` files beside completed recordings; toast notifications surface API/server errors directly in the WebUI.

## Quick Start

```bash
docker compose up -d
```

Open:

```text
http://localhost:8080
```

You'll land on a one-time setup page to create the first account — always an
admin, nothing to configure by hand first. Further accounts (and their
roles) are managed from Settings → Users once you're in. See
[Authentication and Users](#authentication-and-users) below for the full
picture, including environment-pinned credentials and Discord login.

On first start the app creates:

```text
data/
  config/config.json
  config/users.json
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
    image: ghcr.io/givinghawk/mutirec:latest
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
- Optional loudness normalization (forces audio to be re-encoded even if video is stream-copied).
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

## Preset Packs

The Sources tab has a "Preset Packs" button next to "+ Add Source" that lists
bundled, ready-to-add sources - currently a set of well-known hardstyle
DJs/streamers/events on Twitch - so you don't have to hand-type their URLs.
Clicking "Add" on a pack adds its source(s) the same way as adding one
manually (enabled, but not auto-recording), and is a no-op if you've already
added it. Presets are bundled read-only with the app (`cmd/web/presets/presets.json`,
served via `GET /api/presets`); they're a starting point, not a restriction -
edit or delete the resulting source like any other afterward. There's no
DEFQON.1 preset since its stages already ship as the default source list.

## Auto-Reconnect

Every enabled, auto-record source that isn't currently live gets retried by
the scheduler automatically, with exponential backoff (5s, 10s, 20s, ... up
to a 5 minute cap) so a source that's genuinely offline isn't hammered with a
restart attempt on every scheduler tick. Most of the time this is silent -
a source waiting for its DJ/event to go live for the first time doesn't show
up in the dashboard or event log at all, since that's just normal background
polling, not a problem.

If a source *was* confirmed live (recorded for at least a minute) and then
stops - a dropped connection, a brief outage, or just the broadcaster ending
their stream - retry attempts for the next 10 minutes are surfaced: the
dashboard shows a "reconnecting" status with a countdown and attempt count,
and each attempt is logged. If nothing comes back within those 10 minutes,
it quietly goes back to silent background retries (a final "no reconnect
within 10m0s" note is logged once) - by that point it's more likely the
stream is over for now than that it's about to come back any second.
Clicking Record on a source clears its backoff and retries immediately.

## Progressive Web App

The WebUI can be installed to a phone or desktop home screen (look for
"Add to Home Screen" / the browser's install icon). Only the static app
shell is cached for offline installability - live state, recordings, and API
calls always go straight to the network, so the installed app never shows
stale data.

## Storage

The recorder checks free space before starting automatic recordings. Defaults:

- Warning threshold: 5 GB free.
- Stop threshold: 1 GB free.

For SMB or NFS storage, mount the share on the Docker host and bind it into the
container:

```yaml
volumes:
  - /mnt/mutirec-recordings:/data/recordings
```

Host mounting is the most predictable approach across Linux, macOS, Windows,
NAS systems, and Docker Desktop.

## Backups

The image includes `rclone`. Mount an `rclone.conf`, enable backups in the UI,
and set a remote path such as:

```text
gdrive:mutirec-recordings
s3:my-bucket/mutirec
dropbox:mutirec
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

## Authentication and Users

The entire WebUI and API sit behind a login page (`/login`) backed by a
session cookie. On first run, with no `AUTH_USERNAME`/`AUTH_PASSWORD` set,
the app redirects to a one-time `/setup` page where you choose a username and
password for the first account, which is always an admin. From there, admins
manage further accounts from **Settings → Users** - this is the "mutual"
part: hand out a login instead of a shared password, and everyone's actions
show up under their own name in the event log.

### Roles

- **Admin** — full access: sources, timetable, settings, backups, and other
  users.
- **Viewer** — can watch live sources, browse and organize recordings, and
  manage their own account (including linking Discord), but can't change
  sources, settings, or users. Secrets (SMTP password, Discord webhook/OAuth
  client secret, rclone args) are never sent to a viewer's browser at all.

There's always at least one admin — the last remaining admin account can't be
demoted or deleted (from either the Users tab or the API), so you can't
accidentally lock yourself out of managing the instance.

### Environment variables (for automated/Docker deployments)

Set `AUTH_USERNAME` and `AUTH_PASSWORD` (for example in `docker-compose.yml`)
to pin one extra admin login externally, on top of whatever's in the Users
tab:

```yaml
environment:
  AUTH_USERNAME: yourname
  AUTH_PASSWORD: a-long-random-password
```

This account is always an admin and is read-only in Settings → Account
(change the environment variables and restart instead) - it doesn't replace
or block the Users tab, it's just an extra fixed login.

### Discord login

Users can also sign in with Discord, but only as a faster login for an
*existing* account - authorizing with Discord can never create a new account
by itself. To set it up:

1. Create an application at
   [discord.com/developers/applications](https://discord.com/developers/applications),
   add an OAuth2 redirect matching `https://your-domain/api/auth/discord/callback`
   exactly (same path for both the login button and account linking), and
   copy its Client ID/Secret.
2. Paste those into **Settings → Discord Login (Admin)** along with the same
   redirect URL, and enable it.
3. Each user links their own Discord account from **Settings → Account** →
   "Link Discord" while signed in normally. After that, the login page shows
   a "Log in with Discord" button that works for their account too.

### General

Either way, don't expose the port beyond localhost until credentials are in
place — the setup wizard is only reachable until you complete it once, so
there's no window where the app runs with a default or guessable password.

Source `streamlinkArgs`/`ffmpegArgs` are passed straight to those tools, so
treat WebUI access as equivalent to shell access to `streamlink`/`ffmpeg` on
the host — only share admin accounts with people you'd trust with that
(viewers never reach source configuration at all).

## Disclaimer

This project is not affiliated with or endorsed by Q-dance, DEFQON.1, Twitch,
YouTube, or any stream provider. Use it only where you have the right to record
and store the content.
