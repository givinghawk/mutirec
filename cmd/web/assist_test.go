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

func TestParseSilenceLog(t *testing.T) {
	log := `[silencedetect @ 0x1234] silence_start: 10.5
[silencedetect @ 0x1234] silence_end: 15.25 | silence_duration: 4.75
some unrelated ffmpeg line
[silencedetect @ 0x1234] silence_end: 42.0 | silence_duration: 1.2`
	got := parseSilenceLog(log, 100)
	if len(got) != 2 {
		t.Fatalf("expected 2 silence gaps, got %d: %+v", len(got), got)
	}
	if got[0].end != 115.25 || got[0].dur != 4.75 {
		t.Errorf("first gap = %+v, want end=115.25 dur=4.75", got[0])
	}
	if got[1].end != 142.0 || got[1].dur != 1.2 {
		t.Errorf("second gap = %+v, want end=142.0 dur=1.2", got[1])
	}
}

func TestParseSilenceLogNoMatches(t *testing.T) {
	if got := parseSilenceLog("nothing relevant here\nanother line", 0); len(got) != 0 {
		t.Errorf("expected no gaps, got %+v", got)
	}
}

func TestPickClosestSilence(t *testing.T) {
	candidates := []silenceGap{
		{end: 100, dur: 2},
		{end: 130, dur: 10}, // closest to boundary 128
		{end: 200, dur: 1},
	}
	got, ok := pickClosestSilence(candidates, 128)
	if !ok || got != 130 {
		t.Errorf("pickClosestSilence = %v, %v; want 130, true", got, ok)
	}
}

func TestPickClosestSilenceTieBreaksOnDuration(t *testing.T) {
	candidates := []silenceGap{
		{end: 90, dur: 2},  // distance 10
		{end: 110, dur: 8}, // distance 10, longer - should win
	}
	got, ok := pickClosestSilence(candidates, 100)
	if !ok || got != 110 {
		t.Errorf("pickClosestSilence tie-break = %v, %v; want 110, true", got, ok)
	}
}

func TestPickClosestSilenceEmpty(t *testing.T) {
	if _, ok := pickClosestSilence(nil, 50); ok {
		t.Error("expected no result for an empty candidate list")
	}
}

func TestMatchWhisperTranscriptFindsArtistName(t *testing.T) {
	transcript := whisperTranscript{Segments: []struct {
		Start float64 `json:"start"`
		Text  string  `json:"text"`
	}{
		{Start: 5.0, Text: "thank you very much"},
		{Start: 12.5, Text: "please welcome DJ Vertex to the stage"},
	}}
	got, ok := matchWhisperTranscript(transcript, 1000, "DJ Vertex")
	if !ok || got != 1012.5 {
		t.Errorf("matchWhisperTranscript = %v, %v; want 1012.5, true", got, ok)
	}
}

func TestMatchWhisperTranscriptFallsBackToHandoffPhrase(t *testing.T) {
	transcript := whisperTranscript{Segments: []struct {
		Start float64 `json:"start"`
		Text  string  `json:"text"`
	}{
		{Start: 3.0, Text: "and now, give it up for the next act"},
	}}
	got, ok := matchWhisperTranscript(transcript, 500, "Some Artist Whose Name Wasn't Said")
	if !ok || got != 503.0 {
		t.Errorf("matchWhisperTranscript fallback = %v, %v; want 503.0, true", got, ok)
	}
}

func TestMatchWhisperTranscriptNoMatch(t *testing.T) {
	transcript := whisperTranscript{Segments: []struct {
		Start float64 `json:"start"`
		Text  string  `json:"text"`
	}{
		{Start: 1.0, Text: "just some crowd noise and music"},
	}}
	if _, ok := matchWhisperTranscript(transcript, 0, "Nonexistent Artist"); ok {
		t.Error("expected no match when neither the artist name nor a handoff phrase appears")
	}
}

func TestWhisperBinaryAbsent(t *testing.T) {
	// In the test sandbox no whisper CLI is installed - confirm this
	// returns false rather than panicking or finding a false positive.
	if _, ok := whisperBinary(); ok {
		t.Skip("a whisper-like binary happens to be on PATH in this environment - skipping the absence check")
	}
}

func TestHandleCutterDetectRequiresAdmin(t *testing.T) {
	dir := t.TempDir()
	app := &App{config: filepath.Join(dir, "config.json"), cfg: AppConfig{Settings: Settings{FinishedDir: dir}}}
	body, _ := json.Marshal(map[string]string{"path": "BLUE/set.mkv"})
	req := httptest.NewRequest(http.MethodPost, "/api/cutter/detect", bytes.NewReader(body))
	w := httptest.NewRecorder()
	app.handleCutterDetect(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for a non-admin detect request, got %d", w.Code)
	}
}

func TestHandleCutterDetectRequiresTimecodeSidecar(t *testing.T) {
	dir := t.TempDir()
	finishedDir := filepath.Join(dir, "finished")
	if err := os.MkdirAll(filepath.Join(finishedDir, "BLUE"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(finishedDir, "BLUE", "set.mkv"), []byte("fake"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	app := &App{config: filepath.Join(dir, "config.json"), cfg: AppConfig{Settings: Settings{FinishedDir: finishedDir}}}

	body, _ := json.Marshal(map[string]string{"path": "BLUE/set.mkv"})
	req := httptest.NewRequest(http.MethodPost, "/api/cutter/detect", bytes.NewReader(body))
	req = withAdminUser(req)
	w := httptest.NewRecorder()
	app.handleCutterDetect(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 without a timecode sidecar, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleCutterDetectRequiresAssignedEvent(t *testing.T) {
	dir := t.TempDir()
	finishedDir := filepath.Join(dir, "finished")
	if err := os.MkdirAll(filepath.Join(finishedDir, "BLUE"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	recPath := filepath.Join(finishedDir, "BLUE", "set.mkv")
	if err := os.WriteFile(recPath, []byte("fake"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	sc := RecordingSidecar{Version: 1, StartedAt: time.Now().UTC(), TimeSource: "manual"}
	if err := writeSidecarJSON(sidecarTimecodePath(recPath), sc); err != nil {
		t.Fatalf("writeSidecarJSON: %v", err)
	}
	app := &App{config: filepath.Join(dir, "config.json"), cfg: AppConfig{Settings: Settings{FinishedDir: finishedDir}}}

	body, _ := json.Marshal(map[string]string{"path": "BLUE/set.mkv"})
	req := httptest.NewRequest(http.MethodPost, "/api/cutter/detect", bytes.NewReader(body))
	req = withAdminUser(req)
	w := httptest.NewRecorder()
	app.handleCutterDetect(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 without an assigned library event, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleCutterDetectStartsJobWhenWellFormed(t *testing.T) {
	dir := t.TempDir()
	finishedDir := filepath.Join(dir, "finished")
	if err := os.MkdirAll(filepath.Join(finishedDir, "BLUE"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	recPath := filepath.Join(finishedDir, "BLUE", "set.mkv")
	if err := os.WriteFile(recPath, []byte("fake"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	sc := RecordingSidecar{Version: 1, StartedAt: time.Now().Add(-time.Hour).UTC(), TimeSource: "manual"}
	if err := writeSidecarJSON(sidecarTimecodePath(recPath), sc); err != nil {
		t.Fatalf("writeSidecarJSON: %v", err)
	}

	app := &App{
		config: filepath.Join(dir, "config.json"),
		cfg: AppConfig{
			Settings: Settings{FinishedDir: finishedDir},
			LibraryEvents: []LibraryEvent{{
				ID: "evt1", Name: "Test Event",
				Timetable: []StageSchedule{{Stage: "BLUE", Sets: []ScheduleSet{
					{ID: "s1", Name: "Opener", Start: "2020-01-01T18:00:00Z", End: "2020-01-01T19:00:00Z"},
					{ID: "s2", Name: "Headliner", Start: "2020-01-01T19:00:00Z", End: "2020-01-01T20:00:00Z"},
				}}},
			}},
			RecordingMeta: map[string]RecordingMeta{"BLUE/set.mkv": {EventID: "evt1", Channel: "BLUE"}},
		},
		detectJobs: map[string]*DetectJob{},
	}

	body, _ := json.Marshal(map[string]string{"path": "BLUE/set.mkv"})
	req := httptest.NewRequest(http.MethodPost, "/api/cutter/detect", bytes.NewReader(body))
	req = withAdminUser(req)
	w := httptest.NewRecorder()
	app.handleCutterDetect(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil || resp["jobId"] == "" {
		t.Fatalf("expected a jobId in the response, got %s (err %v)", w.Body.String(), err)
	}
	if _, ok := app.getDetectJob(resp["jobId"]); !ok {
		t.Fatal("expected the job to be retrievable immediately after starting")
	}
}

func TestHandleCutterDetectJobItemMissingIs404(t *testing.T) {
	app := &App{detectJobs: map[string]*DetectJob{}}
	req := httptest.NewRequest(http.MethodGet, "/api/cutter/detect/jobs/does-not-exist", nil)
	w := httptest.NewRecorder()
	app.handleCutterDetectJobItem(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for an unknown detect job id, got %d", w.Code)
	}
}
