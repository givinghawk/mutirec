package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ============================================================================
// Fetch-from-URL: downloads a file (or a whole shared folder) straight into
// the file explorer tree from a direct link or a public share link, as a
// background job so a large download doesn't need a browser tab left open.
//
// Specifically supports the ownCloud/Nextcloud "public share" URL
// convention - TransIP Stack (like a number of other self-hosted file-share
// products) is built on it - where a share link such as
// https://host/s/<token> or https://host/index.php/s/<token> serves the
// share's landing page at that URL, but appending "/download" to it returns
// the actual file (or, for a shared folder, a zip of the whole thing) and,
// for a password-protected share, HTTP Basic auth with the share token as
// the username and the share password as the password. Any other URL is
// just downloaded directly (with optional HTTP Basic auth, which covers
// most other authenticated direct-download links generically).
// ============================================================================

// URLFetchJob tracks one in-progress or finished fetch-from-URL download.
// All fields are guarded by mu; use view() for a JSON-safe snapshot.
type URLFetchJob struct {
	mu sync.Mutex

	id         string
	sourceURL  string
	destName   string
	status     string // "running" | "done" | "error"
	startedAt  time.Time
	finishedAt time.Time

	totalBytes, transferredBytes int64
	speedBps                     float64
	errMsg                       string
	log                          []ShareJobLogLine

	lastSampleAt    time.Time
	lastSampleBytes int64
}

// URLFetchJobView is the JSON-safe snapshot of a URLFetchJob.
type URLFetchJobView struct {
	ID               string            `json:"id"`
	SourceURL        string            `json:"sourceUrl"`
	DestName         string            `json:"destName"`
	Status           string            `json:"status"`
	StartedAt        time.Time         `json:"startedAt"`
	FinishedAt       *time.Time        `json:"finishedAt,omitempty"`
	TotalBytes       int64             `json:"totalBytes"`
	TransferredBytes int64             `json:"transferredBytes"`
	SpeedBps         float64           `json:"speedBps"`
	Error            string            `json:"error,omitempty"`
	Log              []ShareJobLogLine `json:"log"`
}

func (j *URLFetchJob) view() URLFetchJobView {
	j.mu.Lock()
	defer j.mu.Unlock()
	v := URLFetchJobView{
		ID: j.id, SourceURL: j.sourceURL, DestName: j.destName, Status: j.status,
		StartedAt: j.startedAt, TotalBytes: j.totalBytes, TransferredBytes: j.transferredBytes,
		SpeedBps: j.speedBps, Error: j.errMsg, Log: append([]ShareJobLogLine(nil), j.log...),
	}
	if !j.finishedAt.IsZero() {
		v.FinishedAt = &j.finishedAt
	}
	return v
}

func (j *URLFetchJob) logf(format string, args ...any) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.log = append(j.log, ShareJobLogLine{Time: time.Now().Format("15:04:05"), Text: fmt.Sprintf(format, args...)})
	if len(j.log) > 500 {
		j.log = j.log[len(j.log)-500:]
	}
}

func (j *URLFetchJob) addBytes(n int64) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.transferredBytes += n
	now := time.Now()
	if j.lastSampleAt.IsZero() {
		j.lastSampleAt, j.lastSampleBytes = now, j.transferredBytes
		return
	}
	if elapsed := now.Sub(j.lastSampleAt); elapsed >= 250*time.Millisecond {
		j.speedBps = float64(j.transferredBytes-j.lastSampleBytes) / elapsed.Seconds()
		j.lastSampleAt, j.lastSampleBytes = now, j.transferredBytes
	}
}

func (j *URLFetchJob) finish(err error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.finishedAt = time.Now()
	j.speedBps = 0
	if err != nil {
		j.status = "error"
		j.errMsg = err.Error()
	} else {
		j.status = "done"
	}
}

func (a *App) putFetchJob(job *URLFetchJob) {
	a.fetchJobsMu.Lock()
	defer a.fetchJobsMu.Unlock()
	if len(a.fetchJobs) > 50 {
		oldest, oldestID := time.Now(), ""
		for id, j := range a.fetchJobs {
			j.mu.Lock()
			t := j.startedAt
			j.mu.Unlock()
			if t.Before(oldest) {
				oldest, oldestID = t, id
			}
		}
		if oldestID != "" {
			delete(a.fetchJobs, oldestID)
		}
	}
	a.fetchJobs[job.id] = job
}

func (a *App) getFetchJob(id string) (*URLFetchJob, bool) {
	a.fetchJobsMu.Lock()
	defer a.fetchJobsMu.Unlock()
	j, ok := a.fetchJobs[id]
	return j, ok
}

// handleExplorerFetchJobItem returns one fetch job's current status by ID,
// for polling.
func (a *App) handleExplorerFetchJobItem(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !a.isAdminReq(r) {
		http.Error(w, "admin only", http.StatusForbidden)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/explorer/fetch/jobs/")
	job, ok := a.getFetchJob(id)
	if !ok {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, job.view())
}

// looksLikeOwncloudShare reports whether a URL looks like an ownCloud/
// Nextcloud (and compatible - including TransIP Stack) public share link,
// i.e. its path contains an "/s/<token>" segment.
func looksLikeOwncloudShare(u *url.URL) (token string, ok bool) {
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	for i, p := range parts {
		if p == "s" && i+1 < len(parts) && parts[i+1] != "" {
			return parts[i+1], true
		}
	}
	return "", false
}

// handleExplorerFetchURL starts a background download of a direct link or a
// public share link (see looksLikeOwncloudShare) into a destination
// directory under the explorer root.
func (a *App) handleExplorerFetchURL(w http.ResponseWriter, r *http.Request) {
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
		Password string `json:"password"`
		Path     string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	req.URL = strings.TrimSpace(req.URL)
	u, err := url.Parse(req.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		writeJSON(w, map[string]any{"ok": false, "error": "enter a full URL including http:// or https://"})
		return
	}
	root := a.explorerRoot(a.snapshotConfig())
	destDir, err := resolveExplorerPath(root, req.Path)
	if err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
		return
	}

	job := &URLFetchJob{id: newID(), sourceURL: req.URL, status: "running", startedAt: time.Now()}
	a.putFetchJob(job)
	a.event("info", "Started URL fetch job "+job.id+" for "+req.URL)
	go a.runURLFetchJob(job, req.URL, req.Password, destDir, root)
	writeJSON(w, map[string]any{"ok": true, "jobId": job.id})
}

// runURLFetchJob is the background worker started by handleExplorerFetchURL.
func (a *App) runURLFetchJob(job *URLFetchJob, rawURL, password, destDir, root string) {
	client := &http.Client{Transport: &http.Transport{
		ResponseHeaderTimeout: responseHeaderTimeout,
	}}

	fetchURL := rawURL
	var basicUser, basicPass string
	if u, err := url.Parse(rawURL); err == nil {
		if token, ok := looksLikeOwncloudShare(u); ok {
			job.logf("Detected an ownCloud/Nextcloud-style share link (token %s) - requesting the direct download", shortHash(token))
			fetchURL = strings.TrimRight(rawURL, "/") + "/download"
			if password != "" {
				basicUser, basicPass = token, password
			}
		}
	}

	job.logf("Fetching %s", fetchURL)
	getReq, err := http.NewRequest(http.MethodGet, fetchURL, nil)
	if err != nil {
		job.logf("Invalid URL: %s", err)
		job.finish(err)
		return
	}
	if basicUser != "" {
		getReq.SetBasicAuth(basicUser, basicPass)
	}
	resp, err := client.Do(getReq)
	if err != nil {
		job.logf("Could not reach that URL: %s", err)
		job.finish(err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		err := fmt.Errorf("that link needs a password (or the one given was wrong) - HTTP %d", resp.StatusCode)
		job.logf("%s", err)
		job.finish(err)
		return
	}
	if resp.StatusCode != http.StatusOK {
		err := fmt.Errorf("the server returned HTTP %d", resp.StatusCode)
		job.logf("%s", err)
		job.finish(err)
		return
	}

	name := urlDownloadDest(resp, fetchURL)
	job.mu.Lock()
	job.destName = name
	job.mu.Unlock()
	if resp.ContentLength > 0 {
		job.mu.Lock()
		job.totalBytes = resp.ContentLength
		job.mu.Unlock()
	}

	dest := filepath.Join(destDir, name)
	tmp := dest + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		job.logf("Could not create destination file: %s", err)
		job.finish(err)
		return
	}
	job.logf("Saving to %s", name)
	var w io.Writer = f
	w = io.MultiWriter(f, progressWriter(job.addBytes))
	_, copyErr := io.Copy(w, resp.Body)
	f.Close()
	if copyErr != nil {
		_ = os.Remove(tmp)
		job.logf("Download failed: %s", copyErr)
		job.finish(copyErr)
		return
	}
	if err := os.Rename(tmp, dest); err != nil {
		job.logf("Could not finalize download: %s", err)
		job.finish(err)
		return
	}
	job.logf("Saved %s (%s)", name, formatBytesGo(job.view().TransferredBytes))

	if strings.EqualFold(filepath.Ext(name), ".zip") {
		extractTo := uniqueSiblingDir(strings.TrimSuffix(dest, filepath.Ext(dest)))
		job.logf("Extracting %s...", name)
		if err := extractZip(dest, extractTo, root); err != nil {
			job.logf("Could not auto-extract the zip (left as-is): %s", err)
		} else {
			job.logf("Extracted into %s", filepath.Base(extractTo))
		}
	}

	job.finish(nil)
}
