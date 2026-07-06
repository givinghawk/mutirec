package main

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ============================================================================
// File explorer: a general-purpose browse/upload/zip/unzip file manager
// rooted at a configurable directory (Settings.FileExplorerRoot, defaulting
// to FinishedDir - the recordings library - when unset). Admin-only, same
// trust level as source streamlink/ffmpeg args: an admin already has the
// equivalent of shell access to the host, so this is a convenience UI for
// that access, not a new privilege boundary.
// ============================================================================

// explorerRoot resolves the configured browsable root to a cleaned absolute
// path, defaulting to FinishedDir (the recordings library) when unset.
func (a *App) explorerRoot(cfg AppConfig) string {
	root := strings.TrimSpace(cfg.Settings.FileExplorerRoot)
	if root == "" {
		root = cfg.Settings.FinishedDir
	}
	return filepath.Clean(localizePath(root))
}

// resolveExplorerPath validates a client-supplied relative path against the
// configured root, rejecting anything that would escape it (empty/"."
// resolves to the root itself).
func resolveExplorerPath(root, relPath string) (string, error) {
	relPath = strings.TrimPrefix(filepath.ToSlash(relPath), "/")
	abs := filepath.Clean(filepath.Join(root, filepath.FromSlash(relPath)))
	if abs != root && !strings.HasPrefix(abs, root+string(os.PathSeparator)) {
		return "", fmt.Errorf("invalid path")
	}
	return abs, nil
}

// sanitizeEntryName validates a single path component (a new folder name, an
// upload's filename, a rename target) - no separators, no "..", not empty.
func sanitizeEntryName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." {
		return "", fmt.Errorf("invalid name")
	}
	if strings.ContainsAny(name, "/\\") {
		return "", fmt.Errorf("name can't contain a path separator")
	}
	return name, nil
}

// explorerEntry is one row in a directory listing.
type explorerEntry struct {
	Name    string    `json:"name"`
	IsDir   bool      `json:"isDir"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"modTime"`
}

// handleExplorerList lists the contents of one directory under the explorer
// root.
func (a *App) handleExplorerList(w http.ResponseWriter, r *http.Request) {
	if !a.isAdminReq(r) {
		http.Error(w, "admin only", http.StatusForbidden)
		return
	}
	cfg := a.snapshotConfig()
	root := a.explorerRoot(cfg)
	abs, err := resolveExplorerPath(root, r.URL.Query().Get("path"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		http.Error(w, "could not read that directory: "+err.Error(), http.StatusBadRequest)
		return
	}
	out := make([]explorerEntry, 0, len(entries))
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, explorerEntry{Name: e.Name(), IsDir: e.IsDir(), Size: info.Size(), ModTime: info.ModTime()})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].IsDir != out[j].IsDir {
			return out[i].IsDir
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	writeJSON(w, map[string]any{"root": filepath.ToSlash(root), "entries": out})
}

// handleExplorerMkdir creates a new subdirectory under path.
func (a *App) handleExplorerMkdir(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !a.isAdminReq(r) {
		http.Error(w, "admin only", http.StatusForbidden)
		return
	}
	var req struct{ Path, Name string }
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	name, err := sanitizeEntryName(req.Name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	root := a.explorerRoot(a.snapshotConfig())
	dir, err := resolveExplorerPath(root, req.Path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := os.Mkdir(filepath.Join(dir, name), 0o755); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

// handleExplorerRename renames an entry in place (basename only - it never
// moves an entry to a different directory).
func (a *App) handleExplorerRename(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !a.isAdminReq(r) {
		http.Error(w, "admin only", http.StatusForbidden)
		return
	}
	var req struct{ Path, NewName string }
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	newName, err := sanitizeEntryName(req.NewName)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	root := a.explorerRoot(a.snapshotConfig())
	abs, err := resolveExplorerPath(root, req.Path)
	if err != nil || abs == root {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}
	dest := filepath.Join(filepath.Dir(abs), newName)
	if !strings.HasPrefix(dest, root) {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}
	if err := os.Rename(abs, dest); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

// handleExplorerDelete removes a file or a whole directory tree.
func (a *App) handleExplorerDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodDelete {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !a.isAdminReq(r) {
		http.Error(w, "admin only", http.StatusForbidden)
		return
	}
	var req struct{ Path string }
	relPath := r.URL.Query().Get("path")
	if relPath == "" {
		_ = json.NewDecoder(r.Body).Decode(&req)
		relPath = req.Path
	}
	root := a.explorerRoot(a.snapshotConfig())
	abs, err := resolveExplorerPath(root, relPath)
	if err != nil || abs == root {
		http.Error(w, "refusing to delete the explorer root", http.StatusBadRequest)
		return
	}
	if err := os.RemoveAll(abs); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]string{"status": "removed"})
}

// handleExplorerDownload streams one or more entries back. A single file is
// served directly; a single directory, or more than one entry of any kind,
// is streamed as an on-the-fly zip (never buffered fully in memory, so this
// scales to a whole recordings tree).
func (a *App) handleExplorerDownload(w http.ResponseWriter, r *http.Request) {
	if !a.isAdminReq(r) {
		http.Error(w, "admin only", http.StatusForbidden)
		return
	}
	root := a.explorerRoot(a.snapshotConfig())
	rels := r.URL.Query()["path"]
	if len(rels) == 0 {
		http.Error(w, "path is required", http.StatusBadRequest)
		return
	}
	type target struct {
		abs, name string
		isDir     bool
	}
	targets := make([]target, 0, len(rels))
	for _, rel := range rels {
		abs, err := resolveExplorerPath(root, rel)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		info, err := os.Stat(abs)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		targets = append(targets, target{abs: abs, name: filepath.Base(abs), isDir: info.IsDir()})
	}

	if len(targets) == 1 && !targets[0].isDir {
		w.Header().Set("Content-Disposition", `attachment; filename="`+targets[0].name+`"`)
		http.ServeFile(w, r, targets[0].abs)
		return
	}

	zipName := "download.zip"
	if len(targets) == 1 {
		zipName = targets[0].name + ".zip"
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+zipName+`"`)
	zw := zip.NewWriter(w)
	defer zw.Close()
	for _, t := range targets {
		if err := addToZip(zw, t.abs, t.name); err != nil {
			a.event("warn", fmt.Sprintf("Explorer zip download: %s", err))
			return
		}
	}
}

// addToZip adds path (a file or a directory, walked recursively) to zw under
// archiveName, preserving the directory's internal structure.
func addToZip(zw *zip.Writer, absPath, archiveName string) error {
	return filepath.Walk(absPath, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(absPath, p)
		if err != nil {
			return err
		}
		name := archiveName
		if rel != "." {
			name = path.Join(archiveName, filepath.ToSlash(rel))
		}
		if info.IsDir() {
			if rel != "." {
				_, err := zw.Create(name + "/")
				return err
			}
			return nil
		}
		header, err := zip.FileInfoHeader(info)
		if err != nil {
			return err
		}
		header.Name = name
		header.Method = zip.Deflate
		fw, err := zw.CreateHeader(header)
		if err != nil {
			return err
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(fw, f)
		return err
	})
}

const maxExplorerUploadBytes = 20 << 30 // 20GB - recordings can be large

// handleExplorerUpload accepts one or more files (multipart field "file")
// into the destination directory named by the path query parameter.
func (a *App) handleExplorerUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !a.isAdminReq(r) {
		http.Error(w, "admin only", http.StatusForbidden)
		return
	}
	root := a.explorerRoot(a.snapshotConfig())
	dir, err := resolveExplorerPath(root, r.URL.Query().Get("path"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxExplorerUploadBytes)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		http.Error(w, "file too large or not a valid upload", http.StatusBadRequest)
		return
	}
	files := r.MultipartForm.File["file"]
	if len(files) == 0 {
		http.Error(w, "no files provided", http.StatusBadRequest)
		return
	}
	saved := 0
	for _, fh := range files {
		name, err := sanitizeEntryName(filepath.Base(fh.Filename))
		if err != nil {
			continue
		}
		src, err := fh.Open()
		if err != nil {
			continue
		}
		dest, err := os.Create(filepath.Join(dir, name))
		if err != nil {
			src.Close()
			continue
		}
		_, copyErr := io.Copy(dest, src)
		src.Close()
		dest.Close()
		if copyErr == nil {
			saved++
		}
	}
	writeJSON(w, map[string]any{"status": "ok", "saved": saved, "total": len(files)})
}

// handleExplorerZip bundles selected entries (files and/or directories,
// already in dir) into one new zip file inside the same directory.
func (a *App) handleExplorerZip(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !a.isAdminReq(r) {
		http.Error(w, "admin only", http.StatusForbidden)
		return
	}
	var req struct {
		Path    string   `json:"path"`
		Names   []string `json:"names"`
		ZipName string   `json:"zipName"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Names) == 0 {
		http.Error(w, "invalid request - names is required", http.StatusBadRequest)
		return
	}
	root := a.explorerRoot(a.snapshotConfig())
	dir, err := resolveExplorerPath(root, req.Path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	zipName := strings.TrimSuffix(strings.TrimSpace(req.ZipName), ".zip")
	if zipName == "" {
		zipName = "archive"
	}
	zipName, err = sanitizeEntryName(zipName + ".zip")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	dest := filepath.Join(dir, zipName)
	if _, err := os.Stat(dest); err == nil {
		http.Error(w, "a file with that name already exists", http.StatusConflict)
		return
	}
	f, err := os.Create(dest)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	zw := zip.NewWriter(f)
	var zipErr error
	for _, name := range req.Names {
		name, sanitizeErr := sanitizeEntryName(name)
		if sanitizeErr != nil {
			continue
		}
		abs := filepath.Join(dir, name)
		if !strings.HasPrefix(abs, root) {
			continue
		}
		if _, statErr := os.Stat(abs); statErr != nil {
			continue
		}
		if err := addToZip(zw, abs, name); err != nil {
			zipErr = err
			break
		}
	}
	zw.Close()
	f.Close()
	if zipErr != nil {
		_ = os.Remove(dest)
		http.Error(w, "could not build the zip: "+zipErr.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]string{"status": "ok", "name": zipName})
}

// handleExplorerUnzip extracts a zip file already in the tree into a sibling
// directory named after it (deduplicated if that name is taken), guarding
// against zip-slip by refusing any entry whose cleaned path would land
// outside the destination directory.
func (a *App) handleExplorerUnzip(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !a.isAdminReq(r) {
		http.Error(w, "admin only", http.StatusForbidden)
		return
	}
	var req struct{ Path string }
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	root := a.explorerRoot(a.snapshotConfig())
	zipAbs, err := resolveExplorerPath(root, req.Path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	destDir := uniqueSiblingDir(strings.TrimSuffix(zipAbs, filepath.Ext(zipAbs)))
	if err := extractZip(zipAbs, destDir, root); err != nil {
		_ = os.RemoveAll(destDir)
		http.Error(w, "could not extract that zip: "+err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]string{"status": "ok", "dir": filepath.Base(destDir)})
}

// uniqueSiblingDir returns base, or base-2/base-3/... if base already exists.
func uniqueSiblingDir(base string) string {
	if _, err := os.Stat(base); err != nil {
		return base
	}
	for i := 2; ; i++ {
		candidate := fmt.Sprintf("%s-%d", base, i)
		if _, err := os.Stat(candidate); err != nil {
			return candidate
		}
	}
}

// extractZip extracts zipPath into destDir (created if needed), rejecting
// any entry that would escape either destDir (zip-slip) or the overall
// explorer root.
func extractZip(zipPath, destDir, root string) error {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return err
	}
	defer zr.Close()
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return err
	}
	destDir = filepath.Clean(destDir)
	for _, f := range zr.File {
		name := filepath.FromSlash(path.Clean("/" + f.Name))
		target := filepath.Join(destDir, name)
		if !strings.HasPrefix(target, destDir+string(os.PathSeparator)) && target != destDir {
			return fmt.Errorf("zip entry %q would escape the destination directory", f.Name)
		}
		if !strings.HasPrefix(target, root) {
			return fmt.Errorf("zip entry %q would escape the explorer root", f.Name)
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		out, err := os.Create(target)
		if err != nil {
			rc.Close()
			return err
		}
		_, copyErr := io.Copy(out, rc)
		rc.Close()
		out.Close()
		if copyErr != nil {
			return copyErr
		}
	}
	return nil
}

// urlDownloadDest builds a destination filename for handleExplorerFetchURL,
// preferring an HTTP response's Content-Disposition filename, then falling
// back to the URL's own path basename.
func urlDownloadDest(resp *http.Response, rawURL string) string {
	if cd := resp.Header.Get("Content-Disposition"); cd != "" {
		if _, params, err := parseContentDisposition(cd); err == nil {
			if name := params["filename"]; name != "" {
				if clean, err := sanitizeEntryName(filepath.Base(name)); err == nil {
					return clean
				}
			}
		}
	}
	if u, err := url.Parse(rawURL); err == nil {
		if base := path.Base(u.Path); base != "" && base != "/" && base != "." {
			if clean, err := sanitizeEntryName(base); err == nil {
				return clean
			}
		}
	}
	return "download-" + time.Now().Format("20060102-150405")
}

// parseContentDisposition is a tiny, dependency-free parser for the one
// thing we need out of a Content-Disposition header: a quoted or bare
// filename parameter.
func parseContentDisposition(header string) (string, map[string]string, error) {
	parts := strings.Split(header, ";")
	params := map[string]string{}
	for _, p := range parts[1:] {
		p = strings.TrimSpace(p)
		kv := strings.SplitN(p, "=", 2)
		if len(kv) != 2 {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(kv[0]))
		val := strings.Trim(strings.TrimSpace(kv[1]), `"`)
		params[key] = val
	}
	disposition := ""
	if len(parts) > 0 {
		disposition = strings.TrimSpace(parts[0])
	}
	return disposition, params, nil
}
