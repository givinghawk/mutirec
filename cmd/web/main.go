package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/smtp"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"mutirec/internal/disk"
)

var version = "dev"

//go:embed static/*
var staticFiles embed.FS

//go:embed presets/presets.json
var presetsFile embed.FS

type AppConfig struct {
	Settings        Settings                 `json:"settings"`
	UI              UISettings               `json:"ui"`
	Sources         []Source                 `json:"sources"`
	Timetable       []StageSchedule          `json:"timetable"`
	TimetableSource *TimetableLink           `json:"timetableSource,omitempty"`
	SavedTimetables []SavedTimetable         `json:"savedTimetables,omitempty"`
	LibraryEvents   []LibraryEvent           `json:"libraryEvents"`
	RecordingMeta   map[string]RecordingMeta `json:"recordingMeta"`
	Festivals       []Festival               `json:"festivals"`
	Organisations   []Organisation           `json:"organisations"`
	Shares          []Share                  `json:"shares,omitempty"`
	// InstanceID identifies this install to *other* installs it collaborates
	// with (currently: Live Cut Sessions) - generated once on first load and
	// persisted, never user-editable. Distinct from any Share/session token,
	// which are per-share/per-session rather than per-install.
	InstanceID string `json:"instanceId,omitempty"`
}

// Festival is the recurring franchise a live Source belongs to (e.g.
// "Neonbeat", "Aurora Nights") - shown to the user simply as "Event". It's
// intentionally a separate, lighter-weight concept from LibraryEvent (which
// represents one specific yearly edition/recording archive): a live Source's
// stream URL is reused every year, so it's grouped by the franchise rather
// than any one edition. Named Festival in code only to avoid colliding with
// the unrelated Event type used for the activity log below.
type Festival struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Description    string `json:"description,omitempty"`
	Color          string `json:"color,omitempty"`
	LogoURL        string `json:"logoUrl,omitempty"`
	OrganisationID string `json:"organisationId,omitempty"`
}

// Organisation is the parent label above Festivals — a promoter, brand, or
// collective that owns one or more Festival franchises. Optional: Festivals
// that don't belong to an Organisation still show up as standalone entries.
type Organisation struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Color       string `json:"color,omitempty"`
	LogoURL     string `json:"logoUrl,omitempty"`
}

// LibraryEvent groups recorded files into one edition of a festival (e.g.
// "Neonbeat 2022"), independent of whatever Sources/Timetable are currently
// configured for live recording. Each event can carry its own archived
// timetable, imported separately, so old recordings can be matched back up
// to the artist/set that was actually playing when they were captured.
type LibraryEvent struct {
	ID          string          `json:"id"`
	Name        string          `json:"name"`
	Year        int             `json:"year,omitempty"`
	StartDate   string          `json:"startDate,omitempty"`
	EndDate     string          `json:"endDate,omitempty"`
	Color       string          `json:"color,omitempty"`
	CoverURL    string          `json:"coverUrl,omitempty"`
	Description string          `json:"description,omitempty"`
	FestivalID  string          `json:"festivalId,omitempty"`
	Timetable   []StageSchedule `json:"timetable,omitempty"`
}

// RecordingMeta links one recorded file (keyed by its path relative to
// FinishedDir) to a LibraryEvent and, optionally, a specific archived
// timetable set. Channel is an optional display-name override for grouping
// in the library UI - if blank, the recording's source folder name is used.
type RecordingMeta struct {
	EventID   string `json:"eventId,omitempty"`
	Channel   string `json:"channel,omitempty"`
	SetID     string `json:"setId,omitempty"`
	Artist    string `json:"artist,omitempty"`
	Start     string `json:"start,omitempty"`
	End       string `json:"end,omitempty"`
	Tracklist string `json:"tracklist,omitempty"`
}

type Settings struct {
	FinishedDir             string             `json:"finishedDir"`
	TempDir                 string             `json:"tempDir"`
	LogDir                  string             `json:"logDir"`
	CheckIntervalSeconds    int                `json:"checkIntervalSeconds"`
	MinFreeBytes            uint64             `json:"minFreeBytes"`
	DefaultQuality          string             `json:"defaultQuality"`
	DefaultContainer        string             `json:"defaultContainer"`
	EnableNFO               bool               `json:"enableNfo"`
	EnableWaveform          bool               `json:"enableWaveform"`
	Backup                  BackupConfig       `json:"backup"`
	Notifications           Notifications      `json:"notifications"`
	AllowLiveProxy          bool               `json:"allowLiveProxy"`
	WarnFreeBytes           uint64             `json:"warnFreeBytes"`
	LiveRewindWindowSeconds int                `json:"liveRewindWindowSeconds"`
	FavoriteSetIDs          []string           `json:"favoriteSetIds"`
	ReminderLeadMinutes     int                `json:"reminderLeadMinutes"`
	RecordingSetLookahead   time.Duration      `json:"-"`
	DiscordOAuth            DiscordOAuthConfig `json:"discordOAuth"`
	Sharing                 SharingConfig      `json:"sharing"`
	FileExplorerRoot        string             `json:"fileExplorerRoot,omitempty"`
	YouTube                 YouTubeConfig      `json:"youtube"`
	TranscodePresets        []TranscodePreset  `json:"transcodePresets,omitempty"`
	TranscodeRules          []TranscodeRule    `json:"transcodeRules,omitempty"`
}

// YouTubeConfig holds this instance's YouTube Data API v3 credentials,
// used to auto-upload finished recordings. Authenticates with a pasted-in
// long-lived OAuth2 refresh token (generated by the admin ahead of time via
// Google's OAuth playground or equivalent) rather than an interactive
// consent flow, paired with the Google Cloud OAuth client ID/secret that
// issued it.
type YouTubeConfig struct {
	Enabled      bool   `json:"enabled"`
	ClientID     string `json:"clientId"`
	ClientSecret string `json:"clientSecret,omitempty"`
	RefreshToken string `json:"refreshToken,omitempty"`
}

// DiscordOAuthConfig holds this instance's Discord OAuth2 application
// credentials (from https://discord.com/developers/applications), used only
// to let an *existing* user (created by an admin) link their Discord account
// for a faster login - there's no self-service signup via Discord, so a
// stranger who happens to have a Discord account still can't get in without
// an admin-created account first.
type DiscordOAuthConfig struct {
	Enabled      bool   `json:"enabled"`
	ClientID     string `json:"clientId"`
	ClientSecret string `json:"clientSecret,omitempty"`
	RedirectURL  string `json:"redirectUrl"`
}

type UISettings struct {
	AppName     string            `json:"appName"`
	LogoURL     string            `json:"logoUrl"`
	Theme       string            `json:"theme"`
	Accent      string            `json:"accent"`
	CustomCSS   string            `json:"customCss"`
	CustomTheme string            `json:"customTheme"`
	ThemeColors map[string]string `json:"themeColors"`
}

type BackupConfig struct {
	Enabled       bool     `json:"enabled"`
	RcloneRemote  string   `json:"rcloneRemote"`
	RcloneArgs    []string `json:"rcloneArgs"`
	AfterComplete bool     `json:"afterComplete"`

	// Method picks which backup mechanism a.backup() uses: "rclone" (the
	// original, default when empty) shells out to the rclone binary above;
	// "webdav" instead PUTs the file directly to WebDAV, for a plain WebDAV
	// server with no rclone remote configured for it.
	Method string       `json:"method,omitempty"`
	WebDAV WebDAVBackup `json:"webdav,omitempty"`
}

// WebDAVBackup holds the destination and credentials for BackupConfig's
// "webdav" method - a finished recording is PUT to WebDAVBackup.URL plus its
// path relative to FinishedDir, creating any intermediate WebDAV
// collections (MKCOL) that don't already exist.
type WebDAVBackup struct {
	URL      string `json:"url"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
	Proxy    bool   `json:"proxy,omitempty"` // route through Settings.Sharing.ProxyURL
}

type Notifications struct {
	DiscordWebhook string     `json:"discordWebhook"`
	SMTP           SMTPConfig `json:"smtp"`
}

type SMTPConfig struct {
	Enabled  bool   `json:"enabled"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Username string `json:"username"`
	Password string `json:"password"`
	From     string `json:"from"`
	To       string `json:"to"`
}

type Source struct {
	ID                string   `json:"id"`
	Name              string   `json:"name"`
	Type              string   `json:"type"`
	URL               string   `json:"url"`
	Enabled           bool     `json:"enabled"`
	Record            bool     `json:"record"`
	Quality           string   `json:"quality"`
	Container         string   `json:"container"`
	AudioOnly         bool     `json:"audioOnly"`
	Transcode         bool     `json:"transcode"`
	HardwareAccel     string   `json:"hardwareAccel"`
	StreamlinkArgs    []string `json:"streamlinkArgs"`
	FFmpegArgs        []string `json:"ffmpegArgs"`
	ExtraNFO          string   `json:"extraNfo"`
	Color             string   `json:"color"`
	LiveRewind        bool     `json:"liveRewind"`
	TimetableStage    string   `json:"timetableStage,omitempty"`
	FestivalID        string   `json:"festivalId,omitempty"`
	LoudnessNormalize bool     `json:"loudnessNormalize,omitempty"`
	HTTPHeaders       []string `json:"httpHeaders,omitempty"`
	YouTubeUpload     bool     `json:"youtubeUpload,omitempty"`
	YouTubePrivacy    string   `json:"youtubePrivacy,omitempty"`
}

// SourcePreset is one bundled, ready-to-add pack of sources - a DJ/streamer,
// an event, or (in the future) a whole festival's set of stages - offered in
// the Sources tab so a common setup can be added in one click instead of
// hand-entering each URL. Bundled read-only in presets/presets.json; nothing
// here is user-editable (users still add/edit their own sources normally).
type SourcePreset struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Category    string   `json:"category,omitempty"`
	Description string   `json:"description,omitempty"`
	LogoURL     string   `json:"logoUrl,omitempty"`
	Sources     []Source `json:"sources"`
}

type StageSchedule struct {
	Stage string        `json:"stage"`
	URL   string        `json:"url"`
	Color string        `json:"color,omitempty"`
	Sets  []ScheduleSet `json:"sets"`
}

type ScheduleSet struct {
	ID    string `json:"id,omitempty"`
	Start string `json:"start"`
	End   string `json:"end"`
	Name  string `json:"name"`
}

// TimetableLink records which timetable.lol event our local timetable was
// last imported from, purely for display/attribution and re-sync - linking
// to timetable.lol is entirely optional and the timetable can just as well
// be edited by hand or via the visual editor.
type TimetableLink struct {
	EventSlug  string    `json:"eventSlug"`
	EventTitle string    `json:"eventTitle"`
	PlanType   string    `json:"planType"`
	SourceURL  string    `json:"sourceUrl"`
	ImportedAt time.Time `json:"importedAt"`
}

// SavedTimetable is a named, timestamped snapshot of an imported timetable,
// kept independently of the live, mutable AppConfig.Timetable so a previous
// import can be switched back to instantly instead of re-fetching or
// re-uploading it. Written automatically by every import path (file upload,
// timetable.lol) - not the raw JSON editor's plain save, which would create
// one of these per keystroke-ish edit instead of per actual import.
type SavedTimetable struct {
	ID         string          `json:"id"`
	Name       string          `json:"name"`
	Source     string          `json:"source,omitempty"` // "file upload" or "timetable.lol"
	ImportedAt time.Time       `json:"importedAt"`
	Stages     int             `json:"stages"`
	Sets       int             `json:"sets"`
	Schedule   []StageSchedule `json:"schedule"`
}

// maxSavedTimetables bounds how many snapshots pile up in config.json - old
// ones are dropped oldest-first once the cap is hit.
const maxSavedTimetables = 30

// snapshotTimetable records schedule as a new saved-timetable entry on cfg,
// called by every import handler right after it overwrites the live
// timetable.
func snapshotTimetable(cfg *AppConfig, name, source string, schedule []StageSchedule) {
	stages := len(schedule)
	sets := 0
	for _, s := range schedule {
		sets += len(s.Sets)
	}
	if name == "" {
		name = time.Now().Format("2006-01-02 15:04")
	}
	cfg.SavedTimetables = append(cfg.SavedTimetables, SavedTimetable{
		ID: newID(), Name: name, Source: source, ImportedAt: time.Now(),
		Stages: stages, Sets: sets, Schedule: append([]StageSchedule(nil), schedule...),
	})
	if len(cfg.SavedTimetables) > maxSavedTimetables {
		cfg.SavedTimetables = cfg.SavedTimetables[len(cfg.SavedTimetables)-maxSavedTimetables:]
	}
}

type State struct {
	Version         string              `json:"version"`
	StartedAt       time.Time           `json:"startedAt"`
	Sources         []SourceStatus      `json:"sources"`
	Events          []Event             `json:"events"`
	Disk            disk.Usage          `json:"disk"`
	Config          AppConfig           `json:"config"`
	ActiveCount     int                 `json:"activeCount"`
	Warnings        []string            `json:"warnings"`
	NowPlaying      map[string]*NowItem `json:"nowPlaying"`
	Role            Role                `json:"role,omitempty"`
	StorageForecast StorageForecast     `json:"storageForecast"`
}

// StorageForecast projects how much recording time is left at the current
// combined write rate of every active recording, so a large multi-stage
// festival with several 4K streams going at once gets a meaningfully
// different estimate than a single audio-only source. Not applicable (no
// hours figure) when nothing is actively recording, since there's no
// current rate to extrapolate from.
type StorageForecast struct {
	Applicable       bool    `json:"applicable"`
	BytesPerSecond   float64 `json:"bytesPerSecond"`
	ActiveRecordings int     `json:"activeRecordings"`
	HoursRemaining   float64 `json:"hoursRemaining,omitempty"`
}

// minForecastSampleSeconds is how long a recording must have been running
// before its size/elapsed-time ratio is trusted as a rate - a recording
// that just started hasn't written enough yet for that ratio to mean much
// (ffmpeg/streamlink startup overhead would dominate it).
const minForecastSampleSeconds = 5.0

// computeStorageForecast estimates the current aggregate write rate across
// every active recording (each one's own size ÷ elapsed time, summed) and
// projects how many hours of recording remain at that rate given the
// volume's current free space.
func computeStorageForecast(active map[string]*recording, freeBytes uint64) StorageForecast {
	var totalBps float64
	count := 0
	now := time.Now()
	for _, rec := range active {
		elapsed := now.Sub(rec.startedAt).Seconds()
		if elapsed < minForecastSampleSeconds {
			continue
		}
		info, err := os.Stat(rec.tempPath)
		if err != nil {
			continue
		}
		totalBps += float64(info.Size()) / elapsed
		count++
	}
	if count == 0 || totalBps <= 0 {
		return StorageForecast{ActiveRecordings: count}
	}
	return StorageForecast{
		Applicable: true, BytesPerSecond: totalBps, ActiveRecordings: count,
		HoursRemaining: float64(freeBytes) / totalBps / 3600,
	}
}

type SourceStatus struct {
	Source
	Status            string    `json:"status"`
	OutputPath        string    `json:"outputPath"`
	MediaPath         string    `json:"mediaPath,omitempty"`
	Size              int64     `json:"size"`
	StartedAt         time.Time `json:"startedAt,omitempty"`
	LastError         string    `json:"lastError,omitempty"`
	CurrentSet        string    `json:"currentSet,omitempty"`
	NextSet           string    `json:"nextSet,omitempty"`
	LogPath           string    `json:"logPath,omitempty"`
	LastHeartbeat     time.Time `json:"lastHeartbeat,omitempty"`
	LiveRewindActive  bool      `json:"liveRewindActive"`
	Orphaned          bool      `json:"orphaned,omitempty"`
	ReconnectAttempts int       `json:"reconnectAttempts,omitempty"`
	NextRetryAt       time.Time `json:"nextRetryAt,omitempty"`
}

// RecordingFile describes a single finished recording on disk, enriched with
// whatever library metadata (event/channel/artist/set) has been assigned to
// it via RecordingMeta.
type RecordingFile struct {
	Name      string    `json:"name"`
	Source    string    `json:"source"`
	Path      string    `json:"path"`
	Size      int64     `json:"size"`
	ModTime   time.Time `json:"modTime"`
	EventID   string    `json:"eventId,omitempty"`
	Channel   string    `json:"channel,omitempty"`
	SetID     string    `json:"setId,omitempty"`
	Artist    string    `json:"artist,omitempty"`
	Start     string    `json:"start,omitempty"`
	End       string    `json:"end,omitempty"`
	Tracklist string    `json:"tracklist,omitempty"`
}

type NowItem struct {
	SetName string `json:"setName"`
	Starts  string `json:"starts"`
	Ends    string `json:"ends"`
}

type Event struct {
	Time  time.Time `json:"time"`
	Level string    `json:"level"`
	Text  string    `json:"text"`
}

type recording struct {
	source       Source
	ctx          context.Context
	cancel       context.CancelFunc
	startedAt    time.Time
	timeOffsetMs int64
	timeSource   string
	tempPath     string
	finalPath    string
	logPath      string
	logFile      *os.File
	lastErr      string
	done         chan struct{}
	hlsDir       string
	manualStop   atomic.Bool
}

type App struct {
	mu            sync.RWMutex
	cfg           AppConfig
	config        string
	startedAt     time.Time
	active        map[string]*recording
	events        []Event
	lastFinished  map[string]string
	authUser      string
	authPass      string
	usersFile     string
	remindersSent map[string]time.Time

	retryMu sync.Mutex
	retry   map[string]*retryState

	sessMu   sync.Mutex
	sessions map[string]sessionInfo

	usersMu    sync.RWMutex
	users      []User
	needsSetup bool

	oauthMu    sync.Mutex
	oauthState map[string]pendingOAuth

	shareNonceMu sync.Mutex
	shareNonces  map[string]time.Time

	shareJobsMu sync.Mutex
	shareJobs   map[string]*ShareJob

	fetchJobsMu sync.Mutex
	fetchJobs   map[string]*URLFetchJob

	liveCutMu       sync.Mutex
	liveCutSessions map[string]*LiveCutSession // token -> session this instance is hosting

	liveCutJoinedMu sync.Mutex
	liveCutJoined   map[string]*joinedLiveCut // token -> a session hosted elsewhere, joined by this instance

	hashMu    sync.Mutex
	hashCache map[string]hashCacheEntry

	thumbGenMu      sync.Mutex
	thumbGenerating map[string]chan struct{}

	cutterJobsMu sync.Mutex
	cutterJobs   map[string]*CutterJob

	transcodeJobsMu sync.Mutex
	transcodeJobs   map[string]*TranscodeJob

	detectJobsMu sync.Mutex
	detectJobs   map[string]*DetectJob

	sourcePresets []SourcePreset
}

// hashCacheEntry remembers the sha256 hash last computed for a recording, so
// re-scans (matchfile export/import) don't re-hash unchanged files every
// time - keyed by path relative to FinishedDir, invalidated by size/mtime
// change. Kept in memory only; a cold restart just re-hashes on first use.
type hashCacheEntry struct {
	ModTime time.Time
	Size    int64
	Hash    string
}

func main() {
	configPath := localizePath(env("CONFIG_PATH", "/app/config/config.json"))
	app, err := NewApp(configPath)
	if err != nil {
		log.Fatal(err)
	}
	app.setupAuth()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go app.scheduler(ctx)

	mux := http.NewServeMux()
	app.routes(mux)

	addr := env("HTTP_ADDR", "")
	if addr == "" {
		if p := os.Getenv("PORT"); p != "" {
			addr = ":" + p
		} else {
			addr = ":8080"
		}
	}
	server := &http.Server{Addr: addr, Handler: securityHeaders(app.requireAuth(mux))}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
		app.stopAll()
	}()

	log.Printf("MutiRec web UI listening on %s", addr)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

func NewApp(configPath string) (*App, error) {
	cfg, err := loadConfig(configPath)
	if err != nil {
		return nil, err
	}
	app := &App{
		cfg:             cfg,
		config:          configPath,
		startedAt:       time.Now(),
		active:          map[string]*recording{},
		lastFinished:    map[string]string{},
		remindersSent:   map[string]time.Time{},
		retry:           map[string]*retryState{},
		sessions:        map[string]sessionInfo{},
		oauthState:      map[string]pendingOAuth{},
		shareNonces:     map[string]time.Time{},
		shareJobs:       map[string]*ShareJob{},
		fetchJobs:       map[string]*URLFetchJob{},
		liveCutSessions: map[string]*LiveCutSession{},
		liveCutJoined:   map[string]*joinedLiveCut{},
		cutterJobs:      map[string]*CutterJob{},
		transcodeJobs:   map[string]*TranscodeJob{},
		detectJobs:      map[string]*DetectJob{},
		sourcePresets:   loadSourcePresets(),
	}
	for _, dir := range []string{cfg.Settings.FinishedDir, cfg.Settings.TempDir, cfg.Settings.LogDir, filepath.Dir(configPath)} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	app.event("info", "Recorder started")
	return app, nil
}

func serveStaticPage(w http.ResponseWriter, r *http.Request, path string) {
	data, err := staticFiles.ReadFile(path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(data)
}

// securityHeaders adds a small set of defensive headers to every response.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

func (a *App) routes(mux *http.ServeMux) {
	staticFS, _ := fs.Sub(staticFiles, "static")
	mux.Handle("/", http.FileServer(http.FS(staticFS)))
	mux.HandleFunc("/login", a.handleLoginPage)
	mux.HandleFunc("/api/login", a.handleLogin)
	mux.HandleFunc("/api/logout", a.handleLogout)
	mux.HandleFunc("/setup", a.handleSetupPage)
	mux.HandleFunc("/api/setup", a.handleSetup)
	mux.HandleFunc("/api/account", a.handleAccount)
	mux.HandleFunc("/api/users", a.handleUsers)
	mux.HandleFunc("/api/users/", a.handleUserItem)
	mux.HandleFunc("/api/auth/discord/status", a.handleDiscordStatus)
	mux.HandleFunc("/api/auth/discord/login/start", a.handleDiscordLoginStart)
	mux.HandleFunc("/api/auth/discord/link/start", a.handleDiscordLinkStart)
	mux.HandleFunc("/api/auth/discord/callback", a.handleDiscordCallback)
	mux.HandleFunc("/api/auth/discord/unlink", a.handleDiscordUnlink)
	mux.HandleFunc("/api/state", a.handleState)
	mux.HandleFunc("/api/system-check", a.handleSystemCheck)
	mux.HandleFunc("/api/config", a.handleConfig)
	mux.HandleFunc("/api/presets", a.handlePresets)
	mux.HandleFunc("/api/sources", a.handleSources)
	mux.HandleFunc("/api/sources/test", a.handleSourceTest)
	mux.HandleFunc("/api/notifications/test", a.handleNotificationsTest)
	mux.HandleFunc("/api/sources/", a.handleSourceItem)
	mux.HandleFunc("/api/timetable", a.handleTimetable)
	mux.HandleFunc("/api/timetable/import", a.handleTimetableImport)
	mux.HandleFunc("/api/timetable/favorites", a.handleTimetableFavorites)
	mux.HandleFunc("/api/timetable/lol-events", a.handleTimetableLolEvents)
	mux.HandleFunc("/api/timetable/lol-import", a.handleTimetableLolImport)
	mux.HandleFunc("/api/timetable/lol-unlink", a.handleTimetableLolUnlink)
	mux.HandleFunc("/api/timetable/saved/", a.handleTimetableSavedItem)
	mux.HandleFunc("/api/record/", a.handleRecordAction)
	mux.HandleFunc("/api/live/", a.handleLive)
	mux.HandleFunc("/api/recordings", a.handleRecordings)
	mux.HandleFunc("/api/recordings/meta", a.handleRecordingMeta)
	mux.HandleFunc("/api/recordings/match-suggestions", a.handleRecordingMatchSuggestions)
	mux.HandleFunc("/api/recordings/matchfile/export", a.handleRecordingsMatchfileExport)
	mux.HandleFunc("/api/recordings/matchfile/import", a.handleRecordingsMatchfileImport)
	mux.HandleFunc("/api/recordings/thumbnail", a.handleRecordingThumbnail)
	mux.HandleFunc("/api/recordings/thumbnail/regenerate", a.handleRecordingThumbnailRegenerate)
	mux.HandleFunc("/api/recordings/timecode", a.handleRecordingTimecode)
	mux.HandleFunc("/api/recordings/waveform", a.handleRecordingWaveform)
	mux.HandleFunc("/api/recordings/backfill-timecodes", a.handleBackfillTimecodes)
	mux.HandleFunc("/api/cutter/markers", a.handleCutterMarkers)
	mux.HandleFunc("/api/cutter/preview", a.handleCutterPreview)
	mux.HandleFunc("/api/cutter/export", a.handleCutterExport)
	mux.HandleFunc("/api/cutter/jobs/", a.handleCutterJobItem)
	mux.HandleFunc("/api/cutter/detect", a.handleCutterDetect)
	mux.HandleFunc("/api/cutter/detect/jobs/", a.handleCutterDetectJobItem)
	mux.HandleFunc("/api/transcode/start", a.handleTranscodeStart)
	mux.HandleFunc("/api/transcode/jobs/", a.handleTranscodeJobItem)
	mux.HandleFunc("/api/transcode/presets", a.handleTranscodePresets)
	mux.HandleFunc("/api/transcode/presets/", a.handleTranscodePresetItem)
	mux.HandleFunc("/api/transcode/rules", a.handleTranscodeRules)
	mux.HandleFunc("/api/transcode/rules/", a.handleTranscodeRuleItem)
	mux.HandleFunc("/api/uploads/image", a.handleImageUpload)
	mux.Handle("/uploads/", http.StripPrefix("/uploads/", http.FileServer(http.Dir(a.uploadsDir()))))
	// File explorer, rooted at Settings.FileExplorerRoot (defaults to FinishedDir).
	mux.HandleFunc("/api/explorer/list", a.handleExplorerList)
	mux.HandleFunc("/api/explorer/mkdir", a.handleExplorerMkdir)
	mux.HandleFunc("/api/explorer/rename", a.handleExplorerRename)
	mux.HandleFunc("/api/explorer/delete", a.handleExplorerDelete)
	mux.HandleFunc("/api/explorer/download", a.handleExplorerDownload)
	mux.HandleFunc("/api/explorer/upload", a.handleExplorerUpload)
	mux.HandleFunc("/api/explorer/zip", a.handleExplorerZip)
	mux.HandleFunc("/api/explorer/unzip", a.handleExplorerUnzip)
	mux.HandleFunc("/api/explorer/fetch", a.handleExplorerFetchURL)
	mux.HandleFunc("/api/explorer/fetch/jobs/", a.handleExplorerFetchJobItem)
	// Peer-to-peer sharing. /api/share/ping and /api/share/get/ are public
	// (see isPublicPath) - the rest are admin-gated by requireAuth/rbacAllowed.
	mux.HandleFunc("/api/share/ping", a.handleSharePing)
	mux.HandleFunc("/api/share/verify", a.handleShareVerify)
	mux.HandleFunc("/api/share/config", a.handleShareConfig)
	mux.HandleFunc("/api/share/preview", a.handleSharePreview)
	mux.HandleFunc("/api/share/import", a.handleShareImport)
	mux.HandleFunc("/api/share/jobs", a.handleShareJobs)
	mux.HandleFunc("/api/share/jobs/", a.handleShareJobItem)
	mux.HandleFunc("/api/share/get/", a.handleShareGet)
	mux.HandleFunc("/api/shares", a.handleShares)
	mux.HandleFunc("/api/shares/", a.handleShareItem)
	// Live Cut Sessions (crowdsourced live transition marking). The two
	// /host/ endpoints are public/token-authed like /api/share/ping and
	// /api/share/get/ above - see isPublicPath - since a remote instance's
	// backend, not a logged-in user of this one, calls them directly.
	mux.HandleFunc("/api/livecut/host/mark", a.handleLiveCutHostMark)
	mux.HandleFunc("/api/livecut/host/feed", a.handleLiveCutHostFeed)
	mux.HandleFunc("/api/livecut/sessions", a.handleLiveCutSessions)
	mux.HandleFunc("/api/livecut/sessions/", a.handleLiveCutSessionItem)
	mux.HandleFunc("/api/livecut/join", a.handleLiveCutJoin)
	mux.HandleFunc("/api/livecut/joined", a.handleLiveCutJoinedList)
	mux.HandleFunc("/api/livecut/joined/", a.handleLiveCutJoinedItem)
	mux.HandleFunc("/api/events", a.handleLibraryEvents)
	mux.HandleFunc("/api/events/", a.handleLibraryEventItem)
	mux.HandleFunc("/api/festivals", a.handleFestivals)
	mux.HandleFunc("/api/festivals/", a.handleFestivalItem)
	mux.HandleFunc("/api/organisations", a.handleOrganisations)
	mux.HandleFunc("/api/organisations/", a.handleOrganisationItem)
	mux.HandleFunc("/media/", a.handleMedia)
}

func (a *App) scheduler(ctx context.Context) {
	a.evaluate()
	interval := time.Duration(max(5, a.cfg.Settings.CheckIntervalSeconds)) * time.Second
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.evaluate()
		}
	}
}

func (a *App) evaluate() {
	cfg := a.snapshotConfig()
	a.checkReminders(cfg)
	usage := disk.Scan(cfg.Settings.FinishedDir)
	if usage.VolumeFree > 0 && usage.VolumeFree < cfg.Settings.MinFreeBytes {
		a.event("error", fmt.Sprintf("Recording paused: free disk space below %.1f GB", gb(cfg.Settings.MinFreeBytes)))
		return
	}
	if usage.VolumeFree > 0 && usage.VolumeFree < cfg.Settings.WarnFreeBytes {
		a.event("warn", fmt.Sprintf("Low disk space: %.1f GB free", gb(usage.VolumeFree)))
	}
	for _, src := range cfg.Sources {
		if !src.Enabled || !src.Record {
			continue
		}
		if a.isActive(src.ID) {
			continue
		}
		if _, blocked := a.retryBlocked(src.ID); blocked {
			continue
		}
		go func() {
			if !a.isSourceLive(src, cfg) {
				a.recordFailure(src.Name, src.ID)
				return
			}
			a.start(src)
		}()
	}
}

// liveCheckTimeout bounds how long the pre-flight liveness probe below is
// allowed to run before treating a source as "not live yet" - well under any
// reasonable CheckIntervalSeconds so a slow/hanging check on one source
// can't back up the rest.
const liveCheckTimeout = 15 * time.Second

// isSourceLive does a lightweight pre-flight check for whether a source
// actually has a live stream available right now, without spawning the full
// streamlink|ffmpeg recording pipeline. This is deliberately a *different*
// (cheaper) method than "just try to record it and see what happens": some
// streamlink plugins return a few KB of a placeholder/offline stream before
// erroring out, which used to be enough to clear minViableRecordingBytes and
// get saved as a real (but junk) recording on every retry of a flaky or
// offline channel. Checking first means an offline source never gets as far
// as producing an output file at all.
func (a *App) isSourceLive(src Source, cfg AppConfig) bool {
	ctx, cancel := context.WithTimeout(context.Background(), liveCheckTimeout)
	defer cancel()

	if src.Type == "http" {
		req, err := http.NewRequestWithContext(ctx, http.MethodHead, src.URL, nil)
		if err != nil {
			return false
		}
		// A token-gated stream (Authorization header, a signed cookie, a
		// custom auth header) needs the same headers the recording pipeline
		// sends via ffmpeg's -headers, or this probe sees a 401/403 and
		// wrongly reports the source as offline - see ffmpegArgs.
		for _, p := range parseHTTPHeaderLines(src.HTTPHeaders) {
			req.Header.Set(p[0], p[1])
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode < 400
	}

	quality := src.Quality
	if quality == "" {
		quality = cfg.Settings.DefaultQuality
	}
	if quality == "" {
		quality = "best"
	}
	slArgs := append([]string{"--stream-url"}, src.StreamlinkArgs...)
	slArgs = append(slArgs, src.URL, quality)
	out, err := exec.CommandContext(ctx, "streamlink", slArgs...).Output()
	return err == nil && strings.TrimSpace(string(out)) != ""
}

func (a *App) start(src Source) {
	a.mu.Lock()
	if _, ok := a.active[src.ID]; ok {
		a.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	started := time.Now()
	// One-shot wall-clock correction against worldtimeapi.org, used to anchor
	// this recording's timecode sidecar - see cutter.go. Best-effort: falls
	// back to the system clock silently if unreachable, bounded to a few
	// seconds so it never meaningfully delays a source starting.
	timeOffsetMs, timeSrc := timeCorrection()
	stageDir := filepath.Join(a.cfg.Settings.FinishedDir, safeName(src.Name))
	tempDir := filepath.Join(a.cfg.Settings.TempDir, safeName(src.Name))
	_ = os.MkdirAll(stageDir, 0o755)
	_ = os.MkdirAll(tempDir, 0o755)
	_ = os.MkdirAll(a.cfg.Settings.LogDir, 0o755)
	ext := src.Container
	if ext == "" {
		ext = a.cfg.Settings.DefaultContainer
	}
	if ext == "" {
		ext = "mkv"
	}
	base := fmt.Sprintf("%s.%s.%s", safeName(src.Name), started.Format("20060102-150405"), ext)
	tempPath := filepath.Join(tempDir, base+".part")
	finalPath := filepath.Join(stageDir, base)
	logPath := filepath.Join(a.cfg.Settings.LogDir, safeName(src.Name)+"-"+started.Format("20060102-150405")+".log")
	logFile, err := os.Create(logPath)
	if err != nil {
		cancel()
		a.mu.Unlock()
		a.event("error", fmt.Sprintf("[%s] cannot create log: %s", src.Name, err))
		return
	}
	var hlsDir string
	if src.LiveRewind {
		hlsDir = filepath.Join(tempDir, "live-hls")
		_ = os.RemoveAll(hlsDir)
		if err := os.MkdirAll(hlsDir, 0o755); err != nil {
			hlsDir = ""
			a.event("warn", fmt.Sprintf("[%s] could not create live rewind buffer: %s", src.Name, err))
		}
	}
	rec := &recording{source: src, ctx: ctx, cancel: cancel, startedAt: started, timeOffsetMs: timeOffsetMs, timeSource: timeSrc, tempPath: tempPath, finalPath: finalPath, logPath: logPath, logFile: logFile, done: make(chan struct{}), hlsDir: hlsDir}
	a.active[src.ID] = rec
	a.mu.Unlock()

	a.event("info", fmt.Sprintf("[%s] starting recording", src.Name))
	go a.runRecording(rec)
}

// minViableRecordingBytes is the smallest output size treated as a real
// recording rather than a bare, empty container. A stream that fails almost
// immediately (bad URL, wrong quality, blocked, offline channel...) can still
// leave ffmpeg's container header on disk - a few hundred bytes to a couple
// of KB with no actual media in it - which used to be silently accepted as a
// "finished" recording, burying the real failure behind a stack of tiny,
// unplayable files.
const minViableRecordingBytes = 64 * 1024

// retryState tracks the auto-reconnect backoff for a single source: how many
// consecutive short-lived failures it has had in a row, when it's next
// allowed to try again, and - separately - whether those retries are
// currently "visible" (logged and shown as a dashboard status) or silent.
// Kept in memory only - a restart just starts clean.
//
// A source that simply hasn't gone live yet retries silently forever (see
// windowUntil): that's normal, expected background polling, not an error.
// Only a source that *was* confirmed live and then stopped - whether that's
// a genuine dropped connection or just the broadcaster ending their stream -
// opens a visible reconnect window (see startReconnectWindow), so the user
// sees retry activity right when it's actually relevant (right after a
// stream they were recording went away) and it quietly stops being noisy if
// nothing comes back within reconnectVisibilityWindow.
type retryState struct {
	attempts    int
	nextAttempt time.Time
	windowUntil time.Time // zero => not currently in a visible reconnect window
}

// minStableRecordingDuration is how long a recording has to run before it's
// considered "the stream was actually working" rather than an immediate
// connection failure. A recording that produced output but died before this
// elapsed (bad URL, offline channel, network blip right at start) counts
// toward the reconnect backoff instead of being retried every scheduler tick.
const minStableRecordingDuration = 60 * time.Second

// reconnectVisibilityWindow is how long retry attempts stay visible (logged
// and shown as a "reconnecting" dashboard status) after a source that was
// confirmed live stops. Past this, the scheduler keeps quietly trying in the
// background - same as a source that simply hasn't gone live yet - since by
// this point it's more likely the broadcaster is done than that they're
// about to come back any second.
const reconnectVisibilityWindow = 10 * time.Minute

const (
	reconnectBaseDelay = 5 * time.Second
	reconnectMaxDelay  = 5 * time.Minute
)

// reconnectDelay computes an exponential backoff (5s, 10s, 20s, ... capped at
// reconnectMaxDelay) from the number of consecutive failures so far, so a
// stream that's genuinely down doesn't get hammered with a restart every
// scheduler tick.
func reconnectDelay(attempts int) time.Duration {
	shift := attempts - 1
	if shift < 0 {
		shift = 0
	}
	if shift > 6 {
		shift = 6
	}
	d := reconnectBaseDelay * time.Duration(1<<uint(shift))
	if d > reconnectMaxDelay {
		d = reconnectMaxDelay
	}
	return d
}

// retryBlocked reports whether a source is still within its reconnect
// backoff window and, if so, when it'll next be eligible. Applies equally to
// silent and visible retries - only the logging/dashboard status cares about
// the difference, not whether the scheduler should currently try again.
func (a *App) retryBlocked(sourceID string) (time.Time, bool) {
	a.retryMu.Lock()
	defer a.retryMu.Unlock()
	st := a.retry[sourceID]
	if st == nil || !time.Now().Before(st.nextAttempt) {
		return time.Time{}, false
	}
	return st.nextAttempt, true
}

// clearRetry resets a source's reconnect backoff entirely (including any
// visible window), used on a manual stop/start - the user is in control,
// not a dropped connection.
func (a *App) clearRetry(sourceID string) {
	a.retryMu.Lock()
	defer a.retryMu.Unlock()
	delete(a.retry, sourceID)
}

// startReconnectWindow opens a fresh, visible reconnect window for a source
// whose recording just ended after running stably (see
// minStableRecordingDuration) - it doesn't matter whether that end was a
// genuine drop or just the broadcaster stopping normally, since either way
// the scheduler is about to start silently retrying it and, unlike a source
// that's never gone live, this one's worth surfacing for a little while.
func (a *App) startReconnectWindow(sourceID string) {
	a.retryMu.Lock()
	defer a.retryMu.Unlock()
	a.retry[sourceID] = &retryState{windowUntil: time.Now().Add(reconnectVisibilityWindow)}
}

// recordFailure bumps a source's reconnect attempt counter and schedules the
// next allowed retry. It only logs the attempt (and only counts as a visible
// "reconnecting" status via reconnectStatus) while an active window from
// startReconnectWindow is still open; once that window lapses, it logs one
// final "giving up" notice and every attempt after that goes quiet, exactly
// like a source that's never gone live at all.
func (a *App) recordFailure(name, sourceID string) {
	a.retryMu.Lock()
	st := a.retry[sourceID]
	if st == nil {
		st = &retryState{}
		a.retry[sourceID] = st
	}
	wasVisible := !st.windowUntil.IsZero()
	stillVisible := wasVisible && time.Now().Before(st.windowUntil)
	st.attempts++
	delay := reconnectDelay(st.attempts)
	st.nextAttempt = time.Now().Add(delay)
	attempts := st.attempts
	if wasVisible && !stillVisible {
		st.windowUntil = time.Time{} // only report "giving up" once
	}
	a.retryMu.Unlock()

	switch {
	case stillVisible:
		a.event("warn", fmt.Sprintf("[%s] stream appears down - will retry in %s (attempt %d)", name, delay.Round(time.Second), attempts))
	case wasVisible:
		a.event("info", fmt.Sprintf("[%s] no reconnect within %s - will keep checking quietly in the background", name, reconnectVisibilityWindow))
	}
}

// reconnectStatus reports a source's live reconnect attempt count and next
// retry time, but only while it's within a visible window (see
// startReconnectWindow) - a source silently waiting to go live for the first
// time never reports as "reconnecting" on the dashboard.
func (a *App) reconnectStatus(sourceID string) (attempts int, nextAttempt time.Time, ok bool) {
	a.retryMu.Lock()
	defer a.retryMu.Unlock()
	st := a.retry[sourceID]
	if st == nil || st.windowUntil.IsZero() || !time.Now().Before(st.windowUntil) {
		return 0, time.Time{}, false
	}
	return st.attempts, st.nextAttempt, true
}

func (a *App) runRecording(rec *recording) {
	defer close(rec.done)
	defer rec.logFile.Close()

	err := a.execute(rec)
	failed := err != nil && rec.ctx.Err() == nil
	if failed {
		rec.lastErr = errorWithLogDetail(err, rec.logPath)
		a.event("error", fmt.Sprintf("[%s] %s", rec.source.Name, rec.lastErr))
	}
	rec.cancel()

	info, statErr := os.Stat(rec.tempPath)
	hasOutput := statErr == nil && info.Size() > 0
	if hasOutput && failed && info.Size() < minViableRecordingBytes {
		_ = os.Remove(rec.tempPath)
		hasOutput = false
		a.event("error", fmt.Sprintf("[%s] discarded a %d-byte recording attempt with no usable media - see the log for why streamlink/ffmpeg failed", rec.source.Name, info.Size()))
	}

	switch {
	case hasOutput:
		_ = os.MkdirAll(filepath.Dir(rec.finalPath), 0o755)
		if err := os.Rename(rec.tempPath, rec.finalPath); err != nil {
			_ = copyFile(rec.tempPath, rec.finalPath)
			_ = os.Remove(rec.tempPath)
		}
		// Auto-transcode rules run first (and synchronously) since a matching
		// rule can change the recording's actual final path (a container
		// change moves the file) - everything below needs to see that
		// updated rec.finalPath, not the pre-transcode one.
		a.autoTranscode(rec)
		a.writeNFO(rec)
		a.backup(rec)
		go a.uploadYouTube(rec)
		if rel, relErr := filepath.Rel(a.snapshotConfig().Settings.FinishedDir, rec.finalPath); relErr == nil {
			audioOnly := rec.source.AudioOnly
			finalPath := rec.finalPath
			relPath := filepath.ToSlash(rel)
			go a.generateThumbnail(finalPath, relPath, audioOnly)
			go a.finalizeRecordingSidecar(rec, finalPath, relPath, rec.timeOffsetMs, rec.timeSource)
			go a.saveEventTimetableSidecar(rec, finalPath)
		}
		if failed {
			// Real content was captured before the error hit (e.g. a network
			// drop partway through a long recording) - worth keeping, but
			// flagged rather than reported as a clean finish.
			go a.notify(fmt.Sprintf("%s stopped early", rec.source.Name), rec.finalPath)
			a.event("warn", fmt.Sprintf("[%s] saved %s despite an error - it may be incomplete", rec.source.Name, rec.finalPath))
		} else {
			go a.notify(fmt.Sprintf("%s finished", rec.source.Name), rec.finalPath)
			a.event("info", fmt.Sprintf("[%s] saved %s", rec.source.Name, rec.finalPath))
		}
		a.mu.Lock()
		a.lastFinished[rec.source.ID] = rec.finalPath
		a.mu.Unlock()
	case rec.ctx.Err() == nil && !failed:
		a.event("warn", fmt.Sprintf("[%s] no output produced", rec.source.Name))
	}

	if rec.hlsDir != "" {
		_ = os.RemoveAll(rec.hlsDir)
	}

	// Auto-reconnect bookkeeping: a manual stop (or app shutdown, which also
	// goes through stop()) is the user/operator in control, not a dropped
	// connection, so it's exempt from backoff. Otherwise, a recording that
	// ran long enough to be considered stable opens a visible reconnect
	// window - the next several retry attempts (if any) will be logged and
	// shown on the dashboard, since the source was just confirmed live and
	// might come right back. Anything shorter (or with no output at all)
	// schedules the next retry with backoff same as always, but stays silent
	// unless it's within an already-open window (recordFailure decides).
	if !rec.manualStop.Load() {
		if hasOutput && time.Since(rec.startedAt) >= minStableRecordingDuration {
			a.startReconnectWindow(rec.source.ID)
		} else {
			a.recordFailure(rec.source.Name, rec.source.ID)
		}
	}

	a.mu.Lock()
	delete(a.active, rec.source.ID)
	a.mu.Unlock()
}

// errorWithLogDetail appends the last non-empty lines of a recording's log
// (streamlink/ffmpeg's own stderr) to a generic exec error like "exit status
// 1", since that's normally where the actual failure reason lives.
func errorWithLogDetail(err error, logPath string) string {
	var detail []string
	for _, l := range tail(logPath, 8) {
		if l = strings.TrimSpace(l); l != "" {
			detail = append(detail, l)
		}
	}
	if len(detail) == 0 {
		return err.Error()
	}
	if len(detail) > 2 {
		detail = detail[len(detail)-2:]
	}
	return fmt.Sprintf("%s (%s)", err.Error(), strings.Join(detail, " / "))
}

// gracefulShutdownDelay is how long a stopped ffmpeg/streamlink process gets
// to react to a graceful interrupt and finalize its output before being hard
// killed. Without this, cmd.Cancel (wired below) has no forceful fallback.
const gracefulShutdownDelay = 5 * time.Second

// prepareGracefulCmd arranges for a context-cancelable command to be asked to
// shut down gracefully - via interruptProcess, which sends a real SIGINT on
// Unix and a CTRL_BREAK_EVENT on Windows - instead of exec's default
// behavior of hard-killing the process the instant its context is canceled.
// A hard kill never gives ffmpeg the chance to flush and finalize its output
// container, which left "stopped" recordings with broken duration/seeking or
// outright unplayable files.
func prepareGracefulCmd(cmd *exec.Cmd) {
	prepareProcessGroup(cmd)
	cmd.Cancel = func() error { return interruptProcess(cmd.Process.Pid) }
	cmd.WaitDelay = gracefulShutdownDelay
}

func (a *App) execute(rec *recording) error {
	src := rec.source
	windowSeconds := a.snapshotConfig().Settings.LiveRewindWindowSeconds
	if src.Type == "http" {
		args := ffmpegArgs(src, src.URL, rec.tempPath, rec.hlsDir, windowSeconds)
		cmd := exec.CommandContext(rec.ctx, "ffmpeg", args...)
		prepareGracefulCmd(cmd)
		return runLogged(cmd, rec.logFile)
	}

	quality := src.Quality
	if quality == "" {
		quality = a.cfg.Settings.DefaultQuality
	}
	if quality == "" {
		quality = "best"
	}
	slArgs := append([]string{"--stdout"}, src.StreamlinkArgs...)
	slArgs = append(slArgs, src.URL, quality)
	streamlink := exec.CommandContext(rec.ctx, "streamlink", slArgs...)
	ffmpeg := exec.CommandContext(rec.ctx, "ffmpeg", ffmpegArgs(src, "pipe:0", rec.tempPath, rec.hlsDir, windowSeconds)...)
	prepareGracefulCmd(streamlink)
	prepareGracefulCmd(ffmpeg)

	pipe, err := streamlink.StdoutPipe()
	if err != nil {
		return err
	}
	ffmpeg.Stdin = pipe
	streamlink.Stderr = rec.logFile
	ffmpeg.Stdout = rec.logFile
	ffmpeg.Stderr = rec.logFile
	if err := streamlink.Start(); err != nil {
		return fmt.Errorf("streamlink: %w", err)
	}
	if err := ffmpeg.Start(); err != nil {
		_ = streamlink.Process.Kill()
		return fmt.Errorf("ffmpeg: %w", err)
	}
	// ffmpeg must be waited on first: exec.Cmd.StdoutPipe's read end is
	// closed as soon as streamlink.Wait() reaps it, and racing that close
	// against ffmpeg's still-in-progress read of the same pipe can truncate
	// whatever was left buffered. Waiting on ffmpeg first guarantees it has
	// already drained the pipe (ffmpeg can't reach EOF until streamlink has
	// exited anyway) before streamlink's side is ever closed.
	ffErr := ffmpeg.Wait()
	slErr := streamlink.Wait()
	if rec.ctx.Err() != nil {
		return nil
	}
	if slErr != nil {
		return fmt.Errorf("streamlink exited: %w", slErr)
	}
	return ffErr
}

// loudnormFilter applies EBU R128 loudness normalization in a single pass
// (as opposed to ffmpeg's two-pass loudnorm, which needs to measure the
// whole file before encoding it and so can't run on a live recording).
// Single-pass is less precise but keeps levels in a sane, consistent range
// across sources/artists that otherwise vary wildly in recorded volume.
const loudnormFilter = "loudnorm=I=-16:TP=-1.5:LRA=11"

// ffmpegArgs builds the archival output plus, when hlsDir is set, a second
// bounded HLS output used for live-rewind DVR playback of the in-progress
// recording. The HLS branch always transcodes to H.264/AAC since it must be
// playable by hls.js/Safari regardless of what codec the archival copy uses.
// parseHTTPHeaderLines turns a Source's HTTPHeaders (one "Key: Value" line
// each, the same convention as a plain HTTP header block) into ordered
// key/value pairs, silently skipping blank lines, "#" comments, and any
// line without a colon rather than failing the whole source over one typo.
func parseHTTPHeaderLines(lines []string) [][2]string {
	var pairs [][2]string
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		idx := strings.Index(line, ":")
		if idx < 0 {
			continue
		}
		key := strings.TrimSpace(line[:idx])
		val := strings.TrimSpace(line[idx+1:])
		if key == "" {
			continue
		}
		pairs = append(pairs, [2]string{key, val})
	}
	return pairs
}

// ffmpegHeadersArg joins header pairs into the single CRLF-terminated
// string ffmpeg's "-headers" input option expects.
func ffmpegHeadersArg(pairs [][2]string) string {
	var b strings.Builder
	for _, p := range pairs {
		b.WriteString(p[0])
		b.WriteString(": ")
		b.WriteString(p[1])
		b.WriteString("\r\n")
	}
	return b.String()
}

func ffmpegArgs(src Source, input, output, hlsDir string, hlsWindowSeconds int) []string {
	args := []string{"-hide_banner", "-y", "-nostdin"}
	if src.HardwareAccel != "" && src.HardwareAccel != "none" {
		args = append(args, "-hwaccel", src.HardwareAccel)
	}
	// -headers only means something when ffmpeg itself is opening a network
	// URL (the "http" source type's direct -i src.URL) - it's meaningless
	// (and harmless either way) for the streamlink pipe:0 case, so gate on
	// the input actually looking like a URL rather than on src.Type.
	if (strings.HasPrefix(input, "http://") || strings.HasPrefix(input, "https://")) && len(src.HTTPHeaders) > 0 {
		if headerArg := ffmpegHeadersArg(parseHTTPHeaderLines(src.HTTPHeaders)); headerArg != "" {
			args = append(args, "-headers", headerArg)
		}
	}
	args = append(args, src.FFmpegArgs...)
	args = append(args, "-i", input)

	// Map only audio/video/subtitles, never "-map 0" wholesale: Twitch (and
	// others) mux a timed_id3 data stream in alongside audio/video for ad
	// markers, and Matroska - our default container - refuses to write a
	// header at all if a data stream is mapped into it ("Only audio, video,
	// and subtitles are supported for Matroska"), producing a near-empty
	// file and failing the recording before a single frame is written. The
	// "?" suffix means ffmpeg won't error if a source has no subtitle track.
	args = append(args, "-map", "0:v?", "-map", "0:a?", "-map", "0:s?")
	// Loudness normalization needs to re-encode audio (a filter can't run on
	// a stream-copied track), even when the source otherwise stream-copies
	// video - so audio and video codecs are chosen independently here rather
	// than the single "-c copy"/"-c:a aac" shortcuts used before.
	encodeAudio := src.Transcode || src.LoudnessNormalize
	if src.AudioOnly {
		args = append(args, "-vn")
	} else if src.Transcode {
		args = append(args, "-c:v", videoEncoder(src.HardwareAccel))
	} else {
		args = append(args, "-c:v", "copy", "-c:s", "copy")
	}
	if encodeAudio {
		args = append(args, "-c:a", "aac", "-b:a", "192k")
		if src.LoudnessNormalize {
			args = append(args, "-af", loudnormFilter)
		}
	} else {
		args = append(args, "-c:a", "copy")
	}
	// The output path ends in ".part" for atomic renaming, so ffmpeg cannot
	// infer the container from the extension - it must be told explicitly.
	args = append(args, "-f", containerFormat(src.Container))
	args = append(args, output)

	if hlsDir != "" {
		args = append(args, "-map", "0:v?", "-map", "0:a?", "-map", "0:s?")
		if src.AudioOnly {
			args = append(args, "-vn", "-c:a", "aac", "-b:a", "160k")
		} else {
			args = append(args, "-c:v", videoEncoder(src.HardwareAccel), "-c:a", "aac", "-b:a", "160k")
			if src.HardwareAccel == "" || src.HardwareAccel == "none" {
				args = append(args, "-preset", "veryfast")
			}
		}
		args = append(args,
			"-f", "hls",
			"-hls_time", "4",
			"-hls_list_size", strconv.Itoa(hlsListSize(hlsWindowSeconds)),
			"-hls_flags", "delete_segments+independent_segments",
			"-hls_segment_filename", filepath.Join(hlsDir, "seg%05d.ts"),
			filepath.Join(hlsDir, "index.m3u8"),
		)
	}
	return args
}

// hlsListSize converts a rewind window (seconds) into a segment count for a
// 4-second HLS segment duration, with a sane floor so a misconfigured or
// zero window still yields a usable rewind buffer.
func hlsListSize(windowSeconds int) int {
	if windowSeconds <= 0 {
		windowSeconds = 1800
	}
	n := windowSeconds / 4
	if n < 10 {
		n = 10
	}
	return n
}

// containerFormat maps a container/extension name to the ffmpeg muxer name
// needed for an explicit -f flag, since the on-disk filename carries a
// ".part" suffix that prevents ffmpeg from guessing the format itself.
func containerFormat(container string) string {
	switch strings.ToLower(container) {
	case "mkv", "matroska":
		return "matroska"
	case "mp4":
		return "mp4"
	case "m4a":
		return "ipod"
	case "ts", "mpegts":
		return "mpegts"
	case "":
		return "matroska"
	default:
		return container
	}
}

func videoEncoder(hw string) string {
	switch hw {
	case "cuda", "nvdec", "nvidia":
		return "h264_nvenc"
	case "qsv":
		return "h264_qsv"
	case "vaapi":
		return "h264_vaapi"
	default:
		return "libx264"
	}
}

func runLogged(cmd *exec.Cmd, w io.Writer) error {
	cmd.Stdout = w
	cmd.Stderr = w
	return cmd.Run()
}

// stop cancels a recording's context. Each command was set up by
// prepareGracefulCmd, so cancellation asks ffmpeg/streamlink to shut down
// gracefully first and only force-kills them after gracefulShutdownDelay -
// see prepareGracefulCmd for why that matters.
func (a *App) stop(id string) {
	a.mu.RLock()
	rec := a.active[id]
	a.mu.RUnlock()
	if rec == nil {
		return
	}
	rec.manualStop.Store(true)
	a.clearRetry(id)
	rec.cancel()
}

func (a *App) stopAll() {
	a.mu.RLock()
	ids := make([]string, 0, len(a.active))
	for id := range a.active {
		ids = append(ids, id)
	}
	a.mu.RUnlock()
	for _, id := range ids {
		a.stop(id)
	}
	for _, id := range ids {
		a.mu.RLock()
		rec := a.active[id]
		a.mu.RUnlock()
		if rec != nil {
			<-rec.done
		}
	}
}

func (a *App) handleState(w http.ResponseWriter, r *http.Request) {
	s := a.state()
	role := roleFromRequest(r)
	s.Role = role
	if role != RoleAdmin {
		redactSecrets(&s.Config)
	}
	writeJSON(w, s)
}

func (a *App) handleConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		cfg := a.snapshotConfig()
		if roleFromRequest(r) != RoleAdmin {
			redactSecrets(&cfg)
		}
		writeJSON(w, cfg)
		return
	}
	if r.Method != http.MethodPut {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var cfg AppConfig
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	normalizeConfig(&cfg)
	if err := a.saveConfig(cfg); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, cfg)
}

// validateSource checks the minimum fields needed for a source to be usable.
func validateSource(src Source) error {
	if strings.TrimSpace(src.Name) == "" {
		return errors.New("name is required")
	}
	if strings.TrimSpace(src.URL) == "" {
		return errors.New("url is required")
	}
	if src.Type != "youtube" && src.Type != "twitch" && src.Type != "http" {
		return errors.New("type must be youtube, twitch, or http")
	}
	return nil
}

// loadSourcePresets parses the bundled presets/presets.json into the list of
// preset packs served by /api/presets. Errors are swallowed to a nil/empty
// slice - a broken embedded file should never take down the whole app, and
// this is checked once at startup.
func loadSourcePresets() []SourcePreset {
	data, err := presetsFile.ReadFile("presets/presets.json")
	if err != nil {
		return nil
	}
	var presets []SourcePreset
	if err := json.Unmarshal(data, &presets); err != nil {
		log.Printf("presets/presets.json is invalid, ignoring: %s", err)
		return nil
	}
	return presets
}

// handlePresets serves the bundled preset packs (well-known DJs/streamers/
// events, ready to add as sources). Read-only - applying one just POSTs each
// of its sources through the normal /api/sources endpoint from the client.
func (a *App) handlePresets(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, a.sourcePresets)
}

func (a *App) handleSources(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var src Source
	if err := json.NewDecoder(r.Body).Decode(&src); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := validateSource(src); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if src.ID == "" {
		src.ID = newID()
	}
	a.mu.Lock()
	for _, existing := range a.cfg.Sources {
		if existing.ID == src.ID {
			a.mu.Unlock()
			http.Error(w, "source id already exists", http.StatusConflict)
			return
		}
	}
	a.cfg.Sources = append(a.cfg.Sources, src)
	cfg := a.cfg
	a.mu.Unlock()
	_ = a.persist(cfg)
	writeJSON(w, src)
}

// handleSourceItem handles per-source operations addressed as /api/sources/{id}.
func (a *App) handleSourceItem(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/sources/")
	if id == "" {
		http.NotFound(w, r)
		return
	}
	switch r.Method {
	case http.MethodPut:
		var src Source
		if err := json.NewDecoder(r.Body).Decode(&src); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := validateSource(src); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		src.ID = id
		a.mu.Lock()
		idx := -1
		for i, existing := range a.cfg.Sources {
			if existing.ID == id {
				idx = i
				break
			}
		}
		if idx == -1 {
			a.mu.Unlock()
			http.NotFound(w, r)
			return
		}
		a.cfg.Sources[idx] = src
		cfg := a.cfg
		a.mu.Unlock()
		_ = a.persist(cfg)
		writeJSON(w, src)
	case http.MethodDelete:
		if a.isActive(id) {
			http.Error(w, "stop the recording before deleting this source", http.StatusConflict)
			return
		}
		a.mu.Lock()
		found := false
		out := a.cfg.Sources[:0:0]
		for _, s := range a.cfg.Sources {
			if s.ID == id {
				found = true
				continue
			}
			out = append(out, s)
		}
		a.cfg.Sources = out
		cfg := a.cfg
		a.mu.Unlock()
		if !found {
			http.NotFound(w, r)
			return
		}
		_ = a.persist(cfg)
		writeJSON(w, map[string]string{"status": "deleted"})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleSourceTest resolves a candidate stream URL without starting a recording,
// so the UI can validate a source before saving it.
func (a *App) handleSourceTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Type        string   `json:"type"`
		URL         string   `json:"url"`
		Quality     string   `json:"quality"`
		HTTPHeaders []string `json:"httpHeaders"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.URL) == "" {
		writeJSON(w, map[string]any{"ok": false, "error": "url is required"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	if req.Type == "http" {
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodHead, req.URL, nil)
		if err != nil {
			writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		for _, p := range parseHTTPHeaderLines(req.HTTPHeaders) {
			httpReq.Header.Set(p[0], p[1])
		}
		resp, err := http.DefaultClient.Do(httpReq)
		if err != nil {
			writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		resp.Body.Close()
		if resp.StatusCode >= 400 {
			writeJSON(w, map[string]any{"ok": false, "error": fmt.Sprintf("server responded with %s", resp.Status)})
			return
		}
		writeJSON(w, map[string]any{"ok": true, "url": req.URL})
		return
	}

	quality := req.Quality
	if quality == "" {
		quality = a.snapshotConfig().Settings.DefaultQuality
	}
	if quality == "" {
		quality = "best"
	}
	out, err := exec.CommandContext(ctx, "streamlink", "--stream-url", req.URL, quality).Output()
	if err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": "streamlink could not resolve this URL/quality"})
		return
	}
	writeJSON(w, map[string]any{"ok": true, "url": strings.TrimSpace(string(out))})
}

func (a *App) handleRecordings(w http.ResponseWriter, r *http.Request) {
	cfg := a.snapshotConfig()
	root := filepath.Clean(cfg.Settings.FinishedDir)
	var files []RecordingFile
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if isSidecarPath(p) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		parts := strings.SplitN(rel, "/", 2)
		source := ""
		if len(parts) > 1 {
			source = parts[0]
		}
		rf := RecordingFile{Name: filepath.Base(p), Source: source, Path: rel, Size: info.Size(), ModTime: info.ModTime(), Channel: channelFromPath(rel)}
		if meta, ok := cfg.RecordingMeta[rel]; ok {
			rf.EventID = meta.EventID
			rf.SetID = meta.SetID
			rf.Artist = meta.Artist
			rf.Start = meta.Start
			rf.End = meta.End
			rf.Tracklist = meta.Tracklist
			if meta.Channel != "" {
				rf.Channel = meta.Channel
			}
		}
		files = append(files, rf)
		return nil
	})
	sort.Slice(files, func(i, j int) bool { return files[i].ModTime.After(files[j].ModTime) })
	writeJSON(w, files)
}

// fileHash returns the sha256 hash of the file at absPath (relPath is used
// only as the cache key), reusing a cached value when the file's size and
// mtime haven't changed since it was last hashed - streamed via io.Copy so
// large recordings never get fully loaded into memory.
func (a *App) fileHash(absPath, relPath string, size int64, modTime time.Time) (string, error) {
	a.hashMu.Lock()
	if entry, ok := a.hashCache[relPath]; ok && entry.Size == size && entry.ModTime.Equal(modTime) {
		a.hashMu.Unlock()
		return entry.Hash, nil
	}
	a.hashMu.Unlock()

	f, err := os.Open(absPath)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	hash := hex.EncodeToString(h.Sum(nil))

	a.hashMu.Lock()
	if a.hashCache == nil {
		a.hashCache = map[string]hashCacheEntry{}
	}
	a.hashCache[relPath] = hashCacheEntry{ModTime: modTime, Size: size, Hash: hash}
	a.hashMu.Unlock()
	return hash, nil
}

// MatchFileEntry is one exported recording's identity - a content hash paired
// with enough denormalized library metadata (names, not local IDs, since IDs
// aren't stable across different users' installs) that an importer can apply
// it without already having a matching LibraryEvent/Festival locally.
type MatchFileEntry struct {
	Hash         string `json:"hash"`
	EventID      string `json:"eventId,omitempty"`
	SetID        string `json:"setId,omitempty"`
	Artist       string `json:"artist,omitempty"`
	Start        string `json:"start,omitempty"`
	End          string `json:"end,omitempty"`
	EventName    string `json:"eventName,omitempty"`
	FestivalName string `json:"festivalName,omitempty"`
	StageName    string `json:"stageName,omitempty"`
}

// handleRecordingsMatchfileExport hashes every organized recording (one
// that's already been assigned to a LibraryEvent) and emits a shareable
// {hash -> metadata} list, so someone else with a different copy of the same
// files can import it and skip organizing manually - the exact-match sibling
// of the fuzzy Smart Match wizard.
func (a *App) handleRecordingsMatchfileExport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	cfg := a.snapshotConfig()
	root := filepath.Clean(cfg.Settings.FinishedDir)
	eventByID := make(map[string]LibraryEvent, len(cfg.LibraryEvents))
	for _, e := range cfg.LibraryEvents {
		eventByID[e.ID] = e
	}
	festivalByID := make(map[string]Festival, len(cfg.Festivals))
	for _, f := range cfg.Festivals {
		festivalByID[f.ID] = f
	}
	out := []MatchFileEntry{}
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || isSidecarPath(p) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		meta, ok := cfg.RecordingMeta[rel]
		if !ok || meta.EventID == "" {
			return nil
		}
		hash, err := a.fileHash(p, rel, info.Size(), info.ModTime())
		if err != nil {
			return nil
		}
		entry := MatchFileEntry{Hash: hash, EventID: meta.EventID, SetID: meta.SetID, Artist: meta.Artist, Start: meta.Start, End: meta.End, StageName: meta.Channel}
		if ev, ok := eventByID[meta.EventID]; ok {
			entry.EventName = ev.Name
			if f, ok := festivalByID[ev.FestivalID]; ok {
				entry.FestivalName = f.Name
			}
		}
		out = append(out, entry)
		return nil
	})
	writeJSON(w, out)
}

// matchfilePreviewItem is one row of the import preview: which local file
// would be organized, and with what metadata.
type matchfilePreviewItem struct {
	Path         string `json:"path"`
	EventName    string `json:"eventName,omitempty"`
	FestivalName string `json:"festivalName,omitempty"`
	StageName    string `json:"stageName,omitempty"`
	Artist       string `json:"artist,omitempty"`
}

// handleRecordingsMatchfileImport hashes every not-yet-organized local
// recording and, on an exact hash match against the imported list, applies
// that entry's metadata - resolving (or creating) a local LibraryEvent/
// Festival by name, since the exporter's IDs mean nothing on this install.
// The imported SetID is deliberately dropped: it points at a timetable set
// in the exporter's own LibraryEvent, which won't exist locally unless that
// archived timetable was also imported separately.
//
// ?dryRun=1 computes the same match list without applying anything, so the
// client can show a review step before changing metadata. A hash appearing
// more than once in the import file keeps its first entry (reported via
// "duplicates" when the copies disagree) rather than letting whichever
// happens to come last silently win.
func (a *App) handleRecordingsMatchfileImport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	dryRun := r.URL.Query().Get("dryRun") != ""
	var entries []MatchFileEntry
	if err := json.NewDecoder(r.Body).Decode(&entries); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	byHash := make(map[string]MatchFileEntry, len(entries))
	duplicates := 0
	for _, e := range entries {
		if e.Hash == "" {
			continue
		}
		if first, seen := byHash[e.Hash]; seen {
			if first != e {
				duplicates++
			}
			continue
		}
		byHash[e.Hash] = e
	}

	cfg := a.snapshotConfig()
	root := filepath.Clean(cfg.Settings.FinishedDir)
	type pendingMatch struct {
		path  string
		entry MatchFileEntry
	}
	var matched []pendingMatch
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || isSidecarPath(p) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if m, ok := cfg.RecordingMeta[rel]; ok && m.EventID != "" {
			return nil
		}
		hash, err := a.fileHash(p, rel, info.Size(), info.ModTime())
		if err != nil {
			return nil
		}
		if entry, ok := byHash[hash]; ok {
			matched = append(matched, pendingMatch{path: rel, entry: entry})
		}
		return nil
	})

	preview := make([]matchfilePreviewItem, 0, len(matched))
	for _, m := range matched {
		preview = append(preview, matchfilePreviewItem{
			Path:         m.path,
			EventName:    m.entry.EventName,
			FestivalName: m.entry.FestivalName,
			StageName:    m.entry.StageName,
			Artist:       m.entry.Artist,
		})
	}

	if !dryRun {
		a.mu.Lock()
		if a.cfg.RecordingMeta == nil {
			a.cfg.RecordingMeta = map[string]RecordingMeta{}
		}
		for _, m := range matched {
			eventID := ""
			if m.entry.EventName != "" {
				eventID = a.resolveOrCreateLibraryEventLocked(m.entry.EventName, m.entry.FestivalName)
			}
			a.cfg.RecordingMeta[m.path] = RecordingMeta{
				EventID: eventID,
				Channel: m.entry.StageName,
				Artist:  m.entry.Artist,
				Start:   m.entry.Start,
				End:     m.entry.End,
			}
		}
		newCfg := a.cfg
		a.mu.Unlock()
		if len(matched) > 0 {
			_ = a.persist(newCfg)
		}
	}
	writeJSON(w, map[string]any{
		"matched":    len(matched),
		"duplicates": duplicates,
		"dryRun":     dryRun,
		"matches":    preview,
	})
}

// resolveOrCreateLibraryEventLocked finds a LibraryEvent by name, creating it
// (and its Festival, if named and not already present) when missing. Caller
// must hold a.mu.
func (a *App) resolveOrCreateLibraryEventLocked(eventName, festivalName string) string {
	for _, e := range a.cfg.LibraryEvents {
		if e.Name == eventName {
			return e.ID
		}
	}
	festivalID := ""
	if festivalName != "" {
		for _, f := range a.cfg.Festivals {
			if f.Name == festivalName {
				festivalID = f.ID
				break
			}
		}
		if festivalID == "" {
			f := Festival{ID: newID(), Name: festivalName}
			a.cfg.Festivals = append(a.cfg.Festivals, f)
			festivalID = f.ID
		}
	}
	e := LibraryEvent{ID: newID(), Name: eventName, FestivalID: festivalID, Timetable: []StageSchedule{}}
	a.cfg.LibraryEvents = append(a.cfg.LibraryEvents, e)
	return e.ID
}

// filenameTimestampRe matches this app's own recording naming convention
// (name.YYYYMMDD-HHMMSS.ext) as well as loose YYYY-MM-DD / YYYYMMDD dates
// that commonly show up in filenames from other sources.
var filenameTimestampRe = regexp.MustCompile(`(\d{4})-?(\d{2})-?(\d{2})[ _.T-]?(\d{2})?:?(\d{2})?:?(\d{2})?`)

// dateDMYRe matches a day-month-year date separated by underscores, dashes,
// or dots (e.g. "25_06_2026") - the convention many non-US recording tools
// (and this app's own suggested festival-recording filenames) use, as
// opposed to the YYYY-first convention filenameTimestampRe expects.
var dateDMYRe = regexp.MustCompile(`(?:^|[_\-. ])(\d{1,2})[_\-.](\d{1,2})[_\-.](\d{4})(?:$|[_\-. ])`)

// weekdayAbbrevs maps the first three letters of a weekday name (case
// folded) to its time.Weekday - used both to disambiguate day/month order
// in dateDMYRe matches when both values are <=12, and to strip a trailing
// weekday word (e.g. "..._Thursday_25_06_2026...") when guessing an artist
// name from the remaining prefix.
var weekdayAbbrevs = map[string]time.Weekday{
	"sun": time.Sunday, "mon": time.Monday, "tue": time.Tuesday, "wed": time.Wednesday,
	"thu": time.Thursday, "fri": time.Friday, "sat": time.Saturday,
}

var weekdayRe = regexp.MustCompile(`(?i)\b(sun|mon|tue|wed|thu|fri|sat)[a-z]*\b`)

// nonWordCharRe splits a filename into rough "words" for artist-name
// guessing - underscore/dash/dot/space are all treated as separators.
var nonWordCharRe = regexp.MustCompile(`[_\-. ]+`)

// nonAlnumRe strips everything but letters/digits, used to compare two
// strings (a guessed stage/artist name and an archived timetable's) in a
// way that's tolerant of case and punctuation/spacing differences.
var nonAlnumRe = regexp.MustCompile(`[^a-z0-9]+`)

// yearWordRe matches a token that is exactly a 4-digit year, and yearScanRe
// finds years anywhere in a string. Years are matched as their own Smart
// Match signal (keywordYears), so they're excluded from the identifying name
// keywords - otherwise "2026" in a filename would spuriously "match" every
// event whose name happens to end in that year.
var yearWordRe = regexp.MustCompile(`^(19|20)\d{2}$`)
var yearScanRe = regexp.MustCompile(`(19|20)\d{2}`)

// matchStopwords are words too generic to identify a specific event - they
// appear across many festivals, so counting them as keyword matches would
// create spurious event matches.
var matchStopwords = map[string]bool{
	"the": true, "festival": true, "fest": true, "live": true, "stage": true,
	"set": true, "dj": true, "official": true, "full": true, "vs": true, "b2b": true,
}

// keywordTokenList splits a string into lowercased alphanumeric keyword
// tokens, dropping single characters, bare years, and generic filler. Used to
// match a recording's filename/folder against an event's name by shared
// keywords (e.g. a festival name and a stage colour), which disambiguates
// editions/festivals that reuse the same stage names and touring artists far
// better than stage+artist alone.
func keywordTokenList(s string) []string {
	raw := nonAlnumRe.ReplaceAllString(strings.ToLower(s), " ")
	var out []string
	for _, w := range strings.Fields(raw) {
		if len(w) < 2 || matchStopwords[w] || yearWordRe.MatchString(w) {
			continue
		}
		out = append(out, w)
	}
	return out
}

func keywordTokenSet(s string) map[string]bool {
	set := map[string]bool{}
	for _, w := range keywordTokenList(s) {
		set[w] = true
	}
	return set
}

// eventNameKeywordScore returns the fraction (0..1) of an event's identifying
// name tokens that appear as keywords in a recording's filename/folder. 1.0
// means every significant word of the event name is present.
func eventNameKeywordScore(eventName string, haystack map[string]bool) float64 {
	toks := keywordTokenList(eventName)
	if len(toks) == 0 {
		return 0
	}
	found := 0
	for _, t := range toks {
		if haystack[t] {
			found++
		}
	}
	return float64(found) / float64(len(toks))
}

// keywordYears extracts every 4-digit year mentioned in a recording's
// filename/folder, so the matcher can prefer the festival edition whose year
// the recording actually names.
func keywordYears(s string) map[int]bool {
	out := map[int]bool{}
	for _, m := range yearScanRe.FindAllString(s, -1) {
		if y, err := strconv.Atoi(m); err == nil {
			out[y] = true
		}
	}
	return out
}

// channelFromPath derives a recording's channel/stage from its path relative
// to FinishedDir: the name of its immediate parent directory. For the flat
// `<source>/<file>` layout the live recorder itself uses, that's the same as
// the source name it always was; for a deeper, manually-organized layout
// like `<event>/<edition>/<day>/<stage>/<file>` it correctly resolves to the
// stage folder rather than the event folder at the top.
func channelFromPath(rel string) string {
	idx := strings.LastIndex(rel, "/")
	if idx < 0 {
		return ""
	}
	dir := rel[:idx]
	if slash := strings.LastIndex(dir, "/"); slash >= 0 {
		return dir[slash+1:]
	}
	return dir
}

var yearSegmentRe = regexp.MustCompile(`^(19|20)\d{2}$`)
var dateSegmentRe = regexp.MustCompile(`^\d{4}[-_.]\d{2}[-_.]\d{2}$|^\d{2}[-_.]\d{2}[-_.]\d{4}$`)

// folderEventHint parses the directories above a recording's stage folder
// into a candidate event name and year, for the recommended layout
// `<event>/<edition-or-year>/<day>/<stage>/<file>` (any subset of the middle
// levels still works, e.g. `<event>/<stage>/<file>`). A year-looking segment
// becomes the edition year; a weekday-name or date-looking segment (a day
// folder) is dropped rather than folded into the event name. Only engages
// with at least two directory levels - a single level is the flat
// `<source>/<file>` layout the live recorder uses, where that one segment
// already means "channel", not "event".
func folderEventHint(rel string) (eventName string, year int, stage string, ok bool) {
	idx := strings.LastIndex(rel, "/")
	if idx < 0 {
		return "", 0, "", false
	}
	dirs := strings.Split(rel[:idx], "/")
	if len(dirs) < 2 {
		return "", 0, "", false
	}
	stage = dirs[len(dirs)-1]
	var nameParts []string
	for _, seg := range dirs[:len(dirs)-1] {
		switch {
		case yearSegmentRe.MatchString(seg):
			year, _ = strconv.Atoi(seg)
		case dateSegmentRe.MatchString(seg) || isWeekdayWord(seg):
			// a day folder - not part of the event's display name
		default:
			nameParts = append(nameParts, seg)
		}
	}
	eventName = strings.TrimSpace(strings.Join(nameParts, " "))
	if eventName == "" {
		return "", 0, "", false
	}
	return eventName, year, stage, true
}

// eventMatchesFolderHint reports whether an existing LibraryEvent is the one
// a folderEventHint is pointing at: same name (ignoring case/punctuation),
// and the same year if both sides have one set.
func eventMatchesFolderHint(ev LibraryEvent, name string, year int) bool {
	if nonAlnumRe.ReplaceAllString(strings.ToLower(ev.Name), "") != nonAlnumRe.ReplaceAllString(strings.ToLower(name), "") {
		return false
	}
	if year != 0 && ev.Year != 0 && ev.Year != year {
		return false
	}
	return true
}

func validDate(year, month, day int) bool {
	return year >= 2000 && year <= 2100 && month >= 1 && month <= 12 && day >= 1 && day <= 31
}

func weekdayFromName(name string) (time.Weekday, bool) {
	m := weekdayRe.FindStringSubmatch(name)
	if m == nil {
		return 0, false
	}
	wd, ok := weekdayAbbrevs[strings.ToLower(m[1])]
	return wd, ok
}

func isWeekdayWord(word string) bool {
	w := strings.ToLower(word)
	if len(w) < 3 {
		return false
	}
	_, ok := weekdayAbbrevs[w[:3]]
	return ok
}

// guessTimeFromName tries to parse a date (and, where present, a time) out
// of a filename, falling back to the file's mtime if nothing looks like a
// date. hasTimeOfDay reports whether an actual clock time was found (as
// opposed to defaulting to midnight for a date-only filename) - callers use
// that to decide whether time-of-day should factor into matching at all.
// Best-effort only - it's a starting point for the match wizard's
// suggestions, not a guarantee.
func guessTimeFromName(name string, modTime time.Time) (guessed time.Time, fromName, hasTimeOfDay bool) {
	if m := filenameTimestampRe.FindStringSubmatch(name); m != nil {
		year, _ := strconv.Atoi(m[1])
		month, _ := strconv.Atoi(m[2])
		day, _ := strconv.Atoi(m[3])
		if validDate(year, month, day) {
			hour, min, sec := 0, 0, 0
			if m[4] != "" {
				hour, _ = strconv.Atoi(m[4])
				hasTimeOfDay = true
			}
			if m[5] != "" {
				min, _ = strconv.Atoi(m[5])
			}
			if m[6] != "" {
				sec, _ = strconv.Atoi(m[6])
			}
			return time.Date(year, time.Month(month), day, hour, min, sec, 0, modTime.Location()), true, hasTimeOfDay
		}
	}

	// Fall back to a day-first date (DD_MM_YYYY / DD-MM-YYYY / DD.MM.YYYY).
	// When both candidate values are <=12 (so the order is genuinely
	// ambiguous, e.g. "03_04_2026"), use an embedded weekday name - if
	// present - to pick whichever ordering actually falls on that weekday;
	// otherwise default to day-first.
	if m := dateDMYRe.FindStringSubmatch(name); m != nil {
		a, _ := strconv.Atoi(m[1])
		b, _ := strconv.Atoi(m[2])
		year, _ := strconv.Atoi(m[3])
		day, month := a, b
		switch {
		case a > 12 && b <= 12:
			day, month = a, b
		case b > 12 && a <= 12:
			day, month = b, a
		default:
			if wd, ok := weekdayFromName(name); ok {
				if validDate(year, a, b) && time.Date(year, time.Month(a), b, 0, 0, 0, 0, time.UTC).Weekday() == wd {
					day, month = b, a
				} else if validDate(year, b, a) && time.Date(year, time.Month(b), a, 0, 0, 0, 0, time.UTC).Weekday() == wd {
					day, month = a, b
				}
			}
		}
		if validDate(year, month, day) {
			return time.Date(year, time.Month(month), day, 0, 0, 0, 0, modTime.Location()), true, false
		}
	}

	return modTime, false, false
}

// guessArtistFromName extracts the likely artist name from a filename -
// everything before the first recognized date token, with a trailing
// weekday word and/or the recording's own channel name peeled off (both
// commonly sit between the artist and the date, e.g.
// "DJ_Isaac_BLUE_Thursday_25_06_2026_..."). Best-effort: only used to
// boost/break ties in match scoring, never applied without a human
// approving the suggestion.
func guessArtistFromName(name, channel string) string {
	base := strings.TrimSuffix(name, filepath.Ext(name))
	cut := len(base)
	if loc := filenameTimestampRe.FindStringIndex(base); loc != nil && loc[0] < cut {
		cut = loc[0]
	}
	if loc := dateDMYRe.FindStringIndex(base); loc != nil && loc[0] < cut {
		cut = loc[0]
	}
	words := nonWordCharRe.Split(strings.Trim(base[:cut], "_-. "), -1)
	for len(words) > 0 {
		last := words[len(words)-1]
		if last == "" || isWeekdayWord(last) || (channel != "" && strings.EqualFold(last, channel)) {
			words = words[:len(words)-1]
			continue
		}
		break
	}
	return strings.Join(words, " ")
}

// normalizeArtistWords lowercases and tokenizes a name for comparison,
// dropping generic filler words ("dj", "b2b", "vs") that shouldn't count
// toward or against a match.
func normalizeArtistWords(s string) []string {
	s = nonAlnumRe.ReplaceAllString(strings.ToLower(s), " ")
	var out []string
	for _, w := range strings.Fields(s) {
		if w == "dj" || w == "b2b" || w == "vs" {
			continue
		}
		out = append(out, w)
	}
	return out
}

// artistSimilarity gives a rough 0..1 score for how closely a guessed
// artist name (parsed from a filename) matches an archived timetable set's
// name, tolerant of case, punctuation, and extra words on either side (e.g.
// "DJ Vertex" vs "DJ Vertex B2B Fenrix" still scores 1.0, not diluted by the
// combo's extra name).
func artistSimilarity(guessed, setName string) float64 {
	g := normalizeArtistWords(guessed)
	s := normalizeArtistWords(setName)
	if len(g) == 0 || len(s) == 0 {
		return 0
	}
	gWords := make(map[string]bool, len(g))
	for _, w := range g {
		gWords[w] = true
	}
	matches := 0
	for _, w := range s {
		if gWords[w] {
			matches++
		}
	}
	shorter := len(g)
	if len(s) < shorter {
		shorter = len(s)
	}
	return float64(matches) / float64(shorter)
}

// MatchSuggestion is one recording's best-guess event/set match, offered to
// the user for approval or correction rather than applied automatically.
type MatchSuggestion struct {
	Path            string `json:"path"`
	Name            string `json:"name"`
	Channel         string `json:"channel"`
	GuessedTime     string `json:"guessedTime,omitempty"`
	GuessedFromName bool   `json:"guessedFromName"`
	GuessedArtist   string `json:"guessedArtist,omitempty"`
	EventID         string `json:"eventId,omitempty"`
	EventName       string `json:"eventName,omitempty"`
	SetID           string `json:"setId,omitempty"`
	Stage           string `json:"stage,omitempty"`
	Artist          string `json:"artist,omitempty"`
	Confidence      string `json:"confidence"` // "high" | "medium" | "low" | "none"
	Reason          string `json:"reason"`
	// NewEventName/NewEventYear are set instead of EventID when the folder
	// layout (see folderEventHint) implies an event that doesn't exist in
	// the Library yet - approving this suggestion creates it first.
	NewEventName string `json:"newEventName,omitempty"`
	NewEventYear int    `json:"newEventYear,omitempty"`
}

// handleRecordingMatchSuggestions scans every recording that isn't already
// assigned to an event and tries to guess which archived timetable set it
// belongs to, from the timestamp embedded in its filename (or, failing
// that, its mtime) and its channel/stage name. Nothing is written - the
// caller reviews and approves/corrects each suggestion via the existing
// PUT /api/recordings/meta.
func (a *App) handleRecordingMatchSuggestions(w http.ResponseWriter, r *http.Request) {
	cfg := a.snapshotConfig()
	root := filepath.Clean(cfg.Settings.FinishedDir)
	var suggestions []MatchSuggestion
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || isSidecarPath(p) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if meta, ok := cfg.RecordingMeta[rel]; ok && meta.EventID != "" {
			return nil // already organized - nothing to suggest
		}
		channel := channelFromPath(rel)
		name := filepath.Base(p)
		guessed, fromName, hasTimeOfDay := guessTimeFromName(name, info.ModTime())
		suggestions = append(suggestions, bestMatchSuggestion(cfg, rel, name, channel, guessed, fromName, hasTimeOfDay))
		return nil
	})
	flagSharedSetCandidates(suggestions)
	sort.Slice(suggestions, func(i, j int) bool { return suggestions[i].Path < suggestions[j].Path })
	writeJSON(w, suggestions)
}

// flagSharedSetCandidates appends a warning to every suggestion whose
// matched set is also the top pick for another recording in this same
// batch. A set normally has exactly one recording; two files landing on the
// same one is almost always either a genuine duplicate, or - since a
// dropped stream now auto-reconnects mid-recording (see the auto-reconnect
// backoff in runRecording) - two parts of what was originally one
// continuous set, split across separate files by the reconnect. Either way
// the user needs to notice before approving more than one of them onto the
// same set silently.
func flagSharedSetCandidates(suggestions []MatchSuggestion) {
	groups := map[string][]int{}
	for i, s := range suggestions {
		if s.SetID == "" {
			continue
		}
		groups[s.EventID+"\x00"+s.SetID] = append(groups[s.EventID+"\x00"+s.SetID], i)
	}
	for _, idxs := range groups {
		if len(idxs) < 2 {
			continue
		}
		for _, i := range idxs {
			suggestions[i].Reason += fmt.Sprintf(" Note: %d other recording(s) in this batch also match this same set - likely duplicate files or parts split by a dropped/reconnected stream; verify before approving more than one.", len(idxs)-1)
		}
	}
}

// matchCandidate is one archived timetable set considered as a possible
// match for a recording, scored by candidateScore.
type matchCandidate struct {
	eventID, eventName, setID, stage, artist string
	eventYear                                int
	delta                                    time.Duration
	contained, stageMatch, sameDay           bool
	artistScore                              float64
	festivalMatch, festivalConflict          bool
	// Keyword signals: how much of the event's name appears in the
	// recording's filename/folder, and whether the year it names agrees with
	// (or contradicts) this event's edition year.
	eventNameScore          float64
	yearMatch, yearConflict bool
}

// candidateScore combines every signal available for one candidate set into
// a single comparable number, so bestMatchSuggestion can just pick the max
// rather than hand-rolling a comparator. Roughly, in descending weight: an
// exact time-window match, the guessed artist name matching the set's name,
// the folder/channel name matching the archived stage, the candidate's event
// belonging to the same Festival the recording's source is explicitly
// linked to (an authoritative signal set by the user, not a guess - see
// festivalIDForChannel), being on the right calendar day, and - only as a
// tiebreaker when an actual clock time was parsed - closeness in time. A
// festival conflict (the source is linked to a *different* Festival than
// this candidate's event) is a strong negative signal, since it usually
// means this candidate is from an unrelated edition that merely happens to
// share a stage name or artist.
func candidateScore(c matchCandidate, hasTimeOfDay bool) float64 {
	score := 0.0
	if c.contained {
		score += 100
	}
	if c.stageMatch {
		score += 40
	}
	if c.festivalMatch {
		score += 50
	}
	if c.festivalConflict {
		score -= 60
	}
	if c.sameDay {
		score += 30
	}
	score += c.artistScore * 70
	// Keyword signals. The event-name match is a strong, festival-link-free
	// disambiguator (the recording literally names the event), and the year
	// keyword settles which edition when several reuse the same stages and
	// artists. A named year that contradicts the event's edition year is a
	// negative signal, like a festival conflict.
	score += c.eventNameScore * 45
	if c.yearMatch {
		score += 25
	}
	if c.yearConflict {
		score -= 25
	}
	if hasTimeOfDay && !c.contained {
		hours := c.delta.Hours()
		if hours < 0 {
			hours = 0
		}
		score += 10 / (1 + hours)
	}
	return score
}

// festivalIDForChannel looks up which Festival (if any) the recording's
// source is explicitly linked to, by matching the recording's folder name
// (channel) back to a configured source's safeName. This is an authoritative
// signal - set by hand on the source, in the Sources tab's "Event" picker -
// rather than a guess, so it's a much stronger disambiguator than stage-name
// or artist-name similarity when multiple festival editions reuse the same
// stage names (e.g. "RED", "MAIN STAGE") or share touring artists.
func festivalIDForChannel(cfg AppConfig, channel string) string {
	if channel == "" {
		return ""
	}
	for _, src := range cfg.Sources {
		if src.FestivalID != "" && safeName(src.Name) == channel {
			return src.FestivalID
		}
	}
	return ""
}

// bestMatchSuggestion checks every LibraryEvent's archived timetable for the
// set that best matches this recording, using whichever signals are
// available: the channel/stage name (from its folder), the guessed artist
// name parsed from the filename, and the guessed date/time (exact window
// containment if the filename included a clock time, otherwise
// same-calendar-day only). Anything after the date in the filename (e.g.
// festival name, edition/theme name, genre) is display-only and not parsed
// further - the channel/date/artist signals already fully disambiguate.
func bestMatchSuggestion(cfg AppConfig, path, name, channel string, guessed time.Time, fromName, hasTimeOfDay bool) MatchSuggestion {
	base := MatchSuggestion{Path: path, Name: name, Channel: channel, GuessedFromName: fromName, Confidence: "none", Reason: "Could not find a matching set - assign manually."}
	if fromName {
		base.GuessedTime = guessed.Format(time.RFC3339)
	}
	guessedArtist := guessArtistFromName(name, channel)
	base.GuessedArtist = guessedArtist

	sourceFestivalID := festivalIDForChannel(cfg, channel)

	// Keyword signals are computed once from the whole path + filename: the
	// set of identifying keywords the recording carries, and any years it
	// names. Both are matched per-candidate below.
	haystack := keywordTokenSet(path + " " + name)
	namedYears := keywordYears(path + " " + name)

	var best *matchCandidate
	var bestScore float64

	for _, ev := range cfg.LibraryEvents {
		festivalMatch := sourceFestivalID != "" && ev.FestivalID != "" && ev.FestivalID == sourceFestivalID
		festivalConflict := sourceFestivalID != "" && ev.FestivalID != "" && ev.FestivalID != sourceFestivalID
		eventNameScore := eventNameKeywordScore(ev.Name, haystack)
		yearMatch := ev.Year != 0 && namedYears[ev.Year]
		yearConflict := ev.Year != 0 && len(namedYears) > 0 && !namedYears[ev.Year]
		for _, stage := range ev.Timetable {
			stageMatch := channel != "" && (strings.EqualFold(stage.Stage, channel) || strings.Contains(strings.ToLower(stage.Stage), strings.ToLower(channel)) || strings.Contains(strings.ToLower(channel), strings.ToLower(stage.Stage)))
			for _, set := range stage.Sets {
				start, errS := time.Parse(time.RFC3339, set.Start)
				end, errE := time.Parse(time.RFC3339, set.End)
				if errS != nil {
					continue
				}
				if errE != nil {
					end = start.Add(time.Hour)
				}
				sameDay := fromName && guessed.Year() == start.Year() && guessed.Month() == start.Month() && guessed.Day() == start.Day()
				contained := hasTimeOfDay && !guessed.Before(start) && guessed.Before(end)
				var delta time.Duration
				if hasTimeOfDay && !contained {
					delta = start.Sub(guessed)
					if delta < 0 {
						delta = -delta
					}
					if d := end.Sub(guessed); d < delta && d > 0 {
						delta = d
					}
				}
				artistScore := 0.0
				if guessedArtist != "" {
					artistScore = artistSimilarity(guessedArtist, set.Name)
				}
				cand := matchCandidate{
					eventID: ev.ID, eventName: ev.Name, eventYear: ev.Year, setID: set.ID, stage: stage.Stage, artist: set.Name,
					delta: delta, contained: contained, stageMatch: stageMatch, sameDay: sameDay, artistScore: artistScore,
					festivalMatch: festivalMatch, festivalConflict: festivalConflict,
					eventNameScore: eventNameScore, yearMatch: yearMatch, yearConflict: yearConflict,
				}
				if score := candidateScore(cand, hasTimeOfDay); best == nil || score > bestScore {
					c := cand
					best = &c
					bestScore = score
				}
			}
		}
	}

	if best == nil {
		return applyFolderEventHint(cfg, base, path)
	}
	base.EventID = best.eventID
	base.EventName = best.eventName
	base.SetID = best.setID
	base.Stage = best.stage
	base.Artist = best.artist
	strongUnderlyingMatch := (best.contained && best.stageMatch) || (best.artistScore >= 0.99 && best.stageMatch && (best.contained || best.sameDay))
	switch {
	case best.festivalConflict && strongUnderlyingMatch:
		base.Confidence = "medium"
		base.Reason = fmt.Sprintf("%s on %s looks like a strong match, but this recording's source is linked to a different Event than \"%s\" - double check the source's Event setting (Sources tab) or approve if this is intentional.", best.artist, best.stage, best.eventName)
	case best.contained && best.stageMatch:
		base.Confidence = "high"
		if best.festivalMatch {
			base.Reason = fmt.Sprintf("%s on %s matches the channel, this source's linked Event, and the guessed time falls within this set's window.", best.artist, best.stage)
		} else {
			base.Reason = fmt.Sprintf("%s on %s matches the channel and the guessed time falls within this set's window.", best.artist, best.stage)
		}
	case best.artistScore >= 0.99 && best.stageMatch && (best.contained || best.sameDay):
		base.Confidence = "high"
		base.Reason = fmt.Sprintf("Filename matches \"%s\" on %s, on the right day.", best.artist, best.stage)
	case best.contained:
		base.Confidence = "medium"
		base.Reason = fmt.Sprintf("Guessed time falls within %s's window, but the channel doesn't match \"%s\" - double check.", best.artist, channel)
	case best.artistScore >= 0.99 && best.sameDay:
		base.Confidence = "medium"
		base.Reason = fmt.Sprintf("Filename matches \"%s\", playing that day, but on a different channel than \"%s\" - double check.", best.artist, channel)
	case best.artistScore >= 0.5 && best.stageMatch && best.sameDay:
		base.Confidence = "medium"
		base.Reason = fmt.Sprintf("Filename partially matches \"%s\" on %s, playing that day - double check.", best.artist, best.stage)
	case best.stageMatch && hasTimeOfDay && best.delta <= 30*time.Minute:
		base.Confidence = "medium"
		base.Reason = fmt.Sprintf("Channel matches %s; closest set by time (%s away).", best.stage, best.delta.Round(time.Minute))
	case best.stageMatch && best.sameDay:
		base.Confidence = "medium"
		base.Reason = fmt.Sprintf("Channel matches %s, and it's the right day - verify the exact set.", best.stage)
	default:
		base.Confidence = "low"
		if hasTimeOfDay {
			base.Reason = fmt.Sprintf("Closest guess: %s on %s, %s away - verify before approving.", best.artist, best.stage, best.delta.Round(time.Minute))
		} else {
			base.Reason = fmt.Sprintf("Closest guess: %s on %s - verify before approving.", best.artist, best.stage)
		}
	}
	// Keyword post-processing: a recording that literally names its event (and,
	// ideally, the right year) is more trustworthy than the time/stage signals
	// alone suggest; one that names a *different* year is less so.
	if best.eventNameScore >= 1 && best.stageMatch {
		if base.Confidence == "low" {
			base.Confidence = "medium"
		}
		yearNote := ""
		if best.yearMatch && best.eventYear != 0 {
			yearNote = fmt.Sprintf(" (%d)", best.eventYear)
		}
		base.Reason += fmt.Sprintf(" The filename also names this event%s.", yearNote)
	}
	if best.yearConflict && base.Confidence == "high" {
		base.Confidence = "medium"
		base.Reason += fmt.Sprintf(" Note: the filename/folder names a different year than %q's edition - check this is the right year.", best.eventName)
	}
	if base.Confidence == "none" || base.Confidence == "low" {
		return applyFolderEventHint(cfg, base, path)
	}
	return base
}

// applyFolderEventHint fills in a weak (or missing) timetable-set match from
// the recording's folder layout instead, for the case Smart Match's per-set
// scoring can't handle at all: a whole day (or whole stage) recorded as one
// file, with no single DJ set to attach it to. If an existing LibraryEvent's
// name (and year, if the folder gave one) matches, the recording is filed
// under it directly; otherwise the suggestion proposes creating that event,
// left for the user to approve rather than done silently. Either way, no
// SetID/Artist is guessed - approving just files the recording under the
// right event and stage.
func applyFolderEventHint(cfg AppConfig, base MatchSuggestion, path string) MatchSuggestion {
	name, year, stage, ok := folderEventHint(path)
	if !ok {
		return base
	}
	for _, ev := range cfg.LibraryEvents {
		if eventMatchesFolderHint(ev, name, year) {
			base.EventID = ev.ID
			base.EventName = ev.Name
			base.SetID = ""
			base.Artist = ""
			base.Stage = stage
			base.Confidence = "medium"
			base.Reason = fmt.Sprintf("Folder path matches the existing event %q, stage %q - no specific set/artist to match, so this will be filed as a full recording with no DJ assigned.", ev.Name, stage)
			return base
		}
	}
	base.NewEventName = name
	base.NewEventYear = year
	base.Stage = stage
	base.Confidence = "medium"
	yearSuffix := ""
	if year != 0 {
		yearSuffix = fmt.Sprintf(" (%d)", year)
	}
	base.Reason = fmt.Sprintf("No event named %q%s exists yet - approving will create it and file this under stage %q.", name, yearSuffix, stage)
	return base
}

// handleLibraryEvents lists or creates LibraryEvents - the "album" grouping
// used to organize recordings from a specific edition of a festival.
func (a *App) handleLibraryEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		writeJSON(w, a.snapshotConfig().LibraryEvents)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var ev LibraryEvent
	if err := json.NewDecoder(r.Body).Decode(&ev); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(ev.Name) == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}
	ev.ID = newID()
	assignScheduleIDs(ev.Timetable)
	a.mu.Lock()
	a.cfg.LibraryEvents = append(a.cfg.LibraryEvents, ev)
	cfg := a.cfg
	a.mu.Unlock()
	_ = a.persist(cfg)
	writeJSON(w, ev)
}

// handleLibraryEventItem handles per-event operations addressed as
// /api/events/{id}, plus /api/events/{id}/timetable for the archived
// timetable belonging to that event.
func (a *App) handleLibraryEventItem(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/events/")
	parts := strings.SplitN(rest, "/", 2)
	id := parts[0]
	if id == "" {
		http.NotFound(w, r)
		return
	}
	if len(parts) == 2 && parts[1] == "timetable" {
		a.handleLibraryEventTimetable(w, r, id)
		return
	}
	switch r.Method {
	case http.MethodPut:
		var ev LibraryEvent
		if err := json.NewDecoder(r.Body).Decode(&ev); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if strings.TrimSpace(ev.Name) == "" {
			http.Error(w, "name is required", http.StatusBadRequest)
			return
		}
		ev.ID = id
		a.mu.Lock()
		idx := -1
		for i, existing := range a.cfg.LibraryEvents {
			if existing.ID == id {
				idx = i
				break
			}
		}
		if idx == -1 {
			a.mu.Unlock()
			http.NotFound(w, r)
			return
		}
		// The event edit form only sends name/dates/branding, never the
		// archived timetable - preserve whatever was previously imported
		// unless this request explicitly included one.
		if ev.Timetable == nil {
			ev.Timetable = a.cfg.LibraryEvents[idx].Timetable
		} else {
			assignScheduleIDs(ev.Timetable)
		}
		a.cfg.LibraryEvents[idx] = ev
		cfg := a.cfg
		a.mu.Unlock()
		_ = a.persist(cfg)
		writeJSON(w, ev)
	case http.MethodDelete:
		a.mu.Lock()
		found := false
		out := a.cfg.LibraryEvents[:0:0]
		for _, e := range a.cfg.LibraryEvents {
			if e.ID == id {
				found = true
				continue
			}
			out = append(out, e)
		}
		a.cfg.LibraryEvents = out
		// Unassign (not delete) any recordings that referenced this event, so
		// the underlying files just fall back into "Unsorted" instead of
		// losing their event link silently.
		for path, meta := range a.cfg.RecordingMeta {
			if meta.EventID == id {
				meta.EventID = ""
				a.cfg.RecordingMeta[path] = meta
			}
		}
		cfg := a.cfg
		a.mu.Unlock()
		if !found {
			http.NotFound(w, r)
			return
		}
		_ = a.persist(cfg)
		writeJSON(w, map[string]string{"status": "deleted"})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleFestivals lists or creates Festivals - the "which recurring event
// does this live Source belong to" grouping shown to the user as "Event",
// used by the Watch tab's source picker and the Dashboard's source groups.
func (a *App) handleFestivals(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		writeJSON(w, a.snapshotConfig().Festivals)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var f Festival
	if err := json.NewDecoder(r.Body).Decode(&f); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(f.Name) == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}
	f.ID = newID()
	a.mu.Lock()
	a.cfg.Festivals = append(a.cfg.Festivals, f)
	cfg := a.cfg
	a.mu.Unlock()
	_ = a.persist(cfg)
	writeJSON(w, f)
}

// handleFestivalItem handles per-festival operations addressed as
// /api/festivals/{id}.
func (a *App) handleFestivalItem(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/festivals/")
	if id == "" {
		http.NotFound(w, r)
		return
	}
	switch r.Method {
	case http.MethodPut:
		var f Festival
		if err := json.NewDecoder(r.Body).Decode(&f); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if strings.TrimSpace(f.Name) == "" {
			http.Error(w, "name is required", http.StatusBadRequest)
			return
		}
		f.ID = id
		a.mu.Lock()
		idx := -1
		for i, existing := range a.cfg.Festivals {
			if existing.ID == id {
				idx = i
				break
			}
		}
		if idx == -1 {
			a.mu.Unlock()
			http.NotFound(w, r)
			return
		}
		a.cfg.Festivals[idx] = f
		cfg := a.cfg
		a.mu.Unlock()
		_ = a.persist(cfg)
		writeJSON(w, f)
	case http.MethodDelete:
		a.mu.Lock()
		found := false
		out := a.cfg.Festivals[:0:0]
		for _, f := range a.cfg.Festivals {
			if f.ID == id {
				found = true
				continue
			}
			out = append(out, f)
		}
		a.cfg.Festivals = out
		// Unassign (not delete) any sources that referenced this festival.
		for i := range a.cfg.Sources {
			if a.cfg.Sources[i].FestivalID == id {
				a.cfg.Sources[i].FestivalID = ""
			}
		}
		cfg := a.cfg
		a.mu.Unlock()
		if !found {
			http.NotFound(w, r)
			return
		}
		_ = a.persist(cfg)
		writeJSON(w, map[string]string{"status": "deleted"})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleOrganisations lists or creates Organisations - the parent grouping
// above Festivals (e.g. a promoter that owns several festival franchises).
func (a *App) handleOrganisations(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		writeJSON(w, a.snapshotConfig().Organisations)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var o Organisation
	if err := json.NewDecoder(r.Body).Decode(&o); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(o.Name) == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}
	o.ID = newID()
	a.mu.Lock()
	a.cfg.Organisations = append(a.cfg.Organisations, o)
	cfg := a.cfg
	a.mu.Unlock()
	_ = a.persist(cfg)
	writeJSON(w, o)
}

// handleOrganisationItem handles per-organisation operations at
// /api/organisations/{id}.
func (a *App) handleOrganisationItem(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/organisations/")
	if id == "" {
		http.NotFound(w, r)
		return
	}
	switch r.Method {
	case http.MethodPut:
		var o Organisation
		if err := json.NewDecoder(r.Body).Decode(&o); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if strings.TrimSpace(o.Name) == "" {
			http.Error(w, "name is required", http.StatusBadRequest)
			return
		}
		o.ID = id
		a.mu.Lock()
		idx := -1
		for i, existing := range a.cfg.Organisations {
			if existing.ID == id {
				idx = i
				break
			}
		}
		if idx == -1 {
			a.mu.Unlock()
			http.NotFound(w, r)
			return
		}
		a.cfg.Organisations[idx] = o
		cfg := a.cfg
		a.mu.Unlock()
		_ = a.persist(cfg)
		writeJSON(w, o)
	case http.MethodDelete:
		a.mu.Lock()
		found := false
		out := a.cfg.Organisations[:0:0]
		for _, o := range a.cfg.Organisations {
			if o.ID == id {
				found = true
				continue
			}
			out = append(out, o)
		}
		a.cfg.Organisations = out
		// Unassign (not delete) any festivals that referenced this organisation.
		for i := range a.cfg.Festivals {
			if a.cfg.Festivals[i].OrganisationID == id {
				a.cfg.Festivals[i].OrganisationID = ""
			}
		}
		cfg := a.cfg
		a.mu.Unlock()
		if !found {
			http.NotFound(w, r)
			return
		}
		_ = a.persist(cfg)
		writeJSON(w, map[string]string{"status": "deleted"})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleLibraryEventTimetable serves and imports the archived timetable for
// one LibraryEvent. POST accepts a compact per-stage JSON shape (an array of
// stage objects with [year,month,day,hour,minute,name] set tuples) so a
// previous year's schedule can be pasted in directly.
func (a *App) handleLibraryEventTimetable(w http.ResponseWriter, r *http.Request, id string) {
	switch r.Method {
	case http.MethodGet:
		cfg := a.snapshotConfig()
		for _, e := range cfg.LibraryEvents {
			if e.ID == id {
				writeJSON(w, e.Timetable)
				return
			}
		}
		http.NotFound(w, r)
	case http.MethodPost:
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		tt, err := parseAnyTimetableJSON(body)
		if err != nil {
			http.Error(w, "could not parse timetable JSON: "+err.Error(), http.StatusBadRequest)
			return
		}
		if len(tt) == 0 {
			http.Error(w, "no stages/sets found in that JSON", http.StatusBadRequest)
			return
		}
		assignScheduleIDs(tt)
		a.mu.Lock()
		idx := -1
		for i, e := range a.cfg.LibraryEvents {
			if e.ID == id {
				idx = i
				break
			}
		}
		if idx == -1 {
			a.mu.Unlock()
			http.NotFound(w, r)
			return
		}
		a.cfg.LibraryEvents[idx].Timetable = tt
		name := a.cfg.LibraryEvents[idx].Name
		cfg := a.cfg
		a.mu.Unlock()
		_ = a.persist(cfg)
		a.event("info", fmt.Sprintf("Imported archived timetable for %q (%d stages)", name, len(tt)))
		writeJSON(w, tt)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleRecordingMeta assigns (PUT) or clears (DELETE) the library metadata
// for one recorded file, keyed by its path relative to FinishedDir.
func (a *App) handleRecordingMeta(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPut:
		var req struct {
			Path      string `json:"path"`
			EventID   string `json:"eventId"`
			Channel   string `json:"channel,omitempty"`
			SetID     string `json:"setId,omitempty"`
			Artist    string `json:"artist,omitempty"`
			Start     string `json:"start,omitempty"`
			End       string `json:"end,omitempty"`
			Tracklist string `json:"tracklist,omitempty"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if strings.TrimSpace(req.Path) == "" || strings.Contains(req.Path, "..") {
			http.Error(w, "a valid path is required", http.StatusBadRequest)
			return
		}
		meta := RecordingMeta{EventID: req.EventID, Channel: req.Channel, SetID: req.SetID, Artist: req.Artist, Start: req.Start, End: req.End, Tracklist: req.Tracklist}
		// A specific timetable set was picked ("by artist") rather than a
		// manual time entry - look up its name/start/end so the client
		// doesn't have to duplicate that logic.
		if req.SetID != "" {
			a.mu.RLock()
			for _, e := range a.cfg.LibraryEvents {
				if e.ID != req.EventID {
					continue
				}
				if _, set := findSetByID(e.Timetable, req.SetID); set != nil {
					meta.Artist = set.Name
					meta.Start = set.Start
					meta.End = set.End
				}
				break
			}
			a.mu.RUnlock()
		}
		a.mu.Lock()
		if a.cfg.RecordingMeta == nil {
			a.cfg.RecordingMeta = map[string]RecordingMeta{}
		}
		a.cfg.RecordingMeta[req.Path] = meta
		cfg := a.cfg
		a.mu.Unlock()
		_ = a.persist(cfg)
		writeJSON(w, meta)
	case http.MethodDelete:
		path := r.URL.Query().Get("path")
		if path == "" {
			http.Error(w, "path is required", http.StatusBadRequest)
			return
		}
		a.mu.Lock()
		delete(a.cfg.RecordingMeta, path)
		cfg := a.cfg
		a.mu.Unlock()
		_ = a.persist(cfg)
		writeJSON(w, map[string]string{"status": "unassigned"})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (a *App) handleTimetable(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		writeJSON(w, a.snapshotConfig().Timetable)
		return
	}
	if r.Method != http.MethodPut {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var tt []StageSchedule
	if err := json.NewDecoder(r.Body).Decode(&tt); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	assignScheduleIDs(tt)
	a.mu.Lock()
	a.cfg.Timetable = tt
	cfg := a.cfg
	a.mu.Unlock()
	_ = a.persist(cfg)
	writeJSON(w, tt)
}

// parseAnyTimetableJSON accepts any timetable shape this app can produce or
// consume, tried in order:
//  1. RFC3339 StageSchedule array — the app's own export format.
//  2. Planner JSON — the format used by timetable.lol and local JSON exports
//     (object with "data": { day: { date, stages: { name: [[id,start,end,artist]] } } }).
//  3. Compact [year,month,day,hour,minute,name?] per-stage tuple array — the
//     shape hand-edited community timetables typically ship in.
func parseAnyTimetableJSON(data []byte) ([]StageSchedule, error) {
	// Try 1: RFC3339 StageSchedule array (app's own export).
	var direct []StageSchedule
	if err := json.Unmarshal(data, &direct); err == nil {
		total := 0
		for _, s := range direct {
			total += len(s.Sets)
		}
		if total > 0 {
			return direct, nil
		}
	}
	// Try 2: Planner JSON (timetable.lol / local planner file format).
	var planner timetableLolPlannerData
	if err := json.Unmarshal(data, &planner); err == nil && len(planner.Data) > 0 {
		tt, _, convErr := convertTimetableLolData(planner)
		if convErr == nil && len(tt) > 0 {
			return tt, nil
		}
	}
	// Try 3: Compact [year,month,day,hour,minute,name?] per-stage tuple array.
	return parseStageTimetableJSON(data)
}

// handleTimetableImport replaces the whole timetable from an uploaded file,
// accepting either supported JSON shape (see parseAnyTimetableJSON) so a
// downloaded community timetable can be applied in one click instead of
// hand-pasting it into the raw JSON editor.
func (a *App) handleTimetableImport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	fileName := strings.TrimSpace(r.URL.Query().Get("name"))
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	tt, err := parseAnyTimetableJSON(body)
	if err != nil {
		http.Error(w, "could not parse that timetable file: "+err.Error(), http.StatusBadRequest)
		return
	}
	if len(tt) == 0 {
		http.Error(w, "no stages or sets found in that file", http.StatusBadRequest)
		return
	}
	assignScheduleIDs(tt)
	a.mu.Lock()
	// Preserve any per-stage stream URLs already configured, matched by stage
	// name, since community timetables usually don't carry playable URLs.
	existingURL := map[string]string{}
	for _, s := range a.cfg.Timetable {
		if s.URL != "" {
			existingURL[strings.ToLower(s.Stage)] = s.URL
		}
	}
	for i := range tt {
		if tt[i].URL == "" {
			if u, ok := existingURL[strings.ToLower(tt[i].Stage)]; ok {
				tt[i].URL = u
			}
		}
	}
	a.cfg.Timetable = tt
	snapshotTimetable(&a.cfg, fileName, "file upload", tt)
	cfg := a.cfg
	a.mu.Unlock()
	_ = a.persist(cfg)
	stageCount := len(tt)
	setCount := 0
	for _, s := range tt {
		setCount += len(s.Sets)
	}
	a.event("info", fmt.Sprintf("Imported timetable from file: %d stage(s), %d set(s)", stageCount, setCount))
	writeJSON(w, map[string]any{"timetable": tt, "stages": stageCount, "sets": setCount})
}

// handleTimetableFavorites replaces the list of favorited/starred set IDs
// that reminders are sent for.
func (a *App) handleTimetableFavorites(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var ids []string
	if err := json.NewDecoder(r.Body).Decode(&ids); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	a.mu.Lock()
	a.cfg.Settings.FavoriteSetIDs = ids
	cfg := a.cfg
	a.mu.Unlock()
	_ = a.persist(cfg)
	writeJSON(w, ids)
}

const timetableLolAPIBase = "https://api.timetable.lol"

// timetableLolSet accepts timetable.lol's PlannerSet, which is either the
// tuple form [stableId, start, end, artist] or an equivalent object.
type timetableLolSet struct {
	ID     string
	Start  string
	End    string
	Artist string
}

func (s *timetableLolSet) UnmarshalJSON(b []byte) error {
	var arr []string
	if err := json.Unmarshal(b, &arr); err == nil {
		if len(arr) < 4 {
			return fmt.Errorf("expected at least 4 items in tuple set, got %d", len(arr))
		}
		s.ID, s.Start, s.End, s.Artist = arr[0], arr[1], arr[2], arr[3]
		return nil
	}
	var obj struct {
		ID     string `json:"id"`
		Start  string `json:"start"`
		End    string `json:"end"`
		Artist string `json:"artist"`
	}
	if err := json.Unmarshal(b, &obj); err != nil {
		return err
	}
	s.ID, s.Start, s.End, s.Artist = obj.ID, obj.Start, obj.End, obj.Artist
	return nil
}

type timetableLolDay struct {
	Date   string                       `json:"date"`
	Stages map[string][]timetableLolSet `json:"stages"`
}

type timetableLolFestivalDay struct {
	Start string `json:"start"`
	End   string `json:"end"`
	Date  string `json:"date"` // YYYY-MM-DD — more reliable than parsing day.Date label
}

type timetableLolPlannerData struct {
	EventSlug     string                             `json:"eventSlug"`
	PlanType      string                             `json:"planType"`
	Title         string                             `json:"title"`
	TimeZone      string                             `json:"timeZone"`
	Data          map[string]timetableLolDay         `json:"data"`
	FestivalRange map[string]timetableLolFestivalDay `json:"festivalRange,omitempty"`
	StageColors   map[string]string                  `json:"stageColors,omitempty"`
}

// handleTimetableLolEvents lists public events from timetable.lol so the
// WebUI can offer a browse/import picker. This is entirely optional - the
// timetable can always be edited by hand or visually instead.
func (a *App) handleTimetableLolEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, timetableLolAPIBase+"/api/events", nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		http.Error(w, fmt.Sprintf("could not reach timetable.lol: %s", err), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		http.Error(w, fmt.Sprintf("timetable.lol responded with %s", resp.Status), http.StatusBadGateway)
		return
	}
	var payload struct {
		Events []map[string]any `json:"events"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		http.Error(w, "could not parse timetable.lol response", http.StatusBadGateway)
		return
	}
	for _, e := range payload.Events {
		if d := firstStringField(e, "startDate", "start_date", "date", "dates", "eventDate", "start"); d != "" {
			e["displayStartDate"] = d
		}
		if d := firstStringField(e, "endDate", "end_date", "end"); d != "" {
			e["displayEndDate"] = d
		}
	}
	writeJSON(w, map[string]any{"events": payload.Events, "attribution": "Timetable data provided by timetable.lol (https://timetable.lol)"})
}

// firstStringField returns the first non-empty string value found in m under
// any of keys, tried in order - used to normalize timetable.lol's event list
// (an opaquely passed-through map[string]any, since its exact field names
// aren't part of any contract with us) into one canonical field the
// frontend can rely on regardless of which name timetable.lol actually uses.
func firstStringField(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			if s, ok := v.(string); ok && s != "" {
				return s
			}
		}
	}
	return ""
}

// handleTimetableLolImport fetches one event's schedule from timetable.lol
// and replaces our local timetable with it, remembering the link for
// display/re-sync. Existing per-stage stream URLs are preserved by matching
// on stage name.
func (a *App) handleTimetableLolImport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		EventSlug string `json:"eventSlug"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.EventSlug) == "" {
		http.Error(w, "eventSlug is required", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	apiURL := timetableLolAPIBase + "/api/events/" + url.PathEscape(req.EventSlug) + "/timetable-data"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		http.Error(w, fmt.Sprintf("could not reach timetable.lol: %s", err), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		http.Error(w, fmt.Sprintf("timetable.lol responded with %s: %s", resp.Status, strings.TrimSpace(string(body))), http.StatusBadGateway)
		return
	}
	var payload timetableLolPlannerData
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		http.Error(w, "could not parse timetable.lol response", http.StatusBadGateway)
		return
	}

	schedule, warnings, err := convertTimetableLolData(payload)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	cfg := a.snapshotConfig()
	existing := map[string]StageSchedule{}
	for _, st := range cfg.Timetable {
		existing[strings.ToLower(st.Stage)] = st
	}
	for i := range schedule {
		if old, ok := existing[strings.ToLower(schedule[i].Stage)]; ok {
			schedule[i].URL = old.URL
		}
	}

	link := &TimetableLink{
		EventSlug:  req.EventSlug,
		EventTitle: payload.Title,
		PlanType:   payload.PlanType,
		SourceURL:  "https://timetable.lol/" + req.EventSlug,
		ImportedAt: time.Now(),
	}

	snapshotName := payload.Title
	if snapshotName == "" {
		snapshotName = req.EventSlug
	}
	a.mu.Lock()
	a.cfg.Timetable = schedule
	a.cfg.TimetableSource = link
	snapshotTimetable(&a.cfg, snapshotName, "timetable.lol", schedule)
	cfg2 := a.cfg
	a.mu.Unlock()
	_ = a.persist(cfg2)
	a.event("info", fmt.Sprintf("Imported timetable for %q from timetable.lol (%d stages)", req.EventSlug, len(schedule)))

	writeJSON(w, map[string]any{"timetable": schedule, "source": link, "warnings": warnings})
}

// handleTimetableLolUnlink forgets which timetable.lol event the local
// timetable was imported from, without touching the timetable data itself.
func (a *App) handleTimetableLolUnlink(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	a.mu.Lock()
	a.cfg.TimetableSource = nil
	cfg := a.cfg
	a.mu.Unlock()
	_ = a.persist(cfg)
	writeJSON(w, map[string]string{"status": "unlinked"})
}

// handleTimetableSavedItem handles the saved-timetable list's per-item
// actions, addressed as /api/timetable/saved/{id}[/activate] - POST
// .../activate switches the live timetable to that snapshot (same per-stage
// URL-preservation as every other import path), DELETE forgets it.
func (a *App) handleTimetableSavedItem(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/timetable/saved/")
	parts := strings.SplitN(rest, "/", 2)
	id := parts[0]
	if id == "" {
		http.NotFound(w, r)
		return
	}
	action := ""
	if len(parts) > 1 {
		action = parts[1]
	}

	switch {
	case r.Method == http.MethodPost && action == "activate":
		a.mu.Lock()
		var found *SavedTimetable
		for i := range a.cfg.SavedTimetables {
			if a.cfg.SavedTimetables[i].ID == id {
				found = &a.cfg.SavedTimetables[i]
				break
			}
		}
		if found == nil {
			a.mu.Unlock()
			http.NotFound(w, r)
			return
		}
		schedule := append([]StageSchedule(nil), found.Schedule...)
		existingURL := map[string]string{}
		for _, s := range a.cfg.Timetable {
			if s.URL != "" {
				existingURL[strings.ToLower(s.Stage)] = s.URL
			}
		}
		for i := range schedule {
			if schedule[i].URL == "" {
				if u, ok := existingURL[strings.ToLower(schedule[i].Stage)]; ok {
					schedule[i].URL = u
				}
			}
		}
		a.cfg.Timetable = schedule
		name := found.Name
		cfg := a.cfg
		a.mu.Unlock()
		_ = a.persist(cfg)
		a.event("info", fmt.Sprintf("Switched active timetable to saved snapshot %q", name))
		writeJSON(w, map[string]any{"timetable": schedule})

	case r.Method == http.MethodDelete && action == "":
		a.mu.Lock()
		out := a.cfg.SavedTimetables[:0]
		for _, s := range a.cfg.SavedTimetables {
			if s.ID != id {
				out = append(out, s)
			}
		}
		a.cfg.SavedTimetables = out
		cfg := a.cfg
		a.mu.Unlock()
		_ = a.persist(cfg)
		writeJSON(w, map[string]string{"status": "deleted"})

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// convertTimetableLolData converts a timetable.lol PlannerData payload into
// our StageSchedule/ScheduleSet shape. Non-timed rows (lineup-only entries
// with no start/end) are skipped since there's nothing to schedule a
// recording or reminder against.
func convertTimetableLolData(payload timetableLolPlannerData) ([]StageSchedule, []string, error) {
	if len(payload.Data) == 0 {
		return nil, nil, errors.New("timetable.lol returned no schedule data for this event")
	}
	loc := time.UTC
	if payload.TimeZone != "" {
		if l, err := time.LoadLocation(payload.TimeZone); err == nil {
			loc = l
		}
	}

	byStage := map[string][]ScheduleSet{}
	var warnings []string

	dayKeys := make([]string, 0, len(payload.Data))
	for k := range payload.Data {
		dayKeys = append(dayKeys, k)
	}
	sort.Strings(dayKeys)

	for _, dayKey := range dayKeys {
		day := payload.Data[dayKey]
		// Prefer festivalRange[dayKey].Date (clean YYYY-MM-DD) over the day
		// label ("Friday 24.06.22") since it needs no regex parsing.
		dateStr := ""
		if fr, ok := payload.FestivalRange[dayKey]; ok && fr.Date != "" {
			dateStr = fr.Date
		}
		if dateStr == "" {
			dateStr = day.Date
		}
		if dateStr == "" {
			dateStr = dayKey
		}
		baseDate, err := parseFlexibleDate(dateStr)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("skipped day %q: could not parse date", dayKey))
			continue
		}
		for stageName, sets := range day.Stages {
			for _, set := range sets {
				if strings.TrimSpace(set.Artist) == "" {
					continue
				}
				if strings.TrimSpace(set.Start) == "" {
					continue
				}
				start, ok := combineDateTime(baseDate, set.Start, loc)
				if !ok {
					warnings = append(warnings, fmt.Sprintf("skipped %q on %s: unparseable start time %q", set.Artist, dateStr, set.Start))
					continue
				}
				end, ok := combineDateTime(baseDate, set.End, loc)
				if !ok {
					end = start.Add(time.Hour)
				}
				if end.Before(start) {
					end = end.Add(24 * time.Hour)
				}
				byStage[stageName] = append(byStage[stageName], ScheduleSet{
					ID:    set.ID,
					Start: start.Format(time.RFC3339),
					End:   end.Format(time.RFC3339),
					Name:  set.Artist,
				})
			}
		}
	}

	stageNames := make([]string, 0, len(byStage))
	for k := range byStage {
		stageNames = append(stageNames, k)
	}
	sort.Strings(stageNames)

	out := make([]StageSchedule, 0, len(stageNames))
	for _, name := range stageNames {
		sets := byStage[name]
		sort.Slice(sets, func(i, j int) bool { return sets[i].Start < sets[j].Start })
		color := payload.StageColors[name]
		out = append(out, StageSchedule{Stage: name, Color: color, Sets: sets})
	}
	assignScheduleIDs(out)
	return out, warnings, nil
}

var timetableLolDateRe = regexp.MustCompile(`(\d{1,2})\.(\d{1,2})\.(\d{2,4})`)

// parseFlexibleDate accepts a bare "YYYY-MM-DD" date, a full RFC3339
// timestamp, or timetable.lol's actual "<Weekday> DD.MM.YY" day label (e.g.
// "Friday 30.05.25"), since the exact shape isn't guaranteed by the API.
func parseFlexibleDate(s string) (time.Time, error) {
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if m := timetableLolDateRe.FindStringSubmatch(s); m != nil {
		day, _ := strconv.Atoi(m[1])
		month, _ := strconv.Atoi(m[2])
		year, _ := strconv.Atoi(m[3])
		if year < 100 {
			year += 2000
		}
		return time.Date(year, time.Month(month), day, 0, 0, 0, 0, time.UTC), nil
	}
	return time.Time{}, fmt.Errorf("unrecognized date %q", s)
}

// festivalDayRolloverHour is the wall-clock hour before which a set listed
// under a given festival "day" is treated as belonging to the *next*
// calendar day. Festival timetables group a program that runs from the
// afternoon/evening into the small hours of the following morning all under
// one day label, so a set listed under "Thursday" at 01:00 is really Friday
// 01:00. There's a wide, reliable gap between when an after-party ends (~6am)
// and when the next day's program opens (late morning at the earliest), so
// any time from midnight up to this hour is unambiguously "after midnight,
// belongs to the next day". 8 is comfortably inside that gap.
const festivalDayRolloverHour = 8

// combineDateTime combines a festival day's date with an "HH:MM" wall-clock
// time in the event's timezone, rolling early-morning times over to the next
// calendar day per the festival-day convention (see festivalDayRolloverHour).
// An "HH:MM" with hour >= 24 (another common after-midnight convention, e.g.
// "25:00") is normalized by time.Date the same way. A value that's already a
// full RFC3339 timestamp is absolute and used as-is with no rollover.
func combineDateTime(base time.Time, hm string, loc *time.Location) (time.Time, bool) {
	hm = strings.TrimSpace(hm)
	if hm == "" {
		return time.Time{}, false
	}
	if t, err := time.Parse(time.RFC3339, hm); err == nil {
		return t, true
	}
	parts := strings.SplitN(hm, ":", 2)
	if len(parts) != 2 {
		return time.Time{}, false
	}
	h, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
	m, err2 := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err1 != nil || err2 != nil {
		return time.Time{}, false
	}
	dayOffset := 0
	if h < festivalDayRolloverHour {
		dayOffset = 1
	}
	return time.Date(base.Year(), base.Month(), base.Day()+dayOffset, h, m, 0, 0, loc), true
}

func (a *App) handleRecordAction(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/record/")
	switch r.Method {
	case http.MethodPost:
		cfg := a.snapshotConfig()
		for _, src := range cfg.Sources {
			if src.ID == id {
				a.clearRetry(src.ID)
				a.start(src)
				writeJSON(w, map[string]string{"status": "started"})
				return
			}
		}
		http.NotFound(w, r)
	case http.MethodDelete:
		a.stop(id)
		writeJSON(w, map[string]string{"status": "stopping"})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (a *App) handleLive(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/live/")
	parts := strings.SplitN(rest, "/", 3)
	id := parts[0]

	if len(parts) >= 2 && parts[1] == "hls" {
		a.handleLiveHLS(w, r, id, parts)
		return
	}

	if !a.snapshotConfig().Settings.AllowLiveProxy {
		http.Error(w, "live proxy disabled", http.StatusForbidden)
		return
	}
	cfg := a.snapshotConfig()
	for _, src := range cfg.Sources {
		if src.ID == id {
			// A token-gated "http" source (custom auth header, signed
			// cookie) needs its request proxied through this server rather
			// than redirected to - a redirect only hands the browser a URL,
			// never the server-held header the URL needs to actually work.
			if src.Type == "http" && len(src.HTTPHeaders) > 0 {
				a.proxyLiveHTTP(w, r, src)
				return
			}
			liveURL := src.URL
			if src.Type != "http" {
				ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
				defer cancel()
				quality := src.Quality
				if quality == "" {
					quality = cfg.Settings.DefaultQuality
				}
				if quality == "" {
					quality = "best"
				}
				out, err := exec.CommandContext(ctx, "streamlink", "--stream-url", src.URL, quality).Output()
				if err != nil {
					http.Error(w, "streamlink could not resolve a live stream", http.StatusBadGateway)
					return
				}
				liveURL = strings.TrimSpace(string(out))
			}
			http.Redirect(w, r, liveURL, http.StatusTemporaryRedirect)
			return
		}
	}
	http.NotFound(w, r)
}

// proxyLiveHTTP streams a token-gated "http" source's live URL through this
// server rather than redirecting the browser to it - the browser only ever
// talks to this endpoint, and this endpoint (not the client) holds the
// configured auth headers. Bound only by the client's own request context
// (no extra timeout), since a live view is meant to keep streaming for as
// long as the tab stays open.
func (a *App) proxyLiveHTTP(w http.ResponseWriter, r *http.Request, src Source) {
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, src.URL, nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	for _, p := range parseHTTPHeaderLines(src.HTTPHeaders) {
		req.Header.Set(p[0], p[1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		http.Error(w, "could not reach the upstream stream", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// handleLiveHLS serves the rolling HLS playlist/segments for a source that is
// currently recording with live rewind enabled, addressed as
// /api/live/{id}/hls/{file}.
func (a *App) handleLiveHLS(w http.ResponseWriter, r *http.Request, id string, parts []string) {
	a.mu.RLock()
	rec := a.active[id]
	a.mu.RUnlock()
	if rec == nil || rec.hlsDir == "" {
		http.Error(w, "live rewind is not active for this source", http.StatusNotFound)
		return
	}
	file := "index.m3u8"
	if len(parts) >= 3 && parts[2] != "" {
		file = parts[2]
	}
	file = filepath.Clean(file)
	if strings.Contains(file, "..") || strings.ContainsAny(file, `/\`) {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}
	full := filepath.Join(rec.hlsDir, file)
	// ffmpeg doesn't write the manifest (or its first segment) until its
	// first hls_time interval elapses, so a player that requests
	// index.m3u8 the instant live rewind is clicked - before ffmpeg has
	// caught up - would otherwise hit a hard 404 and give up instead of
	// simply waiting the few seconds recording actually needs to start.
	if file == "index.m3u8" {
		waitForFile(r.Context(), full, 10*time.Second)
	}
	if _, err := os.Stat(full); err != nil {
		http.Error(w, "live rewind buffer is still starting - try again in a few seconds", http.StatusNotFound)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	http.ServeFile(w, r, full)
}

// waitForFile polls for path to exist, up to timeout, so a request that
// narrowly beats a background writer doesn't have to fail outright.
func waitForFile(ctx context.Context, path string, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func (a *App) handleMedia(w http.ResponseWriter, r *http.Request) {
	rel := strings.TrimPrefix(r.URL.Path, "/media/")
	root := filepath.Clean(a.snapshotConfig().Settings.FinishedDir)
	target := filepath.Clean(filepath.Join(root, rel))
	if !strings.HasPrefix(target, root) {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}
	http.ServeFile(w, r, target)
}

func (a *App) state() State {
	cfg := a.snapshotConfig()
	a.mu.RLock()
	defer a.mu.RUnlock()
	statuses := make([]SourceStatus, 0, len(cfg.Sources))
	now := map[string]*NowItem{}
	for _, src := range cfg.Sources {
		st := SourceStatus{Source: src, Status: "idle"}
		if !src.Enabled {
			st.Status = "disabled"
		}
		if rec := a.active[src.ID]; rec != nil {
			st.Status = "recording"
			st.OutputPath = rec.finalPath
			st.StartedAt = rec.startedAt
			st.LogPath = rec.logPath
			st.LastError = rec.lastErr
			st.LiveRewindActive = rec.hlsDir != ""
			if info, err := os.Stat(rec.tempPath); err == nil {
				st.Size = info.Size()
				st.LastHeartbeat = info.ModTime()
			}
		} else if last, ok := a.lastFinished[src.ID]; ok {
			st.OutputPath = last
			if info, err := os.Stat(last); err == nil {
				st.Size = info.Size()
			}
		}
		if st.Status == "idle" {
			if attempts, next, ok := a.reconnectStatus(src.ID); ok && time.Now().Before(next) {
				st.Status = "reconnecting"
				st.ReconnectAttempts = attempts
				st.NextRetryAt = next
			}
		}
		if st.OutputPath != "" {
			if rel, err := filepath.Rel(cfg.Settings.FinishedDir, st.OutputPath); err == nil && !strings.HasPrefix(rel, "..") {
				st.MediaPath = filepath.ToSlash(rel)
			}
		}
		stageKey := src.Name
		if strings.TrimSpace(src.TimetableStage) != "" {
			stageKey = src.TimetableStage
		}
		cur, next := scheduleFor(cfg.Timetable, stageKey, time.Now())
		if cur != nil {
			st.CurrentSet = cur.Name
			now[src.ID] = &NowItem{SetName: cur.Name, Starts: cur.Start, Ends: cur.End}
		}
		if next != nil {
			st.NextSet = next.Name
		}
		statuses = append(statuses, st)
	}
	// A source can still be actively recording after being deleted from the
	// config (deletion is blocked while recording, but this also covers any
	// recording left over from before that guard existed). Surface it so it
	// stays visible and stoppable instead of silently running forever.
	for id, rec := range a.active {
		found := false
		for _, src := range cfg.Sources {
			if src.ID == id {
				found = true
				break
			}
		}
		if found {
			continue
		}
		st := SourceStatus{Source: rec.source, Status: "recording", Orphaned: true}
		st.OutputPath = rec.finalPath
		st.StartedAt = rec.startedAt
		st.LogPath = rec.logPath
		st.LastError = rec.lastErr
		st.LiveRewindActive = rec.hlsDir != ""
		if info, err := os.Stat(rec.tempPath); err == nil {
			st.Size = info.Size()
			st.LastHeartbeat = info.ModTime()
		}
		if st.OutputPath != "" {
			if rel, err := filepath.Rel(cfg.Settings.FinishedDir, st.OutputPath); err == nil && !strings.HasPrefix(rel, "..") {
				st.MediaPath = filepath.ToSlash(rel)
			}
		}
		statuses = append(statuses, st)
	}
	warnings := freeWarnings(cfg)
	events := make([]Event, len(a.events))
	copy(events, a.events)
	diskUsage := disk.Scan(cfg.Settings.FinishedDir)
	forecast := computeStorageForecast(a.active, diskUsage.VolumeFree)
	return State{Version: version, StartedAt: a.startedAt, Sources: statuses, Events: events, Disk: diskUsage, Config: cfg, ActiveCount: len(a.active), Warnings: warnings, NowPlaying: now, StorageForecast: forecast}
}

func freeWarnings(cfg AppConfig) []string {
	usage := disk.Scan(cfg.Settings.FinishedDir)
	warnings := []string{}
	if usage.VolumeFree > 0 && usage.VolumeFree < cfg.Settings.WarnFreeBytes {
		warnings = append(warnings, fmt.Sprintf("Only %.1f GB free in finished directory", gb(usage.VolumeFree)))
	}
	if usage.VolumeFree > 0 && usage.VolumeFree < cfg.Settings.MinFreeBytes {
		warnings = append(warnings, "Recording is paused until more free space is available")
	}
	return warnings
}

func (a *App) snapshotConfig() AppConfig {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.cfg
}

func (a *App) isActive(id string) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.active[id] != nil
}

func (a *App) saveConfig(cfg AppConfig) error {
	a.mu.Lock()
	a.cfg = cfg
	a.mu.Unlock()
	return a.persist(cfg)
}

func (a *App) persist(cfg AppConfig) error {
	if err := os.MkdirAll(filepath.Dir(a.config), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(a.config, data, 0o644)
}

func (a *App) event(level, text string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.events = append(a.events, Event{Time: time.Now(), Level: level, Text: text})
	if len(a.events) > 300 {
		a.events = a.events[len(a.events)-300:]
	}
	log.Printf("[%s] %s", level, text)
}

func (a *App) writeNFO(rec *recording) {
	if !a.cfg.Settings.EnableNFO {
		return
	}
	nfo := strings.TrimSpace(fmt.Sprintf(`Title: %s
Source: %s
URL: %s
Started: %s
Finished: %s
Recorder: MutiRec %s

%s
`, rec.source.Name, rec.source.Type, rec.source.URL, rec.startedAt.Format(time.RFC3339), time.Now().Format(time.RFC3339), version, rec.source.ExtraNFO))
	_ = os.WriteFile(strings.TrimSuffix(rec.finalPath, filepath.Ext(rec.finalPath))+".nfo", []byte(nfo+"\n"), 0o644)
}

func (a *App) backup(rec *recording) {
	cfg := a.snapshotConfig()
	if !cfg.Settings.Backup.Enabled || !cfg.Settings.Backup.AfterComplete {
		return
	}
	if cfg.Settings.Backup.Method == "webdav" {
		if err := a.backupWebDAV(rec, cfg); err != nil {
			a.event("error", fmt.Sprintf("[%s] WebDAV backup failed: %s", rec.source.Name, err))
			return
		}
		a.event("info", fmt.Sprintf("[%s] WebDAV backup complete", rec.source.Name))
		return
	}
	if cfg.Settings.Backup.RcloneRemote == "" {
		return
	}
	args := append([]string{"copy", rec.finalPath, cfg.Settings.Backup.RcloneRemote}, cfg.Settings.Backup.RcloneArgs...)
	cmd := exec.Command("rclone", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		a.event("error", fmt.Sprintf("[%s] backup failed: %s %s", rec.source.Name, err, strings.TrimSpace(string(out))))
		return
	}
	a.event("info", fmt.Sprintf("[%s] backup complete", rec.source.Name))
}

// notify sends subject/body to whichever channels are configured (Discord
// webhook, SMTP, both, or neither). Always call this via `go a.notify(...)`
// from a recording-lifecycle path - a slow or hanging webhook/SMTP server
// must never delay finishing a recording. Both channels are attempted
// concurrently so one being slow doesn't delay the other, and every failure
// is logged to the event feed the same way, so a silently-misconfigured
// webhook doesn't just look like "nothing happened."
func (a *App) notify(subject, body string) {
	cfg := a.snapshotConfig()
	var wg sync.WaitGroup
	if cfg.Settings.Notifications.DiscordWebhook != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := sendDiscordWebhook(cfg.Settings.Notifications.DiscordWebhook, subject, body); err != nil {
				a.event("error", fmt.Sprintf("Discord notification failed: %s", err))
			}
		}()
	}
	s := cfg.Settings.Notifications.SMTP
	if s.Enabled && s.Host != "" && s.To != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := sendSMTP(s, subject, body); err != nil {
				a.event("error", fmt.Sprintf("email notification failed: %s", err))
			}
		}()
	}
	wg.Wait()
}

// discordContentLimit is Discord's hard cap on a webhook message's "content"
// field. A notification body built from a file path is normally tiny, but a
// long ffmpeg/streamlink error message (passed straight through by callers
// like execute()) could exceed it - Discord would otherwise reject the whole
// message with a 400 and the notification would silently vanish.
const discordContentLimit = 2000

var discordWebhookClient = &http.Client{Timeout: 15 * time.Second}

// sendDiscordWebhook posts a plain-content message to a Discord webhook URL,
// used by both notify() and the Settings "send test notification" button so
// there's exactly one place that builds/truncates/validates the payload.
func sendDiscordWebhook(webhookURL, subject, body string) error {
	content := subject
	if body != "" {
		content += "\n" + body
	}
	// Truncate by rune, not byte - Discord's limit is a character count, and
	// slicing by byte could both cut a multi-byte rune in half and, since the
	// "…" marker itself is multi-byte, push the result past the limit anyway.
	if runes := []rune(content); len(runes) > discordContentLimit {
		content = string(runes[:discordContentLimit-1]) + "…"
	}
	data, err := json.Marshal(map[string]string{"content": content})
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, webhookURL, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := discordWebhookClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<10))
		return fmt.Errorf("webhook responded with %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	return nil
}

// notificationTestResult is one channel's outcome from
// handleNotificationsTest: Tested is false when that channel wasn't
// configured at all (nothing to report), as opposed to Ok=false which means
// it was configured and attempted but failed.
type notificationTestResult struct {
	Tested bool   `json:"tested"`
	Ok     bool   `json:"ok"`
	Error  string `json:"error,omitempty"`
}

// handleNotificationsTest sends a small test message through whichever
// channel(s) are filled in on the request - the Settings page's current form
// values, not necessarily what's saved yet, the same "test before you save"
// convention handleSourceTest already established for sources (an admin's
// GET /api/config returns the real SMTP password unredacted, so the form
// already holds it - no masked-placeholder handling is needed here).
func (a *App) handleNotificationsTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !a.isAdminReq(r) {
		http.Error(w, "admin only", http.StatusForbidden)
		return
	}
	var req struct {
		DiscordWebhook string     `json:"discordWebhook"`
		SMTP           SMTPConfig `json:"smtp"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}

	const subject = "MutiRec test notification"
	const body = "If you're reading this, notifications are configured correctly."

	out := struct {
		Discord notificationTestResult `json:"discord"`
		SMTP    notificationTestResult `json:"smtp"`
	}{}

	if webhook := strings.TrimSpace(req.DiscordWebhook); webhook != "" {
		out.Discord.Tested = true
		if err := sendDiscordWebhook(webhook, subject, body); err != nil {
			out.Discord.Error = err.Error()
		} else {
			out.Discord.Ok = true
		}
	}
	if req.SMTP.Enabled && req.SMTP.Host != "" && req.SMTP.To != "" {
		out.SMTP.Tested = true
		if err := sendSMTP(req.SMTP, subject, body); err != nil {
			out.SMTP.Error = err.Error()
		} else {
			out.SMTP.Ok = true
		}
	}
	if !out.Discord.Tested && !out.SMTP.Tested {
		writeJSON(w, map[string]any{"error": "nothing to test - fill in a Discord webhook URL or enable SMTP with a host and recipient first"})
		return
	}
	writeJSON(w, out)
}

// sendSMTP delivers a plain-text email, supporting both STARTTLS (typically
// port 587) and implicit TLS (typically port 465) since net/smtp.SendMail
// only supports the former and silently fails against implicit-TLS servers.
func sendSMTP(s SMTPConfig, subject, body string) error {
	port := s.Port
	if port == 0 {
		port = 587
	}
	addr := fmt.Sprintf("%s:%d", s.Host, port)
	from := s.From
	if from == "" {
		from = s.Username
	}
	msg := []byte("From: " + from + "\r\nTo: " + s.To + "\r\nSubject: " + subject + "\r\n\r\n" + body)
	auth := smtp.PlainAuth("", s.Username, s.Password, s.Host)

	if port == 465 {
		conn, err := tls.Dial("tcp", addr, &tls.Config{ServerName: s.Host})
		if err != nil {
			return err
		}
		defer conn.Close()
		client, err := smtp.NewClient(conn, s.Host)
		if err != nil {
			return err
		}
		defer client.Close()
		if s.Username != "" {
			if err := client.Auth(auth); err != nil {
				return err
			}
		}
		if err := client.Mail(from); err != nil {
			return err
		}
		if err := client.Rcpt(s.To); err != nil {
			return err
		}
		w, err := client.Data()
		if err != nil {
			return err
		}
		if _, err := w.Write(msg); err != nil {
			return err
		}
		if err := w.Close(); err != nil {
			return err
		}
		return client.Quit()
	}

	return smtp.SendMail(addr, auth, from, []string{s.To}, msg)
}

func loadConfig(path string) (AppConfig, error) {
	if data, err := os.ReadFile(path); err == nil {
		var cfg AppConfig
		if err := json.Unmarshal(data, &cfg); err != nil {
			return cfg, err
		}
		normalizeConfig(&cfg)
		return cfg, nil
	}
	cfg := defaultConfig()
	normalizeConfig(&cfg)
	data, _ := json.MarshalIndent(cfg, "", "  ")
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	return cfg, os.WriteFile(path, data, 0o644)
}

func defaultConfig() AppConfig {
	return AppConfig{
		Settings: Settings{
			FinishedDir:             localizePath(env("FINISHED_DIR", "/data/recordings")),
			TempDir:                 localizePath(env("TEMP_DIR", "/data/incomplete")),
			LogDir:                  localizePath(env("LOG_DIR", "/data/logs")),
			CheckIntervalSeconds:    30,
			MinFreeBytes:            1024 * 1024 * 1024,
			WarnFreeBytes:           5 * 1024 * 1024 * 1024,
			DefaultQuality:          "best",
			DefaultContainer:        "mkv",
			EnableNFO:               true,
			EnableWaveform:          true,
			AllowLiveProxy:          true,
			LiveRewindWindowSeconds: 1800,
		},
		UI:      UISettings{AppName: "MutiRec", Theme: "midnight", Accent: "red"},
		Sources: []Source{},
	}
}

func normalizeConfig(cfg *AppConfig) {
	if cfg.Settings.FinishedDir == "" {
		cfg.Settings.FinishedDir = localizePath("/data/recordings")
	}
	if cfg.Settings.TempDir == "" {
		cfg.Settings.TempDir = localizePath("/data/incomplete")
	}
	if cfg.Settings.LogDir == "" {
		cfg.Settings.LogDir = localizePath("/data/logs")
	}
	if cfg.Settings.CheckIntervalSeconds == 0 {
		cfg.Settings.CheckIntervalSeconds = 30
	}
	if cfg.Settings.MinFreeBytes == 0 {
		cfg.Settings.MinFreeBytes = 1024 * 1024 * 1024
	}
	if cfg.Settings.WarnFreeBytes == 0 {
		cfg.Settings.WarnFreeBytes = 5 * 1024 * 1024 * 1024
	}
	if cfg.Settings.DefaultContainer == "" {
		cfg.Settings.DefaultContainer = "mkv"
	}
	if cfg.Settings.LiveRewindWindowSeconds == 0 {
		cfg.Settings.LiveRewindWindowSeconds = 1800
	}
	if cfg.UI.AppName == "" {
		cfg.UI.AppName = "MutiRec"
	}
	for i := range cfg.Sources {
		if cfg.Sources[i].ID == "" {
			cfg.Sources[i].ID = newID()
		}
		if cfg.Sources[i].Quality == "" {
			cfg.Sources[i].Quality = cfg.Settings.DefaultQuality
		}
		if cfg.Sources[i].Container == "" {
			cfg.Sources[i].Container = cfg.Settings.DefaultContainer
		}
	}
	if cfg.Settings.ReminderLeadMinutes == 0 {
		cfg.Settings.ReminderLeadMinutes = 15
	}
	// Slices must never round-trip as JSON null: encoding/json marshals a nil
	// slice as `null`, and the frontend calls array methods on these fields
	// without expecting that - one nil slice throws and silently aborts the
	// whole dashboard render (see the "no source cards" bug this fixed).
	if cfg.Sources == nil {
		cfg.Sources = []Source{}
	}
	if cfg.Timetable == nil {
		cfg.Timetable = []StageSchedule{}
	}
	if cfg.Settings.FavoriteSetIDs == nil {
		cfg.Settings.FavoriteSetIDs = []string{}
	}
	if cfg.LibraryEvents == nil {
		cfg.LibraryEvents = []LibraryEvent{}
	}
	if cfg.RecordingMeta == nil {
		cfg.RecordingMeta = map[string]RecordingMeta{}
	}
	if cfg.Festivals == nil {
		cfg.Festivals = []Festival{}
	}
	if cfg.Organisations == nil {
		cfg.Organisations = []Organisation{}
	}
	if cfg.InstanceID == "" {
		cfg.InstanceID = shortToken()
	}
	assignScheduleIDs(cfg.Timetable)
	for i := range cfg.LibraryEvents {
		if cfg.LibraryEvents[i].ID == "" {
			cfg.LibraryEvents[i].ID = newID()
		}
		if cfg.LibraryEvents[i].Timetable == nil {
			cfg.LibraryEvents[i].Timetable = []StageSchedule{}
		}
		assignScheduleIDs(cfg.LibraryEvents[i].Timetable)
	}
	for i := range cfg.Festivals {
		if cfg.Festivals[i].ID == "" {
			cfg.Festivals[i].ID = newID()
		}
	}
	for i := range cfg.Organisations {
		if cfg.Organisations[i].ID == "" {
			cfg.Organisations[i].ID = newID()
		}
	}
}

// assignScheduleIDs gives every set a stable ID (used for favoriting/reminders)
// if it doesn't already have one, e.g. from hand-edited JSON.
func assignScheduleIDs(tt []StageSchedule) {
	for i := range tt {
		for j := range tt[i].Sets {
			if tt[i].Sets[j].ID == "" {
				tt[i].Sets[j].ID = newID()
			}
		}
	}
}

type rawStage struct {
	Stage string  `json:"stage"`
	URL   string  `json:"url"`
	Sets  [][]any `json:"sets"`
}

// parseStageTimetableJSON parses a compact per-stage timetable JSON shape -
// an array of stage objects whose "sets" are
// [year, month, day, hour, minute, name?] tuples - used when pasting an
// archived timetable into a LibraryEvent. A row with no name marks only the
// end time of the previous set.
func parseStageTimetableJSON(data []byte) ([]StageSchedule, error) {
	var raw []rawStage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	// Compact timetables carry explicit calendar days per row, but still use
	// the after-midnight hour convention (e.g. an hour of 25, or a 1am set
	// authored on the previous day's row); time.Date normalizes both into a
	// valid instant rather than emitting an out-of-range "T25:00:00" string.
	cest := time.FixedZone("CEST", 2*60*60)
	var out []StageSchedule
	for _, stage := range raw {
		var sets []ScheduleSet
		var last *ScheduleSet
		for _, row := range stage.Sets {
			if len(row) < 5 {
				continue
			}
			start := time.Date(toInt(row[0]), time.Month(toInt(row[1])), toInt(row[2]), toInt(row[3]), toInt(row[4]), 0, 0, cest).Format(time.RFC3339)
			name := ""
			if len(row) > 5 {
				var parts []string
				for _, p := range row[5:] {
					if s, ok := p.(string); ok {
						parts = append(parts, s)
					}
				}
				name = strings.Join(parts, " ")
			}
			if last != nil && last.End == "" {
				last.End = start
			}
			if name == "" {
				continue
			}
			sets = append(sets, ScheduleSet{Start: start, Name: name})
			last = &sets[len(sets)-1]
		}
		if last != nil && last.End == "" {
			if t, err := time.Parse(time.RFC3339, last.Start); err == nil {
				last.End = t.Add(time.Hour).Format(time.RFC3339)
			}
		}
		out = append(out, StageSchedule{Stage: stage.Stage, URL: stage.URL, Sets: sets})
	}
	return out, nil
}

func scheduleFor(tt []StageSchedule, stage string, now time.Time) (*ScheduleSet, *ScheduleSet) {
	for _, st := range tt {
		if !strings.EqualFold(st.Stage, stage) {
			continue
		}
		sets := append([]ScheduleSet(nil), st.Sets...)
		sort.SliceStable(sets, func(i, j int) bool { return sets[i].Start < sets[j].Start })
		var next *ScheduleSet
		for i := range sets {
			start, sErr := time.Parse(time.RFC3339, sets[i].Start)
			end, eErr := time.Parse(time.RFC3339, sets[i].End)
			if sErr == nil && eErr == nil && !now.Before(start) && now.Before(end) {
				return &sets[i], nil
			}
			if sErr == nil && start.After(now) && next == nil {
				next = &sets[i]
			}
		}
		return nil, next
	}
	return nil, nil
}

// findSetByID looks up a single scheduled set by its stable ID across every
// stage, returning the owning stage name alongside it.
func findSetByID(tt []StageSchedule, id string) (string, *ScheduleSet) {
	for _, st := range tt {
		for i := range st.Sets {
			if st.Sets[i].ID == id {
				return st.Stage, &st.Sets[i]
			}
		}
	}
	return "", nil
}

// checkReminders sends a notification (Discord/SMTP, whatever is configured)
// for each favorited set that is about to start, once per set occurrence.
// Reminders are best-effort and only tracked in memory, so a restart shortly
// before a set starts may re-send one - an acceptable tradeoff for simplicity.
func (a *App) checkReminders(cfg AppConfig) {
	if len(cfg.Settings.FavoriteSetIDs) == 0 {
		return
	}
	lead := time.Duration(cfg.Settings.ReminderLeadMinutes) * time.Minute
	if lead <= 0 {
		lead = 15 * time.Minute
	}
	now := time.Now()

	a.mu.Lock()
	defer a.mu.Unlock()
	for id, sentAt := range a.remindersSent {
		if now.Sub(sentAt) > 24*time.Hour {
			delete(a.remindersSent, id)
		}
	}
	for _, id := range cfg.Settings.FavoriteSetIDs {
		if _, already := a.remindersSent[id]; already {
			continue
		}
		stage, set := findSetByID(cfg.Timetable, id)
		if set == nil {
			continue
		}
		start, err := time.Parse(time.RFC3339, set.Start)
		if err != nil {
			continue
		}
		if now.Before(start.Add(-lead)) || now.After(start) {
			continue
		}
		a.remindersSent[id] = now
		go a.notify(fmt.Sprintf("Starting soon: %s", set.Name), fmt.Sprintf("%s starts on %s at %s", set.Name, stage, start.Format(time.RFC1123)))
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// isDockerish reports whether we appear to be running inside the project's
// own Docker image, where /app and /data are always present and writable.
// /.dockerenv is created by the Docker runtime itself in every container.
func isDockerish() bool {
	if runtime.GOOS == "windows" {
		return false
	}
	_, err := os.Stat("/.dockerenv")
	return err == nil
}

// localizePath rewrites the project's Docker-convention defaults (/app/...,
// /data/...) into paths relative to the working directory when not running
// in Docker, so the app has somewhere sane to write on a bare "go run" or a
// native Windows/macOS/Linux install without requiring CONFIG_PATH/
// FINISHED_DIR/TEMP_DIR/LOG_DIR to be set by hand. Paths that don't match
// either convention (including a user's own explicit override) pass through
// unchanged.
func localizePath(p string) string {
	if isDockerish() {
		return p
	}
	if rest, ok := strings.CutPrefix(p, "/app/"); ok {
		return filepath.Join(".", rest)
	}
	if rest, ok := strings.CutPrefix(p, "/data/"); ok {
		return filepath.Join(".", rest)
	}
	return p
}

func safeName(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		} else if r == '-' || r == '_' || r == '.' {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "source"
	}
	return b.String()
}

func newID() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func toInt(v any) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case int:
		return x
	default:
		return 0
	}
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func gb(v uint64) float64 { return float64(v) / 1024 / 1024 / 1024 }

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

func tail(path string, limit int) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var lines []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		lines = append(lines, sc.Text())
		if len(lines) > limit {
			lines = lines[len(lines)-limit:]
		}
	}
	return lines
}
