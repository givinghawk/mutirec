# Roadmap

## Done (merged in PR #9)
- Video.js migration for all players
- Custom dropdowns everywhere (container, transcode, live rewind, etc.)
- Watch (Live View) tab with source grouping and PiP
- Dashboard source-group accordions and charts
- Smart Match wizard (fuzzy timetable matching for unorganized recordings)

## Done (this session)
- **Organisations**: `Organisation` type + CRUD at `/api/organisations`, `OrganisationID` on `Festival`
- **FestivalID on LibraryEvent**: links editions back to their franchise for the Events tab
- **Events nav tab**: grid of Festivals/Organisations, upcoming timetable sets, active editions, per-festival detail view (sources, editions, timetable, stats), full CRUD for both Organisations and Festivals from this tab
- **Recordings player page**: replaced modal overlay with a full-page `<section id="player">` with two-column layout (player + details left, recommendations sidebar right), back button tracks which tab you came from, download button
- **Day/stage split in Recordings event view**: Channel > Day > horizontal set row grouping (days only shown as sub-headers when a channel spans multiple days)

## Remaining (in suggested order)

### 1. Shareable hash-based match file
- Hash every recording's content (`crypto/sha256`, streamed - don't load whole files into
  memory) and store `hash -> RecordingMeta`-like data keyed by hash instead of path (unlike
  the existing path-keyed `RecordingMeta`), since hashes - unlike paths - match across
  different users' copies of the same file.
- New endpoints: `GET /api/recordings/matchfile/export` (JSON array of
  `{hash, eventId, setId, artist, start, end, eventName, festivalName, stageName}`, with
  enough denormalized context to be useful without requiring the importer to already have
  the same LibraryEvent) and `POST /api/recordings/matchfile/import` (hash local unorganized
  recordings, apply metadata on hash match - essentially an exact-match sibling to the
  fuzzy Smart Match wizard already built).
- Cache computed hashes (keyed by path + mtime + size) so re-scans don't re-hash unchanged
  files every time - hashing a large library on every request would be slow.
- Frontend: export/import buttons, probably alongside the existing Smart Match UI in the
  Recordings tab.

### 2. Tracklist + description on recording player
- Add a `Tracklist string` field to `RecordingMeta` (newline-separated, nil-safe).
- The player page already has the `#rec-tracklist-panel` and `#rec-tracklist` elements wired
  up — just needs the data model and an edit UI (probably a small textarea in the Organize
  modal alongside existing fields).

### 3. Events tab polish / deeper linking
- Source cards in the per-festival detail should be clickable (jump to Sources, highlight
  that source) — currently they show status text but aren't actionable.
- Edition cards should open in the Recordings library directly (the `openLibraryEvent` call
  in `renderEvFestivalDetail` is correct but navigates to recordings tab which then needs an
  extra click to reach the event view — consider deeplink via URL hash).
- Organisation editor currently only accessible via the Events tab grid headers; should also
  allow linking an existing Org to a Festival from the Sources tab's festival picker.

## Other backlog
- First-run setup wizard: still missing guided source presets, test-record buttons, and storage checks.
- Add per-user accounts and role-based access (current auth is a single shared login).
- Consider server-side HLS restreaming for sources whose CDN blocks cross-origin playback.
- Add backup queue history with retry controls.
- Add Prometheus metrics and healthcheck endpoint.

## Patterns established - reuse these rather than reinventing
- Custom dropdowns: `setupCustomDropdowns()` / `setDropdownOptions(id, options, opts)` /
  `setDropdownValue(id, value)` in `app.js` — `id` is the base name without `-dropdown`
  suffix; the function creates the hidden input inside the container.
- Simple CRUD handler pair: `handleFestivals` / `handleFestivalItem` in `main.go` is the
  cleanest template to copy for new resource types.
- View switching: `switchToView(viewId)` shows/hides `.view` sections and highlights the
  matching nav button; for pseudo-tabs without a nav button (like the player page), just
  call it directly and track the previous view in a `let xPreviousView` variable.
- Video.js player: `ensureXPlayer()` (lazy singleton) + `setupXPlayerControls(player)` (bind
  once) - see `ensureWatchPlayer`/`setupWatchPlayerControls` and
  `ensureRecPlayer`/`setupCustomPlayerControls`.
- Visualizer: `startVisualizer(videoEl, canvasId)` / `stopVisualizer(canvasId)` already
  support multiple independent instances - just pick a new canvas id per player.
- PiP: `setupPiP(button, getVideoElFn)` is reusable as-is for any new player.
- Accordion groups: `.source-group`/`.source-group-head`/`.source-group-body` CSS +
  `toggleSourceGroup`/`openGroupIds` in `app.js` — same pattern fits the Events tab's
  per-organisation grouping.
- **ID collisions**: the activity log already uses `id="events"` — new sections must use
  distinct IDs (e.g. `id="events-tab"`).
- **Always test flows in a real browser preview before calling something done** — `go
  build`/`go vet` alone missed two real bugs in a prior session. Also: go embed bakes
  static files at compile time, so the preview server must be restarted (not just reloaded)
  after editing HTML/JS/CSS.
