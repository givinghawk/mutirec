package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/subtle"
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
	"syscall"
	"time"

	"golang.org/x/crypto/bcrypt"

	"defqon-stream-recorder/internal/disk"
)

var version = "dev"

//go:embed static/*
var staticFiles embed.FS

type AppConfig struct {
	Settings        Settings                 `json:"settings"`
	UI              UISettings               `json:"ui"`
	Sources         []Source                 `json:"sources"`
	Timetable       []StageSchedule          `json:"timetable"`
	TimetableSource *TimetableLink           `json:"timetableSource,omitempty"`
	LibraryEvents   []LibraryEvent           `json:"libraryEvents"`
	RecordingMeta   map[string]RecordingMeta `json:"recordingMeta"`
	Festivals       []Festival               `json:"festivals"`
}

// Festival is the recurring franchise a live Source belongs to (e.g.
// "Defqon.1", "Sensation") - shown to the user simply as "Event". It's
// intentionally a separate, lighter-weight concept from LibraryEvent (which
// represents one specific yearly edition/recording archive): a live Source's
// stream URL is reused every year, so it's grouped by the franchise rather
// than any one edition. Named Festival in code only to avoid colliding with
// the unrelated Event type used for the activity log below.
type Festival struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Color       string `json:"color,omitempty"`
	LogoURL     string `json:"logoUrl,omitempty"`
}

// LibraryEvent groups recorded files into one edition of a festival (e.g.
// "Defqon.1 2022"), independent of whatever Sources/Timetable are currently
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
	Timetable   []StageSchedule `json:"timetable,omitempty"`
}

// RecordingMeta links one recorded file (keyed by its path relative to
// FinishedDir) to a LibraryEvent and, optionally, a specific archived
// timetable set. Channel is an optional display-name override for grouping
// in the library UI - if blank, the recording's source folder name is used.
type RecordingMeta struct {
	EventID string `json:"eventId,omitempty"`
	Channel string `json:"channel,omitempty"`
	SetID   string `json:"setId,omitempty"`
	Artist  string `json:"artist,omitempty"`
	Start   string `json:"start,omitempty"`
	End     string `json:"end,omitempty"`
}

type Settings struct {
	FinishedDir             string        `json:"finishedDir"`
	TempDir                 string        `json:"tempDir"`
	LogDir                  string        `json:"logDir"`
	CheckIntervalSeconds    int           `json:"checkIntervalSeconds"`
	MinFreeBytes            uint64        `json:"minFreeBytes"`
	DefaultQuality          string        `json:"defaultQuality"`
	DefaultContainer        string        `json:"defaultContainer"`
	EnableNFO               bool          `json:"enableNfo"`
	EnableWaveform          bool          `json:"enableWaveform"`
	Backup                  BackupConfig  `json:"backup"`
	Notifications           Notifications `json:"notifications"`
	AllowLiveProxy          bool          `json:"allowLiveProxy"`
	WarnFreeBytes           uint64        `json:"warnFreeBytes"`
	LiveRewindWindowSeconds int           `json:"liveRewindWindowSeconds"`
	FavoriteSetIDs          []string      `json:"favoriteSetIds"`
	ReminderLeadMinutes     int           `json:"reminderLeadMinutes"`
	RecordingSetLookahead   time.Duration `json:"-"`
}

type UISettings struct {
	AppName    string            `json:"appName"`
	LogoURL    string            `json:"logoUrl"`
	Theme      string            `json:"theme"`
	Accent     string            `json:"accent"`
	CustomCSS  string            `json:"customCss"`
	CustomTheme string           `json:"customTheme"`
	ThemeColors map[string]string `json:"themeColors"`
}

type BackupConfig struct {
	Enabled       bool     `json:"enabled"`
	RcloneRemote  string   `json:"rcloneRemote"`
	RcloneArgs    []string `json:"rcloneArgs"`
	AfterComplete bool     `json:"afterComplete"`
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
	ID             string   `json:"id"`
	Name           string   `json:"name"`
	Type           string   `json:"type"`
	URL            string   `json:"url"`
	Enabled        bool     `json:"enabled"`
	Record         bool     `json:"record"`
	Quality        string   `json:"quality"`
	Container      string   `json:"container"`
	AudioOnly      bool     `json:"audioOnly"`
	Transcode      bool     `json:"transcode"`
	HardwareAccel  string   `json:"hardwareAccel"`
	StreamlinkArgs []string `json:"streamlinkArgs"`
	FFmpegArgs     []string `json:"ffmpegArgs"`
	ExtraNFO       string   `json:"extraNfo"`
	Color          string   `json:"color"`
	LiveRewind     bool     `json:"liveRewind"`
	TimetableStage string   `json:"timetableStage,omitempty"`
	FestivalID     string   `json:"festivalId,omitempty"`
}

type StageSchedule struct {
	Stage string        `json:"stage"`
	URL   string        `json:"url"`
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

type State struct {
	Version     string              `json:"version"`
	StartedAt   time.Time           `json:"startedAt"`
	Sources     []SourceStatus      `json:"sources"`
	Events      []Event             `json:"events"`
	Disk        disk.Usage          `json:"disk"`
	Config      AppConfig           `json:"config"`
	ActiveCount int                 `json:"activeCount"`
	Warnings    []string            `json:"warnings"`
	NowPlaying  map[string]*NowItem `json:"nowPlaying"`
}

type SourceStatus struct {
	Source
	Status           string    `json:"status"`
	OutputPath       string    `json:"outputPath"`
	MediaPath        string    `json:"mediaPath,omitempty"`
	Size             int64     `json:"size"`
	StartedAt        time.Time `json:"startedAt,omitempty"`
	LastError        string    `json:"lastError,omitempty"`
	CurrentSet       string    `json:"currentSet,omitempty"`
	NextSet          string    `json:"nextSet,omitempty"`
	LogPath          string    `json:"logPath,omitempty"`
	LastHeartbeat    time.Time `json:"lastHeartbeat,omitempty"`
	LiveRewindActive bool      `json:"liveRewindActive"`
	Orphaned         bool      `json:"orphaned,omitempty"`
}

// RecordingFile describes a single finished recording on disk, enriched with
// whatever library metadata (event/channel/artist/set) has been assigned to
// it via RecordingMeta.
type RecordingFile struct {
	Name    string    `json:"name"`
	Source  string    `json:"source"`
	Path    string    `json:"path"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"modTime"`
	EventID string    `json:"eventId,omitempty"`
	Channel string    `json:"channel,omitempty"`
	SetID   string    `json:"setId,omitempty"`
	Artist  string    `json:"artist,omitempty"`
	Start   string    `json:"start,omitempty"`
	End     string    `json:"end,omitempty"`
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
	source    Source
	ctx       context.Context
	cancel    context.CancelFunc
	startedAt time.Time
	tempPath  string
	finalPath string
	logPath   string
	logFile   *os.File
	lastErr   string
	done      chan struct{}
	hlsDir    string
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
	authFile      string
	remindersSent map[string]time.Time

	sessMu   sync.Mutex
	sessions map[string]time.Time

	credMu     sync.RWMutex
	credUser   string
	credHash   []byte
	needsSetup bool
}

const sessionCookieName = "dqr_session"
const sessionTTL = 30 * 24 * time.Hour

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

	addr := env("HTTP_ADDR", ":8080")
	server := &http.Server{Addr: addr, Handler: securityHeaders(app.requireAuth(mux))}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
		app.stopAll()
	}()

	log.Printf("Defqon recorder web UI listening on %s", addr)
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
		cfg:           cfg,
		config:        configPath,
		startedAt:     time.Now(),
		active:        map[string]*recording{},
		lastFinished:  map[string]string{},
		remindersSent: map[string]time.Time{},
		sessions:      map[string]time.Time{},
	}
	for _, dir := range []string{cfg.Settings.FinishedDir, cfg.Settings.TempDir, cfg.Settings.LogDir, filepath.Dir(configPath)} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	app.event("info", "Recorder started")
	return app, nil
}

// storedCredentials is the on-disk shape of auth.json: a username and a
// bcrypt hash, written by the in-browser setup wizard or the account
// settings form so nobody has to touch environment variables by hand.
type storedCredentials struct {
	Username     string `json:"username"`
	PasswordHash string `json:"passwordHash"`
}

// setupAuth decides how this instance authenticates, in priority order:
//  1. AUTH_USERNAME/AUTH_PASSWORD from the environment, for Docker-style
//     deployments that prefer to manage credentials externally.
//  2. Credentials previously saved to auth.json by the setup wizard or the
//     account settings form.
//  3. Neither: needsSetup is set, and the web UI serves a one-time setup
//     wizard instead of ever generating or printing a throwaway password.
func (a *App) setupAuth() {
	a.authFile = filepath.Join(filepath.Dir(a.config), "auth.json")
	a.authUser = os.Getenv("AUTH_USERNAME")
	a.authPass = os.Getenv("AUTH_PASSWORD")
	if a.authPass != "" {
		if a.authUser == "" {
			a.authUser = "admin"
		}
		log.Printf("Using AUTH_USERNAME/AUTH_PASSWORD from the environment.")
		return
	}
	a.authUser = ""

	if creds, err := loadStoredCredentials(a.authFile); err == nil {
		a.credUser = creds.Username
		a.credHash = []byte(creds.PasswordHash)
		log.Printf("Using saved credentials from %s (change them any time from Settings).", a.authFile)
		return
	}

	a.credMu.Lock()
	a.needsSetup = true
	a.credMu.Unlock()
	log.Printf("No credentials configured yet - open the web UI to finish setup and choose a username/password.")
}

func loadStoredCredentials(path string) (storedCredentials, error) {
	var creds storedCredentials
	data, err := os.ReadFile(path)
	if err != nil {
		return creds, err
	}
	if err := json.Unmarshal(data, &creds); err != nil {
		return creds, err
	}
	if creds.Username == "" || creds.PasswordHash == "" {
		return creds, errors.New("incomplete credentials file")
	}
	return creds, nil
}

// setCredentials hashes and persists a new username/password to auth.json,
// used by both the first-run setup wizard and the account settings form.
func (a *App) setCredentials(username, password string) error {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(storedCredentials{Username: username, PasswordHash: string(hash)}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(a.authFile), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(a.authFile, data, 0o600); err != nil {
		return err
	}
	a.credMu.Lock()
	a.credUser = username
	a.credHash = hash
	a.needsSetup = false
	a.credMu.Unlock()
	return nil
}

func (a *App) isSetupNeeded() bool {
	a.credMu.RLock()
	defer a.credMu.RUnlock()
	return a.needsSetup
}

// checkCredentials verifies a login attempt against whichever credential
// source setupAuth selected: env vars (plain constant-time compare) or a
// saved bcrypt hash.
func (a *App) checkCredentials(username, password string) bool {
	if a.authPass != "" {
		userMatch := subtle.ConstantTimeCompare([]byte(username), []byte(a.authUser)) == 1
		passMatch := subtle.ConstantTimeCompare([]byte(password), []byte(a.authPass)) == 1
		return userMatch && passMatch
	}
	a.credMu.RLock()
	credUser, credHash := a.credUser, a.credHash
	a.credMu.RUnlock()
	if len(credHash) == 0 {
		return false
	}
	if subtle.ConstantTimeCompare([]byte(username), []byte(credUser)) != 1 {
		return false
	}
	return bcrypt.CompareHashAndPassword(credHash, []byte(password)) == nil
}

// isPublicPath lists the handful of routes reachable without a session, so
// the login/setup pages (and the requests that submit them) don't get
// redirected back to themselves.
func isPublicPath(p string) bool {
	switch p {
	case "/login", "/api/login", "/setup", "/api/setup", "/app.css":
		return true
	}
	return false
}

// requireAuth gates every request (UI and API) behind a session cookie set by
// a real login page (see handleLogin), redirecting page loads to /setup or
// /login as appropriate and returning error JSON for API calls when the
// session is missing, expired, or credentials haven't been chosen yet.
func (a *App) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isPublicPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		if a.isSetupNeeded() {
			// The setup wizard's system check needs to run - and be
			// re-run - before any credentials exist, so it can't wait
			// behind a session the user has no way to create yet.
			if r.URL.Path == "/api/system-check" {
				next.ServeHTTP(w, r)
				return
			}
			if strings.HasPrefix(r.URL.Path, "/api/") {
				http.Error(w, "setup required", http.StatusUnauthorized)
				return
			}
			http.Redirect(w, r, "/setup", http.StatusFound)
			return
		}
		if a.validSession(r) {
			next.ServeHTTP(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		http.Redirect(w, r, "/login", http.StatusFound)
	})
}

// createSession mints a new random session token, records its expiry, and
// sets it as an HttpOnly cookie on the response.
func (a *App) createSession(w http.ResponseWriter) {
	var b [32]byte
	_, _ = rand.Read(b[:])
	token := hex.EncodeToString(b[:])
	expiry := time.Now().Add(sessionTTL)
	a.sessMu.Lock()
	a.sessions[token] = expiry
	a.sessMu.Unlock()
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/",
		Expires:  expiry,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

func (a *App) validSession(r *http.Request) bool {
	c, err := r.Cookie(sessionCookieName)
	if err != nil || c.Value == "" {
		return false
	}
	a.sessMu.Lock()
	defer a.sessMu.Unlock()
	expiry, ok := a.sessions[c.Value]
	if !ok {
		return false
	}
	if time.Now().After(expiry) {
		delete(a.sessions, c.Value)
		return false
	}
	return true
}

func (a *App) destroySession(r *http.Request) {
	c, err := r.Cookie(sessionCookieName)
	if err != nil {
		return
	}
	a.sessMu.Lock()
	delete(a.sessions, c.Value)
	a.sessMu.Unlock()
}

// handleLoginPage serves the standalone login page, unauthenticated. Visitors
// who haven't chosen credentials yet are sent to the setup wizard instead.
func (a *App) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if a.isSetupNeeded() {
		http.Redirect(w, r, "/setup", http.StatusFound)
		return
	}
	serveStaticPage(w, r, "static/login.html")
}

// handleLogin verifies the configured credentials (env vars or a saved
// bcrypt hash) and, on success, starts a session so the rest of the app is
// reachable without re-authenticating on every request.
func (a *App) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if a.isSetupNeeded() {
		http.Error(w, "setup required", http.StatusConflict)
		return
	}
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	if !a.checkCredentials(req.Username, req.Password) {
		http.Error(w, "invalid username or password", http.StatusUnauthorized)
		return
	}
	a.createSession(w)
	writeJSON(w, map[string]string{"status": "ok"})
}

func (a *App) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	a.destroySession(r)
	http.SetCookie(w, &http.Cookie{Name: sessionCookieName, Value: "", Path: "/", MaxAge: -1})
	writeJSON(w, map[string]string{"status": "ok"})
}

// handleSetupPage serves the first-run setup wizard. Once credentials exist
// it redirects to /login instead, so the wizard can't be replayed to hijack
// an already-configured instance.
func (a *App) handleSetupPage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !a.isSetupNeeded() {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}
	serveStaticPage(w, r, "static/setup.html")
}

// handleSetup completes first-run setup by saving the chosen username and
// password, then immediately starts a session. Only reachable once, before
// any credentials exist - afterwards use handleAccount to change them.
func (a *App) handleSetup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !a.isSetupNeeded() {
		http.Error(w, "setup has already been completed", http.StatusConflict)
		return
	}
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	req.Username = strings.TrimSpace(req.Username)
	if req.Username == "" || len(req.Password) < 8 {
		http.Error(w, "a username and a password of at least 8 characters are required", http.StatusBadRequest)
		return
	}
	if err := a.setCredentials(req.Username, req.Password); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	a.event("info", fmt.Sprintf("Initial setup completed for user %q", req.Username))
	a.createSession(w)
	writeJSON(w, map[string]string{"status": "ok"})
}

// handleAccount lets a signed-in user view or change their credentials from
// the Settings tab, so nothing ever has to be edited by hand on disk or in
// the environment - unless this deployment pins credentials via
// AUTH_USERNAME/AUTH_PASSWORD, in which case they're read-only here.
func (a *App) handleAccount(w http.ResponseWriter, r *http.Request) {
	managedByEnv := a.authPass != ""
	switch r.Method {
	case http.MethodGet:
		username := a.authUser
		if !managedByEnv {
			a.credMu.RLock()
			username = a.credUser
			a.credMu.RUnlock()
		}
		writeJSON(w, map[string]any{"username": username, "managedByEnv": managedByEnv})
	case http.MethodPost:
		if managedByEnv {
			http.Error(w, "credentials for this deployment are set via AUTH_USERNAME/AUTH_PASSWORD and can't be changed here", http.StatusConflict)
			return
		}
		var req struct {
			CurrentPassword string `json:"currentPassword"`
			Username        string `json:"username"`
			Password        string `json:"password"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		req.Username = strings.TrimSpace(req.Username)
		if req.Username == "" || len(req.Password) < 8 {
			http.Error(w, "a username and a password of at least 8 characters are required", http.StatusBadRequest)
			return
		}
		a.credMu.RLock()
		currentUser := a.credUser
		a.credMu.RUnlock()
		if !a.checkCredentials(currentUser, req.CurrentPassword) {
			http.Error(w, "current password is incorrect", http.StatusUnauthorized)
			return
		}
		if err := a.setCredentials(req.Username, req.Password); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		a.event("info", fmt.Sprintf("Credentials updated for user %q", req.Username))
		writeJSON(w, map[string]string{"status": "ok"})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
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
	mux.HandleFunc("/api/state", a.handleState)
	mux.HandleFunc("/api/system-check", a.handleSystemCheck)
	mux.HandleFunc("/api/config", a.handleConfig)
	mux.HandleFunc("/api/sources", a.handleSources)
	mux.HandleFunc("/api/sources/test", a.handleSourceTest)
	mux.HandleFunc("/api/sources/", a.handleSourceItem)
	mux.HandleFunc("/api/timetable", a.handleTimetable)
	mux.HandleFunc("/api/timetable/favorites", a.handleTimetableFavorites)
	mux.HandleFunc("/api/timetable/lol-events", a.handleTimetableLolEvents)
	mux.HandleFunc("/api/timetable/lol-import", a.handleTimetableLolImport)
	mux.HandleFunc("/api/timetable/lol-unlink", a.handleTimetableLolUnlink)
	mux.HandleFunc("/api/record/", a.handleRecordAction)
	mux.HandleFunc("/api/live/", a.handleLive)
	mux.HandleFunc("/api/recordings", a.handleRecordings)
	mux.HandleFunc("/api/recordings/meta", a.handleRecordingMeta)
	mux.HandleFunc("/api/recordings/match-suggestions", a.handleRecordingMatchSuggestions)
	mux.HandleFunc("/api/events", a.handleLibraryEvents)
	mux.HandleFunc("/api/events/", a.handleLibraryEventItem)
	mux.HandleFunc("/api/festivals", a.handleFestivals)
	mux.HandleFunc("/api/festivals/", a.handleFestivalItem)
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
		a.start(src)
	}
}

func (a *App) start(src Source) {
	a.mu.Lock()
	if _, ok := a.active[src.ID]; ok {
		a.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	started := time.Now()
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
	rec := &recording{source: src, ctx: ctx, cancel: cancel, startedAt: started, tempPath: tempPath, finalPath: finalPath, logPath: logPath, logFile: logFile, done: make(chan struct{}), hlsDir: hlsDir}
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
		a.writeNFO(rec)
		a.backup(rec)
		if failed {
			// Real content was captured before the error hit (e.g. a network
			// drop partway through a long recording) - worth keeping, but
			// flagged rather than reported as a clean finish.
			a.notify(fmt.Sprintf("%s stopped early", rec.source.Name), rec.finalPath)
			a.event("warn", fmt.Sprintf("[%s] saved %s despite an error - it may be incomplete", rec.source.Name, rec.finalPath))
		} else {
			a.notify(fmt.Sprintf("%s finished", rec.source.Name), rec.finalPath)
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

// ffmpegArgs builds the archival output plus, when hlsDir is set, a second
// bounded HLS output used for live-rewind DVR playback of the in-progress
// recording. The HLS branch always transcodes to H.264/AAC since it must be
// playable by hls.js/Safari regardless of what codec the archival copy uses.
func ffmpegArgs(src Source, input, output, hlsDir string, hlsWindowSeconds int) []string {
	args := []string{"-hide_banner", "-y", "-nostdin"}
	if src.HardwareAccel != "" && src.HardwareAccel != "none" {
		args = append(args, "-hwaccel", src.HardwareAccel)
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
	if src.AudioOnly {
		if src.Transcode {
			args = append(args, "-vn", "-c:a", "aac", "-b:a", "192k")
		} else {
			args = append(args, "-vn", "-c:a", "copy")
		}
	} else if src.Transcode {
		args = append(args, "-c:v", videoEncoder(src.HardwareAccel), "-c:a", "aac", "-b:a", "192k")
	} else {
		args = append(args, "-c", "copy")
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
	writeJSON(w, a.state())
}

func (a *App) handleConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		writeJSON(w, a.snapshotConfig())
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
		Type    string `json:"type"`
		URL     string `json:"url"`
		Quality string `json:"quality"`
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
		if strings.HasSuffix(strings.ToLower(p), ".nfo") {
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
		rf := RecordingFile{Name: filepath.Base(p), Source: source, Path: rel, Size: info.Size(), ModTime: info.ModTime(), Channel: source}
		if meta, ok := cfg.RecordingMeta[rel]; ok {
			rf.EventID = meta.EventID
			rf.SetID = meta.SetID
			rf.Artist = meta.Artist
			rf.Start = meta.Start
			rf.End = meta.End
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

// filenameTimestampRe matches this app's own recording naming convention
// (name.YYYYMMDD-HHMMSS.ext) as well as loose YYYY-MM-DD / YYYYMMDD dates
// that commonly show up in filenames from other sources.
var filenameTimestampRe = regexp.MustCompile(`(\d{4})-?(\d{2})-?(\d{2})[ _.T-]?(\d{2})?:?(\d{2})?:?(\d{2})?`)

// guessTimeFromName tries to parse a date/time out of a filename, falling
// back to the file's mtime if nothing looks like a date. Best-effort only -
// it's a starting point for the match wizard's suggestions, not a guarantee.
func guessTimeFromName(name string, modTime time.Time) (time.Time, bool) {
	m := filenameTimestampRe.FindStringSubmatch(name)
	if m == nil {
		return modTime, false
	}
	year, _ := strconv.Atoi(m[1])
	month, _ := strconv.Atoi(m[2])
	day, _ := strconv.Atoi(m[3])
	if year < 2000 || year > 2100 || month < 1 || month > 12 || day < 1 || day > 31 {
		return modTime, false
	}
	hour, min, sec := 0, 0, 0
	if m[4] != "" {
		hour, _ = strconv.Atoi(m[4])
	}
	if m[5] != "" {
		min, _ = strconv.Atoi(m[5])
	}
	if m[6] != "" {
		sec, _ = strconv.Atoi(m[6])
	}
	return time.Date(year, time.Month(month), day, hour, min, sec, 0, modTime.Location()), true
}

// MatchSuggestion is one recording's best-guess event/set match, offered to
// the user for approval or correction rather than applied automatically.
type MatchSuggestion struct {
	Path            string `json:"path"`
	Name            string `json:"name"`
	Channel         string `json:"channel"`
	GuessedTime     string `json:"guessedTime,omitempty"`
	GuessedFromName bool   `json:"guessedFromName"`
	EventID         string `json:"eventId,omitempty"`
	EventName       string `json:"eventName,omitempty"`
	SetID           string `json:"setId,omitempty"`
	Stage           string `json:"stage,omitempty"`
	Artist          string `json:"artist,omitempty"`
	Confidence      string `json:"confidence"` // "high" | "medium" | "low" | "none"
	Reason          string `json:"reason"`
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
		if err != nil || d.IsDir() || strings.HasSuffix(strings.ToLower(p), ".nfo") {
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
		parts := strings.SplitN(rel, "/", 2)
		channel := ""
		if len(parts) > 1 {
			channel = parts[0]
		}
		name := filepath.Base(p)
		guessed, fromName := guessTimeFromName(name, info.ModTime())
		suggestions = append(suggestions, bestMatchSuggestion(cfg, rel, name, channel, guessed, fromName))
		return nil
	})
	sort.Slice(suggestions, func(i, j int) bool { return suggestions[i].Path < suggestions[j].Path })
	writeJSON(w, suggestions)
}

// matchCandidate is one archived timetable set considered as a possible
// match for a recording, scored by bestMatchSuggestion/betterCandidate.
type matchCandidate struct {
	eventID, eventName, setID, stage, artist string
	delta                                    time.Duration
	contained, stageMatch                    bool
}

// bestMatchSuggestion checks every LibraryEvent's archived timetable for the
// set whose stage name best matches this recording's channel and whose
// [start,end) window best contains (or is closest to) the guessed time.
func bestMatchSuggestion(cfg AppConfig, path, name, channel string, guessed time.Time, fromName bool) MatchSuggestion {
	base := MatchSuggestion{Path: path, Name: name, Channel: channel, GuessedFromName: fromName, Confidence: "none", Reason: "Could not find a matching set - assign manually."}
	if fromName {
		base.GuessedTime = guessed.Format(time.RFC3339)
	}

	var best *matchCandidate

	for _, ev := range cfg.LibraryEvents {
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
				contained := !guessed.Before(start) && guessed.Before(end)
				var delta time.Duration
				if contained {
					delta = 0
				} else {
					delta = start.Sub(guessed)
					if delta < 0 {
						delta = -delta
					}
					if end.Sub(guessed) < delta && end.Sub(guessed) > 0 {
						delta = end.Sub(guessed)
					}
				}
				cand := matchCandidate{eventID: ev.ID, eventName: ev.Name, setID: set.ID, stage: stage.Stage, artist: set.Name, delta: delta, contained: contained, stageMatch: stageMatch}
				if best == nil || betterCandidate(cand, *best) {
					c := cand
					best = &c
				}
			}
		}
	}

	if best == nil {
		return base
	}
	base.EventID = best.eventID
	base.EventName = best.eventName
	base.SetID = best.setID
	base.Stage = best.stage
	base.Artist = best.artist
	switch {
	case best.contained && best.stageMatch:
		base.Confidence = "high"
		base.Reason = fmt.Sprintf("%s on %s matches the channel and the guessed time falls within this set's window.", best.artist, best.stage)
	case best.contained:
		base.Confidence = "medium"
		base.Reason = fmt.Sprintf("Guessed time falls within %s's window, but the stage name doesn't match \"%s\" - double check.", best.artist, channel)
	case best.stageMatch && best.delta <= 30*time.Minute:
		base.Confidence = "medium"
		base.Reason = fmt.Sprintf("Channel matches %s; closest set by time (%s away).", best.stage, best.delta.Round(time.Minute))
	default:
		base.Confidence = "low"
		base.Reason = fmt.Sprintf("Closest guess: %s on %s, %s away - verify before approving.", best.artist, best.stage, best.delta.Round(time.Minute))
	}
	return base
}

// betterCandidate prefers a contained+stage-matching set over anything
// else, then contained over not, then stage match over not, then smaller
// time delta.
func betterCandidate(a, b matchCandidate) bool {
	if a.contained != b.contained {
		return a.contained
	}
	if a.stageMatch != b.stageMatch {
		return a.stageMatch
	}
	return a.delta < b.delta
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

// handleLibraryEventTimetable serves and imports the archived timetable for
// one LibraryEvent. POST accepts the same raw JSON shape as dq-timetable.json
// (an array of stage objects with [year,month,day,hour,minute,name] set
// tuples) so a previous year's schedule can be pasted in directly.
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
		tt, err := parseDQTimetableJSON(body)
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
			Path    string `json:"path"`
			EventID string `json:"eventId"`
			Channel string `json:"channel,omitempty"`
			SetID   string `json:"setId,omitempty"`
			Artist  string `json:"artist,omitempty"`
			Start   string `json:"start,omitempty"`
			End     string `json:"end,omitempty"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if strings.TrimSpace(req.Path) == "" || strings.Contains(req.Path, "..") {
			http.Error(w, "a valid path is required", http.StatusBadRequest)
			return
		}
		meta := RecordingMeta{EventID: req.EventID, Channel: req.Channel, SetID: req.SetID, Artist: req.Artist, Start: req.Start, End: req.End}
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

type timetableLolPlannerData struct {
	EventSlug string                     `json:"eventSlug"`
	PlanType  string                     `json:"planType"`
	Title     string                     `json:"title"`
	TimeZone  string                     `json:"timeZone"`
	Data      map[string]timetableLolDay `json:"data"`
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
	writeJSON(w, map[string]any{"events": payload.Events, "attribution": "Timetable data provided by timetable.lol (https://timetable.lol)"})
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

	a.mu.Lock()
	a.cfg.Timetable = schedule
	a.cfg.TimetableSource = link
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
		dateStr := day.Date
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
		out = append(out, StageSchedule{Stage: name, Sets: sets})
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

// combineDateTime combines a day's date with an "HH:MM" wall-clock time in
// the event's timezone. Hours >= 24 (a common festival-timetable convention
// for after-midnight sets) roll over into the next calendar day automatically
// via time.Date's normalization.
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
	return time.Date(base.Year(), base.Month(), base.Day(), h, m, 0, 0, loc), true
}

func (a *App) handleRecordAction(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/record/")
	switch r.Method {
	case http.MethodPost:
		cfg := a.snapshotConfig()
		for _, src := range cfg.Sources {
			if src.ID == id {
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
	return State{Version: version, StartedAt: a.startedAt, Sources: statuses, Events: events, Disk: disk.Scan(cfg.Settings.FinishedDir), Config: cfg, ActiveCount: len(a.active), Warnings: warnings, NowPlaying: now}
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
Recorder: Defqon Stream Recorder %s

%s
`, rec.source.Name, rec.source.Type, rec.source.URL, rec.startedAt.Format(time.RFC3339), time.Now().Format(time.RFC3339), version, rec.source.ExtraNFO))
	_ = os.WriteFile(strings.TrimSuffix(rec.finalPath, filepath.Ext(rec.finalPath))+".nfo", []byte(nfo+"\n"), 0o644)
}

func (a *App) backup(rec *recording) {
	cfg := a.snapshotConfig()
	if !cfg.Settings.Backup.Enabled || !cfg.Settings.Backup.AfterComplete || cfg.Settings.Backup.RcloneRemote == "" {
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

func (a *App) notify(subject, body string) {
	cfg := a.snapshotConfig()
	if cfg.Settings.Notifications.DiscordWebhook != "" {
		payload := strings.NewReader(fmt.Sprintf(`{"content":%q}`, subject+"\n"+body))
		resp, err := http.Post(cfg.Settings.Notifications.DiscordWebhook, "application/json", payload)
		if err == nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
	}
	s := cfg.Settings.Notifications.SMTP
	if s.Enabled && s.Host != "" && s.To != "" {
		if err := sendSMTP(s, subject, body); err != nil {
			a.event("error", fmt.Sprintf("email notification failed: %s", err))
		}
	}
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
	if tt := loadDQTimetable("dq-timetable.json"); len(tt) > 0 {
		cfg.Timetable = tt
		cfg.Sources = sourcesFromTimetable(tt)
	}
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
		UI: UISettings{AppName: "Defqon Stream Recorder", Theme: "midnight", Accent: "red"},
		Sources: []Source{{
			ID:        "red",
			Name:      "RED",
			Type:      "youtube",
			URL:       "https://www.youtube.com/@qdance/live",
			Enabled:   true,
			Record:    false,
			Quality:   "best",
			Container: "mkv",
			Color:     "#ef4444",
		}},
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
		cfg.UI.AppName = "Defqon Stream Recorder"
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

func loadDQTimetable(path string) []StageSchedule {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	tt, err := parseDQTimetableJSON(data)
	if err != nil {
		return nil
	}
	return tt
}

// parseDQTimetableJSON parses the raw per-stage timetable JSON shape used by
// dq-timetable.json (and, by extension, any archived timetable pasted into a
// LibraryEvent): an array of stage objects whose "sets" are
// [year, month, day, hour, minute, name?] tuples. A row with no name marks
// only the end time of the previous set.
func parseDQTimetableJSON(data []byte) ([]StageSchedule, error) {
	var raw []rawStage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	var out []StageSchedule
	for _, stage := range raw {
		var sets []ScheduleSet
		var last *ScheduleSet
		for _, row := range stage.Sets {
			if len(row) < 5 {
				continue
			}
			start := fmt.Sprintf("%04d-%02d-%02dT%02d:%02d:00+02:00", toInt(row[0]), toInt(row[1]), toInt(row[2]), toInt(row[3]), toInt(row[4]))
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

func sourcesFromTimetable(tt []StageSchedule) []Source {
	colors := map[string]string{"RED": "#ef4444", "BLUE": "#3b82f6", "BLACK": "#a3a3a3", "UV": "#a855f7", "MAGENTA": "#d946ef", "YELLOW": "#eab308", "ORANGE": "#f97316", "GREEN": "#22c55e"}
	var srcs []Source
	for _, st := range tt {
		if st.URL == "" {
			continue
		}
		typ := "http"
		if strings.Contains(st.URL, "youtube") || strings.Contains(st.URL, "youtu.be") {
			typ = "youtube"
		}
		if strings.Contains(st.URL, "twitch.tv") || strings.Contains(st.URL, "mixlr.com") {
			typ = "twitch"
		}
		srcs = append(srcs, Source{ID: strings.ToLower(safeName(st.Stage)), Name: st.Stage, Type: typ, URL: st.URL, Enabled: true, Record: false, Quality: "best", Container: "mkv", Color: colors[st.Stage]})
	}
	return srcs
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
