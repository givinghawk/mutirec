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
