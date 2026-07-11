package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTranscodeAudioCodecName(t *testing.T) {
	cases := map[string]string{
		"opus":    "libopus",
		"mp3":     "libmp3lame",
		"flac":    "flac",
		"aac":     "aac",
		"unknown": "aac",
		"":        "aac",
	}
	for in, want := range cases {
		if got := transcodeAudioCodecName(in); got != want {
			t.Errorf("transcodeAudioCodecName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTranscodeVideoEncoder(t *testing.T) {
	if got := transcodeVideoEncoder("h264", ""); got != "libx264" {
		t.Errorf("h264/no-hw = %q, want libx264", got)
	}
	if got := transcodeVideoEncoder("h264", "cuda"); got != "h264_nvenc" {
		t.Errorf("h264/cuda = %q, want h264_nvenc", got)
	}
	if got := transcodeVideoEncoder("h265", ""); got != "libx265" {
		t.Errorf("h265/no-hw = %q, want libx265", got)
	}
	if got := transcodeVideoEncoder("hevc", "qsv"); got != "hevc_qsv" {
		t.Errorf("hevc/qsv = %q, want hevc_qsv", got)
	}
	if got := transcodeVideoEncoder("h265", "vaapi"); got != "hevc_vaapi" {
		t.Errorf("h265/vaapi = %q, want hevc_vaapi", got)
	}
}

func TestContainerExt(t *testing.T) {
	cases := map[string]string{
		"mkv":      ".mkv",
		"matroska": ".mkv",
		"MP4":      ".mp4",
		"mp3":      ".mp3",
		"":         "",
	}
	for in, want := range cases {
		if got := containerExt(in); got != want {
			t.Errorf("containerExt(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBuildTranscodeArgsCopyDefaults(t *testing.T) {
	args := buildTranscodeArgs("in.mkv", "out.mkv", "mkv", TranscodeOptions{})
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "-c:v copy") || !strings.Contains(joined, "-c:a copy") {
		t.Errorf("expected stream-copy defaults, got: %s", joined)
	}
}

func TestBuildTranscodeArgsReencode(t *testing.T) {
	args := buildTranscodeArgs("in.mkv", "out.mp4", "mp4", TranscodeOptions{
		VideoCodec: "h265", CRF: 20, AudioCodec: "opus", AudioBitrateKbps: 128, HardwareAccel: "vaapi",
	})
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "-hwaccel vaapi") {
		t.Errorf("expected -hwaccel vaapi, got: %s", joined)
	}
	if !strings.Contains(joined, "hevc_vaapi") || !strings.Contains(joined, "-crf 20") {
		t.Errorf("expected hevc_vaapi + -crf 20, got: %s", joined)
	}
	if !strings.Contains(joined, "libopus") || !strings.Contains(joined, "-b:a 128k") {
		t.Errorf("expected libopus + -b:a 128k, got: %s", joined)
	}
}

func TestBuildTranscodeArgsAudioOnlyExtraction(t *testing.T) {
	args := buildTranscodeArgs("in.mkv", "out.mp3", "mp3", TranscodeOptions{VideoCodec: "none", AudioCodec: "mp3"})
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "-vn") {
		t.Errorf("expected -vn for audio-only extraction, got: %s", joined)
	}
	if strings.Contains(joined, "-c:v") {
		t.Errorf("did not expect a -c:v flag when stripping video, got: %s", joined)
	}
}

func TestParseFFmpegTimestamp(t *testing.T) {
	cases := []struct {
		in     string
		want   float64
		wantOK bool
	}{
		{"00:01:30.500000", 90.5, true},
		{"01:00:00.000000", 3600, true},
		{"garbage", 0, false},
		{"", 0, false},
	}
	for _, c := range cases {
		got, ok := parseFFmpegTimestamp(c.in)
		if ok != c.wantOK || (ok && got != c.want) {
			t.Errorf("parseFFmpegTimestamp(%q) = (%v, %v), want (%v, %v)", c.in, got, ok, c.want, c.wantOK)
		}
	}
}

func TestFormatTranscodeProgress(t *testing.T) {
	got := formatTranscodeProgress(map[string]string{"out_time": "00:00:30.000000", "speed": "2.5x", "fps": "60"}, 60)
	if !strings.Contains(got, "50%") || !strings.Contains(got, "2.5x speed") || !strings.Contains(got, "60 fps") {
		t.Errorf("expected 50%%/2.5x speed/60 fps, got %q", got)
	}
	if got := formatTranscodeProgress(map[string]string{}, 0); got != "in progress" {
		t.Errorf("expected a fallback for no usable fields, got %q", got)
	}
	if got := formatTranscodeProgress(map[string]string{"speed": "N/A"}, 0); strings.Contains(got, "N/A") {
		t.Errorf("expected an N/A speed to be dropped, got %q", got)
	}
}

// fakeFFmpegOnPath drops a fake "ffmpeg" script on PATH for this test only,
// so runFFmpegTranscode can be exercised without a real media pipeline.
// script is the shell body; it receives no special args, just needs to
// write "-progress pipe:1" style lines to stdout and/or stderr and exit.
func fakeFFmpegOnPath(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "ffmpeg")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatalf("could not write fake ffmpeg: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestRunFFmpegTranscodeLogsProgressAndSucceeds(t *testing.T) {
	fakeFFmpegOnPath(t, `
echo "out_time=00:00:15.000000"
echo "speed=1.0x"
echo "fps=30"
echo "progress=continue"
echo "progress=end"
exit 0
`)
	job := &TranscodeJob{id: "t1", status: "running"}
	err := runFFmpegTranscode(job, "some/file.mkv", []string{"-i", "in.mkv", "out.mkv"}, 30)
	if err != nil {
		t.Fatalf("expected success, got error: %v", err)
	}
}

func TestRunFFmpegTranscodeReturnsStderrOnFailure(t *testing.T) {
	fakeFFmpegOnPath(t, `
echo "ffmpeg: something went wrong" >&2
exit 1
`)
	job := &TranscodeJob{id: "t2", status: "running"}
	err := runFFmpegTranscode(job, "some/file.mkv", []string{"-i", "in.mkv", "out.mkv"}, 30)
	if err == nil {
		t.Fatal("expected an error for a nonzero exit")
	}
	if !strings.Contains(err.Error(), "something went wrong") {
		t.Errorf("expected the captured stderr in the error, got: %v", err)
	}
}

func TestHandleTranscodeStartRequiresAdmin(t *testing.T) {
	dir := t.TempDir()
	app := &App{config: filepath.Join(dir, "config.json"), cfg: AppConfig{Settings: Settings{FinishedDir: dir}}}
	body, _ := json.Marshal(map[string]any{"paths": []string{"BLUE/set.mkv"}})
	req := httptest.NewRequest(http.MethodPost, "/api/transcode/start", bytes.NewReader(body))
	w := httptest.NewRecorder()
	app.handleTranscodeStart(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for a non-admin transcode start, got %d", w.Code)
	}
}

func TestHandleTranscodeStartRejectsEmptyPaths(t *testing.T) {
	dir := t.TempDir()
	app := &App{config: filepath.Join(dir, "config.json"), cfg: AppConfig{Settings: Settings{FinishedDir: dir}}}
	body, _ := json.Marshal(map[string]any{"paths": []string{}})
	req := httptest.NewRequest(http.MethodPost, "/api/transcode/start", bytes.NewReader(body))
	req = withAdminUser(req)
	w := httptest.NewRecorder()
	app.handleTranscodeStart(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for an empty paths list, got %d", w.Code)
	}
}

func TestHandleTranscodeStartAndPollJob(t *testing.T) {
	dir := t.TempDir()
	finishedDir := filepath.Join(dir, "finished")
	if err := os.MkdirAll(filepath.Join(finishedDir, "BLUE"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(finishedDir, "BLUE", "set.mkv"), []byte("fake media"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	app := &App{
		config:        filepath.Join(dir, "config.json"),
		cfg:           AppConfig{Settings: Settings{FinishedDir: finishedDir}},
		transcodeJobs: map[string]*TranscodeJob{},
	}

	body, _ := json.Marshal(map[string]any{
		"paths":   []string{"BLUE/set.mkv"},
		"options": TranscodeOptions{Container: "mp4", VideoCodec: "h264"},
	})
	req := httptest.NewRequest(http.MethodPost, "/api/transcode/start", bytes.NewReader(body))
	req = withAdminUser(req)
	w := httptest.NewRecorder()
	app.handleTranscodeStart(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil || resp["jobId"] == "" {
		t.Fatalf("expected a jobId in the response, got %s (err %v)", w.Body.String(), err)
	}

	// The job runs in a goroutine and ffmpeg isn't necessarily installed in
	// the test environment - just confirm the job is pollable and finishes
	// (successfully or with a recorded per-file error, either is fine here).
	job, ok := app.getTranscodeJob(resp["jobId"])
	if !ok {
		t.Fatal("expected the job to be retrievable immediately after starting")
	}
	for i := 0; i < 100 && job.view().Status == "running"; i++ {
		// The fake input isn't real media, so ffmpeg (if present) fails fast,
		// and if ffmpeg is absent the job fails instantly - either way this
		// should never actually need the full budget below.
		time.Sleep(10 * time.Millisecond)
	}
	getReq := httptest.NewRequest(http.MethodGet, "/api/transcode/jobs/"+resp["jobId"], nil)
	getW := httptest.NewRecorder()
	app.handleTranscodeJobItem(getW, getReq)
	if getW.Code != http.StatusOK {
		t.Fatalf("expected 200 polling the job, got %d", getW.Code)
	}
}

func TestHandleTranscodeJobItemMissingIs404(t *testing.T) {
	app := &App{transcodeJobs: map[string]*TranscodeJob{}}
	req := httptest.NewRequest(http.MethodGet, "/api/transcode/jobs/does-not-exist", nil)
	w := httptest.NewRecorder()
	app.handleTranscodeJobItem(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for an unknown job id, got %d", w.Code)
	}
}
