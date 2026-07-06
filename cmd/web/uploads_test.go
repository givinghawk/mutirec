package main

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// smallestValidPNG is a 1x1 transparent PNG - enough for http.DetectContentType
// to sniff it as image/png without needing a real image library.
var smallestValidPNG = []byte{
	0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x00, 0x00, 0x0d,
	0x49, 0x48, 0x44, 0x52, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
	0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4, 0x89, 0x00, 0x00, 0x00,
	0x0a, 0x49, 0x44, 0x41, 0x54, 0x78, 0x9c, 0x63, 0x00, 0x01, 0x00, 0x00,
	0x05, 0x00, 0x01, 0x0d, 0x0a, 0x2d, 0xb4, 0x00, 0x00, 0x00, 0x00, 0x49,
	0x45, 0x4e, 0x44, 0xae, 0x42, 0x60, 0x82,
}

func multipartImageRequest(t *testing.T, field, filename string, data []byte) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	part, err := w.CreateFormFile(field, filename)
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	if _, err := part.Write(data); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/uploads/image", &buf)
	req.Header.Set("Content-Type", w.FormDataContentType())
	return req
}

func TestReadImageUploadAcceptsPNG(t *testing.T) {
	req := multipartImageRequest(t, "image", "logo.png", smallestValidPNG)
	data, ext, err := readImageUpload(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ext != ".png" {
		t.Fatalf("expected .png, got %q", ext)
	}
	if !bytes.Equal(data, smallestValidPNG) {
		t.Fatal("returned data doesn't match the uploaded bytes")
	}
}

func TestReadImageUploadRejectsUnknownType(t *testing.T) {
	req := multipartImageRequest(t, "image", "not-an-image.bin", []byte("just some plain text bytes, not an image"))
	if _, _, err := readImageUpload(req); err == nil {
		t.Fatal("expected an unsupported-type error for non-image content")
	}
}

func TestReadImageUploadRejectsEmptyFile(t *testing.T) {
	req := multipartImageRequest(t, "image", "empty.png", nil)
	if _, _, err := readImageUpload(req); err == nil {
		t.Fatal("expected an error for an empty upload")
	}
}

func TestReadImageUploadRejectsMissingField(t *testing.T) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	_ = w.Close()
	req := httptest.NewRequest(http.MethodPost, "/api/uploads/image", &buf)
	req.Header.Set("Content-Type", w.FormDataContentType())
	if _, _, err := readImageUpload(req); err == nil {
		t.Fatal("expected an error when no 'image' field is present")
	}
}

func newTestUploadsApp(t *testing.T) *App {
	t.Helper()
	dir := t.TempDir()
	return &App{config: filepath.Join(dir, "config.json")}
}

func TestThumbKeyStableAndDistinct(t *testing.T) {
	a := thumbKey("BLUE/DJ Set.mkv")
	b := thumbKey("BLUE/DJ Set.mkv")
	c := thumbKey("RED/DJ Set.mkv")
	if a != b {
		t.Fatal("thumbKey should be deterministic for the same path")
	}
	if a == c {
		t.Fatal("thumbKey should differ for different paths")
	}
}

func TestFindAndRemoveThumbnail(t *testing.T) {
	app := newTestUploadsApp(t)
	rel := "BLUE/DJ Set.mkv"
	if _, ok := app.findThumbnail(rel); ok {
		t.Fatal("expected no thumbnail before one is written")
	}
	dir := app.thumbsDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	path := filepath.Join(dir, thumbKey(rel)+".jpg")
	if err := os.WriteFile(path, []byte("fake jpeg bytes"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	got, ok := app.findThumbnail(rel)
	if !ok || got != path {
		t.Fatalf("expected to find %q, got %q ok=%v", path, got, ok)
	}
	// A different recording's key must not collide.
	if _, ok := app.findThumbnail("RED/DJ Set.mkv"); ok {
		t.Fatal("unrelated recording should not have a thumbnail")
	}
	app.removeThumbnail(rel)
	if _, ok := app.findThumbnail(rel); ok {
		t.Fatal("expected thumbnail to be gone after removeThumbnail")
	}
}

func TestGenerateThumbnailSkipsAudioOnly(t *testing.T) {
	app := newTestUploadsApp(t)
	if app.generateThumbnail("/does/not/matter.mp3", "chan/track.mp3", true) {
		t.Fatal("expected generateThumbnail to return false immediately for audio-only sources")
	}
	if _, ok := app.findThumbnail("chan/track.mp3"); ok {
		t.Fatal("no thumbnail file should have been created for an audio-only source")
	}
}

func TestGenerateThumbnailMissingFile(t *testing.T) {
	app := newTestUploadsApp(t)
	if app.generateThumbnail(filepath.Join(t.TempDir(), "missing.mp4"), "chan/missing.mp4", false) {
		t.Fatal("expected generateThumbnail to fail for a file that doesn't exist")
	}
}
