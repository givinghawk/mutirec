package main

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
)

// ============================================================================
// Batch Match: a guided, manual alternative to Smart Match for organizing a
// folder of already-downloaded sets that don't follow the recommended
// <Event>/<Edition>/<Day>/<Stage>/<file> layout closely enough for Smart
// Match's automatic folder-hint parsing (see folderEventHint) to pick up
// cleanly. The user marks one File Explorer folder as belonging to a single
// LibraryEvent, optionally maps its day-folders to real calendar dates and
// its stage-folders to display names (either step can be skipped if there
// are no such subfolders, or the user doesn't want overrides), and every
// file underneath gets its artist name guessed from its filename (reusing
// guessArtistFromName, the same heuristic Smart Match itself uses). Nothing
// is written to RecordingMeta until the wizard's final "Apply" step - the
// same request shape with dryRun:true powers the preview shown along the way.
// ============================================================================

// batchMatchDay maps one folder (relative to the marked root, "/"-joined,
// e.g. "Saturday" or "Day 1/Undercard") to a calendar date. Days is entirely
// optional - an empty slice just means no date gets recorded.
type batchMatchDay struct {
	RelPath string `json:"relPath"`
	Date    string `json:"date,omitempty"`
}

// batchMatchStage maps one folder (again relative to the marked root, at
// whatever depth it actually sits - directly under the root, or under a day
// folder) to a display name for the stage/channel. Stages is also entirely
// optional - a file whose path doesn't match any entry falls back to its own
// immediate parent folder's name.
type batchMatchStage struct {
	RelPath string `json:"relPath"`
	Name    string `json:"name"`
}

type batchMatchRequest struct {
	Path           string            `json:"path"`
	EventID        string            `json:"eventId,omitempty"`
	NewEventName   string            `json:"newEventName,omitempty"`
	NewEventYear   int               `json:"newEventYear,omitempty"`
	NewEventFestID string            `json:"newEventFestivalId,omitempty"`
	Days           []batchMatchDay   `json:"days,omitempty"`
	Stages         []batchMatchStage `json:"stages,omitempty"`
	DryRun         bool              `json:"dryRun"`
}

// BatchMatchFileResult is one file's outcome, in both the dry-run preview
// and the real applied result.
type BatchMatchFileResult struct {
	Path   string `json:"path"` // relative to FinishedDir - the RecordingMeta key
	Name   string `json:"name"`
	Stage  string `json:"stage,omitempty"`
	Date   string `json:"date,omitempty"`
	Artist string `json:"artist,omitempty"`
}

// handleBatchMatch previews (dryRun:true) or applies (dryRun:false) a batch
// match over every file under req.Path, a folder under the File Explorer
// root. Since RecordingMeta is keyed by path relative to FinishedDir, this
// only works for a folder that actually resolves inside FinishedDir - which
// it always will unless FileExplorerRoot has been pointed somewhere wider
// than the recordings library (an advanced, rarely-used setting).
func (a *App) handleBatchMatch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !a.isAdminReq(r) {
		http.Error(w, "admin only", http.StatusForbidden)
		return
	}
	var req batchMatchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}

	cfg := a.snapshotConfig()
	explorerRoot := a.explorerRoot(cfg)
	abs, err := resolveExplorerPath(explorerRoot, req.Path)
	if err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	finishedRoot := filepath.Clean(cfg.Settings.FinishedDir)
	relFromFinished, err := filepath.Rel(finishedRoot, abs)
	if err != nil || relFromFinished == ".." || strings.HasPrefix(relFromFinished, ".."+string(filepath.Separator)) {
		writeJSON(w, map[string]any{"ok": false, "error": "this folder is outside the recordings library (the Finished directory) - Batch Match only works on library folders"})
		return
	}
	relFromFinished = filepath.ToSlash(relFromFinished)
	if relFromFinished == "." {
		relFromFinished = ""
	}

	if !req.DryRun && req.EventID == "" && strings.TrimSpace(req.NewEventName) == "" {
		writeJSON(w, map[string]any{"ok": false, "error": "choose an existing event or name a new one"})
		return
	}

	var results []BatchMatchFileResult
	_ = filepath.WalkDir(abs, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || isSidecarPath(p) {
			return nil
		}
		relFromRoot, err := filepath.Rel(abs, p)
		if err != nil {
			return nil
		}
		relFromRoot = filepath.ToSlash(relFromRoot)
		stage, date := batchMatchLookup(relFromRoot, req.Days, req.Stages)
		name := filepath.Base(p)
		path := name
		if relFromFinished != "" {
			path = relFromFinished + "/" + relFromRoot
		} else {
			path = relFromRoot
		}
		results = append(results, BatchMatchFileResult{
			Path:   path,
			Name:   name,
			Stage:  stage,
			Date:   date,
			Artist: guessArtistFromName(name, stage),
		})
		return nil
	})
	sort.Slice(results, func(i, j int) bool { return results[i].Path < results[j].Path })

	if req.DryRun {
		writeJSON(w, map[string]any{"ok": true, "files": results})
		return
	}

	a.mu.Lock()
	eventID := req.EventID
	if eventID == "" {
		ev := LibraryEvent{ID: newID(), Name: strings.TrimSpace(req.NewEventName), Year: req.NewEventYear, FestivalID: req.NewEventFestID, Timetable: []StageSchedule{}}
		a.cfg.LibraryEvents = append(a.cfg.LibraryEvents, ev)
		eventID = ev.ID
	}
	if a.cfg.RecordingMeta == nil {
		a.cfg.RecordingMeta = map[string]RecordingMeta{}
	}
	for _, f := range results {
		existing := a.cfg.RecordingMeta[f.Path]
		existing.EventID = eventID
		if f.Stage != "" {
			existing.Channel = f.Stage
		}
		if f.Artist != "" {
			existing.Artist = f.Artist
		}
		if f.Date != "" {
			existing.Start = f.Date
		}
		a.cfg.RecordingMeta[f.Path] = existing
	}
	cfgCopy := a.cfg
	a.mu.Unlock()
	_ = a.persist(cfgCopy)
	a.event("info", fmt.Sprintf("Batch matched %d file(s) under %s", len(results), req.Path))

	writeJSON(w, map[string]any{"ok": true, "eventId": eventID, "count": len(results)})
}

// batchMatchLookup finds which (if any) configured day/stage entry
// relFromRoot falls under - a file matches an entry whose RelPath is either
// an exact match or a directory prefix of its own path. Stage falls back to
// the file's own immediate parent folder name when nothing configured
// matches (or nothing was configured at all), same as the rest of the app's
// "channel" convention; a file sitting directly in the marked root (no
// subfolder at all) gets no stage.
func batchMatchLookup(relFromRoot string, days []batchMatchDay, stages []batchMatchStage) (stage, date string) {
	for _, d := range days {
		if d.RelPath == "" {
			continue
		}
		if relFromRoot == d.RelPath || strings.HasPrefix(relFromRoot, d.RelPath+"/") {
			date = d.Date
			break
		}
	}
	bestLen := -1
	for _, s := range stages {
		if s.RelPath == "" {
			continue
		}
		if (relFromRoot == s.RelPath || strings.HasPrefix(relFromRoot, s.RelPath+"/")) && len(s.RelPath) > bestLen {
			bestLen = len(s.RelPath)
			stage = s.Name
		}
	}
	if stage == "" {
		if idx := strings.LastIndex(relFromRoot, "/"); idx >= 0 {
			stage = filepath.Base(relFromRoot[:idx])
		}
	}
	return stage, date
}
