package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ============================================================================
// YouTube (and, incidentally, anywhere else yt-dlp supports) video/playlist
// downloads straight into the file explorer tree, as a background job
// queued through the same shared worker pool as Fetch-from-URL (see
// downloadqueue.go). Shells out to the yt-dlp binary rather than
// reimplementing extraction, matching this codebase's existing approach of
// shelling out to streamlink/ffmpeg for anything protocol-heavy.
// ============================================================================

// YouTubeDownloadJob tracks one in-progress or finished yt-dlp download. All
// fields are guarded by mu; use view() for a JSON-safe snapshot.
type YouTubeDownloadJob struct {
	mu sync.Mutex

	id         string
	sourceURL  string
	destName   string
	status     string // "queued" | "running" | "done" | "error"
	startedAt  time.Time
	finishedAt time.Time

	format   string // yt-dlp -f selector; empty means yt-dlp's own default
	playlist bool   // download the whole playlist a URL belongs to, not just one video
	proxyURL string

	progressPct  float64
	progressLine string
	errMsg       string
	log          []ShareJobLogLine

	lastLogAt time.Time
}

// YouTubeDownloadJobView is the JSON-safe snapshot of a YouTubeDownloadJob.
type YouTubeDownloadJobView struct {
	ID           string            `json:"id"`
	SourceURL    string            `json:"sourceUrl"`
	DestName     string            `json:"destName"`
	Status       string            `json:"status"`
	StartedAt    time.Time         `json:"startedAt"`
	FinishedAt   *time.Time        `json:"finishedAt,omitempty"`
	ProgressPct  float64           `json:"progressPct"`
	ProgressLine string            `json:"progressLine"`
	Error        string            `json:"error,omitempty"`
	Log          []ShareJobLogLine `json:"log"`
}

func (j *YouTubeDownloadJob) view() YouTubeDownloadJobView {
	j.mu.Lock()
	defer j.mu.Unlock()
	v := YouTubeDownloadJobView{
		ID: j.id, SourceURL: j.sourceURL, DestName: j.destName, Status: j.status,
		StartedAt: j.startedAt, ProgressPct: j.progressPct, ProgressLine: j.progressLine,
		Error: j.errMsg, Log: append([]ShareJobLogLine(nil), j.log...),
	}
	if !j.finishedAt.IsZero() {
		v.FinishedAt = &j.finishedAt
	}
	return v
}

func (j *YouTubeDownloadJob) logf(format string, args ...any) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.log = append(j.log, ShareJobLogLine{Time: time.Now().Format("15:04:05"), Text: fmt.Sprintf(format, args...)})
	if len(j.log) > 500 {
		j.log = j.log[len(j.log)-500:]
	}
}

func (j *YouTubeDownloadJob) finish(err error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.finishedAt = time.Now()
	if err != nil {
		j.status = "error"
		j.errMsg = err.Error()
	} else {
		j.status = "done"
	}
}

func (a *App) putYTJob(job *YouTubeDownloadJob) {
	a.ytJobsMu.Lock()
	defer a.ytJobsMu.Unlock()
	if len(a.ytJobs) > 50 {
		oldest, oldestID := time.Now(), ""
		for id, j := range a.ytJobs {
			j.mu.Lock()
			t := j.startedAt
			j.mu.Unlock()
			if t.Before(oldest) {
				oldest, oldestID = t, id
			}
		}
		if oldestID != "" {
			delete(a.ytJobs, oldestID)
		}
	}
	a.ytJobs[job.id] = job
}

func (a *App) getYTJob(id string) (*YouTubeDownloadJob, bool) {
	a.ytJobsMu.Lock()
	defer a.ytJobsMu.Unlock()
	j, ok := a.ytJobs[id]
	return j, ok
}

// handleExplorerYouTubeDownload starts a queued yt-dlp download into a
// destination directory under the explorer root.
func (a *App) handleExplorerYouTubeDownload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !a.isAdminReq(r) {
		http.Error(w, "admin only", http.StatusForbidden)
		return
	}
	var req struct {
		URL      string `json:"url"`
		Path     string `json:"path"`
		Format   string `json:"format"`
		Playlist bool   `json:"playlist"`
		UseProxy bool   `json:"useProxy"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	req.URL = strings.TrimSpace(req.URL)
	if req.URL == "" {
		writeJSON(w, map[string]any{"ok": false, "error": "enter a video/playlist URL first"})
		return
	}
	ytdlpPath, err := exec.LookPath(ytDlpBinary)
	if err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": "yt-dlp isn't installed in this container/host - it ships in the official Docker image; if running outside Docker, install yt-dlp and make sure it's on PATH"})
		return
	}

	cfg := a.snapshotConfig()
	root := a.explorerRoot(cfg)
	destDir, err := resolveExplorerPath(root, req.Path)
	if err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
		return
	}

	proxyURL := ""
	if req.UseProxy {
		proxyURL = cfg.Settings.Sharing.ProxyURL
		if proxyURL == "" {
			writeJSON(w, map[string]any{"ok": false, "error": "no proxy is configured - set one in Settings -> Peer Sharing (P2P) first"})
			return
		}
	}

	job := &YouTubeDownloadJob{
		id: newID(), sourceURL: req.URL, status: "queued", startedAt: time.Now(),
		format: strings.TrimSpace(req.Format), playlist: req.Playlist, proxyURL: proxyURL,
	}
	a.putYTJob(job)
	a.event("info", "Queued YouTube download job "+job.id+" for "+req.URL)
	a.enqueueDownload(func() {
		job.mu.Lock()
		job.status = "running"
		job.startedAt = time.Now()
		job.mu.Unlock()
		a.runYouTubeDownloadJob(job, ytdlpPath, req.URL, destDir)
	})
	writeJSON(w, map[string]any{"ok": true, "jobId": job.id})
}

// handleExplorerYouTubeJobItem returns one YouTube download job's current
// status by ID, for polling.
func (a *App) handleExplorerYouTubeJobItem(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !a.isAdminReq(r) {
		http.Error(w, "admin only", http.StatusForbidden)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/explorer/youtube/jobs/")
	job, ok := a.getYTJob(id)
	if !ok {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, job.view())
}

// ytDlpBinary is the executable name looked up on PATH - a package-level
// var (not a const) purely so a test can point it at a fake script.
var ytDlpBinary = "yt-dlp"

// ytdlpProgressLine matches yt-dlp's "--newline" progress output, e.g.
// "[download]  42.0% of   10.00MiB at    1.20MiB/s ETA 00:05".
var ytdlpProgressLine = regexp.MustCompile(`^\[download\]\s+([0-9.]+)%`)

// ytdlpProgressLogInterval throttles how often a progress line gets written
// to the job's log (every line still updates the numeric/text progress
// fields) - otherwise a fast download logs hundreds of near-identical lines.
const ytdlpProgressLogInterval = 2 * time.Second

// runYouTubeDownloadJob is the background worker started by
// handleExplorerYouTubeDownload. It shells out to yt-dlp with --newline so
// each progress update is its own line, and --print
// "after_move:filepath" so it can tell exactly which file(s) were produced
// (works for both a single video and a whole playlist) without having to
// guess yt-dlp's output-template naming itself.
func (a *App) runYouTubeDownloadJob(job *YouTubeDownloadJob, ytdlpPath, rawURL, destDir string) {
	outTemplate := filepath.Join(destDir, "%(title).200B [%(id)s].%(ext)s")
	args := []string{"--newline", "--no-colors", "--no-part", "-o", outTemplate}
	if !job.playlist {
		args = append(args, "--no-playlist")
	}
	if job.format != "" {
		args = append(args, "-f", job.format)
	}
	if job.proxyURL != "" {
		args = append(args, "--proxy", job.proxyURL)
	}
	args = append(args, "--print", "after_move:filepath", rawURL)

	job.logf("Running yt-dlp for %s", rawURL)
	cmd := exec.Command(ytdlpPath, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		job.logf("Could not start yt-dlp: %s", err)
		job.finish(err)
		return
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		job.logf("Could not start yt-dlp: %s", err)
		job.finish(err)
		return
	}
	if err := cmd.Start(); err != nil {
		job.logf("Could not start yt-dlp: %s", err)
		job.finish(err)
		return
	}

	var producedFiles []string
	var stderrTail []string
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				continue
			}
			if m := ytdlpProgressLine.FindStringSubmatch(line); m != nil {
				pct, _ := strconv.ParseFloat(m[1], 64)
				job.mu.Lock()
				job.progressPct = pct
				job.progressLine = line
				shouldLog := time.Since(job.lastLogAt) >= ytdlpProgressLogInterval
				if shouldLog {
					job.lastLogAt = time.Now()
				}
				job.mu.Unlock()
				if shouldLog {
					job.logf("%s", line)
				}
				continue
			}
			if strings.HasPrefix(line, "[") || strings.HasPrefix(line, "Deleting") {
				// A yt-dlp status line ([youtube], [info], [Merger], ...) -
				// worth keeping in the job log for troubleshooting.
				job.logf("%s", line)
				continue
			}
			// Anything else on stdout at this point is a line printed by
			// --print after_move:filepath: the absolute path yt-dlp just
			// finished writing.
			producedFiles = append(producedFiles, line)
			job.logf("Produced %s", filepath.Base(line))
		}
	}()
	go func() {
		defer wg.Done()
		scanner := bufio.NewScanner(stderr)
		scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				continue
			}
			job.logf("%s", line)
			stderrTail = append(stderrTail, line)
			if len(stderrTail) > 10 {
				stderrTail = stderrTail[len(stderrTail)-10:]
			}
		}
	}()

	wg.Wait()
	waitErr := cmd.Wait()
	if waitErr != nil {
		msg := waitErr.Error()
		if len(stderrTail) > 0 {
			msg = stderrTail[len(stderrTail)-1]
		}
		err := fmt.Errorf("yt-dlp failed: %s", msg)
		job.finish(err)
		return
	}

	if len(producedFiles) == 0 {
		job.logf("yt-dlp finished but reported no output files")
		job.finish(nil)
		return
	}

	for _, f := range producedFiles {
		a.verifyDownloadHash(job.logf, f)
	}
	job.mu.Lock()
	job.destName = filepath.Base(producedFiles[len(producedFiles)-1])
	job.progressPct = 100
	job.mu.Unlock()
	if len(producedFiles) == 1 {
		job.logf("Saved %s", job.destName)
	} else {
		job.logf("Saved %d files", len(producedFiles))
	}
	job.finish(nil)
}
