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

	"defqon-stream-recorder/internal/disk"
)

var version = "dev"

//go:embed static/*
var staticFiles embed.FS

type AppConfig struct {
	Settings        Settings        `json:"settings"`
	UI              UISettings      `json:"ui"`
	Sources         []Source        `json:"sources"`
	Timetable       []StageSchedule `json:"timetable"`
	TimetableSource *TimetableLink  `json:"timetableSource,omitempty"`
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
	AppName   string `json:"appName"`
	LogoURL   string `json:"logoUrl"`
	Theme     string `json:"theme"`
	Accent    string `json:"accent"`
	CustomCSS string `json:"customCss"`
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
}

// RecordingFile describes a single finished recording on disk.
type RecordingFile struct {
	Name    string    `json:"name"`
	Source  string    `json:"source"`
	Path    string    `json:"path"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"modTime"`
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
	cmds      []*exec.Cmd
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
	remindersSent map[string]time.Time
}

func main() {
	configPath := env("CONFIG_PATH", "/app/config/config.json")
	if runtime.GOOS == "windows" && strings.HasPrefix(configPath, "/app/") {
		configPath = filepath.Join(".", strings.TrimPrefix(configPath, "/app/"))
	} else if runtime.GOOS == "windows" && strings.HasPrefix(configPath, "/data/") {
		configPath = filepath.Join(".", strings.TrimPrefix(configPath, "/data/"))
	}
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
	}
	for _, dir := range []string{cfg.Settings.FinishedDir, cfg.Settings.TempDir, cfg.Settings.LogDir, filepath.Dir(configPath)} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	app.event("info", "Recorder started")
	return app, nil
}

// setupAuth loads AUTH_USERNAME/AUTH_PASSWORD from the environment. If no
// password is configured, a random one is generated and printed once so the
// UI is never reachable without credentials, even on a bare first run.
func (a *App) setupAuth() {
	a.authUser = env("AUTH_USERNAME", "admin")
	a.authPass = os.Getenv("AUTH_PASSWORD")
	if a.authPass == "" {
		var b [12]byte
		_, _ = rand.Read(b[:])
		a.authPass = hex.EncodeToString(b[:])
		log.Printf("AUTH_PASSWORD not set - generated a one-time password for this run.")
		log.Printf("Login with username %q and password %q (set AUTH_USERNAME/AUTH_PASSWORD to persist credentials).", a.authUser, a.authPass)
	}
}

// requireAuth gates every request (UI and API) behind HTTP Basic Auth.
func (a *App) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		userMatch := subtle.ConstantTimeCompare([]byte(user), []byte(a.authUser)) == 1
		passMatch := subtle.ConstantTimeCompare([]byte(pass), []byte(a.authPass)) == 1
		if !ok || !userMatch || !passMatch {
			w.Header().Set("WWW-Authenticate", `Basic realm="Defqon Stream Recorder", charset="UTF-8"`)
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
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
	mux.HandleFunc("/api/state", a.handleState)
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

func (a *App) runRecording(rec *recording) {
	defer close(rec.done)
	defer rec.logFile.Close()

	err := a.execute(rec)
	if err != nil && rec.ctx.Err() == nil {
		rec.lastErr = err.Error()
		a.event("error", fmt.Sprintf("[%s] %s", rec.source.Name, err))
	}
	rec.cancel()
	if info, statErr := os.Stat(rec.tempPath); statErr == nil && info.Size() > 0 {
		_ = os.MkdirAll(filepath.Dir(rec.finalPath), 0o755)
		if err := os.Rename(rec.tempPath, rec.finalPath); err != nil {
			_ = copyFile(rec.tempPath, rec.finalPath)
			_ = os.Remove(rec.tempPath)
		}
		a.writeNFO(rec)
		a.backup(rec)
		a.notify(fmt.Sprintf("%s finished", rec.source.Name), rec.finalPath)
		a.event("info", fmt.Sprintf("[%s] saved %s", rec.source.Name, rec.finalPath))
		a.mu.Lock()
		a.lastFinished[rec.source.ID] = rec.finalPath
		a.mu.Unlock()
	} else if rec.ctx.Err() == nil {
		a.event("warn", fmt.Sprintf("[%s] no output produced", rec.source.Name))
	}

	if rec.hlsDir != "" {
		_ = os.RemoveAll(rec.hlsDir)
	}

	a.mu.Lock()
	delete(a.active, rec.source.ID)
	a.mu.Unlock()
}

func (a *App) execute(rec *recording) error {
	src := rec.source
	windowSeconds := a.snapshotConfig().Settings.LiveRewindWindowSeconds
	if src.Type == "http" {
		args := ffmpegArgs(src, src.URL, rec.tempPath, rec.hlsDir, windowSeconds)
		cmd := exec.CommandContext(rec.ctx, "ffmpeg", args...)
		rec.cmds = []*exec.Cmd{cmd}
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
	rec.cmds = []*exec.Cmd{streamlink, ffmpeg}

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
	slErr := streamlink.Wait()
	ffErr := ffmpeg.Wait()
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

	args = append(args, "-map", "0")
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
		args = append(args, "-map", "0")
		if src.AudioOnly {
			args = append(args, "-vn", "-c:a", "aac", "-b:a", "160k")
		} else {
			args = append(args, "-c:v", videoEncoder(src.HardwareAccel), "-preset", "veryfast", "-c:a", "aac", "-b:a", "160k")
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

func (a *App) stop(id string) {
	a.mu.RLock()
	rec := a.active[id]
	a.mu.RUnlock()
	if rec == nil {
		return
	}
	rec.cancel()
	for _, cmd := range rec.cmds {
		if cmd.Process != nil {
			_ = cmd.Process.Signal(os.Interrupt)
			time.AfterFunc(5*time.Second, func() {
				if cmd.Process != nil {
					_ = cmd.Process.Kill()
				}
			})
		}
	}
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
		a.stop(id)
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
	root := filepath.Clean(a.snapshotConfig().Settings.FinishedDir)
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
		files = append(files, RecordingFile{Name: filepath.Base(p), Source: source, Path: rel, Size: info.Size(), ModTime: info.ModTime()})
		return nil
	})
	sort.Slice(files, func(i, j int) bool { return files[i].ModTime.After(files[j].ModTime) })
	writeJSON(w, files)
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
	w.Header().Set("Cache-Control", "no-store")
	http.ServeFile(w, r, filepath.Join(rec.hlsDir, file))
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
	warnings := freeWarnings(cfg)
	return State{Version: version, StartedAt: a.startedAt, Sources: statuses, Events: append([]Event(nil), a.events...), Disk: disk.Scan(cfg.Settings.FinishedDir), Config: cfg, ActiveCount: len(a.active), Warnings: warnings, NowPlaying: now}
}

func freeWarnings(cfg AppConfig) []string {
	usage := disk.Scan(cfg.Settings.FinishedDir)
	var warnings []string
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
			FinishedDir:             env("FINISHED_DIR", "/data/recordings"),
			TempDir:                 env("TEMP_DIR", "/data/incomplete"),
			LogDir:                  env("LOG_DIR", "/data/logs"),
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
		cfg.Settings.FinishedDir = "/data/recordings"
	}
	if cfg.Settings.TempDir == "" {
		cfg.Settings.TempDir = "/data/incomplete"
	}
	if cfg.Settings.LogDir == "" {
		cfg.Settings.LogDir = "/data/logs"
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
	assignScheduleIDs(cfg.Timetable)
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
	var raw []rawStage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil
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
	return out
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
