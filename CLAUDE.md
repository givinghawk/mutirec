# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

MutiRec is a self-hosted, Docker-based stream recorder: a single Go binary
(`cmd/web`) serves both the WebUI (embedded static files) and the recording
engine (shells out to `streamlink`/`ffmpeg`/`ffprobe`). Config, users, and
recordings persist as JSON/files on disk — there is no database.

## Commands

```bash
make build     # go build -ldflags ... -o mutirec ./cmd/web
make run       # build + run locally
make test      # go test ./...
make vet       # go vet ./...
make fmt       # gofmt -s -w .
make check     # fmt + vet + test — run this before considering a change done
make docker    # build the container image (VERSION baked in via ldflags)
```

Single test / package:

```bash
go test ./cmd/web/... -run TestName -v
go test ./internal/disk/...
```

Local dev without Docker: `go run ./cmd/web` — it auto-swaps the Docker-style
`/data`, `/app` default paths for paths relative to the working directory, so
no env vars are required to just try it. `.claude/launch.json` runs it with
`autoPort: true` for the preview tool.

No JS build step — `cmd/web/static/*` (HTML/CSS/vanilla JS, Tailwind via CDN)
is served directly via Go's `embed.FS`. **Static files are baked in at
compile time**: after editing anything under `cmd/web/static/`, the running
process must be rebuilt/restarted, not just have the browser refreshed.

## Architecture

### Single process, in-memory state, JSON persistence

`App` (`cmd/web/main.go`) is the one long-lived object: an `AppConfig` guarded
by `mu sync.RWMutex`, plus several independent smaller mutex+map pairs for
subsystems that don't need to block on the whole config (`sessions`,
`retry`/reconnect backoff, `oauthState`, `shareNonces`, `shareJobs`,
`hashCache`). Read a config snapshot via `a.snapshotConfig()`; mutate under
`a.mu.Lock()` then `a.persist(cfg)` to write it back to
`config.json`/`CONFIG_PATH`. Everything except sessions/users/hash-cache/retry
state lives inside `AppConfig` and round-trips through that one JSON file —
there is deliberately no database.

Two on-disk stores sit next to `config.json`: `users.json` (accounts,
`auth.go`) and, since the P2P/uploads work, `uploads/` (branding images,
content-addressed by sha256) and `thumbnails/` (recording thumbnails,
addressed by a hash of the recording's relative path). A legacy single-user
`auth.json` migrates into `users.json` automatically on first boot under the
new code.

### Recording lifecycle

`scheduler()` runs on a ticker and calls `evaluate()`, which starts a
`recording` goroutine (`a.start(src)` → `runRecording`) for every
enabled+auto-record source that isn't already active and isn't inside a
reconnect backoff window (`retryBlocked`). A source that stops before
`minStableRecordingDuration` (60s) schedules an automatic retry with
exponential backoff (`reconnectDelay`); one that *was* stable and then drops
opens a 10-minute "visible" reconnect window (dashboard shows retry countdown)
before going back to silent background polling. A manual stop/start
(`stop()`/`handleRecordAction`) always bypasses backoff.

`execute()` builds the `streamlink`/`ffmpeg` command line (`ffmpegArgs`,
hardware-encoder selection, loudness normalization, optional HLS rewind
segmenting) and runs it under `rec.ctx`, writing to a `.tmp`-style file in
`TEMP_DIR` before an atomic rename into `FINISHED_DIR/<source>/` on success.
After a successful finish, `runRecording` also fires off (non-blocking,
`go a.generateThumbnail(...)`) an auto-thumbnail grab for video sources.

### HTTP layer

All routes are registered in `(a *App) routes()`; everything goes through
`requireAuth` → `rbacAllowed(method, path, role)` (`auth.go`) except the
handful of paths in `isPublicPath` (login/setup pages, PWA
manifest/icons/service-worker, and the two P2P endpoints that are
token-authed instead of session-authed: `/api/share/ping`,
`/api/share/get/`). RBAC is centralized there rather than scattered per
handler — any authenticated role can read almost everything, but mutating
requests need an admin role except a short self-service allow-list (own
password, own Discord link/unlink, logout). `redactSecrets` strips
SMTP/Discord/rclone secrets from `/api/state`/`/api/config` responses before
they reach a non-admin.

Background jobs that outlive one request (P2P share imports) follow the same
shape: a handler validates input, starts a goroutine, and returns an ID
immediately; a separate polling endpoint reads a mutex-guarded status struct.
See `ShareJob`/`runShareImportJob` in `sharing.go` for the current instance of
this pattern — reuse it rather than reinventing another background-job
mechanism.

### Frontend conventions (`cmd/web/static/app.js`, single file, no bundler)

- `$(id)` for `document.getElementById`; `api(path, opts)` wraps `fetch`,
  auto-toasts non-2xx responses and redirects to `/login` on 401.
- `escapeHtml`/`escapeAttr` for anything interpolated into `innerHTML`.
- Custom dropdowns: `setupCustomDropdowns()` / `setDropdownOptions(id, opts)` /
  `setDropdownValue(id, value)` — `id` is the base name without the
  `-dropdown` suffix; the hidden input backing the value is created inside
  the container the first time options are set.
- Dynamically-rendered card lists: bind interactions with
  `document.querySelectorAll(...).forEach(el => el.addEventListener(...))`
  right after setting `innerHTML`, using `data-*` attributes — not inline
  `onclick="..."` strings.
- `data-admin-only` + `applyRoleVisibility()` hides admin-only UI from viewer
  accounts client-side; the real enforcement is always server-side
  `rbacAllowed`, this is just UX.
- Modals follow one pattern: `.modal-overlay`/`.modal-panel`, toggle the
  `hidden` class to open/close, close on backdrop click and an explicit ×
  button.
- Content-addressed images (branding uploads, recording thumbnails) are
  requested directly by URL and a 404 is handled client-side (`onerror`)
  rather than checked for existence first.

### Cross-install data exchange

Two features move data between separate MutiRec installs (which don't share
IDs): the shareable matchfile export/import and P2P sharing. Both denormalize
to human-readable names (event/festival name, not local `newID()` values) and
resolve-or-create the matching local record by name on import
(`resolveOrCreateLibraryEventLocked`) — reuse this pattern for anything else
that needs to move data between installs, since local IDs are never stable
across separate config files.

## Notable non-obvious constraints

- Source `streamlinkArgs`/`ffmpegArgs` are passed straight to those binaries —
  treat WebUI/API access as equivalent to shell access on the host.
- Sessions, reconnect backoff, OAuth pending-state, share nonces, share jobs,
  and the recording-hash cache are all in-memory only; a process restart
  clears them (this is intentional, not a bug to fix).
- The repo ships with **no bundled festival/timetable/source data** — this was
  deliberately stripped (see TODO.md "Rebrand" entry) to avoid trademark
  entanglement. Don't reintroduce real, specific festival/artist names into
  code, test fixtures, or presets; invent placeholders instead.
- `go vet`'s copylocks check matters here: types with an internal `sync.Mutex`
  (e.g. `ShareJob`) must never be copied directly — always produce a plain
  DTO (`.view()`) for anything that needs to cross a JSON boundary or leave
  the type's own methods.

## Where to look first

- `cmd/web/main.go` — config types, scheduler/recording lifecycle, most CRUD
  handlers (sources, timetable, library events/festivals/organisations, Smart
  Match).
- `cmd/web/auth.go` — users, sessions, RBAC, Discord OAuth.
- `cmd/web/sharing.go` — P2P share creation/import, background job tracking.
- `cmd/web/uploads.go` — branding image uploads, recording thumbnails.
- `cmd/web/syscheck.go` — first-run system/hardware checks.
- `cmd/web/static/app.js` — all frontend logic (one file, ~3500 lines).
- `TODO.md` — running log of what's been built each session and why, plus a
  "Patterns established" section at the bottom worth reading before adding a
  new feature in an area it covers.
