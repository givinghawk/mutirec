# Roadmap

- First-run setup wizard now covers credentials (no env vars required); still missing guided source presets, test-record buttons, and storage checks.
- Add per-user accounts and role-based access (current auth is a single shared login).
- Consider server-side HLS restreaming for sources whose CDN blocks cross-origin playback (non-recording live playback still uses a direct redirect + hls.js).
- Add backup queue history with retry controls.
- Add Prometheus metrics and healthcheck endpoint.

## In-progress big feature request (see PR #9, branch `claude/videojs-custom-dropdowns`)

A large multi-session request covering: Video.js migration, custom dropdowns everywhere,
a "Watch" live-view tab, Dashboard source groups/charts, and a Smart Match wizard - all
**done** and in PR #9 (check `gh pr view 9` / the GitHub API first; if merged, branch off
`master` fresh instead of continuing on that branch). Still remaining, in suggested order:

### 1. Organisations + full Events tab (biggest remaining piece)
- Add an `Organisation` Go type in `cmd/web/main.go`, mirroring `Festival` exactly
  (ID/Name/Description/Color/LogoURL, `AppConfig.Organisations`, CRUD at
  `/api/organisations` copying `handleFestivals`/`handleFestivalItem` verbatim, nil-safety
  in `normalizeConfig`).
- Give `Festival` an `OrganisationID` field (currently absent - was deliberately deferred).
- Give `LibraryEvent` a `FestivalID` field (currently absent) - this is the missing link
  needed to list an event's "previous editions".
- New "Events" nav tab: a dashboard sub-view (calendar of upcoming timetable sets, list of
  "active" LibraryEvents - today within start/end date, grid of Festivals/Organisations),
  plus a per-Festival sub-page (client-side show/hide `<div>`, same pattern as the
  Recordings library's event view) listing: its Sources (`Source.FestivalID` match), its
  live timetable, its LibraryEvents/editions (`LibraryEvent.FestivalID` match), a link into
  the Recordings library filtered to that event, and basic stats (recording count, total
  hours, storage used - reuse `disk.Usage.PerStage` and `/api/recordings` data).

### 2. Recordings player page (replace the modal with a real page)
- `openRecordingPlayer()` in `app.js` currently opens `#recording-player-overlay` as a
  modal. Convert to a full-page view instead (new `<section>`, shown/hidden like other
  tabs) with a two-column layout: player + details on the left, recommendations sidebar on
  the right. Reuse `setupCustomPlayerControls`/waveform/visualizer wiring almost verbatim -
  just move the markup out of the modal.
- Add "More from this channel/event/organisation" rows, reusing the existing
  `.lib-row-scroll`/`.lib-set-card` markup pattern from the Recordings library.
- Add tracklist + description + "about the artist" panels. Simplest data model: add a
  `Tracklist string` field to `RecordingMeta` (newline-separated, nil-safe).
- Update `.lib-set-play` click handlers to navigate to the new page instead of opening the
  modal.

### 3. Day/stage split in the Recordings event view
- `renderLibraryEventView()` in `app.js` currently groups by Channel only
  (`.lib-channel-row`). Add a day sub-grouping (extract the date from `r.start`) either as
  sub-headers within each channel's row, or restructure to Channel > Day > horizontal set
  row.

### 4. Shareable hash-based match file
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

### Patterns established this session - reuse these rather than reinventing
- Custom dropdowns: `setupCustomDropdowns()` / `setDropdownOptions(id, options, opts)` /
  `setDropdownValue(id, value)` in `app.js` - use for any new select-like UI.
- Simple CRUD handler pair: `handleFestivals` / `handleFestivalItem` in `main.go` is the
  cleanest template to copy for `Organisation`.
- Video.js player: `ensureXPlayer()` (lazy singleton) + `setupXPlayerControls(player)` (bind
  once) - see `ensureWatchPlayer`/`setupWatchPlayerControls` and
  `ensureRecPlayer`/`setupCustomPlayerControls`.
- Visualizer: `startVisualizer(videoEl, canvasId)` / `stopVisualizer(canvasId)` already
  support multiple independent instances - just pick a new canvas id per player.
- PiP: `setupPiP(button, getVideoElFn)` is reusable as-is for any new player.
- Accordion groups: `.source-group`/`.source-group-head`/`.source-group-body` CSS +
  `toggleSourceGroup`/`openGroupIds` in `app.js` - same pattern fits the Events tab's
  per-organisation grouping.
- **Always test flows in a real browser preview before calling something done** - `go
  build`/`go vet` alone missed two real bugs this session (a WaveSurfer `media`+`url`
  option resetting playback, and an uncaught error from attaching a listener to a
  dynamically-created dropdown element before it existed, which silently broke the theme
  picker on every page load).
