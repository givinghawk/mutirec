package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ============================================================================
// Image uploads (branding logos/covers) and recording thumbnails.
//
// Every place that used to ask for an image URL (app logo, Organisation logo,
// Festival logo, Event cover art) now accepts a direct file upload instead;
// the uploaded file is stored locally and served back from this instance, and
// the same URL-shaped string field in the config just points at
// "/uploads/<hash>.<ext>" rather than an external address.
//
// Recording thumbnails are a separate, content-addressed-by-recording-path
// store: a video recording gets one generated automatically (a single frame
// grabbed from a random point, away from the intro/outro), audio-only
// recordings get none automatically, and either can have a thumbnail
// uploaded/replaced by hand from the Organize modal.
// ============================================================================

const maxImageUploadBytes = 12 << 20 // 12MB

var allowedImageExt = map[string]string{
	"image/jpeg": ".jpg",
	"image/png":  ".png",
	"image/webp": ".webp",
	"image/gif":  ".gif",
}

// uploadsDir stores general branding images (logos, cover art), named by
// content hash so re-uploading the same image is a no-op and unrelated
// uploads never collide.
func (a *App) uploadsDir() string {
	return filepath.Join(filepath.Dir(a.config), "uploads")
}

// thumbsDir stores recording thumbnails, named by a hash of the recording's
// path (not content, since it's regenerable/replaceable per-recording) -
// deliberately kept out of FinishedDir so the recordings scanner never picks
// a thumbnail file up as if it were a recording.
func (a *App) thumbsDir() string {
	return filepath.Join(filepath.Dir(a.config), "thumbnails")
}

// readImageUpload validates and returns the raw bytes + matching extension
// for the "image" multipart field of a request, capped at
// maxImageUploadBytes and restricted to a handful of common image types
// (sniffed from content, not trusted from the filename or a client-supplied
// Content-Type).
func readImageUpload(r *http.Request) (data []byte, ext string, err error) {
	if err := r.ParseMultipartForm(maxImageUploadBytes); err != nil {
		return nil, "", fmt.Errorf("file too large or not a valid upload")
	}
	file, _, err := r.FormFile("image")
	if err != nil {
		return nil, "", fmt.Errorf("no image file provided")
	}
	defer file.Close()
	data, err = io.ReadAll(io.LimitReader(file, maxImageUploadBytes+1))
	if err != nil {
		return nil, "", err
	}
	if len(data) == 0 {
		return nil, "", fmt.Errorf("empty file")
	}
	if int64(len(data)) > maxImageUploadBytes {
		return nil, "", fmt.Errorf("image is larger than %s", formatBytesGo(maxImageUploadBytes))
	}
	sniff := data
	if len(sniff) > 512 {
		sniff = sniff[:512]
	}
	ext, ok := allowedImageExt[http.DetectContentType(sniff)]
	if !ok {
		return nil, "", fmt.Errorf("unsupported image type - use JPEG, PNG, WebP, or GIF")
	}
	return data, ext, nil
}

// handleImageUpload accepts a branding image (app logo, Organisation/Festival
// logo, Event cover) and stores it content-addressed under uploadsDir,
// returning the URL to save on whichever config field it's for.
func (a *App) handleImageUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !a.isAdminReq(r) {
		http.Error(w, "admin only", http.StatusForbidden)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxImageUploadBytes+1<<20)
	data, ext, err := readImageUpload(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	sum := sha256.Sum256(data)
	name := hex.EncodeToString(sum[:]) + ext
	dir := a.uploadsDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	dest := filepath.Join(dir, name)
	if _, err := os.Stat(dest); err != nil {
		if err := os.WriteFile(dest, data, 0o644); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	writeJSON(w, map[string]string{"url": "/uploads/" + name})
}

// thumbKey derives a stable, filesystem-safe identifier for a recording's
// thumbnail file from its path relative to FinishedDir.
func thumbKey(relPath string) string {
	sum := sha256.Sum256([]byte(relPath))
	return hex.EncodeToString(sum[:])
}

// findThumbnail locates an existing thumbnail for a recording regardless of
// its stored extension (auto-generated ones are always .jpg; uploaded ones
// keep whatever format was sniffed).
func (a *App) findThumbnail(relPath string) (string, bool) {
	matches, _ := filepath.Glob(filepath.Join(a.thumbsDir(), thumbKey(relPath)+".*"))
	if len(matches) == 0 {
		return "", false
	}
	return matches[0], true
}

func (a *App) removeThumbnail(relPath string) {
	matches, _ := filepath.Glob(filepath.Join(a.thumbsDir(), thumbKey(relPath)+".*"))
	for _, m := range matches {
		_ = os.Remove(m)
	}
}

// handleRecordingThumbnail serves (GET), uploads/replaces (POST), or removes
// (DELETE) the thumbnail for one recording, addressed by its path relative to
// FinishedDir.
func (a *App) handleRecordingThumbnail(w http.ResponseWriter, r *http.Request) {
	relPath := filepath.ToSlash(strings.TrimSpace(r.URL.Query().Get("path")))
	if relPath == "" || strings.Contains(relPath, "..") {
		http.NotFound(w, r)
		return
	}
	switch r.Method {
	case http.MethodGet:
		p, ok := a.findThumbnail(relPath)
		if !ok {
			http.NotFound(w, r)
			return
		}
		// Thumbnails change when a set is re-generated or re-uploaded but
		// keep the same URL (it's derived from the recording path, not
		// content) - so the browser must always revalidate rather than
		// cache the old image indefinitely.
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeFile(w, r, p)
	case http.MethodPost:
		if !a.isAdminReq(r) {
			http.Error(w, "admin only", http.StatusForbidden)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxImageUploadBytes+1<<20)
		data, ext, err := readImageUpload(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		dir := a.thumbsDir()
		if err := os.MkdirAll(dir, 0o755); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		a.removeThumbnail(relPath)
		dest := filepath.Join(dir, thumbKey(relPath)+ext)
		if err := os.WriteFile(dest, data, 0o644); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]string{"status": "ok"})
	case http.MethodDelete:
		if !a.isAdminReq(r) {
			http.Error(w, "admin only", http.StatusForbidden)
			return
		}
		a.removeThumbnail(relPath)
		writeJSON(w, map[string]string{"status": "removed"})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleRecordingThumbnailRegenerate re-grabs a fresh random-time frame for a
// video recording that already exists in the library - lets a user reroll an
// auto-generated thumbnail they don't like, or backfill one for a recording
// made before this feature existed.
func (a *App) handleRecordingThumbnailRegenerate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !a.isAdminReq(r) {
		http.Error(w, "admin only", http.StatusForbidden)
		return
	}
	relPath := filepath.ToSlash(strings.TrimSpace(r.URL.Query().Get("path")))
	if relPath == "" || strings.Contains(relPath, "..") {
		http.Error(w, "path is required", http.StatusBadRequest)
		return
	}
	cfg := a.snapshotConfig()
	abs := filepath.Clean(filepath.Join(cfg.Settings.FinishedDir, filepath.FromSlash(relPath)))
	if !strings.HasPrefix(abs, filepath.Clean(cfg.Settings.FinishedDir)+string(os.PathSeparator)) {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}
	if _, err := os.Stat(abs); err != nil {
		http.NotFound(w, r)
		return
	}
	if !a.generateThumbnail(abs, relPath, false) {
		http.Error(w, "could not generate a thumbnail - is this a video file with a readable video stream?", http.StatusUnprocessableEntity)
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

// generateThumbnail grabs a single frame from a random point in a finished
// video recording (skipping the first/last 10% to dodge black intro/outro
// frames or a stage's holding slate) and stores it as that recording's
// thumbnail. Audio-only sources get no automatic thumbnail at all - silently
// returns instead, since a manual upload from the Organize modal is always
// available. Returns whether a thumbnail was produced.
func (a *App) generateThumbnail(finalPath, relPath string, audioOnly bool) bool {
	if audioOnly {
		return false
	}
	dur, err := probeMediaDuration(finalPath)
	if err != nil || dur <= 3*time.Second {
		return false
	}
	lo, hi := dur.Seconds()*0.1, dur.Seconds()*0.9
	if hi <= lo {
		lo, hi = 0, dur.Seconds()
	}
	at := lo + rand.Float64()*(hi-lo)

	dir := a.thumbsDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false
	}
	a.removeThumbnail(relPath)
	dest := filepath.Join(dir, thumbKey(relPath)+".jpg")
	cmd := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
		"-ss", strconv.FormatFloat(at, 'f', 2, 64), "-i", finalPath,
		"-frames:v", "1", "-vf", "scale=480:-2", dest)
	if err := cmd.Run(); err != nil {
		_ = os.Remove(dest)
		return false
	}
	if info, err := os.Stat(dest); err != nil || info.Size() == 0 {
		_ = os.Remove(dest)
		return false
	}
	return true
}

// probeMediaDuration reads a media file's duration via ffprobe.
func probeMediaDuration(path string) (time.Duration, error) {
	out, err := exec.Command("ffprobe", "-v", "error", "-show_entries", "format=duration",
		"-of", "default=noprint_wrappers=1:nokey=1", path).Output()
	if err != nil {
		return 0, err
	}
	secs, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	if err != nil {
		return 0, err
	}
	return time.Duration(secs * float64(time.Second)), nil
}
