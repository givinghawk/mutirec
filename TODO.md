# Roadmap

## Done (merged in PR #9)
- Video.js migration for all players
- Custom dropdowns everywhere (container, transcode, live rewind, etc.)
- Watch (Live View) tab with source grouping and PiP
- Dashboard source-group accordions and charts
- Smart Match wizard (fuzzy timetable matching for unorganized recordings)

## Done (merged in PR #10)
- **Organisations**: `Organisation` type + CRUD at `/api/organisations`, `OrganisationID` on `Festival`
- **FestivalID on LibraryEvent**: links editions back to their franchise for the Events tab
- **Events nav tab**: grid of Festivals/Organisations, upcoming timetable sets, active editions, per-festival detail view (sources, editions, timetable, stats), full CRUD for both Organisations and Festivals from this tab
- **Recordings player page**: replaced modal overlay with a full-page `<section id="player">` with two-column layout (player + details left, recommendations sidebar right), back button tracks which tab you came from, download button
- **Day/stage split in Recordings event view**: Channel > Day > horizontal set row grouping (days only shown as sub-headers when a channel spans multiple days)
- **Tracklist field**: `RecordingMeta.Tracklist` (newline-separated), editable from the Organize modal, displayed on the recording player page
- **Hash-based shareable match file**: `GET /api/recordings/matchfile/export` / `POST /api/recordings/matchfile/import`, denormalized by name so it works across installs. Export/Import buttons in the Recordings toolbar.

## Done (this session)
- **Events tab deep linking**: source cards in the per-festival detail are now clickable (jumps to Sources tab, expands and highlights that source, via the existing `highlightSourceId` mechanism); edition cards jump straight into the Recordings library's event view instead of landing on the library home screen (`.ev-detail-source` / `.ev-detail-edition` + `addEventListener`, replacing the old inline-`onclick`-with-escaped-quotes approach).
- **Smart Match filename parsing overhaul** — the fuzzy matcher (`bestMatchSuggestion` in `main.go`) previously only understood YYYY-first dates and had zero artist-name awareness, so filenames like `DJ_Isaac_BLUE_Thursday_25_06_2026_Defqon_1_Sacred_Oath_HardDance.mp3` (day-first date, no clock time) mostly missed:
  - `guessTimeFromName` now also parses `DD_MM_YYYY`/`DD-MM-YYYY`/`DD.MM.YYYY`, disambiguating day/month order (when both are ≤12) using an embedded weekday name if present (e.g. "Thursday" + "25_06_2026" → confirms day-first). Returns a new `hasTimeOfDay` bool so date-only filenames (midnight default) don't get treated as if they had a real clock time.
  - `guessArtistFromName` extracts the artist name from the filename prefix (before the date), stripping a trailing weekday word and/or the recording's own channel name.
  - `artistSimilarity` does tolerant word-overlap scoring between the guessed artist and each archived set's name (ignores "dj"/"b2b"/"vs", handles combos like "DJ Isaac" matching inside "DJ Isaac B2B Adaro").
  - `filenameContainsStage` checks whether an archived stage's *real* name (e.g. "Sacred Oath") appears anywhere in the filename - this catches the case where the folder/channel name is just a video-feed label ("BLUE") that doesn't match the actual stage name at all, which the old channel-only matching had no way to recover from.
  - All signals feed into `candidateScore`, a single weighted score (replacing the old hand-rolled `betterCandidate` comparator) so the best candidate can be picked in one pass; confidence labels/reasons shown to the user were updated to explain *which* signal drove the match (exact time window, filename-matched artist/stage, or same-day-only).
  - `MatchSuggestion.GuessedArtist` is now returned so the Smart Match UI shows "Filename suggests: ..." even when no confident match was found, so the user has a starting point for manual assignment.
  - Added `cmd/web/match_test.go` with unit tests covering date parsing (including the weekday-disambiguation case), artist extraction, similarity scoring, and a full end-to-end scenario using the exact filename reported by the user - confirms "high" confidence with correct artist/stage even though the channel folder doesn't match the real stage name.
- **Dev server portability fix**: the app previously only listened on `HTTP_ADDR` (default `:8080`) with no way to run on an arbitrary port; added `PORT` env var support (`main.go`) and switched `.claude/launch.json` to `autoPort: true` so the preview tooling can run alongside other things already bound to 8080.

## Remaining (in suggested order)

### 1. Organisation linking from the Sources tab
- The Organisation editor is currently only reachable via the Events tab grid headers.
  Should also be reachable from the Sources tab's Festival picker (e.g. an inline "manage
  organisations" link), so users don't have to switch tabs to set this up while adding a
  new live source.

### 2. Matchfile follow-ups (optional, not blocking)
- Import currently only matches on exact hash; consider a summary/preview step (like Smart
  Match's review list) before applying, so users can see what's about to change rather than
  it applying silently.
- No dedupe/merge if the same hash appears twice in one import file with different metadata
  — last one in the array wins. Unlikely in practice (a well-formed export has one entry per
  hash) but worth a defensive check if this becomes user-facing/shared widely.

### 3. Smart Match follow-ups (optional, not blocking)
- The festival/stage-name/genre tail of filenames like "..._Defqon_1_Sacred_Oath_HardDance"
  is not fully parsed (only the stage-name substring is checked, via
  `filenameContainsStage`). Could extract genre as informational metadata, or use the
  festival-name substring to narrow which `LibraryEvent` to search first when multiple
  events share stage names.
- `artistSimilarity` is a simple word-overlap score - good enough for exact/near-exact
  names but won't catch typos or transliteration differences. Not worth a full edit-distance
  implementation unless real-world testing shows it's needed.

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
- Content-addressed cross-install data: when exporting anything meant to be applied on a
  *different* install (like the matchfile), denormalize to human-readable names instead of
  local IDs, and resolve/auto-create the matching local record by name on import - IDs
  generated by `newID()` are never stable across separate config files.
- Dynamically-rendered card lists (`.ev-detail-source`, `.ev-detail-edition`, etc.): bind
  interactions with `document.querySelectorAll(...).forEach(el => el.addEventListener(...))`
  right after setting `innerHTML`, using `data-*` attributes - not inline `onclick="..."`
  strings with manually escaped quotes, which are fragile and hard to read.
- Combining multiple weak signals into one match (Smart Match's `candidateScore`): give each
  signal a weight and sum them, then pick the max - much easier to extend/tune than a
  hand-written cascade of comparator conditions, and the confidence-label logic can still
  inspect the winning candidate's individual flags afterward for a human-readable reason.
- **ID collisions**: the activity log already uses `id="events"` — new sections must use
  distinct IDs (e.g. `id="events-tab"`).
- **`preview_click` is unreliable on this app** across sessions - this app's `refresh()`
  polling loop periodically replaces DOM nodes, so coordinate-based clicks can land after a
  re-render has already swapped the target out from under them. Prefer driving interactions
  through `preview_eval` by calling the relevant JS function directly (e.g.
  `document.querySelector(...).click()` inside a single eval, or calling the handler
  function itself like `approveSuggestion(0)`), and always verify via a follow-up `fetch()`
  against the real API rather than trusting a screenshot alone.
- **Always test flows in a real browser preview before calling something done** — `go
  build`/`go vet` alone missed two real bugs in a prior session. Also: go embed bakes
  static files at compile time, so the preview server must be restarted (not just reloaded)
  after editing HTML/JS/CSS. For backend-only logic (hashing, matchfile CRUD, Smart Match
  scoring), a Go unit test (see `match_test.go`) is more reliable than browser testing for
  pinning down exact parsing/scoring behavior - use the preview browser to confirm the UI
  wiring on top of that, not to validate the algorithm itself.
