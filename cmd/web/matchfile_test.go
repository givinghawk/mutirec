package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

type matchfileImportResponse struct {
	Matched    int                    `json:"matched"`
	Duplicates int                    `json:"duplicates"`
	DryRun     bool                   `json:"dryRun"`
	Matches    []matchfilePreviewItem `json:"matches"`
}

func matchfileTestApp(t *testing.T) (*App, string) {
	t.Helper()
	dir := t.TempDir()
	finished := filepath.Join(dir, "finished")
	if err := os.MkdirAll(filepath.Join(finished, "MainStage"), 0o755); err != nil {
		t.Fatal(err)
	}
	content := []byte("fake recording bytes")
	if err := os.WriteFile(filepath.Join(finished, "MainStage", "set.mp3"), content, 0o644); err != nil {
		t.Fatal(err)
	}
	app := &App{config: filepath.Join(dir, "config.json"), cfg: AppConfig{Settings: Settings{FinishedDir: finished}}}
	sum := sha256.Sum256(content)
	return app, hex.EncodeToString(sum[:])
}

func postMatchfileImport(t *testing.T, app *App, url string, entries []MatchFileEntry) matchfileImportResponse {
	t.Helper()
	body, _ := json.Marshal(entries)
	req := httptest.NewRequest("POST", url, bytes.NewReader(body))
	rec := httptest.NewRecorder()
	app.handleRecordingsMatchfileImport(rec, req)
	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp matchfileImportResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	return resp
}

// TestMatchfileImportDryRun: a dry run must report exactly what a real import
// would apply - matched paths and their metadata - without changing any
// RecordingMeta or creating any LibraryEvent.
func TestMatchfileImportDryRun(t *testing.T) {
	app, hash := matchfileTestApp(t)
	entries := []MatchFileEntry{{Hash: hash, EventName: "Neonbeat 2026", FestivalName: "Neonbeat", StageName: "MainStage", Artist: "DJ Vertex"}}

	resp := postMatchfileImport(t, app, "/api/recordings/matchfile/import?dryRun=1", entries)
	if !resp.DryRun || resp.Matched != 1 || len(resp.Matches) != 1 {
		t.Fatalf("expected dry-run with 1 match, got %+v", resp)
	}
	m := resp.Matches[0]
	if m.Path != "MainStage/set.mp3" || m.EventName != "Neonbeat 2026" || m.Artist != "DJ Vertex" {
		t.Fatalf("unexpected preview item: %+v", m)
	}
	if len(app.cfg.RecordingMeta) != 0 {
		t.Fatalf("dry run must not write RecordingMeta, got %v", app.cfg.RecordingMeta)
	}
	if len(app.cfg.LibraryEvents) != 0 {
		t.Fatalf("dry run must not create LibraryEvents, got %v", app.cfg.LibraryEvents)
	}
	if _, err := os.Stat(app.config); err == nil {
		t.Fatal("dry run must not persist config.json")
	}
}

// TestMatchfileImportApply: the real (non-dry-run) import applies metadata and
// resolves/creates the named LibraryEvent.
func TestMatchfileImportApply(t *testing.T) {
	app, hash := matchfileTestApp(t)
	entries := []MatchFileEntry{{Hash: hash, EventName: "Neonbeat 2026", FestivalName: "Neonbeat", StageName: "MainStage", Artist: "DJ Vertex"}}

	resp := postMatchfileImport(t, app, "/api/recordings/matchfile/import", entries)
	if resp.DryRun || resp.Matched != 1 {
		t.Fatalf("expected applied import with 1 match, got %+v", resp)
	}
	meta, ok := app.cfg.RecordingMeta["MainStage/set.mp3"]
	if !ok || meta.Artist != "DJ Vertex" || meta.EventID == "" {
		t.Fatalf("expected applied RecordingMeta with a resolved event, got %+v (ok=%v)", meta, ok)
	}
	if len(app.cfg.LibraryEvents) != 1 || app.cfg.LibraryEvents[0].Name != "Neonbeat 2026" {
		t.Fatalf("expected the named LibraryEvent to be created, got %v", app.cfg.LibraryEvents)
	}
}

// TestMatchfileImportDuplicateHashes: when one hash appears twice with
// different metadata, the first entry wins and the conflict is reported;
// an identical repeat is not counted as a conflict.
func TestMatchfileImportDuplicateHashes(t *testing.T) {
	app, hash := matchfileTestApp(t)
	first := MatchFileEntry{Hash: hash, EventName: "Neonbeat 2026", Artist: "DJ Vertex"}
	entries := []MatchFileEntry{
		first,
		first, // identical repeat - not a conflict
		{Hash: hash, EventName: "Some Other Event", Artist: "Somebody Else"}, // conflicting - reported, ignored
	}

	resp := postMatchfileImport(t, app, "/api/recordings/matchfile/import", entries)
	if resp.Duplicates != 1 {
		t.Fatalf("expected 1 conflicting duplicate reported, got %d", resp.Duplicates)
	}
	meta := app.cfg.RecordingMeta["MainStage/set.mp3"]
	if meta.Artist != "DJ Vertex" {
		t.Fatalf("expected the first entry for a duplicated hash to win, got %+v", meta)
	}
}
