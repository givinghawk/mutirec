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

## Done (merged in PR #11)
- **Events tab deep linking**: source cards in the per-festival detail are now clickable (jumps to Sources tab, expands and highlights that source, via the existing `highlightSourceId` mechanism); edition cards jump straight into the Recordings library's event view instead of landing on the library home screen (`.ev-detail-source` / `.ev-detail-edition` + `addEventListener`, replacing the old inline-`onclick`-with-escaped-quotes approach).
- **Smart Match filename parsing overhaul** — the fuzzy matcher (`bestMatchSuggestion` in `main.go`) previously only understood YYYY-first dates and had zero artist-name awareness, so filenames like `DJ_Isaac_BLUE_Thursday_25_06_2026_Defqon_1_Sacred_Oath_HardDance.mp3` (day-first date, no clock time) mostly missed:
  - `guessTimeFromName` now also parses `DD_MM_YYYY`/`DD-MM-YYYY`/`DD.MM.YYYY`, disambiguating day/month order (when both are ≤12) using an embedded weekday name if present (e.g. "Thursday" + "25_06_2026" → confirms day-first). Returns a new `hasTimeOfDay` bool so date-only filenames (midnight default) don't get treated as if they had a real clock time.
  - `guessArtistFromName` extracts the artist name from the filename prefix (before the date), stripping a trailing weekday word and/or the recording's own channel name.
  - `artistSimilarity` does tolerant word-overlap scoring between the guessed artist and each archived set's name (ignores "dj"/"b2b"/"vs", handles combos like "DJ Isaac" matching inside "DJ Isaac B2B Adaro").
  - All signals feed into `candidateScore`, a single weighted score (replacing the old hand-rolled `betterCandidate` comparator) so the best candidate can be picked in one pass; "high" confidence requires the artist-name match to be corroborated by either exact time containment or a real channel/stage match (`stageMatch`, from the recording's folder name), not an artist-name coincidence alone.
  - `MatchSuggestion.GuessedArtist` is now returned so the Smart Match UI shows "Filename suggests: ..." even when no confident match was found, so the user has a starting point for manual assignment.
  - Added `cmd/web/match_test.go` with unit tests covering date parsing (including the weekday-disambiguation case), artist extraction, similarity scoring, and a full end-to-end scenario using the exact filename reported by the user - confirms "high" confidence with the correct artist/stage/day.
  - **Correction**: an earlier draft of this change added a `filenameContainsStage` heuristic based on misreading "Sacred Oath" in the example filename as a real stage name — it's actually that year's festival edition/theme name ("Defqon.1 - Sacred Oath"), and "BLUE" (already handled correctly by the existing folder-based channel match) is the real stage. Removed the heuristic entirely rather than leave a signal built on a wrong premise; the channel/date/artist signals already fully disambiguate without it.
- **Dev server portability fix**: the app previously only listened on `HTTP_ADDR` (default `:8080`) with no way to run on an arbitrary port; added `PORT` env var support (`main.go`) and switched `.claude/launch.json` to `autoPort: true` so the preview tooling can run alongside other things already bound to 8080.

## Done (this session)
- **Auto-reconnect / stream-health watchdog**: a source whose recording ends without having
  run for at least `minStableRecordingDuration` (60s) - a dropped connection, a bad/offline
  URL, etc. - now schedules an automatic retry with exponential backoff (`reconnectDelay`:
  5s, 10s, 20s, ... capped at 5 minutes) instead of either being retried every scheduler tick
  or left stopped until a manual restart. `evaluate()` skips sources still inside their
  backoff window; a recording that *does* run long enough clears the backoff. A manual
  stop/start (`stop()`, or clicking Record via `handleRecordAction`) is exempt from backoff
  entirely and clears it - `recording.manualStop` (an `atomic.Bool`, since `stop()` and
  `runRecording`'s goroutine touch it from different goroutines) distinguishes "user pressed
  stop" from "the stream died". Surfaced in `SourceStatus`/`state()` as a new `reconnecting`
  status with `reconnectAttempts`/`nextRetryAt`, rendered on the dashboard source card as an
  amber "Stream appears down - retrying in Xs (attempt N)" line with a matching pulsing dot.
  Backoff state lives in memory only (`App.retry`); a restart starts clean.
- **Loudness normalization**: new per-source `loudnessNormalize` bool. `ffmpegArgs` now
  chooses video/audio codecs independently (`-c:v ...` / `-c:a ...` instead of the old single
  `-c copy` shortcut, with an explicit `-c:s copy` added where video is also copied so
  subtitle handling doesn't change) so a stream-copied video can still have its audio
  re-encoded with a single-pass EBU R128 `loudnorm` filter (`loudnormFilter =
  "loudnorm=I=-16:TP=-1.5:LRA=11"`) - two-pass loudnorm needs to measure the whole file
  first, which isn't possible on a live recording. UI checkbox in the Source Manager next to
  "Audio only". Covered by `TestFFmpegArgsLoudnessNormalize` in the new `ffmpeg_test.go`
  (stream-copy, transcode, and audio-only combinations).
- **PWA installability**: `static/manifest.json` + a minimal `static/sw.js` (network-first,
  caches only the static app shell - `/`, `/app.css`, `/app.js`, manifest, icons - and
  explicitly never touches `/api/*`, `/media/*`, `/login`, or `/setup`, so an installed app
  never shows stale state). Generated `static/icons/icon-{192,512,512-maskable}.png` (a
  simple zinc/red "recording dot" glyph matching the existing theme, drawn programmatically
  since no image tooling was available - see the icon generator if it needs regenerating).
  `manifest.json`/`sw.js`/`icons/*` added to `isPublicPath` in `main.go` since the browser's
  install prompt and the login/setup pages need them unauthenticated. Manifest/icon links
  added to `index.html`, `login.html`, and `setup.html`; `app.js` registers the service
  worker on load.
- **First-run wizard → Quick Add handoff**: the setup wizard (`setup.html`) already had a
  System Check panel covering storage/hardware requirements, and the Sources tab already had
  a full "Quick Add Source" wizard (type presets, name/URL, Test Stream via
  `/api/sources/test`, create via `POST /api/sources`) - but first run ended at account
  creation with no path into it, landing on an empty dashboard. `handleSetup`'s success
  redirect now goes to `/?onboarding=1`; `app.js`'s `maybeStartOnboarding()` (called once from
  `refresh()`) detects that flag, strips it from the URL, switches to the Sources tab, and
  calls the existing `openWizard()` - no new wizard UI needed, just wiring the existing one
  into first run.
- **Smart Match: Festival-scoped disambiguation**: `bestMatchSuggestion` previously scored
  every `LibraryEvent`'s timetable globally, with no notion of which Festival a recording
  actually belongs to - a real risk once a library spans multiple editions/years that reuse
  the same stage-name convention (e.g. "RED") or share touring artists, since the
  channel/date/artist signals alone can't tell those apart. Sources already carry an explicit
  `FestivalID` (set by hand in the Sources tab's Event picker) and `LibraryEvent` already
  carries the matching field - just not wired into matching. Added `festivalIDForChannel`
  (maps a recording's folder/channel back to its source's `FestivalID` via `safeName`) and
  two new `matchCandidate` signals: `festivalMatch` (+50 score) when a candidate's event
  belongs to the source's linked Festival, `festivalConflict` (-60 score) when it belongs to
  a *different* one. A conflict on an otherwise-strong match is called out explicitly rather
  than silently downgraded ("looks like a strong match, but this recording's source is linked
  to a different Event... double check"), since that usually means either the source's Event
  link is wrong or the candidate genuinely is a different show. Covered by
  `TestBestMatchSuggestion_FestivalScoping` in `match_test.go` (two editions sharing a stage
  name and artist on the same day; the source's own Festival link breaks the tie either way,
  and an unlinked source still gets a match rather than being penalized).
- **Smart Match: split-recording detection**: auto-reconnect (this session's other change)
  means a single dropped/restarted stream now produces two separate recording files for what
  was originally one continuous set - and Smart Match would previously suggest both onto the
  same set with no indication there's a second file to reconcile. Added
  `flagSharedSetCandidates`, a post-process pass over one batch of suggestions that appends a
  note to every suggestion whose matched (event, set) pair is also the top pick for another
  recording in the same batch ("N other recording(s) in this batch also match this same set -
  likely duplicate files or parts split by a dropped/reconnected stream"). Covered by
  `TestFlagSharedSetCandidates`.
- **Preset Packs**: new `SourcePreset` type (`id`, `name`, `category`, optional `description`/
  `logoUrl`, and a `sources` array reusing the existing `Source` shape) bundled read-only via
  `//go:embed presets/presets.json` (no Dockerfile change needed, unlike `dq-timetable.json` -
  embedding bakes it into the binary regardless of working directory) and served by
  `GET /api/presets`. Sources tab has a new "Preset Packs" button/overlay
  (`openPresets`/`renderPresetsList`/`applyPreset` in `app.js`) listing each pack with an
  "Add"/"Added" button - applying one just `POST`s each not-yet-added source (matched by URL)
  through the existing `/api/sources` endpoint, same as the Quick Add wizard, so no new
  backend write path was needed. Seeded with 17 hardstyle DJ/streamer/event Twitch channels
  (HSU, VIORIT, The Smiler, Equal2, Rubaz, RealHardstyle, Pulse, Wasted Penguinz, CREST, Sven
  Carnage, Missterious, Rooler, United Music Events, HVRIZON, The Event Without Name,
  MizzBehave, GPF) - deliberately no Defqon preset, since its stages already ship as the
  default source list via `dq-timetable.json`/`sourcesFromTimetable`. No `logoUrl`s were set
  for any of these - a real Twitch avatar CDN URL can't be derived from a channel name/handle
  (it's a content-addressed hash assigned per-account), so one would have to be fabricated;
  the field is left empty rather than guessed. If avatars matter later, fetch them for real via
  Twitch's Helix API (needs a registered app + OAuth token, so it's a deliberate follow-up, not
  something to wire in speculatively) rather than guessing URLs.

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
- The festival/edition-name/genre tail of filenames like "..._Defqon_1_Sacred_Oath_HardDance"
  (festival name + that edition's theme name + genre) is intentionally not parsed at all -
  the channel, date, and artist-name signals already fully disambiguate which set a
  recording belongs to, so there was no need to also parse this tail. If multiple
  `LibraryEvent`s ever share both a stage name and overlapping dates (e.g. two editions
  using placeholder dates), the edition-name text could help disambiguate then - but don't
  add it speculatively before that's an actual problem.
- `artistSimilarity` is a simple word-overlap score - good enough for exact/near-exact
  names but won't catch typos or transliteration differences. Not worth a full edit-distance
  implementation unless real-world testing shows it's needed.

## Other backlog
- Add per-user accounts and role-based access (current auth is a single shared login).
- Consider server-side HLS restreaming for sources whose CDN blocks cross-origin playback.
- Add backup queue history with retry controls.
- Add Prometheus metrics and healthcheck endpoint.
- The reconnect backoff is per-source and in-memory only; if this app is ever run with more
  than one process/replica behind a shared config, backoff state won't be shared. Not a
  problem for the single-process deployment this app currently assumes.
- Add a Defqon.1 preset pack once there's a clean way to express "this is the same as the
  built-in default sources" without duplicating `dq-timetable.json`'s stage list by hand -
  not done yet since it'd just be a second copy of data that already ships by default.
  Consider more preset packs (other festivals/events) as they come up.

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
- **Defqon.1 recording filename convention** (from a real user example):
  `{Artist}_{Stage}_{Weekday}_{DD}_{MM}_{YYYY}_{Festival}_{EditionTheme}_{Genre}.ext`, e.g.
  `DJ_Isaac_BLUE_Thursday_25_06_2026_Defqon_1_Sacred_Oath_HardDance.mp3`. Stage names are
  short channel-style labels ("BLUE", "RED", "BLACK", etc.) that match the recording's own
  folder/channel - they are *not* the flowery per-edition theme name that follows the date
  (e.g. "Sacred Oath" is 2026's edition subtitle, not a stage). Don't guess meaning from a
  single example filename without checking - ask, or verify against how the surrounding
  fields (channel folder names, existing LibraryEvent names) are actually used elsewhere in
  the app before building matching logic around an assumption.
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
