package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeYtDlpOnDisk writes a fake yt-dlp shell script to disk and returns its
// path, so runYouTubeDownloadJob can be exercised without a real yt-dlp
// binary or network access.
func fakeYtDlpOnDisk(t *testing.T, script string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "yt-dlp")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatalf("could not write fake yt-dlp: %v", err)
	}
	return path
}

func TestRunYouTubeDownloadJobSucceeds(t *testing.T) {
	destDir := t.TempDir()
	outFile := filepath.Join(destDir, "Some Video [abc123].mp4")
	if err := os.WriteFile(outFile, []byte("video bytes"), 0o644); err != nil {
		t.Fatal(err)
	}

	script := `
echo "[youtube] abc123: Downloading webpage"
echo "[download]  50.0% of   10.00MiB at    1.00MiB/s ETA 00:05"
echo "[download] 100.0% of   10.00MiB at    1.00MiB/s ETA 00:00"
echo "` + outFile + `"
exit 0
`
	ytdlpPath := fakeYtDlpOnDisk(t, script)

	a := &App{config: filepath.Join(destDir, "config.json"), cfg: AppConfig{Settings: Settings{FinishedDir: destDir}}}
	job := &YouTubeDownloadJob{id: "j1", status: "running"}
	a.runYouTubeDownloadJob(job, ytdlpPath, "https://youtube.example/watch?v=abc123", destDir)

	v := job.view()
	if v.Status != "done" {
		t.Fatalf("expected status done, got %s (error: %s)", v.Status, v.Error)
	}
	if v.DestName != "Some Video [abc123].mp4" {
		t.Fatalf("unexpected destName: %s", v.DestName)
	}
	if v.ProgressPct != 100 {
		t.Fatalf("expected progress 100, got %v", v.ProgressPct)
	}
	joined := strings.Join(logLinesText(v.Log), "\n")
	if !strings.Contains(joined, "sha256") {
		t.Fatalf("expected a hash-verification log line, got:\n%s", joined)
	}
}

func TestRunYouTubeDownloadJobReportsFailure(t *testing.T) {
	destDir := t.TempDir()
	script := `
echo "ERROR: Video unavailable" 1>&2
exit 1
`
	ytdlpPath := fakeYtDlpOnDisk(t, script)

	a := &App{config: filepath.Join(destDir, "config.json"), cfg: AppConfig{Settings: Settings{FinishedDir: destDir}}}
	job := &YouTubeDownloadJob{id: "j2", status: "running"}
	a.runYouTubeDownloadJob(job, ytdlpPath, "https://youtube.example/watch?v=missing", destDir)

	v := job.view()
	if v.Status != "error" {
		t.Fatalf("expected status error, got %s", v.Status)
	}
	if !strings.Contains(v.Error, "Video unavailable") {
		t.Fatalf("expected error to include yt-dlp's stderr, got: %s", v.Error)
	}
}

func logLinesText(log []ShareJobLogLine) []string {
	out := make([]string, len(log))
	for i, l := range log {
		out[i] = l.Text
	}
	return out
}
