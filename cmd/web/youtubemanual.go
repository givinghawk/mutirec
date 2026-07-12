package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ============================================================================
// Manual, one-off YouTube upload of an already-finished recording - as
// opposed to uploadYouTube in youtube.go, which fires automatically right
// after a recording finishes for a source with auto-upload enabled. Reuses
// the same youtubeUploadFile call, just triggered on demand (e.g. from the
// recording player) and tracked as a background job so the page doesn't
// have to hold a request open for a large upload.
// ============================================================================

// YouTubeUploadJob tracks one in-progress or finished manual upload.
type YouTubeUploadJob struct {
	mu sync.Mutex

	id         string
	path       string
	status     string // "running" | "done" | "error"
	startedAt  time.Time
	finishedAt time.Time
	videoURL   string
	errMsg     string
}

// YouTubeUploadJobView is the JSON-safe snapshot of a YouTubeUploadJob.
type YouTubeUploadJobView struct {
	ID         string     `json:"id"`
	Path       string     `json:"path"`
	Status     string     `json:"status"`
	StartedAt  time.Time  `json:"startedAt"`
	FinishedAt *time.Time `json:"finishedAt,omitempty"`
	VideoURL   string     `json:"videoUrl,omitempty"`
	Error      string     `json:"error,omitempty"`
}

func (j *YouTubeUploadJob) view() YouTubeUploadJobView {
	j.mu.Lock()
	defer j.mu.Unlock()
	v := YouTubeUploadJobView{ID: j.id, Path: j.path, Status: j.status, StartedAt: j.startedAt, VideoURL: j.videoURL, Error: j.errMsg}
	if !j.finishedAt.IsZero() {
		v.FinishedAt = &j.finishedAt
	}
	return v
}

func (j *YouTubeUploadJob) finish(videoURL string, err error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.finishedAt = time.Now()
	if err != nil {
		j.status = "error"
		j.errMsg = err.Error()
		return
	}
	j.status = "done"
	j.videoURL = videoURL
}

func (a *App) putYouTubeUploadJob(job *YouTubeUploadJob) {
	a.ytUploadJobsMu.Lock()
	defer a.ytUploadJobsMu.Unlock()
	if len(a.ytUploadJobs) > 50 {
		oldest, oldestID := time.Now(), ""
		for id, j := range a.ytUploadJobs {
			j.mu.Lock()
			t := j.startedAt
			j.mu.Unlock()
			if t.Before(oldest) {
				oldest, oldestID = t, id
			}
		}
		if oldestID != "" {
			delete(a.ytUploadJobs, oldestID)
		}
	}
	a.ytUploadJobs[job.id] = job
}

func (a *App) getYouTubeUploadJob(id string) (*YouTubeUploadJob, bool) {
	a.ytUploadJobsMu.Lock()
	defer a.ytUploadJobsMu.Unlock()
	j, ok := a.ytUploadJobs[id]
	return j, ok
}

// handleRecordingYouTubeUpload starts a one-off upload of an already-
// finished recording to YouTube, using whatever RecordingMeta is known
// about it (artist/channel) to build a default title unless one is given.
func (a *App) handleRecordingYouTubeUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !a.isAdminReq(r) {
		http.Error(w, "admin only", http.StatusForbidden)
		return
	}
	var req struct {
		Path    string `json:"path"`
		Title   string `json:"title,omitempty"`
		Privacy string `json:"privacy,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	relPath := filepath.ToSlash(strings.TrimSpace(req.Path))
	if relPath == "" || strings.Contains(relPath, "..") {
		writeJSON(w, map[string]any{"ok": false, "error": "a valid path is required"})
		return
	}
	abs, ok := a.resolveRecordingPath(relPath)
	if !ok {
		writeJSON(w, map[string]any{"ok": false, "error": "invalid path"})
		return
	}
	cfg := a.snapshotConfig()
	if !youtubeConfigured(cfg.Settings.YouTube) {
		writeJSON(w, map[string]any{"ok": false, "error": "YouTube isn't configured yet - set it up in Settings -> YouTube Auto-Upload first"})
		return
	}

	title := strings.TrimSpace(req.Title)
	if title == "" {
		title = recordingUploadTitle(cfg, relPath)
	}
	description := fmt.Sprintf("Uploaded from MutiRec\nOriginal file: %s", filepath.Base(abs))
	privacy := youtubePrivacyOrDefault(req.Privacy)

	job := &YouTubeUploadJob{id: newID(), path: relPath, status: "running", startedAt: time.Now()}
	a.putYouTubeUploadJob(job)
	a.event("info", "Started manual YouTube upload of "+relPath)
	go func() {
		token, err := youtubeAccessToken(cfg.Settings.YouTube)
		if err != nil {
			wrapped := fmt.Errorf("could not refresh access token: %w", err)
			job.finish("", wrapped)
			a.event("error", "YouTube upload failed for "+relPath+": "+wrapped.Error())
			return
		}
		videoURL, err := youtubeUploadFile(token, abs, title, description, privacy)
		if err != nil {
			job.finish("", err)
			a.event("error", "YouTube upload failed for "+relPath+": "+err.Error())
			return
		}
		job.finish(videoURL, nil)
		a.event("info", "Uploaded to YouTube ("+privacy+"): "+videoURL)
	}()
	writeJSON(w, map[string]any{"ok": true, "jobId": job.id})
}

// handleRecordingYouTubeUploadJobItem returns one manual upload job's
// current status by ID, for polling.
func (a *App) handleRecordingYouTubeUploadJobItem(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/recordings/youtube-upload/jobs/")
	job, ok := a.getYouTubeUploadJob(id)
	if !ok {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, job.view())
}

// recordingUploadTitle builds a reasonable default title for a manual
// upload from whatever RecordingMeta is known about the file (artist and/or
// channel), falling back to the file's own name.
func recordingUploadTitle(cfg AppConfig, relPath string) string {
	if meta, ok := cfg.RecordingMeta[relPath]; ok {
		var parts []string
		if meta.Artist != "" {
			parts = append(parts, meta.Artist)
		}
		if meta.Channel != "" {
			parts = append(parts, meta.Channel)
		}
		if len(parts) > 0 {
			return strings.Join(parts, " - ")
		}
	}
	name := filepath.Base(relPath)
	return strings.TrimSuffix(name, filepath.Ext(name))
}
