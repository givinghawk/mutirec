package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// withAdminUser stashes an admin User on the request context the same way
// requireAuth does after a real session lookup, so handlers gated by
// isAdminReq can be exercised directly in tests without going through login.
func withAdminUser(r *http.Request) *http.Request {
	u := User{ID: "test-admin", Username: "admin", Role: RoleAdmin}
	return r.WithContext(context.WithValue(r.Context(), userContextKey{}, u))
}

func TestIsSidecarPath(t *testing.T) {
	cases := map[string]bool{
		"BLUE/DJ Set.mkv":               false,
		"BLUE/DJ Set.nfo":               true,
		"BLUE/DJ Set.NFO":               true,
		"BLUE/DJ Set.timecode.json":     true,
		"BLUE/DJ Set.markers.json":      true,
		"BLUE/DJ Set.mkv.timecode.json": true,
		"BLUE/some-other-file.json":     false,
	}
	for p, want := range cases {
		if got := isSidecarPath(p); got != want {
			t.Errorf("isSidecarPath(%q) = %v, want %v", p, got, want)
		}
	}
}

func TestSidecarPathsSwapExtension(t *testing.T) {
	final := "/data/finished/BLUE/DJ Set.20220623-180005.mkv"
	if got, want := sidecarTimecodePath(final), "/data/finished/BLUE/DJ Set.20220623-180005.timecode.json"; got != want {
		t.Errorf("sidecarTimecodePath = %q, want %q", got, want)
	}
	if got, want := sidecarMarkersPath(final), "/data/finished/BLUE/DJ Set.20220623-180005.markers.json"; got != want {
		t.Errorf("sidecarMarkersPath = %q, want %q", got, want)
	}
}

func TestParseFrameRate(t *testing.T) {
	cases := []struct {
		in   string
		want float64
	}{
		{"30/1", 30},
		{"30000/1001", 29.97002997002997},
		{"", 0},
		{"garbage", 0},
		{"1/0", 0},
	}
	for _, c := range cases {
		if got := parseFrameRate(c.in); got != c.want {
			t.Errorf("parseFrameRate(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestBuildSidecarFromProbe(t *testing.T) {
	probe := ffprobeOutput{
		Streams: []ffprobeStreamInfo{
			{CodecType: "video", CodecName: "h264", Width: 1920, Height: 1080, RFrameRate: "30/1"},
			{CodecType: "audio", CodecName: "aac", Channels: 2, SampleRate: "48000"},
		},
		Format: ffprobeFormatInfo{Duration: "3661.5", Size: "123456789", BitRate: "256000"},
	}
	var sc RecordingSidecar
	buildSidecarFromProbe(&sc, probe)

	if sc.DurationSec != 3661.5 {
		t.Errorf("DurationSec = %v, want 3661.5", sc.DurationSec)
	}
	if sc.SizeBytes != 123456789 {
		t.Errorf("SizeBytes = %v, want 123456789", sc.SizeBytes)
	}
	if sc.BitrateBps != 256000 {
		t.Errorf("BitrateBps = %v, want 256000", sc.BitrateBps)
	}
	if sc.VideoCodec != "h264" || sc.Width != 1920 || sc.Height != 1080 || sc.FrameRate != 30 {
		t.Errorf("video fields wrong: %+v", sc)
	}
	if sc.AudioCodec != "aac" || sc.AudioChannels != 2 || sc.SampleRateHz != 48000 {
		t.Errorf("audio fields wrong: %+v", sc)
	}
}

func TestBuildSidecarFromProbeAudioOnly(t *testing.T) {
	probe := ffprobeOutput{
		Streams: []ffprobeStreamInfo{
			{CodecType: "audio", CodecName: "opus", Channels: 1, SampleRate: "44100"},
		},
		Format: ffprobeFormatInfo{Duration: "120"},
	}
	var sc RecordingSidecar
	buildSidecarFromProbe(&sc, probe)
	if sc.VideoCodec != "" || sc.Width != 0 || sc.Height != 0 {
		t.Errorf("expected no video fields for an audio-only probe, got %+v", sc)
	}
	if sc.AudioCodec != "opus" || sc.AudioChannels != 1 || sc.SampleRateHz != 44100 {
		t.Errorf("audio fields wrong: %+v", sc)
	}
}

func TestWaveformKeyStableAndDistinctFromThumbKey(t *testing.T) {
	app := newTestUploadsApp(t)
	rel := "BLUE/DJ Set.mkv"
	a := app.waveformKey(rel)
	b := app.waveformKey(rel)
	if a != b {
		t.Fatal("waveformKey should be deterministic for the same path")
	}
	if a == thumbKey(rel) {
		t.Fatal("waveformKey should not collide with thumbKey for the same recording")
	}
}

func TestGenerateWaveformMissingFFmpegFails(t *testing.T) {
	app := newTestUploadsApp(t)
	// No ffmpeg guaranteed on PATH in the test sandbox; regardless, a
	// nonexistent input file must never produce a "successful" waveform.
	if app.generateWaveform(filepath.Join(t.TempDir(), "missing.mkv"), "chan/missing.mkv") {
		t.Fatal("expected generateWaveform to fail for a nonexistent input file")
	}
}

func TestResolveRecordingPathRejectsEscape(t *testing.T) {
	dir := t.TempDir()
	finishedDir := filepath.Join(dir, "finished")
	if err := os.MkdirAll(finishedDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	app := &App{config: filepath.Join(dir, "config.json"), cfg: AppConfig{Settings: Settings{FinishedDir: finishedDir}}}

	if _, ok := app.resolveRecordingPath("../../etc/passwd"); ok {
		t.Fatal("expected a path-escape attempt to be rejected")
	}
	if abs, ok := app.resolveRecordingPath("BLUE/set.mkv"); !ok || filepath.Dir(abs) != filepath.Join(finishedDir, "BLUE") {
		t.Fatalf("expected a normal relative path to resolve under FinishedDir, got %q ok=%v", abs, ok)
	}
}

func TestHandleRecordingTimecodeGetMissingIs404(t *testing.T) {
	dir := t.TempDir()
	finishedDir := filepath.Join(dir, "finished")
	if err := os.MkdirAll(finishedDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	app := &App{config: filepath.Join(dir, "config.json"), cfg: AppConfig{Settings: Settings{FinishedDir: finishedDir}}}

	req := httptest.NewRequest(http.MethodGet, "/api/recordings/timecode?path=BLUE/set.mkv", nil)
	w := httptest.NewRecorder()
	app.handleRecordingTimecode(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for a recording with no sidecar yet, got %d", w.Code)
	}
}

func TestHandleRecordingTimecodePostManualBackfill(t *testing.T) {
	dir := t.TempDir()
	finishedDir := filepath.Join(dir, "finished")
	if err := os.MkdirAll(filepath.Join(finishedDir, "BLUE"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	recPath := filepath.Join(finishedDir, "BLUE", "set.mkv")
	if err := os.WriteFile(recPath, []byte("fake media bytes"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	app := &App{config: filepath.Join(dir, "config.json"), cfg: AppConfig{Settings: Settings{FinishedDir: finishedDir}}}

	body, _ := json.Marshal(map[string]string{
		"startedAt":  "2022-06-23T18:00:05Z",
		"timezone":   "Europe/Amsterdam",
		"timeSource": "manual",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/recordings/timecode?path=BLUE/set.mkv", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = withAdminUser(req)
	w := httptest.NewRecorder()
	app.handleRecordingTimecode(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var sc RecordingSidecar
	if err := json.Unmarshal(w.Body.Bytes(), &sc); err != nil {
		t.Fatalf("response wasn't valid JSON: %v", err)
	}
	if !sc.StartedAt.Equal(time.Date(2022, 6, 23, 18, 0, 5, 0, time.UTC)) {
		t.Errorf("StartedAt = %s, want 2022-06-23T18:00:05Z", sc.StartedAt)
	}
	if sc.Timezone != "Europe/Amsterdam" || sc.TimeSource != "manual" {
		t.Errorf("unexpected metadata: %+v", sc)
	}

	// GET should now return the sidecar just written.
	getReq := httptest.NewRequest(http.MethodGet, "/api/recordings/timecode?path=BLUE/set.mkv", nil)
	getW := httptest.NewRecorder()
	app.handleRecordingTimecode(getW, getReq)
	if getW.Code != http.StatusOK {
		t.Fatalf("expected 200 on GET after backfill, got %d", getW.Code)
	}
}

func TestIsAudioOnlyExt(t *testing.T) {
	for _, ext := range []string{".mp3", ".MP3", ".m4a", ".flac", ".opus"} {
		if !isAudioOnlyExt(ext) {
			t.Errorf("isAudioOnlyExt(%q) = false, want true", ext)
		}
	}
	for _, ext := range []string{".mkv", ".mp4", ""} {
		if isAudioOnlyExt(ext) {
			t.Errorf("isAudioOnlyExt(%q) = true, want false", ext)
		}
	}
}

func TestCutterExportPathWithLibraryEvent(t *testing.T) {
	ev := &LibraryEvent{Name: "Neon Beat", Year: 2026}
	m := CutterMarker{Channel: "BLUE", Artist: "DJ Vertex", Start: "2026-06-25T19:00:00Z"}
	got := cutterExportPath("BLUE/set.mkv", m, ev, ".mkv")
	want := "NeonBeat/2026/BLUE/sets/DJVertex_BLUE_2026-06-25.mkv"
	if got != want {
		t.Errorf("cutterExportPath = %q, want %q", got, want)
	}
}

func TestCutterExportPathWithoutLibraryEvent(t *testing.T) {
	m := CutterMarker{Name: "Opening Set"}
	got := cutterExportPath("SomeChannel/set.mp3", m, nil, ".mp3")
	if !strings.HasPrefix(got, "SomeChannel/sets/OpeningSet_SomeChannel_") {
		t.Errorf("cutterExportPath without an event = %q, want prefix SomeChannel/sets/OpeningSet_SomeChannel_", got)
	}
	if !strings.HasSuffix(got, ".mp3") {
		t.Errorf("cutterExportPath should keep the source extension, got %q", got)
	}
}

func TestHandleCutterMarkersRoundTrip(t *testing.T) {
	dir := t.TempDir()
	finishedDir := filepath.Join(dir, "finished")
	if err := os.MkdirAll(filepath.Join(finishedDir, "BLUE"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	app := &App{config: filepath.Join(dir, "config.json"), cfg: AppConfig{Settings: Settings{FinishedDir: finishedDir}}}

	// No markers saved yet - GET should return an empty array, not an error.
	getReq := httptest.NewRequest(http.MethodGet, "/api/cutter/markers?path=BLUE/set.mkv", nil)
	getW := httptest.NewRecorder()
	app.handleCutterMarkers(getW, getReq)
	if getW.Code != http.StatusOK {
		t.Fatalf("expected 200 for markers GET with nothing saved yet, got %d", getW.Code)
	}
	var empty []CutterMarker
	if err := json.Unmarshal(getW.Body.Bytes(), &empty); err != nil || len(empty) != 0 {
		t.Fatalf("expected an empty array, got %s (err %v)", getW.Body.String(), err)
	}

	markers := []CutterMarker{
		{ID: "m1", OffsetSec: 0, Name: "First"},
		{ID: "m2", OffsetSec: 1800, Name: "Second"},
	}
	body, _ := json.Marshal(markers)
	putReq := httptest.NewRequest(http.MethodPut, "/api/cutter/markers?path=BLUE/set.mkv", bytes.NewReader(body))
	putReq = withAdminUser(putReq)
	putW := httptest.NewRecorder()
	app.handleCutterMarkers(putW, putReq)
	if putW.Code != http.StatusOK {
		t.Fatalf("expected 200 on markers PUT, got %d: %s", putW.Code, putW.Body.String())
	}

	getReq2 := httptest.NewRequest(http.MethodGet, "/api/cutter/markers?path=BLUE/set.mkv", nil)
	getW2 := httptest.NewRecorder()
	app.handleCutterMarkers(getW2, getReq2)
	var got []CutterMarker
	if err := json.Unmarshal(getW2.Body.Bytes(), &got); err != nil {
		t.Fatalf("GET after PUT wasn't valid JSON: %v", err)
	}
	if len(got) != 2 || got[0].Name != "First" || got[1].Name != "Second" {
		t.Fatalf("markers didn't round-trip: %+v", got)
	}
}

func TestHandleCutterMarkersPutRequiresAdmin(t *testing.T) {
	dir := t.TempDir()
	finishedDir := filepath.Join(dir, "finished")
	if err := os.MkdirAll(finishedDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	app := &App{config: filepath.Join(dir, "config.json"), cfg: AppConfig{Settings: Settings{FinishedDir: finishedDir}}}

	body, _ := json.Marshal([]CutterMarker{{ID: "m1"}})
	req := httptest.NewRequest(http.MethodPut, "/api/cutter/markers?path=BLUE/set.mkv", bytes.NewReader(body))
	w := httptest.NewRecorder()
	app.handleCutterMarkers(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for an unauthenticated markers PUT, got %d", w.Code)
	}
}
