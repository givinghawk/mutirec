package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestBatchMatchLookup(t *testing.T) {
	days := []batchMatchDay{{RelPath: "Saturday", Date: "2022-06-25"}, {RelPath: "Sunday", Date: "2022-06-26"}}
	stages := []batchMatchStage{{RelPath: "Saturday/MainStage", Name: "Main Stage"}, {RelPath: "Sunday/MainStage", Name: "Main Stage"}}

	stage, date := batchMatchLookup("Saturday/MainStage/DJ Vertex.mp4", days, stages)
	if stage != "Main Stage" || date != "2022-06-25" {
		t.Fatalf("got stage=%q date=%q", stage, date)
	}

	// No stage entry matches this one - falls back to the parent folder name.
	stage, date = batchMatchLookup("Sunday/RedStage/DJ Isaac.mp4", days, stages)
	if stage != "RedStage" || date != "2022-06-26" {
		t.Fatalf("got stage=%q date=%q", stage, date)
	}

	// File directly in the root - no parent folder, no day/stage match.
	stage, date = batchMatchLookup("flat.mp4", nil, nil)
	if stage != "" || date != "" {
		t.Fatalf("expected no stage/date for a root-level file, got stage=%q date=%q", stage, date)
	}
}

func newBatchMatchTestApp(t *testing.T) (*App, string) {
	t.Helper()
	dir := t.TempDir()
	a := &App{config: filepath.Join(dir, "config.json"), cfg: AppConfig{Settings: Settings{FinishedDir: dir}}}
	return a, dir
}

func postBatchMatch(t *testing.T, a *App, req batchMatchRequest) map[string]any {
	t.Helper()
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/api/explorer/batch-match", bytes.NewReader(body))
	r = withAdminUser(r)
	w := httptest.NewRecorder()
	a.handleBatchMatch(w, r)
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("could not decode response %s: %v", w.Body.String(), err)
	}
	return out
}

func TestHandleBatchMatchDryRunPreview(t *testing.T) {
	a, dir := newBatchMatchTestApp(t)
	root := filepath.Join(dir, "Defqon.1", "2022")
	must(t, os.MkdirAll(filepath.Join(root, "Saturday", "MainStage"), 0o755))
	must(t, os.WriteFile(filepath.Join(root, "Saturday", "MainStage", "DJ_Vertex_25_06_2022.mp4"), []byte("x"), 0o644))

	out := postBatchMatch(t, a, batchMatchRequest{
		Path:   "Defqon.1/2022",
		DryRun: true,
		Days:   []batchMatchDay{{RelPath: "Saturday", Date: "2022-06-25"}},
		Stages: []batchMatchStage{{RelPath: "Saturday/MainStage", Name: "Main Stage"}},
	})
	if ok, _ := out["ok"].(bool); !ok {
		t.Fatalf("expected ok=true, got %v", out)
	}
	files, ok := out["files"].([]any)
	if !ok || len(files) != 1 {
		t.Fatalf("expected 1 file in preview, got %v", out["files"])
	}
	f := files[0].(map[string]any)
	if f["path"] != "Defqon.1/2022/Saturday/MainStage/DJ_Vertex_25_06_2022.mp4" {
		t.Fatalf("unexpected path: %v", f["path"])
	}
	if f["stage"] != "Main Stage" {
		t.Fatalf("unexpected stage: %v", f["stage"])
	}
	if f["date"] != "2022-06-25" {
		t.Fatalf("unexpected date: %v", f["date"])
	}
	if f["artist"] != "DJ Vertex" {
		t.Fatalf("unexpected artist: %v", f["artist"])
	}
	// Dry run must not touch RecordingMeta or create an event.
	if len(a.cfg.LibraryEvents) != 0 || len(a.cfg.RecordingMeta) != 0 {
		t.Fatalf("dry run should not persist anything, got events=%v meta=%v", a.cfg.LibraryEvents, a.cfg.RecordingMeta)
	}
}

func TestHandleBatchMatchApplyCreatesEventAndMeta(t *testing.T) {
	a, dir := newBatchMatchTestApp(t)
	root := filepath.Join(dir, "Defqon.1", "2022")
	must(t, os.MkdirAll(filepath.Join(root, "Saturday", "MainStage"), 0o755))
	must(t, os.WriteFile(filepath.Join(root, "Saturday", "MainStage", "DJ_Vertex_25_06_2022.mp4"), []byte("x"), 0o644))

	out := postBatchMatch(t, a, batchMatchRequest{
		Path:         "Defqon.1/2022",
		NewEventName: "Defqon.1",
		NewEventYear: 2022,
		Days:         []batchMatchDay{{RelPath: "Saturday", Date: "2022-06-25"}},
		Stages:       []batchMatchStage{{RelPath: "Saturday/MainStage", Name: "Main Stage"}},
	})
	if ok, _ := out["ok"].(bool); !ok {
		t.Fatalf("expected ok=true, got %v", out)
	}
	if len(a.cfg.LibraryEvents) != 1 || a.cfg.LibraryEvents[0].Name != "Defqon.1" || a.cfg.LibraryEvents[0].Year != 2022 {
		t.Fatalf("expected a new Defqon.1/2022 event, got %v", a.cfg.LibraryEvents)
	}
	eventID := a.cfg.LibraryEvents[0].ID
	meta, ok := a.cfg.RecordingMeta["Defqon.1/2022/Saturday/MainStage/DJ_Vertex_25_06_2022.mp4"]
	if !ok {
		t.Fatalf("expected RecordingMeta for the file, got %v", a.cfg.RecordingMeta)
	}
	if meta.EventID != eventID || meta.Channel != "Main Stage" || meta.Artist != "DJ Vertex" || meta.Start != "2022-06-25" {
		t.Fatalf("unexpected meta: %+v", meta)
	}

	// The config file should also have actually been written to disk.
	if _, err := os.Stat(a.config); err != nil {
		t.Fatalf("expected config to be persisted: %v", err)
	}
}

func TestHandleBatchMatchRejectsPathOutsideFinishedDir(t *testing.T) {
	dir := t.TempDir()
	finished := filepath.Join(dir, "recordings")
	must(t, os.MkdirAll(finished, 0o755))
	other := filepath.Join(dir, "other")
	must(t, os.MkdirAll(filepath.Join(other, "stuff"), 0o755))

	a := &App{config: filepath.Join(dir, "config.json"), cfg: AppConfig{Settings: Settings{
		FinishedDir:      finished,
		FileExplorerRoot: dir,
	}}}

	out := postBatchMatch(t, a, batchMatchRequest{Path: "other", NewEventName: "Whatever", DryRun: true})
	if ok, _ := out["ok"].(bool); ok {
		t.Fatalf("expected an error for a folder outside FinishedDir, got %v", out)
	}
}

func TestHandleBatchMatchRequiresEventChoiceToApply(t *testing.T) {
	a, dir := newBatchMatchTestApp(t)
	must(t, os.MkdirAll(filepath.Join(dir, "Empty"), 0o755))

	out := postBatchMatch(t, a, batchMatchRequest{Path: "Empty"})
	if ok, _ := out["ok"].(bool); ok {
		t.Fatalf("expected an error when neither eventId nor newEventName is set, got %v", out)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
