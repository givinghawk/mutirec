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
- **Smart Match filename parsing overhaul** — the fuzzy matcher (`bestMatchSuggestion` in `main.go`) previously only understood YYYY-first dates and had zero artist-name awareness, so filenames like `DJ_Vertex_BLUE_Thursday_25_06_2026_Neonbeat_Prime_Directive_HardDance.mp3` (day-first date, no clock time) mostly missed:
  - `guessTimeFromName` now also parses `DD_MM_YYYY`/`DD-MM-YYYY`/`DD.MM.YYYY`, disambiguating day/month order (when both are ≤12) using an embedded weekday name if present (e.g. "Thursday" + "25_06_2026" → confirms day-first). Returns a new `hasTimeOfDay` bool so date-only filenames (midnight default) don't get treated as if they had a real clock time.
  - `guessArtistFromName` extracts the artist name from the filename prefix (before the date), stripping a trailing weekday word and/or the recording's own channel name.
  - `artistSimilarity` does tolerant word-overlap scoring between the guessed artist and each archived set's name (ignores "dj"/"b2b"/"vs", handles combos like "DJ Vertex" matching inside "DJ Vertex B2B Fenrix").
  - All signals feed into `candidateScore`, a single weighted score (replacing the old hand-rolled `betterCandidate` comparator) so the best candidate can be picked in one pass; "high" confidence requires the artist-name match to be corroborated by either exact time containment or a real channel/stage match (`stageMatch`, from the recording's folder name), not an artist-name coincidence alone.
  - `MatchSuggestion.GuessedArtist` is now returned so the Smart Match UI shows "Filename suggests: ..." even when no confident match was found, so the user has a starting point for manual assignment.
  - Added `cmd/web/match_test.go` with unit tests covering date parsing (including the weekday-disambiguation case), artist extraction, similarity scoring, and a full end-to-end scenario using the exact filename reported by the user - confirms "high" confidence with the correct artist/stage/day.
  - **Correction**: an earlier draft of this change added a `filenameContainsStage` heuristic based on misreading "Prime Directive" in the example filename as a real stage name — it's actually that year's festival edition/theme name ("Neonbeat - Prime Directive"), and "BLUE" (already handled correctly by the existing folder-based channel match) is the real stage. Removed the heuristic entirely rather than leave a signal built on a wrong premise; the channel/date/artist signals already fully disambiguate without it.
- **Dev server portability fix**: the app previously only listened on `HTTP_ADDR` (default `:8080`) with no way to run on an arbitrary port; added `PORT` env var support (`main.go`) and switched `.claude/launch.json` to `autoPort: true` so the preview tooling can run alongside other things already bound to 8080.

## Done (this session)
- **Peer-to-peer set sharing** (`sharing.go`, new file): one instance publishes a bundle of
  recordings and hands another a short share code (`base64url(JSON{u:publicURL, t:token})`) to
  pull them directly over HTTP. Config: `Settings.Sharing` (`SharingConfig`: enabled/publicURL/
  verifiedAt) + `AppConfig.Shares` (`[]Share`, each a token + resolved file-path list).
  - **Setup-first, checked**: `POST /api/share/verify` generates a one-time nonce and fetches
    `{publicURL}/api/share/ping?nonce=…` back at itself; success proves the URL routes to *this*
    instance and is reachable (nonce is single-use, `consumeShareNonce`). `looksPublicHost`
    warns when a verified URL is actually loopback/RFC1918/single-label (so localhost testing
    works but the user is told it's LAN-only). Sharing won't create shares until verified
    (`412` otherwise).
  - **Sender**: `POST /api/shares` accepts explicit paths + `eventIds` + `stages`, resolved to a
    concrete deduped file list (`resolveSharePaths`) at creation. `GET /api/share/get/{token}`
    (public, token-authed) serves the manifest; `/f/{i}` and `/nfo/{i}` stream the file and its
    sidecar by index (never by caller-supplied path). `DELETE /api/shares/{token}` revokes.
  - **Receiver**: `POST /api/share/preview` fetches+returns the remote manifest; `POST
    /api/share/import` downloads selected items (streamed via `.part`→rename, `downloadTo`),
    pulls `.nfo` sidecars, and recreates event/festival grouping by name
    (`resolveOrCreateLibraryEventLocked`, same content-addressed pattern as matchfile import).
    Destination paths are sanitized (`shareImportDest` → `safeName`'d channel+basename, verified
    under FinishedDir) so a malicious manifest can't traverse out; existing files are skipped,
    never clobbered.
  - **Auth**: `/api/share/ping` and `/api/share/get/` are public (`isPublicPath`) - the download
    surface is authed purely by the unguessable path token, since the receiving instance has no
    account here; everything else is admin-gated (`isAdminReq` in handlers on top of `rbac`).
    Share tokens are stripped from non-admin `/api/state`/`/api/config` via `redactSecrets`.
  - **UI**: Settings → "Peer Sharing (P2P)" panel (URL + Verify/Disable + status). Recordings
    toolbar → "Share Sets" (multi-select recordings grouped by stage, per-stage select-all,
    create code, copy, list/revoke active shares) and "Receive" (paste code → preview with
    per-item checkboxes → import) overlay views, mirroring the Smart Match view pattern; both
    admin-only (`data-admin-only`).
  - Verified end-to-end with **two real instances on localhost**: verify (loopback nonce),
    create a stage share (code = 76 chars), preview+import on the receiver (files + NFO arrived
    byte-identical), re-import skips, revoke → receiver gets 404; plus auth-boundary checks
    (unauth management → 401, bad token → 404, unverified create → 412). Unit tests in
    `sharing_test.go` cover code round-trip/rejection, `looksPublicHost`, path-traversal safety,
    and one-time nonce use.
- **UI polish pass** (`app.css` rewritten around a design-token layer): introduced `:root`
  tokens (`--surface-1/2/3`, `--border`/`--border-strong`, `--radius*`, `--shadow-1/2/3`,
  `--ring`, `--ease`) that everything keys off, all still driven by the runtime `--accent`.
  Refinements across panels (subtle top-sheen gradient + depth shadow), buttons (primary now a
  glowing accent gradient, hover lift, active press), nav (accent-gradient active state), source
  cards (accent-glow when now-playing, lift on hover), status pills/dots, inputs (accent focus
  ring, `:focus-visible` rings globally for a11y), dropdowns, toasts (accent left-bar +
  backdrop blur), timetable blocks, library cards, charts (gradient fills, animated widths),
  plus new helpers (`.empty-state`, `.spinner`, `.skeleton`), custom scrollbars, a faint fixed
  accent "aurora" behind the page top, a gradient wordmark on `#app-name`, and a
  `prefers-reduced-motion` guard. `applyThemeColors` now feeds the token layer (`--accent`,
  `--bg`, `--text`, `--text-muted`) instead of hard-overriding individual component rules, so
  custom themes recolour the whole polished UI. Dashboard/source-editor empty states upgraded to
  the `.empty-state` component with a call-to-action (new `goToView(id)` helper). Verified by
  rendering a component gallery (real `app.css`, no Tailwind needed) in a headless browser -
  Tailwind/video.js CDN are blocked by the sandbox proxy so full-page shots aren't possible
  here, but no layout markup changed, only custom-class styling.
- **After-midnight timetable rollover fix**: a set listed under a festival "day" at an
  early-morning wall-clock time (e.g. an after-party at 01:00 under Thursday) was being combined
  with the label day's date, landing ~23h too early on the same day instead of the next morning.
  `combineDateTime` now rolls any time before `festivalDayRolloverHour` (08:00) to the next
  calendar day (and still normalizes the ">=24:00" convention via `time.Date`); the compact
  parser builds its timestamps through `time.Date` too so a `25:00`-style hour can't produce an
  unparseable string. Covered by `timetable_test.go` (boundary cases, an absolute-timestamp
  passthrough, an end-to-end timetable.lol import, and a compact hour-overflow case); verified
  against the real recovered timetable that "Encore with Rebelion" 23:30 ends 01:00 on the *next*
  day.
- **Video.js everywhere**: the two embedded players (Watch live, Recordings) already used
  Video.js; the one hold-out was the dashboard source card's "Open" button, which dumped the raw
  finished file into a new browser tab (native player). Replaced with "Open latest" →
  `openSourceLatest(id)` → the same in-app `openRecordingPlayer` (Video.js) as everywhere else.
  No raw `<video>`/`<audio>` elements remain; the only native-browser media links left are
  explicit downloads.
- **Downloadable timetables + import-from-file**: restored the community DEFQON.1 timetable under
  `timetables/defqon1-2026.json` (with a `timetables/README.md` documenting format + trademark
  provenance) - it is *not* embedded in the binary or copied into the image, only shipped as a
  release asset by a new `.github/workflows/release.yml` (tag-triggered; attaches
  `timetables/*.json` + a zip via `softprops/action-gh-release`). New `POST /api/timetable/import`
  endpoint + `parseAnyTimetableJSON` accept either the app's RFC3339 export shape or the compact
  numeric-tuple community shape (RFC3339 tried first; a compact file fails that decode and falls
  through), preserving existing per-stage stream URLs by stage name. New "Import from file" panel
  in the Timetable tab uploads to it. Verified end-to-end importing the real 14-stage / 372-set
  file. Covered by `TestParseAnyTimetableJSON`.
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
  MizzBehave, GPF) - deliberately no festival-specific preset, since its stages already ship as the
  default source list via `dq-timetable.json`/`sourcesFromTimetable`. No `logoUrl`s were set
  for any of these - a real Twitch avatar CDN URL can't be derived from a channel name/handle
  (it's a content-addressed hash assigned per-account), so one would have to be fabricated;
  the field is left empty rather than guessed. If avatars matter later, fetch them for real via
  Twitch's Helix API (needs a registered app + OAuth token, so it's a deliberate follow-up, not
  something to wire in speculatively) rather than guessing URLs.
- **Auto-reconnect: silent-until-live, then a 10-minute visible window**: the original
  auto-reconnect (previous session) logged/showed every single retry attempt uniformly,
  including a source that's simply never gone live yet - noisy in exactly the wrong case,
  since "waiting for a DJ to go live" is normal, expected, and can go on for hours/days, while
  the actually-interesting case (a source that *was* live and then stopped) got no special
  treatment. `retryState` gained a `windowUntil` field (zero = silent); a new
  `startReconnectWindow` opens a `reconnectVisibilityWindow` (10 minutes) the moment a
  *stable* recording ends (`runRecording`'s bookkeeping calls it instead of the old
  `clearRetry`) - it doesn't matter whether that end was a genuine drop or the broadcaster
  just stopping normally, since either way the scheduler is about to start silently retrying
  it and this is the one case worth surfacing. `recordFailure` now takes the source name too
  and does its own logging: silent while `windowUntil` is zero or already lapsed, logs the
  usual "stream appears down - will retry in Xs (attempt N)" while inside an open window, and
  logs exactly one "no reconnect within 10m0s - will keep checking quietly" the moment the
  window lapses (clearing `windowUntil` so that notice doesn't repeat). `reconnectStatus`
  (used by `state()` for the dashboard's "reconnecting" badge) now returns `ok=false` outside
  an open window, so a never-been-live source never shows that status either.
  `retryBlocked` (used by `evaluate()` to skip a source still in backoff) is unchanged and
  applies identically whether the retries are silent or visible - only the *display* differs,
  not whether the scheduler keeps trying. Covered by four new tests in `reconnect_test.go`:
  silent-without-a-window, visible-within-a-window, gives-up-after-the-window-lapses (and
  stays quiet after, doesn't repeat the notice), and manual clearRetry resetting everything.
- **Multi-user accounts (admin/viewer roles) + Discord OAuth login**: replaced the single
  shared username/password with a `User` list (`cmd/web/auth.go`, new file) persisted to
  `users.json` (alongside `config.json`/the old `auth.json`) - an existing single-user install
  migrates automatically into the first admin user the first time it starts under the new
  code (`setupAuth`), no manual action needed. `AUTH_USERNAME`/`AUTH_PASSWORD` still work
  exactly as before, but now as one *extra* pinned admin login (`envUserID` virtual user, read
  -only in Account settings) layered on top of `users.json`, not a replacement for it.
  - **Sessions** now map to a specific user (`sessionInfo{UserID, Expiry}` instead of a bare
    expiry), so `requireAuth` can resolve *who* is asking, not just *whether* they're
    authenticated - stashed on the request context (`userContextKey`) for handlers and the
    new `rbacAllowed(method, path, role)` check to read.
  - **RBAC** is centralized in `rbacAllowed`, not scattered per-handler: any authenticated role
    can read (GET/HEAD) almost everything (except `/api/users`, admin-only even to view);
    mutating requests need an admin role, except a short self-service allow-list (own
    password, own Discord link/unlink, logout). Chose this over wrapping every individual
    handler since the codebase already funnels every request through one `requireAuth`
    middleware - one centralized check is far easier to audit than ~25 call sites.
  - **Secret redaction**: `redactSecrets` blanks SMTP password, the Discord notification
    webhook, the Discord OAuth client secret, and rclone args before `/api/state` or
    `/api/config` (GET) ever reach a viewer's browser - these were previously sent to anyone
    with a valid session since there was only one role.
  - **Discord OAuth is link-only by design** (explicit product decision, not a shortcut): it
    can never create an account by itself, only let an *existing* user (created by an admin)
    log in faster. A single shared callback (`/api/auth/discord/callback`) handles both the
    login and link flows, dispatching on a server-side pending-state token's `intent` field
    (`pendingOAuth`) rather than two separate callback URLs - Discord only allows redirecting
    to one exact, pre-registered URL per flow-initiation call, and using the same one for both
    flows means only one redirect URI ever needs registering in the Discord dev portal. State
    tokens are single-use and expire after 5 minutes (`consumePendingOAuth`).
  - **Last-admin protection**: `handleUserItem` refuses to demote or delete the last remaining
    admin (checked via `countAdmins`), so an admin can't accidentally lock everyone out of
    managing the instance.
  - Frontend: Settings tab gained **Users** (list/add/change-role/delete) and **Discord Login
    (Admin)** (Client ID/Secret/Redirect URL) panels, both wrapped in `[data-admin-only]` and
    hidden client-side for viewers via `applyRoleVisibility()` (real enforcement is still
    server-side `rbacAllowed` - this is UX, not the boundary); Account section gained
    Link/Unlink Discord buttons; login page gained a "Log in with Discord" button (shown only
    when `/api/auth/discord/status` reports it configured) and human-readable
    `?discordError=...` messages.
  - Covered by `auth_test.go`: the full `rbacAllowed` matrix, user CRUD (including
    case-insensitive duplicate-username rejection and persistence across a fresh load),
    env-pinned-admin-first credential checking with real users still reachable by a different
    username, session lifecycle (including invalidation when the underlying user is deleted),
    secret redaction, Discord config validation, the authorize URL builder, and pending-OAuth
    single-use/expiry semantics.
  - Verified end-to-end against a running instance (not just unit tests): setup → admin
    creates a viewer → viewer can read `/api/state` (with secrets redacted) but gets 403 from
    `/api/users` and `POST /api/sources`, while `POST /api/account` (self-service) still
    succeeds; last-admin demote/delete both correctly rejected with 409; a legacy
    single-credential `auth.json` install migrates into `users.json` on first boot and logs in
    successfully; Discord settings save/reload correctly and the login-start redirect produces
    a well-formed `https://discord.com/api/oauth2/authorize` URL.
- **Rebrand to MutiRec ("Mutual Recorder") and full removal of DEFQON.1-specific content**:
  the repo name's missing "l" (multirec → mutirec) became the identity - "Muti-" as in
  Mutual, reflecting the multi-source/multi-user shape the app had already grown into. First
  pass renamed every user-facing "Defqon Stream Recorder" string (page titles, sidebar app
  name default, `.nfo` "Recorder:" line, PWA manifest, `LICENSE`, local dev binary/image
  names) and rewrote the README with a title/tagline/origin-story callout, badges, a table of
  contents, and a features list regrouped by theme.
  - **Second, deeper pass** removed every actual tie to the real, trademarked DEFQON.1
    festival, not just the product's own name: the Go module itself is now `mutirec` (was
    `defqon-stream-recorder` - every import path updated accordingly); deleted the bundled
    `dq-timetable.json` entirely (it carried a real festival's real 2026 stage lineup, artist
    names, and Discord channel/emoji IDs) along with the Dockerfile `COPY` that shipped it and
    the `defaultConfig()`/`loadConfig()` bootstrap path that seeded a default "RED" source
    pointed at `youtube.com/@qdance`- a fresh install now starts with zero sources and no
    timetable, same as any other empty state, guided by the existing onboarding wizard rather
    than pre-seeded content. `loadDQTimetable` and `sourcesFromTimetable` (only ever used by
    that bootstrap path) were deleted outright; `parseDQTimetableJSON` survived under the
    generic name `parseStageTimetableJSON` since it's genuinely reusable - it's the format
    for pasting an archived timetable into a LibraryEvent, not tied to any one festival.
  - Theme presets in `app.js` (`festivalThemes`) were carrying *other* real trademarked
    festival names too (Qlimax, Mysteryland, Tomorrowland, alongside several "Defqon ..."
    variants) - renamed all eight to purely descriptive colour names (Crimson, Violet Pulse,
    Rose, Cyan Wave, Amber, Lime, Orchid, Ocean Blue) with the same colour values, since the
    underlying concern (shipping real festival branding) applied equally to those, not just
    the one the request named.
  - Scrubbed remaining incidental mentions across doc comments (`main.go`, `syscheck.go`),
    placeholder examples (`index.html`'s "e.g. Defqon.1"/"e.g. Q-dance" input hints), and test
    fixtures (`match_test.go`'s example filename used a real artist name and a real festival's
    edition subtitle; `internal/disk/disk_test.go` had a real festival name in a test
    filename) - replaced with fully fictional stand-ins (a placeholder festival, artist, and
    edition name invented for this purpose) that exercise the same code paths without
    referencing anything real. Left `openapi.json` alone - it's a vendored copy of
    timetable.lol's own public API spec (not this project's content), and one of its example
    values happens to reference real channel slugs; editing a third-party reference doc to
    scrub someone else's example text isn't this project's call to make, and it's never
    served or shipped as running code.
  - README's Quick Start/Preset Packs sections updated to match: no more "bundled
    `dq-timetable.json` seeds DEFQON.1 stages" language, and the disclaimer no longer names
    Q-dance/DEFQON.1 specifically.

## Done (this session, part 2)
- **P2P import: background jobs, hash verification, live progress/log** (`sharing.go`): the
  synchronous `handleShareImport` (previous session) blocked the request for however long a
  transfer took, so a large import needed a browser tab left open for hours. `handleShareImport`
  now only validates the code, fetches the manifest, and starts a `ShareJob` goroutine
  (`runShareImportJob`), returning a `jobId` immediately; the transfer itself continues
  server-side regardless of whether the tab stays open. `ShareJob` (mutex-guarded fields, a
  `view()` method producing a JSON-safe `ShareJobView` snapshot) tracks per-file and aggregate
  progress (bytes transferred/total, current file, transfer speed sampled every ~250ms via
  `addBytes`, a capped 500-line live log via `logf`), polled from `GET /api/share/jobs/{id}`
  (`GET /api/share/jobs` lists all, most recent first). Job history is capped at ~50 entries
  (`putShareJob` evicts the oldest by start time) so a long-running instance doesn't accumulate
  unbounded memory. Each downloaded file's actual sha256 (via `fileHash`, using the real
  post-download size/mtime - not the manifest's claimed size, which would poison the hash cache)
  is checked against the manifest's declared hash before being kept; a mismatch discards the file
  and marks it failed rather than silently keeping a corrupted download. `downloadTo` gained an
  `onBytes` callback (an `io.MultiWriter` tee via a tiny `progressWriter` adapter) to report
  progress without a custom reader/writer wrapper. Frontend (`app.js`): the Receive view's
  Import button now starts the job and polls it (`pollReceiveJob`, 1s interval) instead of
  blocking, rendering a progress bar (reusing the existing `.storage-bar-track`/`-fill` styling),
  transferred/total/speed/file-count stats, the current file, and the scrolling live log; polling
  and the disabled Import button state are reset on close/reopen so a stale poll can't leak.
  Covered by new tests in `sharing_test.go` (`ShareJob.view()` returns an independent copy of its
  log slice, `finishFile`/`finish` outcome bookkeeping, `putShareJob` eviction).
- **Recording thumbnails** (`uploads.go`, new file): video recordings get a thumbnail
  auto-generated the moment they finish (`runRecording` in `main.go` spawns
  `go a.generateThumbnail(...)` right after `a.backup(rec)`, non-blocking) - a single frame
  grabbed via `ffmpeg` from a random point between 10%-90% of the file's duration (avoiding a
  black intro/outro or a stage's holding slate), skipped entirely for audio-only sources
  (`generateThumbnail` returns `false` immediately). Thumbnails are stored content-independent,
  keyed by a hash of the recording's *relative path* (`thumbKey`) rather than file content, so a
  regenerated or manually-replaced thumbnail doesn't need any pointer elsewhere updated - the
  frontend just requests `/api/recordings/thumbnail?path=...` and handles a 404 as "no
  thumbnail yet" (`findThumbnail`/`removeThumbnail`). `POST`/`DELETE` on the same endpoint
  upload/remove a thumbnail by hand (any recording, audio or video); a separate
  `POST /api/recordings/thumbnail/regenerate` re-rolls a fresh random frame for an existing
  video. Frontend: library set cards (`libSetCardHtml`) now try to load a thumbnail image over
  the existing gradient/play-icon placeholder (hidden via `onerror` if none exists, so no extra
  round-trip is needed to check existence first); the Organize modal gained an upload/
  regenerate/remove thumbnail section wired to the same endpoints. Covered by new tests in
  `uploads_test.go` (`thumbKey` determinism/uniqueness, find/remove round-trip,
  `generateThumbnail` skipping audio-only and failing gracefully on a missing file).
- **Image uploads replace "paste a URL" everywhere**: the four fields that previously asked for
  an external image URL (app logo, Organisation logo, Festival logo, Event cover art) now upload
  a file directly instead. New `POST /api/uploads/image` (`uploads.go`) sniffs the uploaded
  bytes' real content type (`http.DetectContentType`, never trusting the client-supplied
  Content-Type or filename extension; JPEG/PNG/WebP/GIF only, 12MB cap) and stores it
  content-addressed by sha256 hash under a new `uploads/` directory next to the config file, so
  re-uploading the same image is a no-op; the returned `/uploads/<hash>.<ext>` URL is what gets
  saved in the same string fields (`logoUrl`/`coverUrl`) that used to hold an external URL - no
  config schema change needed, only what populates the value changed. Frontend: a reusable
  `data-image-field` component (`setupImageUploadFields()`/`syncImageUploadPreview()` in
  `app.js`) replaces each text input with a hidden input (still read/written by the existing
  save-payload code, unchanged) plus a preview thumbnail, an Upload button (opens a file picker,
  POSTs to the endpoint, fills the hidden input), and a Remove button. `/uploads/` is served as
  static files but deliberately *not* added to `isPublicPath` - unlike PWA icons, these images
  are only ever shown inside the already-authenticated app, so the browser's session cookie
  covers it. Covered by new tests in `uploads_test.go` (`readImageUpload` accepting a real PNG,
  rejecting non-image content/empty uploads/a missing form field).

## Done (this session, part 3)
- **Peer Sharing: manual override for the self-verification check + outbound
  proxy support** (`sharing.go`, new `proxy.go`): a real user's setup had the
  sender's self-check pass (it can reach its own public URL just fine, e.g.
  routed internally by a VPN-gated firewall) while actual outside clients
  hitting the same URL over the public internet couldn't connect at all -
  the reachability check has no way to distinguish "I can reach myself" from
  "the internet can reach me," so an override was needed for cases where the
  admin has already confirmed reachability some other way. `handleShareVerify`
  now accepts `force: true` to skip the nonce round-trip entirely and just
  save+enable (still validates the URL is well-formed); `SharingConfig`
  gained a `Forced` flag so this state is visible in Settings and logged as a
  `warn`-level event rather than silently indistinguishable from a real
  verification. UI: a "Skip verification (enable anyway)" checkbox next to
  Verify & enable.
  - **Outbound proxy**: new `SharingConfig.ProxyURL`, applied to every
    sharing-related outbound request (`shareHTTPClient`, used by the
    self-verify ping, `fetchShareManifest`'s preview/import fetch, and
    `runShareImportJob`'s downloads) via a new `proxy.go`. Supports
    `http`/`https` (standard `net/http` proxying via `http.ProxyURL`),
    `socks5`/`socks5h` (via `golang.org/x/net/proxy` - the only new external
    dependency added), and `socks4`/`socks4a` (hand-rolled - `x/net/proxy`
    doesn't implement SOCKS4 at all, so `proxy.go` implements the CONNECT
    handshake directly per the SOCKS4/4a spec). The proxy can be saved
    independently of enabling/disabling sharing (`handleShareConfig`'s POST
    now takes an optional `proxyUrl` alongside `enabled`, using a `*string`
    so an omitted field doesn't clobber a previously-saved proxy - the
    frontend's Disable button only ever sends `{enabled:false}`). Proxy URLs
    can carry embedded credentials (`user:pass@host`), so `ProxyURL` is
    blanked by `redactSecrets` before a non-admin ever sees `/api/state` or
    `/api/config`, same as SMTP/Discord/rclone secrets.
  - Covered by `proxy_test.go` (transport construction for every scheme,
    malformed/unsupported-scheme rejection, a fake SOCKS4 TCP listener
    exercising both IP-mode and SOCKS4a hostname-mode wire format, and a
    rejection-response case) and new cases in `sharing_test.go`
    (`handleShareVerify`'s force path skips the network check entirely while
    the normal path still fails against an unreachable address, a bad proxy
    URL is rejected even with force, and `handleShareConfig`'s proxy-only
    update doesn't clobber the enabled flag or vice versa).

## Done (this session, part 4)
- **File Explorer** (`cmd/web/explorer.go`, new file): a general-purpose
  browse/upload/zip/unzip/rename/delete file manager rooted at a configurable
  directory, new `Settings.FileExplorerRoot` (blank = defaults to
  `FinishedDir`, the recordings library - recommended default; an admin can
  point it anywhere, same shell-equivalent trust level as source stream/
  ffmpeg args). New "Explorer" nav tab, admin-only (added to the same
  `['sources', 'diagnostics', 'events-tab', ...]` visibility list
  `applyRoleVisibility` already used, plus every handler independently
  double-checks `isAdminReq`).
  - Endpoints: `list` (dir contents), `mkdir`, `rename` (basename only, never
    moves across directories), `delete` (recursive, refuses to delete the
    root itself), `download` (a single file streams directly; a directory,
    or more than one selected entry of any kind, streams as an on-the-fly zip
    via `archive/zip` straight to the response - never buffered fully in
    memory, so this scales to a whole recordings tree), `upload` (multipart,
    multiple files, 20GB cap), `zip` (bundle selected entries into a new zip
    in the same directory), `unzip` (extract into a deduplicated sibling
    directory, e.g. `name-2` if `name` is taken).
  - **Path safety**: every handler resolves the client-supplied relative path
    through `resolveExplorerPath` (clean + join + prefix check against the
    root, same pattern as `shareImportDest` elsewhere) and every new
    file/folder name through `sanitizeEntryName` (rejects empty/"."/".."
    and any embedded path separator).
  - **Zip-slip defense** (`extractZip`): each entry's path is rooted
    (`path.Clean("/" + entry.Name)`) before being joined to the destination,
    which collapses any leading `..` segments to a harmless in-bounds path
    rather than escaping it (the standard safe pattern for this), plus a
    belt-and-suspenders prefix check against the overall explorer root in
    case a caller ever passes a destDir that isn't really under it.
  - Covered by `explorer_test.go`: path-traversal/sanitizer rejection,
    mkdir/rename/delete round-trip, directory-first listing sort, a
    zip-slip attempt landing safely inside the destination (confirmed by
    checking it does *not* end up outside), zip+unzip round-tripping real
    file content, and the sibling-dir disambiguation helper.
- **Fetch from URL / TransIP Stack support** (`cmd/web/urlfetch.go`, new
  file): downloads a direct link - or a public share link - straight into
  the current Explorer folder as a background job (`URLFetchJob`, same
  progress/speed/live-log shape as `ShareJob`, polled via
  `GET /api/explorer/fetch/jobs/{id}`), so a large download doesn't need a
  browser tab left open. `looksLikeOwncloudShare` detects the ownCloud/
  Nextcloud public-share URL convention (a `/s/<token>` path segment) that
  TransIP Stack - and a number of other self-hosted "share a folder" tools
  people use to hand out festival sets - is built on: for those links it
  requests `{url}/download` (the convention's actual-file endpoint) and, if
  a password is supplied, sends it as HTTP Basic auth with the share token
  as the username (the standard protected-share convention). Any other URL
  is downloaded directly, with the destination filename inferred from
  `Content-Disposition` first, then the URL path, then a timestamp fallback.
  A downloaded `.zip` is auto-extracted into a sibling folder afterward
  (reusing `extractZip`). Covered by `urlfetch_test.go` (share-link
  detection across several URL shapes, filename inference from each
  fallback source, and job view/finish semantics).
  - **Also fixed while touching this code**: `shareHTTPClient` (used by P2P
    sharing's downloads too) previously set a blanket 30s `http.Client.
    Timeout`, which covers the *entire* response including the body read -
    meaning any download taking longer than 30 seconds (a realistic case for
    a multi-GB recording) would have been silently killed partway through.
    Replaced with transport-level `DialContext`/`TLSHandshakeTimeout`/
    `ResponseHeaderTimeout` bounds (catches "can't connect"/"server never
    responds" within a reasonable time) and no overall timeout at all, so
    body streaming is only bounded by however long the transfer actually
    takes.

## Done (this session, part 5)
- **Thumbnails now backfill on demand**: the Recordings page showed no
  thumbnail for anything that didn't arrive through the live recording
  pipeline (a file dropped in via the File Explorer/URL fetch, a P2P import,
  or a recording that predates the thumbnail feature) - `generateThumbnail`
  was only ever called from `runRecording`'s success path. Fixed by adding
  `generateThumbnailOnDemand` (`cmd/web/uploads.go`): the thumbnail GET
  handler now generates one synchronously the first time it's requested and
  none exists, instead of just 404ing forever. In-flight generation is
  deduped per relPath (`App.thumbGenMu`/`thumbGenerating`, a
  `map[string]chan struct{}`) so a burst of card renders for the same
  recording can't spawn a pile of redundant `ffmpeg` processes.
- **Folder-layout convention for manually-added recordings, and Smart Match
  support for it**: users have real festival sets already organized as
  `<event>/<edition>/<day>/<stage>/<file>` (e.g. a whole day of one stage
  recorded as a single file - not any specific DJ's set). Previously,
  channel/stage was always derived from the *first* path segment
  (`handleRecordings`, `handleRecordingMatchSuggestions`), which is right for
  the live recorder's own flat `<source>/<file>` layout but wrong for a
  nested one (it would read "stage" as the event name instead). Added
  `channelFromPath` (`cmd/web/main.go`) - the immediate parent directory,
  which is a no-op for the flat 1-level case and correctly resolves to the
  stage folder for anything deeper - and switched both call sites to it.
  - Added `folderEventHint`/`eventMatchesFolderHint`/`applyFolderEventHint`:
    when Smart Match's normal per-set scoring comes back weak (confidence
    "none" or "low" - i.e. nothing in an archived timetable actually
    matches), it now falls back to parsing the folder path itself. A
    year-shaped segment becomes the edition year; a weekday-name or
    date-shaped segment (a day folder) is dropped rather than folded into
    the name; whatever's left becomes the candidate event name. If an
    existing `LibraryEvent` matches by name (and year, if both sides have
    one), the suggestion files straight into it; otherwise it proposes
    *creating* that event - new `MatchSuggestion.NewEventName`/
    `NewEventYear` fields - left for the user to approve like every other
    suggestion, never applied silently. Deliberately never guesses an
    artist/set for these: a whole-day/stage recording isn't one DJ's set,
    and a real archived-timetable match is always preferred when one scores
    well enough on its own.
  - Frontend (`app.js`): the Smart Match list now shows "New event: X" for a
    folder-only suggestion and its Approve button reads "Create Event &
    Approve"; approving it calls `POST /api/events` first (using the
    existing create-event endpoint) and folds the new ID into the normal
    `PUT /api/recordings/meta` call. That call now also forwards
    `guessedTime` as `start` unconditionally (harmless for a real
    set-match, since `handleRecordingMeta` overwrites `Start`/`End` from the
    matched set when `setId` is present anyway) so a folder/filename-only
    match still buckets into the right day in the library view instead of
    falling back to file mtime.
  - **In-app documentation**: a "Folder Layout" button next to Smart Match
    (and one in the File Explorer toolbar - same modal, `#lib-folder-help-overlay`)
    opens a plain-language explanation of the recommended layout with a
    genericized example (no real festival names, per the no-bundled-festival-
    data constraint). Mirrored in a new README section, "Recordings Library &
    Smart Match".
  - Tests: `TestChannelFromPath`, `TestFolderEventHint`,
    `TestBestMatchSuggestion_WholeStageFolderConvention` (both the
    matches-existing-event and proposes-new-event branches) in
    `match_test.go`.

## Done (this session, part 6)
- **Liveness pre-check before every reconnect attempt**: the scheduler used to
  just start the full `streamlink|ffmpeg` recording pipeline on every retry
  and treat that as the liveness check - fine in principle, but some
  streamlink plugins return a few KB of a placeholder/offline stream before
  erroring out, which was enough to clear `minViableRecordingBytes` and get
  saved as a real (but junk) recording on every retry of a flaky/offline
  channel. Added `App.isSourceLive` (`cmd/web/main.go`): a cheap, separate
  probe run before `a.start(src)` in `evaluate()` - `streamlink --stream-url`
  (with the source's own `StreamlinkArgs`/quality) for streamlink-based
  sources, an HTTP HEAD for direct-URL sources. Only a successful probe leads
  to `a.start`; a failed one now goes through `recordFailure` directly (same
  backoff/visible-window bookkeeping as a failed recording attempt) without
  ever spawning ffmpeg or touching disk. Runs in a per-source goroutine off
  the scheduler tick so one slow/hanging probe can't back up the rest.
  README's Auto-Reconnect section documents the two-step check. Tests in
  `cmd/web/livecheck_test.go` (`TestIsSourceLiveHTTPType` against a real
  httptest server, `TestIsSourceLiveStreamlinkTypeWhenLive`/`WhenOffline`
  against a stub `streamlink` script substituted onto `PATH`).

## Done (this session, part 7)
- **Set Cutter, phase 1 - sidecar timecode + rich metadata + waveform**
  (`cmd/web/cutter.go`, new file). Every finished recording now gets a
  `<recording>.timecode.json` sidecar (deliberately separate from the
  human-readable `.nfo`) written right after the atomic rename in
  `runRecording`, plus a cached waveform PNG under `thumbnails/` (same
  content-addressed-by-relative-path convention as recording thumbnails, just
  a different hash namespace so the two never collide) - generated for every
  recording, audio-only or video, since the eventual Set Cutter cuts by audio
  waveform either way.
  - `RecordingSidecar` carries as much metadata as a single `ffprobe` pass
    can produce - duration, size, bitrate, video/audio codec, resolution,
    frame rate, channels, sample rate - plus the recording's wall-clock
    anchor (`startedAt`, `offsetMs`, `timeSource`) and the source it came
    from (id/name/type/url/quality/container/flags).
  - Wall-clock accuracy: `timeCorrection()` does a one-shot, 3s-timeout
    `GET https://worldtimeapi.org/api/ip` at `a.start(src)` and stores the
    correction (`offsetMs`) against the system clock; any failure (offline,
    blocked, rate-limited) falls back to the system clock silently with
    `timeSource: "system-ntp"` - never blocks or fails the recording.
  - `GET /api/recordings/timecode?path=` serves the sidecar (404 if none
    yet); `POST` (admin only) writes/overwrites one - either from a supplied
    RFC3339 `startedAt` (manual correction) or, if none is given, falls back
    to `file mtime - probed duration` (`timeSource: "file-mtime-fallback"`)
    for recordings that predate this feature or arrived outside the live
    recording pipeline (File Explorer, URL fetch, P2P import).
  - `GET /api/recordings/waveform?path=` serves the cached PNG, generating
    it on first request if missing (same on-demand + cache pattern as
    thumbnails).
  - `POST /api/recordings/backfill-timecodes` (admin only) walks the whole
    `FinishedDir` tree and writes a sidecar + waveform for anything missing
    one - wired to a new "Backfill timecodes & waveforms" button in
    Settings → Recorder, with a toast summarizing scanned/written/skipped/
    failed counts.
  - The recordings scanner's old `strings.HasSuffix(p, ".nfo")` exclusion
    (four call sites) is now the shared `isSidecarPath()` helper, extended to
    also skip `.timecode.json` and the upcoming `.markers.json` (Set Cutter
    phase 2) so neither is ever mistaken for a recording in its own right.
  - Tests in `cmd/web/cutter_test.go`: sidecar path derivation, frame-rate
    parsing, `ffprobe` JSON → `RecordingSidecar` field mapping (video+audio
    and audio-only cases), waveform/thumbnail key non-collision, path-escape
    rejection, and the timecode GET/POST handlers end-to-end (including the
    manual-backfill round-trip).
- **Set Cutter, phase 2 - the Cutter UI itself** (`cmd/web/cutter.go` +
  `cmd/web/static/{index.html,app.js,app.css}`). A "Cut" button (scissors
  icon) on every library card and search result opens a modal that splits a
  whole-day recording into individual set files.
  - Markers (`CutterMarker`: offset, name, channel, optional
    `eventId`/`setId`, artist, start/end, tracklist) are edited in the modal
    and persisted to a `<recording>.markers.json` sidecar via
    `GET`/`PUT /api/cutter/markers?path=` - the third and last sidecar kind
    `isSidecarPath()` now excludes from the recordings scanner.
  - The modal always plays the real file (`<audio>` for audio-only
    containers, `<video>` for everything else - `cutterIsAudioOnly()` picks
    by extension) alongside the waveform image from phase 1, so "video sets
    are also cuttable, treated like audio but with the video running
    alongside" per the request: markers are placed by ear/eye against the
    same waveform in both cases, just with a video element instead of a bare
    audio one. Clicking the waveform seeks the player; "Add marker at
    current time" reads its `currentTime`.
  - "Load from timetable" uses the recording's assigned `eventId` + channel
    to find the matching `StageSchedule` in that event's archived timetable,
    and the phase-1 sidecar's `startedAt` to convert each set's wall-clock
    start into a file offset - only sets that actually fall within the
    file's span become markers, and one already within 1s of an existing
    marker is skipped rather than duplicated.
  - Export (`POST /api/cutter/export`, admin only) runs as a background
    `CutterJob` (same mutex-guarded-struct-plus-`view()` shape as
    `URLFetchJob`/`ShareJob`) polled via `GET /api/cutter/jobs/<id>` - the
    settings answer was "background job with a progress toast", so the
    client shows one immediately and polls every 2s rather than blocking on
    the request. Stream-copies (`-c copy`) by default for a near-instant,
    lossless split; an "Advanced options" `<details>` panel (both the
    silence-threshold sliders **and** this checkbox were asked to live under
    Advanced, not inline) exposes "Precise cut" which re-encodes audio
    (`-c:v copy -c:a aac`) for a frame-accurate boundary instead of only
    keyframe-accurate.
  - Each segment's output path follows the fixed convention settled on:
    `<event>/<year>/<stage>/sets/<Artist>_<Stage>_<date>.<ext>`
    (`cutterExportPath`), falling back to the recording's own folder/channel
    when a marker isn't linked to a library event. A `RecordingMeta` entry
    is written for every exported segment (same map `handleRecordingMeta`
    already uses) so it shows up organized in the library immediately, and a
    thumbnail is generated for any segment that isn't a known audio-only
    container.
  - Tests in `cmd/web/cutter_test.go`: export path building (with and
    without a linked library event), audio-extension detection, and the
    markers GET/PUT handlers (empty-array default, round-trip, admin-only
    enforcement on PUT).
  - Verified end-to-end against a running instance (no ffmpeg/ffprobe
    installed in that sandbox, so waveform/export correctly report
    unavailable/failed rather than silently succeeding) plus a Playwright
    check that the audio/video element toggle picks the right element by
    file extension.
  - **Still to come**: assisted mode (silence detection + optional Whisper,
    both scoped to ≤20-minute windows around timetable slot boundaries,
    with silence-threshold sliders under Advanced options) and a general
    mass-transcode tool for bulk re-encoding recordings.

- **Mass Transcode** (`cmd/web/transcode.go`, new file, plus a "Mass
  Transcode" view in the Library tab). Bulk re-encode a batch of recordings
  - video or audio - in one background job, following the same
  handler-starts-a-goroutine-returns-a-job-ID shape as every other
  long-running operation (`URLFetchJob`/`ShareJob`/`CutterJob`).
  - `TranscodeOptions`: target container (blank keeps the source's own),
    video codec (`copy`/`h264`/`h265`/`none` for audio-only extraction from a
    video source), audio codec (`copy`/`aac`/`opus`/`mp3`/`flac`), CRF and
    audio bitrate (both default sensibly when unset), hardware accel (same
    values as a Source's `HardwareAccel`), and `Replace` (overwrite the
    original in place vs. write a new `-transcoded` file alongside it).
  - `POST /api/transcode/start` (admin only) validates every path resolves
    under `FinishedDir`, starts a `TranscodeJob`, and returns its ID
    immediately; `GET /api/transcode/jobs/<id>` polls per-file progress
    (done/failed counts + a log line per file). Files are processed
    **sequentially, not in parallel** - re-encoding is CPU/GPU-bound, so a
    batch of concurrent ffmpeg processes would just contend with each other
    (and with any live recording in progress) rather than finish faster.
  - `transcodeOneFile` writes to a `.transcoding.tmp` path first and only
    replaces/places the final file after ffmpeg succeeds - a failed file
    never touches the original. If a container change means the output path
    differs from the input, the old file is removed and its `RecordingMeta`
    entry migrated to the new path (`migrateRecordingMeta`) so the library
    assignment survives the encode. `rewriteSidecarsAfterTranscode` carries
    forward whatever timing metadata the old `.timecode.json` had (the
    recording's real wall-clock start doesn't change just because it was
    re-encoded) and regenerates the waveform + a thumbnail against the new
    file.
  - Frontend mirrors the existing "Share Sets" view almost exactly (same
    group-by-channel checkbox list, filter box, select-all/clear) since it's
    the same "pick some recordings" interaction - a new `<select>`-based
    options form above it, then a progress bar + log reused from the
    Receive-a-share job box pattern.
  - Tests in `cmd/web/transcode_test.go`: codec/encoder/container-extension
    mapping, ffmpeg argument construction (copy defaults, a full re-encode
    with hardware accel, audio-only extraction), and the start/poll HTTP
    handlers (admin-only enforcement, empty-paths rejection, a real
    start→poll round trip).
  - Verified end-to-end against a running instance (no ffmpeg installed in
    that sandbox, so the job correctly records a per-file error instead of
    silently succeeding) plus a Playwright screenshot confirming the Mass
    Transcode view renders and its selection checkboxes wire up correctly.

- **Set Cutter, phase 3 - assisted mode** (`cmd/web/assist.go`, new file).
  "Auto-detect cuts" in the Set Cutter modal proposes a refined cut point at
  every timetable set boundary, without ever processing more than a bounded
  window per boundary.
  - `POST /api/cutter/detect` validates the recording has a timecode sidecar
    with a real `startedAt` and is assigned to a library event with an
    archived timetable for its channel (all three are hard requirements -
    without them there's nothing to map wall-clock boundaries onto), then
    starts a background `DetectJob` and returns its ID; `GET
    /api/cutter/detect/jobs/<id>` polls it, same shape as every other job
    type in this app.
  - For each boundary (one set's end = the next set's start) that falls
    within the recording's own span, `runDetectJob` opens a ±10-minute
    (20-minute total) window and runs **silence detection always**
    (`detectSilenceNear`/`parseSilenceLog`/`pickClosestSilence` - parses
    ffmpeg's `silencedetect` stderr output, picks the gap whose end lands
    closest to the expected boundary, ties broken by the longer silence)
    plus **Whisper optionally** (`detectWhisperNear`/`matchWhisperTranscript`
    - extracts the window to a 16kHz mono WAV, runs whichever of
      `whisper`/`whisper-cli`/`faster-whisper` is on `PATH`, and scans the
      transcript for the next set's own artist name first, falling back to
      a generic MC handoff phrase in English or Dutch).
  - The two signals are combined into a confidence score: both agreeing
    within 30s → `"high"`/`"combined"`; either alone → `"medium"`; neither →
    `"low"`/`"timetable-only"` (falls back to the raw, un-refined timetable
    time). Nothing is written automatically - the client reviews each
    `DetectedMarker` proposal and accepts it (or all of them) into the
    working marker list.
  - Silence threshold (`-50dB` default) and minimum duration (`2s` default)
    are exposed as sliders, and Whisper as a checkbox + language dropdown
    (`"auto"` default, or a fixed language), both tucked under the Set
    Cutter's existing "Advanced options" panel per the plan - some stages
    have continuous crowd noise with no true silence, so these needed to be
    adjustable rather than hardcoded.
  - `syscheck.go` gained a `checkWhisper()` row (optional - a warning, not a
    failure, when absent) so it's visible from Diagnostics whether
    Whisper-assisted detection is available on this install.
  - Tests in `cmd/web/assist_test.go`: silence-log parsing, closest-silence
    selection (including the duration tie-break), Whisper transcript
    matching (artist name, generic phrase fallback, no-match case), and the
    detect handler's three-part validation (admin-only, requires a timecode
    sidecar, requires an assigned event+timetable) plus a full
    request→job-exists round trip.
  - Verified end-to-end against a running instance: created a real library
    event + archived timetable + timecode sidecar, confirmed the detect
    endpoint's validation chain passes and the job starts, and (since ffmpeg
    isn't installed in that sandbox) correctly records a probe error rather
    than silently succeeding. Confirmed via direct JS-state assertions
    (not just a screenshot, since the sandbox's Tailwind CDN load was
    unreliable) that the proposals panel starts hidden, becomes visible
    with rendered rows once a job returns proposals, and correctly
    transfers an accepted proposal into the marker list.

  This completes all three planned Set Cutter phases from this session.

## Done (this session, part 8)
- **Free-space settings shown in GB, not raw bytes**
  (`cmd/web/static/{index.html,app.js}`). Settings → Recorder's minimum/
  warning free-space fields now take/display GB (`minFreeGb`/`warnFreeGb`
  inputs, one decimal place); `config.settings.minFreeBytes`/`warnFreeBytes`
  are unchanged on disk (still bytes, for backward compatibility with
  existing `config.json` files) - `fillSettings`/`readSettings` convert at
  the UI boundary via new `bytesToGb`/`gbToBytes` helpers.
- **Storage forecasting on the Dashboard** (`main.go`: `StorageForecast`,
  `computeStorageForecast`). Estimates the current combined write rate
  across every active recording (each one's temp-file size ÷ its own
  elapsed time, summed - a recording under `minForecastSampleSeconds` (5s)
  old is excluded since its rate isn't meaningful yet) and projects how many
  hours of recording remain at that rate given the volume's current free
  space. Exposed as a new `storageForecast` field on `/api/state`, shown as
  a one-line summary under the Storage panel ("~14.3 MB/s across 2 active
  recordings — about 6.2 hours of storage left"); blank when nothing is
  actively recording, since there's no rate to extrapolate from. Tests in
  `cmd/web/storage_test.go` cover no-active-recordings, the just-started
  exclusion, a single-recording projection, and summing across multiple
  concurrent recordings.
- **Token-gated HTTP stream support** (`main.go`: `Source.HTTPHeaders`,
  `parseHTTPHeaderLines`, `ffmpegHeadersArg`, `proxyLiveHTTP`). A live HTTP
  stream that needs an `Authorization` header, a signed cookie, or any other
  custom header now has somewhere to configure that: a new "HTTP headers"
  textarea in the Source editor (one `Key: Value` per line, only relevant
  for `http`-type sources), stored as `Source.HTTPHeaders []string`. Applied
  everywhere this app talks to that URL, since all three had to agree or a
  token-gated source would work in some places and silently fail in others:
  - **Recording**: `ffmpegArgs` adds ffmpeg's `-headers` option before `-i`
    when the input is an actual URL (not the streamlink `pipe:0` case, where
    it would be meaningless).
  - **Liveness pre-check**: `isSourceLive`'s HTTP HEAD probe now sends the
    same headers - without this, a token-gated source would get a 401/403
    on every liveness check and never be allowed to start recording at all,
    even though the recording pipeline itself would have worked fine.
  - **"Test Stream" button**: `handleSourceTest` gained the same
    `httpHeaders` field so the editor's test button reflects reality instead
    of reporting a false failure.
  - **Live preview**: `handleLive`'s `http`-type branch used to just
    `http.Redirect` the browser straight to the source URL - which can't
    carry a server-held auth header, so a token-gated source's live preview
    would fail even though recording worked. When headers are configured it
    now proxies the request through the server instead (`proxyLiveHTTP`,
    bound only by the client's own request context so it can stream for as
    long as the tab stays open); the plain redirect is kept as the
    zero-overhead default when no headers are set.
  - Tests in `cmd/web/httpheaders_test.go`: header-line parsing (including
    comments/blank lines/values containing a colon), the ffmpeg `-headers`
    string format, `-headers` placement relative to `-i` and its omission
    for `pipe:0` input or when nothing is configured, `isSourceLive`
    succeeding/failing based on whether the header was actually sent (real
    `httptest` server), and `proxyLiveHTTP` forwarding both the header and
    the response body/content-type.
  - Verified end-to-end against a running instance with a real token-gated
    fake upstream (a small Python HTTP server requiring
    `Authorization: Bearer secret-token`): source creation persists
    `httpHeaders`, the "Test Stream" endpoint succeeds with the header and
    fails with a real 401 without it, and `GET /api/live/<id>` returns the
    upstream's actual `200` + body instead of a redirect the browser could
    never have authenticated through on its own.

## Done (this session, part 9)
- **MilkDrop-style music visualizer** (Butterchurn, a WebGL port of the
  Winamp MilkDrop plugin, replacing the previous plain frequency-bar
  canvas). Vendored (not CDN) at
  `cmd/web/static/vendor/butterchurn/{butterchurn.min.js,butterchurn-presets.min.js}`
  and included via `<script>` tags in `index.html` — vendored rather than
  loaded from unpkg like the other CDN scripts so the visualizer keeps
  working on a fully offline/self-hosted install, consistent with the rest
  of the app's no-external-dependency posture.
  - `ensureVizGraph`/`startVisualizer`/`stopVisualizer` in `app.js` keep
    their existing signatures (`startVisualizer(videoEl, canvasId)`,
    `stopVisualizer(canvasId)`) so both existing call sites — the Watch
    tab's live preview (`#watch-visualizer`) and the Recordings player's
    audio-only playback (`#cp-visualizer`) — needed no changes beyond
    wiring up a new "Next preset" button.
  - The `AudioContext`/`MediaElementSourceNode` graph is unchanged
    (`source.connect(audioCtx.destination)` for actual playback); Butterchurn
    attaches via `visualizer.connectAudio(source)` and renders via
    `visualizer.render()` each animation frame, replacing the old
    `AnalyserNode` + 2D bar-drawing loop as the primary path. The old bar
    visualizer is kept as an automatic fallback for any browser/context
    where `butterchurn.createVisualizer` isn't available or throws (e.g. no
    WebGL2) — built lazily so a working WebGL2 context always wins first.
  - `nextVisualizerPreset(canvasId, random)` cycles or randomly jumps
    presets with a 1.5s blend; wired to new "Next preset" buttons next to
    the Watch tab's visualizer toggle and as an overlay button on the
    Recordings player's audio stage. A preset is also chosen at random on
    first start so every recording doesn't open on the same pattern.
  - Canvas resizing is handled by a `ResizeObserver` that calls
    `visualizer.setRendererSize(w, h)` — needed since Butterchurn's
    internal WebGL viewport doesn't auto-track its canvas's CSS size the
    way a 2D canvas redraw loop does.
  - **Real bugs caught only by loading the actual page in a browser** (`go
    vet`/`go test` don't touch frontend JS at all here since this feature
    has no Go-side changes): (1) both vendored UMD builds nest their real
    API under a non-enumerable `.default` property
    (`window.butterchurn.default.createVisualizer`, not
    `window.butterchurn.createVisualizer` directly) — `Object.keys()` on
    the global shows nothing useful either way since the interop props
    aren't enumerable, so this only surfaces by actually calling the
    function and checking `typeof`. (2) A canvas element permanently locks
    to whichever context type (`"webgl2"` vs `"2d"`) is first *successfully*
    established — an early `canvas.getContext('webgl2')` feature-detection
    probe (before deciding whether to try Butterchurn) silently poisoned
    the canvas for its own 2D fallback path once Butterchurn failed to
    initialize, crashing on `canvas.getContext('2d')` returning `null`. Fix
    was to remove the standalone probe entirely and let
    `butterchurn.createVisualizer`'s own internal `getContext('webgl2')`
    call be the only attempt, falling back to 2D only if that throws.
  - Verified with a Playwright script driving the real embedded page (login
    → construct a dummy `<audio>` element with a silent data-URI WAV so
    `createMediaElementSource` has something to attach to → call
    `startVisualizer` directly → read back WebGL pixel data via
    `gl.readPixels` to confirm non-blank output, not just "no exception
    thrown") rather than trusting unit tests alone, since this is a
    rendering bug class Go's test suite has no way to catch.

## Done (this session, part 10)
- **Live Cut Sessions - crowdsourced live transition marking** (new
  `cmd/web/livecut.go`). Lets multiple MutiRec installs collaboratively flag
  candidate transition points *while a source is still recording live*,
  instead of one person re-listening to the whole thing alone afterward in
  the Set Cutter.
  - **Architecture reuses the existing P2P sharing shape almost exactly**
    (`sharing.go`): a "host" instance ties a session to one of its own live
    sources and exposes two public, token-authed endpoints -
    `POST /api/livecut/host/mark` and `GET /api/livecut/host/feed` - the
    same trust model as `/api/share/ping`/`/api/share/get/` (an unguessable
    token is the only credential; no session/account needed by the caller).
    A "guest" instance joins with a short code of the *exact same*
    `{u: publicURL, t: token}` shape as a P2P share code -
    `encodeShareCode`/`decodeShareCode` are reused as-is, no new encoding
    invented.
  - **One clock, not many**: the host stamps every mark's wall-clock
    timestamp itself (`LiveCutSession.addEvent`), regardless of which
    instance it came from, so cross-instance clock skew never enters the
    picture. Each mark is tagged with the submitting instance's `InstanceID`
    (new field on `AppConfig`, generated once via `shortToken()` and
    persisted like everything else in `config.json`), its `UI.AppName`, and
    the acting user's username.
  - **Any authenticated role can mark, not just admins** - crowdsourcing the
    button press across everyone watching is the point. `rbacAllowed`
    (`auth.go`) special-cases any path ending in `/mark` under
    `/api/livecut/sessions/` or `/api/livecut/joined/`; starting/closing/
    joining/importing a session stays admin-only (checked in-handler, same
    as the rest of this app's mutating admin-gated actions).
  - **Guest side is a thin read/write-through proxy, not a mirror**: guest
    endpoints (`/api/livecut/joined/{token}/mark`, `.../feed`) just forward
    to whatever instance is actually hosting the session and relay the
    response - no local caching or background job, since polling is already
    cheap and this avoids a second place marks could get out of sync (one
    fewer moving part than a `ShareJob`-style background job).
  - **Import into the Set Cutter**: `handleLiveCutImport` converts a
    session's collected wall-clock marks into `CutterMarker` offsets
    (`(markTs - recording.startedAt) / 1000`) and merges them into that
    recording's existing marker sidecar via the same
    `sidecarMarkersPath`/`writeSidecarJSON` helpers `handleCutterMarkers`
    already uses - so a crowdsourced session just pre-populates the normal
    Set Cutter review flow rather than being a separate export path.
  - Sessions are deliberately in-memory only (`App.liveCutSessions`/
    `liveCutJoined`, both `map[string]*...` guarded by their own mutex, same
    shape as `shareJobs`) - never persisted to `config.json`, cleared on
    restart, consistent with the existing sessions/backoff/share-nonce
    precedent.
  - **Frontend** (`index.html`/`app.js`): new "Live Cut Session" panel in
    the Watch tab, next to the source you're currently watching - matches
    the chosen entry point since this is inherently about a source that's
    actively recording. Shows host controls (code, "Mark Transition", live
    feed, close, "Send to Set Cutter") when hosting a session for the
    selected source, and an always-available "Join someone else's session"
    box independent of the selected source (a guest instance may have no
    matching local source at all). Feed updates via a plain ~1.5s
    `setInterval` poll (same convention as this app's existing 5s dashboard
    refresh and `ShareJob` progress polling) - no WebSocket/long-polling
    added, since marks are already wall-clock-stamped so sub-second delivery
    isn't needed.
  - Tests in `cmd/web/livecut_test.go`: event sequencing/`since`-cursor
    correctness, unknown/closed-token rejection on the public endpoints, the
    rbac exception actually letting a viewer role mark, the offset-conversion
    math plus the sidecar file it writes, and a full join→mark→feed round
    trip against a real `httptest.Server` standing in for "the host" (two
    independent `*App` instances talking over a real HTTP socket, not just
    in-process function calls).
  - Verified against two real, separately-running `mutirec` processes on
    different ports (admin setup/login, config with a public URL, the
    `POST /api/livecut/sessions` safeguard correctly returning 412 for a
    source that isn't actively recording) and a real browser session driving
    the new Watch tab panel (Start button correctly disabled until the
    source is recording, an invalid join code surfacing a clear error) -
    this sandbox has no `ffmpeg`, so a genuine live recording (and therefore
    the full cross-instance mark/feed flow between two real OS processes)
    couldn't be exercised end-to-end here; the `httptest`-backed Go test
    above is the strongest available substitute; anyone bringing up two real
    installs on their own machines gets full ffmpeg-backed verification for
    free the first time they use it.
  - **Hardening pass** (same feature, follow-up): the public token-authed
    mark endpoint is reachable by a whole crowd sharing one token, so
    `addEvent` now bounds each session at `maxLiveCutEvents` (5000 - far
    above any real session) and returns 429 past it instead of growing
    memory unbounded; hosted sessions are bounded at `maxLiveCutSessions`
    (100) via a new `putLiveCutSession` that evicts the least-useful session
    first (closed before open, oldest among equals - mirroring
    `putShareJob`); and starting a session twice for the same source now
    returns the existing open one (`openLiveCutSessionForSource`) instead of
    spawning a duplicate that competes for the same recording. Covered by
    `TestLiveCutSessionCapsEvents`, `TestHandleLiveCutHostMarkRejectsPastCap`,
    `TestHandleLiveCutSessionsReusesOpenSessionForSource`, and
    `TestPutLiveCutSessionEvictsClosedFirst`; the locking changes are
    `go test -race` clean.

## Done (this session, part 11) - Recordings tab polish

- **Recording thumbnails now actually show when set** (`app.js`,
  `uploads.go`). The library set cards used `loading="lazy"` on an `<img>`
  that started with the `hidden` class and only removed `hidden` in its
  `onload` handler - a deadlock: a `display:none` image never enters the
  viewport, so native lazy-loading never fetches it, so `onload` never fires,
  so it's never revealed. (This was masked in dev because Tailwind's CDN
  `.hidden` is what makes it `display:none`; the bug only bites where Tailwind
  is present, i.e. production.) Replaced with an `IntersectionObserver`-based
  lazy loader: cards render `data-thumb` (no `src`), and `observeThumbnails()`
  assigns the real `src` only when the card scrolls into view - reliable in
  the horizontal scroll rows where native lazy-loading is flaky, no hidden
  state to get stuck in, and the `onerror` fallback uses an inline style
  rather than depending on Tailwind's `.hidden`. Also made the "no thumbnail
  yet" 404 `Cache-Control: no-store` so a later-uploaded thumbnail shows
  without a hard refresh. Verified end-to-end in a real browser (image
  requested, 200, `naturalWidth>0`).
- **Smart Match now uses filename/folder keywords** (`main.go`,
  `match_test.go`). Previously it scored only stage, parsed time, guessed
  artist, and the source's manually-linked Festival - so two editions (or two
  different festivals) that reuse the same stage name and a touring artist on
  the same day were a coin flip. Added two keyword signals extracted from the
  whole path + filename: an event-name match (fraction of the event's
  identifying words present, e.g. a festival name and stage colour -
  generic filler, single chars, and bare years are dropped) worth +45, and a
  year match/conflict (±25) that settles which edition. These need no manual
  Festival linking, so they help the common case. Confidence/reason are
  enriched: a recording that literally names its event reads more trustworthy,
  and a named year that contradicts the matched edition knocks a "high" down
  to "medium" with a warning. New tests cover the helpers plus two
  disambiguation scenarios (festival-name keyword breaking a stage+artist tie;
  year keyword picking the right edition when the filename carries no date) -
  all placeholder names per the no-real-festival-data rule.
- **Live Cut Sessions frontend cleanup** (`app.js`, following up the earlier
  backend hardening): `livecutFeeds` (per-token event state) is now pruned
  each poll tick to only the sessions this instance is actually hosting or
  joined to, so switching sources or closing sessions no longer leaks their
  event lists for the life of the Watch tab; closing a hosted session clears
  its feed immediately; and the host code field is only rewritten when it
  changes, so the ~1.5s poll re-render no longer clobbers a manual
  text-selection mid-copy.

## Done (this session, part 12) - Notifications hardening + a test button

- **Fixed two real correctness bugs in `notify()`** (`main.go`). (1) The two
  call sites on the recording-finish path (`runRecording`) called
  `a.notify(...)` synchronously - a hanging Discord webhook or SMTP server
  would delay finishing a recording (and delay the `a.lastFinished` map
  write other features, like Live Cut Session import, read). Both now go
  through `go a.notify(...)`, matching the reminder call site's existing
  pattern. (2) The Discord webhook send checked neither the HTTP error nor
  the response status code - a deleted/rate-limited/misconfigured webhook
  failed completely silently, with nothing in the event log, unlike the SMTP
  path right next to it which already logged failures. Both channels are now
  sent concurrently (`sync.WaitGroup`) with a 15s timeout on the Discord
  client (`http.DefaultClient` has none), and both log failures to the event
  feed the same way via a shared `sendDiscordWebhook` helper.
- **Discord content-limit truncation**: a long body (e.g. a full
  ffmpeg/streamlink error message passed straight through) could exceed
  Discord's ~2000-character webhook content limit, which Discord rejects
  outright with a 400 - previously exactly the kind of failure point 1 above
  swallowed silently, so the notification would just vanish. Truncated by
  rune (not byte - a byte-slice both risks cutting a multi-byte rune in half
  and, since the "…" marker itself is multi-byte, could push the byte length
  past the limit even after "truncating") to exactly the limit.
- **New `POST /api/notifications/test` + "Send test notification" button**
  (Settings → Notifications). Tests whatever is currently typed into the
  Discord webhook / SMTP fields - not necessarily saved yet, the same
  "test before you save" convention `handleSourceTest` already established
  for sources - and reports each configured channel's result separately
  (`{tested, ok, error}` per channel), so a user can verify their setup
  immediately instead of waiting for a real recording or timetable reminder.
  Admin-only, matching every other settings-mutating action.
- Tests in `cmd/web/notifications_test.go`: webhook success/failure/
  truncation (including the rune-vs-byte truncation bug found while writing
  the test), the test endpoint's "nothing configured" case, a Discord-only
  test leaving SMTP untested, and admin-only enforcement. `go test -race`
  clean (the new `sync.WaitGroup` in `notify()`).
- Verified end-to-end in a real browser against a real running instance: a
  small local HTTP server standing in for a Discord webhook (since this
  sandbox has no network access to a real Discord webhook) received the
  exact POST body the button sent and returned success; pointing the field
  at a closed port produced the expected connection-refused error, rendered
  in the UI, not just logged.

## Done (this session, part 13) - Visual Timetable: midnight grouping, vertical layout, hidden empty stages

- **The "midnight thing" was a display bug, not an import bug.** The earlier
  fix (part-of-a-prior-session, `combineDateTime`) already dates an
  early-morning afterparty set to the correct *next* calendar day when
  importing - but the Timetable tab's visual grid then grouped sets by that
  literal ISO date, which put the afterparty on the *next* day's tab as a
  disconnected early-morning blip instead of keeping it attached to the
  festival day it's actually part of (a "Friday" program that runs to
  4am Saturday should still all show under Friday). Added
  `festivalDayOf`/`festivalMinutes` (`app.js`) mirroring the backend's
  `festivalDayRolloverHour` convention (before 08:00 belongs to the
  *previous* festival day) and used them for both `timetableDays()` and the
  set-to-day grouping/positioning that used to key everything off `sp.date`
  directly. Verified in a real browser: a stage with a 20:00-23:00 main set
  and a genuinely-next-calendar-day 01:00-04:00 afterparty set now produces
  exactly one day tab, with the afterparty sorted after the main set and
  displayed as "01:00–04:00", not a phantom second day.
- **Stages with no sets on the selected day are hidden by default**, with a
  small "+N stage(s) with no sets today" button to reveal them (and "Hide N
  empty stage(s)" once shown). Deliberately per-day, not "ever had zero sets
  globally" - a stage can be fully programmed for Friday and still have
  nothing entered for Saturday yet, and hiding it there shouldn't make it
  vanish permanently. Also deliberately never hides *everything*: if no stage
  has a set on the selected day at all, every stage still shows (with its
  "+ Set" button) so there's still a way to add the day's first set - a
  global hide would otherwise be a dead end, since new stages are only ever
  created via the raw JSON/import path (there's no separate "add stage" UI).
- **Side-by-side stage columns for 4 or fewer stages** (`maxVerticalStages`
  in `app.js`, new `.tt-col*` rules in `app.css`). Compressing a whole
  festival day into a handful of thin horizontal timeline tracks (the
  existing layout, still used above the threshold) reads poorly and wastes
  most of the panel as empty space when there are only 1-4 stages - each
  stage is now a column, its sets listed top-to-bottom in chronological
  order with real clock times, like a printed festival day-schedule poster.
  Reuses the exact same `editTimetableSet`/`addTimetableSet`/`toggleFavorite`
  handlers as the row layout, so nothing about *editing* a set differs
  between the two - only how they're arranged. The 4-vs-5 boundary is based
  on stages actually visible that day (after the empty-stage hiding above),
  not the timetable's total stage count. Verified in a real browser: exactly
  4 stages-with-sets renders `.tt-cols`, a 5th flips it back to `.tt-row`.

## Done (this session, part 14) - YouTube auto-upload, Stack share fix, Watch tab source filter
- **YouTube auto-upload** (`youtube.go`, new file): finished recordings can now auto-upload to
  YouTube as private/unlisted/public (default unlisted). `Settings.YouTube` (`YouTubeConfig`:
  enabled/clientId/clientSecret/refreshToken) holds a pasted-in long-lived OAuth2 refresh token
  (generated by the admin ahead of time, e.g. via Google's OAuth 2.0 Playground) rather than a
  full interactive consent flow - simpler to wire up than adding a whole Google OAuth redirect
  dance for a single admin-configured integration. New per-source fields `YouTubeUpload` (bool)
  and `YouTubePrivacy` (string, defaults to "unlisted" via `youtubePrivacyOrDefault`). Upload is
  the standard two-step resumable protocol: POST metadata to get a session `Location` URL, then
  one-shot PUT the file to it (no chunked retry - matches the codebase's existing "best-effort,
  event-logged, never blocks the recording" shape used by `a.backup()`). Hooked in right after
  `a.backup(rec)` in `runRecording`. `ClientSecret`/`RefreshToken` redacted for non-admins in
  `redactSecrets`. Settings panel + per-source checkbox/privacy dropdown in `index.html`/`app.js`.
  Audio-only sources are skipped with a logged warning (not supported yet).
- **Stack (stackstorage) share download fix** (`urlfetch.go`): the existing "fetch from URL"
  explorer feature assumed all self-hosted share links follow the ownCloud/Nextcloud convention
  (`/s/<token>/download` returns the file directly) - Stack instead serves a single-page app at
  that URL, so the "download" just saved the SPA's HTML shell. Added a second code path that
  talks to Stack's actual v2 JSON API: `parseStackFilesURL` recognizes a share URL that already
  names a folder (`/s/<token>/<locale>/files/<nodeID>`) and goes straight to the API; a bare
  `/s/<token>` link still tries the ownCloud-style guess first and falls back to the Stack API
  (best-effort at the share's root) if the response comes back as `text/html`. `listStackNodes`/
  `gatherStackFiles` paginate and recursively walk `nodes?parentID=...`, `downloadStackFile`
  streams each file from `files/<id>/download/<name>`; a CSRF token is best-effort scraped out of
  the share page's HTML (`extractStackCSRFToken`) since the download endpoint wants one but the
  listing endpoints don't require it. Names are sanitized (`sanitizeStackName`) before being
  joined into paths.
- **Watch tab source filter**: the Watch (Live View) tab's source picker and each source card's
  "Watch Live" button used to show/appear for every source, but playback for a source without
  Live Rewind enabled just resolves the raw `streamlink --stream-url` output, which usually isn't
  something a `<video>` tag can actually play - it looked broken with no indication why. Both
  `populateWatchSourceDropdown` and the source card button now only show for sources with
  `liveRewind` enabled, where the HLS rewind buffer actually gives something playable.

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
- The festival/edition-name/genre tail of filenames like "..._Neonbeat_Prime_Directive_HardDance"
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
- Consider server-side HLS restreaming for sources whose CDN blocks cross-origin playback.
- Add backup queue history with retry controls.
- Add Prometheus metrics and healthcheck endpoint.
- The reconnect backoff is per-source and in-memory only; if this app is ever run with more
  than one process/replica behind a shared config, backoff state won't be shared. Not a
  problem for the single-process deployment this app currently assumes.
- Consider more preset packs (other festivals/events) as they come up - now that there's no
  bundled default timetable/source list at all (removed to avoid shipping any specific
  festival's real schedule/branding), a preset pack is the only "starter content" this app
  offers, so it's a reasonable place to grow.
- Per-user favorites/reminders: `Settings.FavoriteSetIDs` is still one global list shared by
  every account, not per-user - a viewer starring a set would (if ever allowed to) affect
  everyone. Fine for now since only admins can reach Settings/the favorite toggle currently,
  but worth revisiting if viewers should get their own reminders later.
- Sessions are in-memory only (same as before multi-user support) - restarting the process
  logs everyone out. Not a problem for how this app is deployed (a single long-running
  container), but would need a persisted session store if that ever changes.
- No "sign out of all other sessions" control yet, and no visibility into which
  sessions/devices are currently logged in as a given user - fine for a small trusted group,
  worth adding if this ever opens up to a larger or less-trusted set of accounts.

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
- **A real reported recording filename convention (genericized here)** (from a real user example):
  `{Artist}_{Stage}_{Weekday}_{DD}_{MM}_{YYYY}_{Festival}_{EditionTheme}_{Genre}.ext`, e.g.
  `DJ_Vertex_BLUE_Thursday_25_06_2026_Neonbeat_Prime_Directive_HardDance.mp3`. Stage names are
  short channel-style labels ("BLUE", "RED", "BLACK", etc.) that match the recording's own
  folder/channel - they are *not* the flowery per-edition theme name that follows the date
  (e.g. "Prime Directive" is 2026's edition subtitle, not a stage). Don't guess meaning from a
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
