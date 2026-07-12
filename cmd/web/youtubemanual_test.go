package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRecordingUploadTitle(t *testing.T) {
	cfg := AppConfig{RecordingMeta: map[string]RecordingMeta{
		"a/b.mkv": {Artist: "DJ Vertex", Channel: "Main Stage"},
		"c/d.mkv": {Artist: "DJ Isaac"},
	}}
	if got := recordingUploadTitle(cfg, "a/b.mkv"); got != "DJ Vertex - Main Stage" {
		t.Fatalf("got %q", got)
	}
	if got := recordingUploadTitle(cfg, "c/d.mkv"); got != "DJ Isaac" {
		t.Fatalf("got %q", got)
	}
	if got := recordingUploadTitle(cfg, "unknown/file.mkv"); got != "file" {
		t.Fatalf("got %q", got)
	}
}

// withFakeYouTubeAPI points youtubeTokenURL/youtubeUploadURL at local test
// servers for the duration of the test, restoring the real URLs afterward.
func withFakeYouTubeAPI(t *testing.T, tokenHandler, uploadHandler http.HandlerFunc) {
	t.Helper()
	tokenSrv := httptest.NewServer(tokenHandler)
	uploadSrv := httptest.NewServer(uploadHandler)
	t.Cleanup(func() {
		tokenSrv.Close()
		uploadSrv.Close()
	})
	origToken, origUpload := youtubeTokenURL, youtubeUploadURL
	youtubeTokenURL, youtubeUploadURL = tokenSrv.URL, uploadSrv.URL
	t.Cleanup(func() { youtubeTokenURL, youtubeUploadURL = origToken, origUpload })
}

func waitForYouTubeUploadJob(t *testing.T, a *App, jobID string) YouTubeUploadJobView {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		job, ok := a.getYouTubeUploadJob(jobID)
		if !ok {
			t.Fatalf("job %s not found", jobID)
		}
		v := job.view()
		if v.Status != "running" {
			return v
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("job never finished")
	return YouTubeUploadJobView{}
}

func TestHandleRecordingYouTubeUploadSucceeds(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "rec.mkv"), []byte("video bytes"), 0o644); err != nil {
		t.Fatal(err)
	}

	var gotSessionPUT bool
	withFakeYouTubeAPI(t,
		func(w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode(map[string]any{"access_token": "tok123", "expires_in": 3600})
		},
		func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost {
				w.Header().Set("Location", "http://"+r.Host+"/session")
				w.WriteHeader(http.StatusOK)
				return
			}
			gotSessionPUT = true
			json.NewEncoder(w).Encode(map[string]any{"id": "abc123"})
		},
	)

	a := &App{
		config:       filepath.Join(dir, "config.json"),
		cfg:          AppConfig{Settings: Settings{FinishedDir: dir, YouTube: YouTubeConfig{Enabled: true, ClientID: "cid", ClientSecret: "secret", RefreshToken: "rtok"}}},
		ytUploadJobs: map[string]*YouTubeUploadJob{},
	}

	body, _ := json.Marshal(map[string]string{"path": "rec.mkv", "privacy": "private"})
	r := httptest.NewRequest(http.MethodPost, "/api/recordings/youtube-upload", bytes.NewReader(body))
	r = withAdminUser(r)
	w := httptest.NewRecorder()
	a.handleRecordingYouTubeUpload(w, r)

	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("could not decode response %s: %v", w.Body.String(), err)
	}
	if ok, _ := out["ok"].(bool); !ok {
		t.Fatalf("expected ok=true, got %v", out)
	}
	jobID, _ := out["jobId"].(string)
	if jobID == "" {
		t.Fatal("expected a jobId")
	}

	v := waitForYouTubeUploadJob(t, a, jobID)
	if v.Status != "done" {
		t.Fatalf("expected status done, got %s (error: %s)", v.Status, v.Error)
	}
	if v.VideoURL != "https://www.youtube.com/watch?v=abc123" {
		t.Fatalf("unexpected video URL: %s", v.VideoURL)
	}
	if !gotSessionPUT {
		t.Fatal("expected the upload session's PUT to have been called")
	}
}

func TestHandleRecordingYouTubeUploadRequiresConfiguration(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "rec.mkv"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	a := &App{config: filepath.Join(dir, "config.json"), cfg: AppConfig{Settings: Settings{FinishedDir: dir}}, ytUploadJobs: map[string]*YouTubeUploadJob{}}

	body, _ := json.Marshal(map[string]string{"path": "rec.mkv"})
	r := httptest.NewRequest(http.MethodPost, "/api/recordings/youtube-upload", bytes.NewReader(body))
	r = withAdminUser(r)
	w := httptest.NewRecorder()
	a.handleRecordingYouTubeUpload(w, r)

	var out map[string]any
	json.Unmarshal(w.Body.Bytes(), &out)
	if ok, _ := out["ok"].(bool); ok {
		t.Fatalf("expected ok=false when YouTube isn't configured, got %v", out)
	}
}
