package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ============================================================================
// Set Cutter, part 1: sidecar timecode + metadata + waveform.
//
// Every finished recording gets two sidecar files written next to it (same
// atomic-after-rename timing as writeNFO):
//
//   <recording>.timecode.json - a wall-clock anchor (startedAt, corrected
//     against worldtimeapi.org where reachable) plus as much ffprobe-derived
//     media metadata as can be captured, so a later Set Cutter pass can map
//     any offset into the file straight onto a timetable slot without
//     re-probing the file each time.
//
//   thumbnails/<hash>-wave.png - a waveform image (ffmpeg showwavespic),
//     generated for every recording (audio-only or video - the Set Cutter
//     always cuts by audio waveform, even for video sources) and cached
//     alongside thumbnails since it's keyed the same way (a hash of the
//     recording's path relative to FinishedDir).
//
// Both are best-effort: a failure to reach worldtimeapi.org or to generate a
// waveform never blocks or fails the recording itself.
// ============================================================================

// RecordingSidecar is the on-disk shape of a recording's ".timecode.json".
// Deliberately kept separate from the human-readable ".nfo" file - this one
// is machine-readable timing/media data consumed by the Set Cutter.
type RecordingSidecar struct {
	Version    int       `json:"version"`
	StartedAt  time.Time `json:"startedAt"`
	FinishedAt time.Time `json:"finishedAt,omitempty"`
	Timezone   string    `json:"timezone,omitempty"`
	OffsetMs   int64     `json:"offsetMs"`
	TimeSource string    `json:"timeSource"`

	SourceID   string `json:"sourceId,omitempty"`
	SourceName string `json:"sourceName,omitempty"`
	SourceType string `json:"sourceType,omitempty"`
	SourceURL  string `json:"sourceUrl,omitempty"`
	Quality    string `json:"quality,omitempty"`

	DurationSec        float64 `json:"durationSec,omitempty"`
	SizeBytes          int64   `json:"sizeBytes,omitempty"`
	Container          string  `json:"container,omitempty"`
	VideoCodec         string  `json:"videoCodec,omitempty"`
	AudioCodec         string  `json:"audioCodec,omitempty"`
	Width              int     `json:"width,omitempty"`
	Height             int     `json:"height,omitempty"`
	FrameRate          float64 `json:"frameRate,omitempty"`
	AudioChannels      int     `json:"audioChannels,omitempty"`
	SampleRateHz       int     `json:"sampleRateHz,omitempty"`
	BitrateBps         int64   `json:"bitrateBps,omitempty"`
	AudioOnly          bool    `json:"audioOnly,omitempty"`
	Transcoded         bool    `json:"transcoded,omitempty"`
	LoudnessNormalized bool    `json:"loudnessNormalized,omitempty"`
	HardwareAccel      string  `json:"hardwareAccel,omitempty"`
	RecorderVersion    string  `json:"recorderVersion,omitempty"`

	WaveformAvailable bool `json:"waveformAvailable"`
}

// isSidecarPath reports whether p is a sidecar file the recordings scanner
// should skip rather than surface as a recording in its own right (mirrors
// the existing ".nfo" exclusion, extended to cover the newer sidecar kinds).
func isSidecarPath(p string) bool {
	lower := strings.ToLower(p)
	for _, suffix := range []string{".nfo", ".timecode.json", ".markers.json", ".timetable.json"} {
		if strings.HasSuffix(lower, suffix) {
			return true
		}
	}
	return false
}

// sidecarTimecodePath derives the ".timecode.json" path for a recording,
// same "swap the extension" convention writeNFO uses for ".nfo".
func sidecarTimecodePath(finalPath string) string {
	return strings.TrimSuffix(finalPath, filepath.Ext(finalPath)) + ".timecode.json"
}

// sidecarMarkersPath derives the ".markers.json" path for a recording (Set
// Cutter part 2), kept alongside the other sidecar helpers since it follows
// the identical naming convention.
func sidecarMarkersPath(finalPath string) string {
	return strings.TrimSuffix(finalPath, filepath.Ext(finalPath)) + ".markers.json"
}

// sidecarTimetablePath derives the ".timetable.json" path for a recording -
// a snapshot of the timetable active at record-finish time, written for
// sources attached to an event (Source.FestivalID set) so the Set Cutter can
// auto-detect set boundaries without requiring the recording to already be
// organized into a LibraryEvent with its own archived timetable.
func sidecarTimetablePath(finalPath string) string {
	return strings.TrimSuffix(finalPath, filepath.Ext(finalPath)) + ".timetable.json"
}

// readTimetableSidecar reads back a ".timetable.json" sidecar written by
// saveEventTimetableSidecar.
func readTimetableSidecar(path string) ([]StageSchedule, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var tt []StageSchedule
	if err := json.Unmarshal(data, &tt); err != nil {
		return nil, err
	}
	return tt, nil
}

// worldTimeTimeout bounds the one-shot correction request so a slow or
// unreachable worldtimeapi.org never meaningfully delays a recording's start.
const worldTimeTimeout = 3 * time.Second

// timeCorrection asks worldtimeapi.org for the current UTC instant and
// returns how far the local clock is off (positive = local clock is behind)
// plus which source produced the result. Failure of any kind - offline,
// rate-limited, blocked by a firewall - falls back to "system-ntp" silently;
// a well-configured server clock is accurate enough on its own for festival
// timeslots (±30s is fine), so this is a nice-to-have correction, not a
// dependency.
func timeCorrection() (offsetMs int64, source string) {
	ctx, cancel := context.WithTimeout(context.Background(), worldTimeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://worldtimeapi.org/api/ip", nil)
	if err != nil {
		return 0, "system-ntp"
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, "system-ntp"
	}
	defer resp.Body.Close()
	sampledAt := time.Now()
	var body struct {
		UnixTime int64 `json:"unixtime"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || body.UnixTime == 0 {
		return 0, "system-ntp"
	}
	trueTime := time.Unix(body.UnixTime, 0)
	return trueTime.Sub(sampledAt).Milliseconds(), "worldtimeapi.org"
}

// ffprobeStreamInfo is the subset of ffprobe's per-stream JSON this app reads.
type ffprobeStreamInfo struct {
	CodecType  string `json:"codec_type"`
	CodecName  string `json:"codec_name"`
	Width      int    `json:"width"`
	Height     int    `json:"height"`
	Channels   int    `json:"channels"`
	SampleRate string `json:"sample_rate"`
	RFrameRate string `json:"r_frame_rate"`
}

type ffprobeFormatInfo struct {
	Duration string `json:"duration"`
	Size     string `json:"size"`
	BitRate  string `json:"bit_rate"`
}

type ffprobeOutput struct {
	Streams []ffprobeStreamInfo `json:"streams"`
	Format  ffprobeFormatInfo   `json:"format"`
}

// probeMediaInfo runs a single ffprobe pass and returns stream/format info,
// used to fill out a recording's sidecar with as much media metadata as can
// be captured without decoding the whole file.
func probeMediaInfo(path string) (ffprobeOutput, error) {
	out, err := exec.Command("ffprobe", "-v", "error", "-show_entries",
		"stream=codec_type,codec_name,width,height,channels,sample_rate,r_frame_rate:format=duration,size,bit_rate",
		"-of", "json", path).Output()
	if err != nil {
		return ffprobeOutput{}, err
	}
	var res ffprobeOutput
	if err := json.Unmarshal(out, &res); err != nil {
		return ffprobeOutput{}, err
	}
	return res, nil
}

// parseFrameRate turns ffprobe's "num/den" r_frame_rate string into a float.
func parseFrameRate(s string) float64 {
	parts := strings.SplitN(s, "/", 2)
	if len(parts) != 2 {
		return 0
	}
	num, err1 := strconv.ParseFloat(parts[0], 64)
	den, err2 := strconv.ParseFloat(parts[1], 64)
	if err1 != nil || err2 != nil || den == 0 {
		return 0
	}
	return num / den
}

// buildSidecarFromProbe fills in the media-metadata fields of a
// RecordingSidecar from one probeMediaInfo() result. Any field ffprobe
// doesn't return (e.g. a stream-copied source with no video) is simply left
// at its zero value.
func buildSidecarFromProbe(sc *RecordingSidecar, probe ffprobeOutput) {
	if d, err := strconv.ParseFloat(strings.TrimSpace(probe.Format.Duration), 64); err == nil {
		sc.DurationSec = d
	}
	if sz, err := strconv.ParseInt(strings.TrimSpace(probe.Format.Size), 10, 64); err == nil {
		sc.SizeBytes = sz
	}
	if br, err := strconv.ParseInt(strings.TrimSpace(probe.Format.BitRate), 10, 64); err == nil {
		sc.BitrateBps = br
	}
	for _, s := range probe.Streams {
		switch s.CodecType {
		case "video":
			if sc.VideoCodec == "" {
				sc.VideoCodec = s.CodecName
				sc.Width = s.Width
				sc.Height = s.Height
				sc.FrameRate = parseFrameRate(s.RFrameRate)
			}
		case "audio":
			if sc.AudioCodec == "" {
				sc.AudioCodec = s.CodecName
				sc.AudioChannels = s.Channels
				if sr, err := strconv.Atoi(strings.TrimSpace(s.SampleRate)); err == nil {
					sc.SampleRateHz = sr
				}
			}
		}
	}
}

// waveformKey derives the content-addressed (by relative path) filename for
// a recording's cached waveform PNG, stored in thumbsDir alongside thumbnail
// images since both are keyed the same way and neither belongs under
// FinishedDir (where the recordings scanner would otherwise pick them up).
func (a *App) waveformKey(relPath string) string {
	sum := sha256.Sum256([]byte("wave:" + relPath))
	return hex.EncodeToString(sum[:])
}

func (a *App) waveformPath(relPath string) string {
	return filepath.Join(a.thumbsDir(), a.waveformKey(relPath)+".png")
}

// waveformWidthPx is a fixed-width waveform image wide enough to remain
// legible when scrolled horizontally in the Set Cutter timeline, even for a
// 12+ hour festival day recording.
const waveformWidthPx = 3000

// generateWaveform renders a waveform PNG for finalPath's audio track (mono,
// downmixed) and caches it under thumbsDir keyed by relPath. Runs for every
// recording - audio-only or video - since the Set Cutter always places cut
// markers against the audio waveform.
func (a *App) generateWaveform(finalPath, relPath string) bool {
	dir := a.thumbsDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false
	}
	dest := a.waveformPath(relPath)
	cmd := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
		"-i", finalPath, "-filter_complex",
		fmt.Sprintf("aformat=channel_layouts=mono,showwavespic=s=%dx120:colors=#22c55e", waveformWidthPx),
		"-frames:v", "1", "-f", "image2", dest)
	if err := cmd.Run(); err != nil {
		_ = os.Remove(dest)
		return false
	}
	info, err := os.Stat(dest)
	if err != nil || info.Size() == 0 {
		_ = os.Remove(dest)
		return false
	}
	return true
}

// finalizeRecordingSidecar probes the finished file and writes its
// ".timecode.json" plus generates its waveform PNG. Called as a goroutine
// right after the atomic rename in runRecording so it never delays the
// recording pipeline moving on to the next source. offsetMs/timeSource come
// from the one-shot worldtimeapi.org correction taken at record start.
func (a *App) finalizeRecordingSidecar(rec *recording, finalPath, relPath string, offsetMs int64, timeSource string) {
	sc := RecordingSidecar{
		Version:            1,
		StartedAt:          rec.startedAt.Add(time.Duration(offsetMs) * time.Millisecond).UTC(),
		FinishedAt:         time.Now().UTC(),
		OffsetMs:           offsetMs,
		TimeSource:         timeSource,
		SourceID:           rec.source.ID,
		SourceName:         rec.source.Name,
		SourceType:         rec.source.Type,
		SourceURL:          rec.source.URL,
		Quality:            rec.source.Quality,
		Container:          rec.source.Container,
		AudioOnly:          rec.source.AudioOnly,
		Transcoded:         rec.source.Transcode,
		LoudnessNormalized: rec.source.LoudnessNormalize,
		HardwareAccel:      rec.source.HardwareAccel,
		RecorderVersion:    version,
	}
	if probe, err := probeMediaInfo(finalPath); err == nil {
		buildSidecarFromProbe(&sc, probe)
	}
	sc.WaveformAvailable = a.generateWaveform(finalPath, relPath)
	_ = writeSidecarJSON(sidecarTimecodePath(finalPath), sc)
}

// saveEventTimetableSidecar writes a ".timetable.json" snapshot of the
// currently active timetable alongside a finished recording, for sources
// attached to an event (Source.FestivalID set). AppConfig.Timetable is
// live and mutable - it can be re-imported or hand-edited at any time after
// this recording finishes - so freezing a copy here means the Set Cutter can
// still find "what was playing" for this specific recording even after the
// live timetable has since moved on to a different event.
func (a *App) saveEventTimetableSidecar(rec *recording, finalPath string) {
	if rec.source.FestivalID == "" {
		return
	}
	cfg := a.snapshotConfig()
	if len(cfg.Timetable) == 0 {
		return
	}
	_ = writeSidecarJSON(sidecarTimetablePath(finalPath), cfg.Timetable)
}

// writeSidecarJSON marshals v and writes it to path, matching the plain
// 0o644 write pattern used for the ".nfo" sidecar (no atomic rename needed -
// nothing else reads this file mid-write since it's created once, after the
// recording it describes is already final).
func writeSidecarJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// resolveRecordingPath validates a client-supplied relative path against
// FinishedDir the same way generateThumbnailOnDemand does, returning the
// cleaned absolute path.
func (a *App) resolveRecordingPath(relPath string) (abs string, ok bool) {
	cfg := a.snapshotConfig()
	root := filepath.Clean(cfg.Settings.FinishedDir)
	abs = filepath.Clean(filepath.Join(root, filepath.FromSlash(relPath)))
	if !strings.HasPrefix(abs, root+string(os.PathSeparator)) {
		return "", false
	}
	return abs, true
}

// handleRecordingTimecode serves (GET) or writes/backfills (POST) a
// recording's ".timecode.json" sidecar.
func (a *App) handleRecordingTimecode(w http.ResponseWriter, r *http.Request) {
	relPath := filepath.ToSlash(strings.TrimSpace(r.URL.Query().Get("path")))
	if relPath == "" || strings.Contains(relPath, "..") {
		http.Error(w, "path is required", http.StatusBadRequest)
		return
	}
	abs, ok := a.resolveRecordingPath(relPath)
	if !ok {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}
	switch r.Method {
	case http.MethodGet:
		data, err := os.ReadFile(sidecarTimecodePath(abs))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(data)
	case http.MethodPost:
		if !a.isAdminReq(r) {
			http.Error(w, "admin only", http.StatusForbidden)
			return
		}
		if _, err := os.Stat(abs); err != nil {
			http.NotFound(w, r)
			return
		}
		var body struct {
			StartedAt  string `json:"startedAt"`
			Timezone   string `json:"timezone"`
			TimeSource string `json:"timeSource"`
		}
		if r.Body != nil {
			_ = json.NewDecoder(r.Body).Decode(&body)
		}
		sc := RecordingSidecar{Version: 1, TimeSource: "manual"}
		if existing, err := os.ReadFile(sidecarTimecodePath(abs)); err == nil {
			_ = json.Unmarshal(existing, &sc)
		}
		if body.StartedAt != "" {
			t, err := time.Parse(time.RFC3339, body.StartedAt)
			if err != nil {
				http.Error(w, "startedAt must be RFC3339", http.StatusBadRequest)
				return
			}
			sc.StartedAt = t.UTC()
		} else if sc.StartedAt.IsZero() {
			// Backfill fallback: file mtime minus probed duration, the best
			// approximation available for a recording made before this
			// feature existed (or one that arrived via File Explorer/URL
			// fetch/P2P import rather than the live recording pipeline).
			info, err := os.Stat(abs)
			if err != nil {
				http.Error(w, "cannot stat recording", http.StatusInternalServerError)
				return
			}
			dur, _ := probeMediaDuration(abs)
			sc.StartedAt = info.ModTime().Add(-dur).UTC()
			sc.TimeSource = "file-mtime-fallback"
		}
		if body.Timezone != "" {
			sc.Timezone = body.Timezone
		}
		if body.TimeSource != "" {
			sc.TimeSource = body.TimeSource
		}
		sc.FinishedAt = time.Now().UTC()
		if probe, err := probeMediaInfo(abs); err == nil {
			buildSidecarFromProbe(&sc, probe)
		}
		sc.WaveformAvailable = a.generateWaveform(abs, relPath)
		if err := writeSidecarJSON(sidecarTimecodePath(abs), sc); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, sc)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleRecordingWaveform serves a recording's cached waveform PNG,
// generating it on first request if it's missing (recordings that predate
// this feature, or arrived outside the live recording pipeline).
func (a *App) handleRecordingWaveform(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	relPath := filepath.ToSlash(strings.TrimSpace(r.URL.Query().Get("path")))
	if relPath == "" || strings.Contains(relPath, "..") {
		http.NotFound(w, r)
		return
	}
	abs, ok := a.resolveRecordingPath(relPath)
	if !ok {
		http.NotFound(w, r)
		return
	}
	dest := a.waveformPath(relPath)
	if _, err := os.Stat(dest); err != nil {
		if _, err := os.Stat(abs); err != nil {
			http.NotFound(w, r)
			return
		}
		if !a.generateWaveform(abs, relPath) {
			http.Error(w, "could not generate a waveform for this file", http.StatusUnprocessableEntity)
			return
		}
	}
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeFile(w, r, dest)
}

// backfillTimecodesResult summarizes a Settings > Recorder "Backfill
// timecodes" run over the whole FinishedDir tree.
type backfillTimecodesResult struct {
	Scanned int `json:"scanned"`
	Written int `json:"written"`
	Skipped int `json:"skipped"`
	Failed  int `json:"failed"`
}

// handleBackfillTimecodes scans FinishedDir for any recording missing a
// ".timecode.json" sidecar and writes one using the file's mtime minus its
// probed duration as a fallback startedAt - the same approximation used by
// the single-file POST endpoint's fallback path, just applied in bulk.
func (a *App) handleBackfillTimecodes(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !a.isAdminReq(r) {
		http.Error(w, "admin only", http.StatusForbidden)
		return
	}
	cfg := a.snapshotConfig()
	root := filepath.Clean(cfg.Settings.FinishedDir)
	var result backfillTimecodesResult
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || isSidecarPath(p) {
			return nil
		}
		result.Scanned++
		if _, err := os.Stat(sidecarTimecodePath(p)); err == nil {
			result.Skipped++
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			result.Failed++
			return nil
		}
		rel = filepath.ToSlash(rel)
		info, err := d.Info()
		if err != nil {
			result.Failed++
			return nil
		}
		dur, _ := probeMediaDuration(p)
		sc := RecordingSidecar{
			Version:    1,
			StartedAt:  info.ModTime().Add(-dur).UTC(),
			FinishedAt: info.ModTime().UTC(),
			TimeSource: "file-mtime-fallback",
		}
		if probe, err := probeMediaInfo(p); err == nil {
			buildSidecarFromProbe(&sc, probe)
		}
		sc.WaveformAvailable = a.generateWaveform(p, rel)
		if err := writeSidecarJSON(sidecarTimecodePath(p), sc); err != nil {
			result.Failed++
			return nil
		}
		result.Written++
		return nil
	})
	writeJSON(w, result)
}

// ============================================================================
// Set Cutter, part 2: markers, preview, and export.
//
// A marker is a cut point - "a new segment starts here" - placed against the
// waveform generated in part 1. Markers for a recording are edited client
// side and persisted to a ".markers.json" sidecar on save, so work survives
// a page reload. Export splits the recording at every marker (stream-copy by
// default; optional re-encode for a frame-precise cut) into standalone
// segment files, each of which gets its own metadata entry so it shows up in
// the library immediately.
// ============================================================================

// CutterMarker is one cut point in a recording's Set Cutter timeline.
type CutterMarker struct {
	ID        string  `json:"id"`
	OffsetSec float64 `json:"offsetSec"`
	Name      string  `json:"name,omitempty"`
	Channel   string  `json:"channel,omitempty"`
	EventID   string  `json:"eventId,omitempty"`
	SetID     string  `json:"setId,omitempty"`
	Artist    string  `json:"artist,omitempty"`
	Start     string  `json:"start,omitempty"`
	End       string  `json:"end,omitempty"`
	Tracklist string  `json:"tracklist,omitempty"`
}

// handleCutterMarkers gets (GET) or saves (PUT) the marker list for one
// recording, addressed the same way as the thumbnail/timecode endpoints.
func (a *App) handleCutterMarkers(w http.ResponseWriter, r *http.Request) {
	relPath := filepath.ToSlash(strings.TrimSpace(r.URL.Query().Get("path")))
	if relPath == "" || strings.Contains(relPath, "..") {
		http.Error(w, "path is required", http.StatusBadRequest)
		return
	}
	abs, ok := a.resolveRecordingPath(relPath)
	if !ok {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}
	switch r.Method {
	case http.MethodGet:
		data, err := os.ReadFile(sidecarMarkersPath(abs))
		if err != nil {
			writeJSON(w, []CutterMarker{})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(data)
	case http.MethodPut:
		if !a.isAdminReq(r) {
			http.Error(w, "admin only", http.StatusForbidden)
			return
		}
		var markers []CutterMarker
		if err := json.NewDecoder(r.Body).Decode(&markers); err != nil {
			http.Error(w, "invalid marker list", http.StatusBadRequest)
			return
		}
		if err := writeSidecarJSON(sidecarMarkersPath(abs), markers); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, markers)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// maxPreviewClipSeconds bounds how much of a recording a single preview
// request can stream, so a mistyped duration can't turn the "quick 30s clip"
// preview feature into a full-file download.
const maxPreviewClipSeconds = 120

// handleCutterPreview streams a short clip starting at offsetSec so the Set
// Cutter can let a user confirm a candidate cut point sounds right without
// downloading the whole (often multi-gigabyte) recording.
func (a *App) handleCutterPreview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	relPath := filepath.ToSlash(strings.TrimSpace(r.URL.Query().Get("path")))
	abs, ok := a.resolveRecordingPath(relPath)
	if relPath == "" || strings.Contains(relPath, "..") || !ok {
		http.NotFound(w, r)
		return
	}
	if _, err := os.Stat(abs); err != nil {
		http.NotFound(w, r)
		return
	}
	offsetSec, _ := strconv.ParseFloat(r.URL.Query().Get("offsetSec"), 64)
	if offsetSec < 0 {
		offsetSec = 0
	}
	durationSec, _ := strconv.ParseFloat(r.URL.Query().Get("durationSec"), 64)
	if durationSec <= 0 {
		durationSec = 30
	}
	if durationSec > maxPreviewClipSeconds {
		durationSec = maxPreviewClipSeconds
	}
	cmd := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-nostdin",
		"-ss", strconv.FormatFloat(offsetSec, 'f', 2, 64), "-i", abs,
		"-t", strconv.FormatFloat(durationSec, 'f', 2, 64),
		"-c:a", "libopus", "-b:a", "96k", "-vn",
		"-f", "webm", "pipe:1")
	out, err := cmd.StdoutPipe()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := cmd.Start(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer func() { _ = cmd.Wait() }()
	w.Header().Set("Content-Type", "audio/webm")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = io.Copy(w, out)
}

// CutterJob tracks one in-progress or finished Set Cutter export. Follows
// the same mutex-guarded-struct-plus-view() shape as URLFetchJob/ShareJob.
type CutterJob struct {
	mu sync.Mutex

	id         string
	sourcePath string
	status     string // "running" | "done" | "error"
	startedAt  time.Time
	finishedAt time.Time

	totalSegments int
	doneSegments  int
	outputs       []string
	errMsg        string
	log           []ShareJobLogLine
}

// CutterJobView is the JSON-safe snapshot of a CutterJob.
type CutterJobView struct {
	ID            string            `json:"id"`
	SourcePath    string            `json:"sourcePath"`
	Status        string            `json:"status"`
	StartedAt     time.Time         `json:"startedAt"`
	FinishedAt    *time.Time        `json:"finishedAt,omitempty"`
	TotalSegments int               `json:"totalSegments"`
	DoneSegments  int               `json:"doneSegments"`
	Outputs       []string          `json:"outputs"`
	Error         string            `json:"error,omitempty"`
	Log           []ShareJobLogLine `json:"log"`
}

func (j *CutterJob) view() CutterJobView {
	j.mu.Lock()
	defer j.mu.Unlock()
	v := CutterJobView{
		ID: j.id, SourcePath: j.sourcePath, Status: j.status, StartedAt: j.startedAt,
		TotalSegments: j.totalSegments, DoneSegments: j.doneSegments,
		Outputs: append([]string(nil), j.outputs...), Error: j.errMsg,
		Log: append([]ShareJobLogLine(nil), j.log...),
	}
	if !j.finishedAt.IsZero() {
		v.FinishedAt = &j.finishedAt
	}
	return v
}

func (j *CutterJob) logf(format string, args ...any) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.log = append(j.log, ShareJobLogLine{Time: time.Now().Format("15:04:05"), Text: fmt.Sprintf(format, args...)})
}

func (j *CutterJob) progress(output string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.doneSegments++
	j.outputs = append(j.outputs, output)
}

func (j *CutterJob) finish(err error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.finishedAt = time.Now()
	if err != nil {
		j.status = "error"
		j.errMsg = err.Error()
	} else {
		j.status = "done"
	}
}

func (a *App) putCutterJob(job *CutterJob) {
	a.cutterJobsMu.Lock()
	defer a.cutterJobsMu.Unlock()
	if len(a.cutterJobs) > 50 {
		oldest, oldestID := time.Now(), ""
		for id, j := range a.cutterJobs {
			j.mu.Lock()
			t := j.startedAt
			j.mu.Unlock()
			if t.Before(oldest) {
				oldest, oldestID = t, id
			}
		}
		if oldestID != "" {
			delete(a.cutterJobs, oldestID)
		}
	}
	a.cutterJobs[job.id] = job
}

func (a *App) getCutterJob(id string) (*CutterJob, bool) {
	a.cutterJobsMu.Lock()
	defer a.cutterJobsMu.Unlock()
	j, ok := a.cutterJobs[id]
	return j, ok
}

// handleCutterJobItem returns one export job's current status by ID, for
// polling from the Set Cutter's export progress toast.
func (a *App) handleCutterJobItem(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/cutter/jobs/")
	job, ok := a.getCutterJob(id)
	if !ok {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, job.view())
}

// audioOnlyExts are containers that never carry a video stream, used to skip
// the (otherwise pointless) thumbnail-generation attempt for an exported
// audio segment.
var audioOnlyExts = map[string]bool{".mp3": true, ".m4a": true, ".aac": true, ".opus": true, ".flac": true, ".ogg": true, ".wav": true}

func isAudioOnlyExt(ext string) bool {
	return audioOnlyExts[strings.ToLower(ext)]
}

// cutterExportPath builds the output path (relative to FinishedDir) for one
// exported segment, following the fixed convention
// <event>/<year>/<stage>/<Artist>_<Stage>_<date>.<ext> - falling back to
// whatever event/stage info is available when a marker isn't linked to a
// library event.
func cutterExportPath(srcRelPath string, m CutterMarker, ev *LibraryEvent, ext string) string {
	stage := m.Channel
	if stage == "" {
		stage = channelFromPath(srcRelPath)
	}
	stageSafe := safeName(stage)
	if stageSafe == "" {
		stageSafe = "unsorted"
	}

	artist := m.Artist
	if artist == "" {
		artist = m.Name
	}
	if artist == "" {
		artist = "Untitled"
	}
	artistSafe := safeName(artist)

	dateStr := ""
	if m.Start != "" {
		if t, err := time.Parse(time.RFC3339, m.Start); err == nil {
			dateStr = t.Format("2006-01-02")
		}
	}
	if dateStr == "" {
		dateStr = time.Now().Format("2006-01-02")
	}

	var dirParts []string
	if ev != nil {
		if name := safeName(ev.Name); name != "" {
			dirParts = append(dirParts, name)
		}
		if ev.Year > 0 {
			dirParts = append(dirParts, strconv.Itoa(ev.Year))
		}
	}
	dirParts = append(dirParts, stageSafe, "sets")

	filename := fmt.Sprintf("%s_%s_%s%s", artistSafe, stageSafe, dateStr, ext)
	return filepath.ToSlash(filepath.Join(append(dirParts, filename)...))
}

// cutterExportRequest is the POST /api/cutter/export body.
type cutterExportRequest struct {
	Path    string         `json:"path"`
	Markers []CutterMarker `json:"markers"`
	Precise bool           `json:"precise"` // re-encode audio for a frame-accurate cut instead of stream-copy
}

// handleCutterExport starts a background job that splits a recording at
// every marker into standalone segment files (see runCutterExport), and
// returns the job ID immediately for the client to poll.
func (a *App) handleCutterExport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !a.isAdminReq(r) {
		http.Error(w, "admin only", http.StatusForbidden)
		return
	}
	var req cutterExportRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	relPath := filepath.ToSlash(strings.TrimSpace(req.Path))
	if relPath == "" || strings.Contains(relPath, "..") {
		http.Error(w, "path is required", http.StatusBadRequest)
		return
	}
	abs, ok := a.resolveRecordingPath(relPath)
	if !ok {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}
	if _, err := os.Stat(abs); err != nil {
		http.NotFound(w, r)
		return
	}
	if len(req.Markers) == 0 {
		http.Error(w, "at least one marker is required", http.StatusBadRequest)
		return
	}

	job := &CutterJob{id: newID(), sourcePath: relPath, status: "running", startedAt: time.Now(), totalSegments: len(req.Markers)}
	a.putCutterJob(job)
	go a.runCutterExport(job, abs, relPath, req)
	writeJSON(w, map[string]string{"jobId": job.id})
}

// runCutterExport splits abs at every marker's offset using ffmpeg stream
// copy (or, if req.Precise, a re-encode for a frame-accurate cut), naming
// and placing each output via cutterExportPath, and registers a
// RecordingMeta entry for each so the segment shows up in the library
// immediately. Runs entirely in the background - the client polls
// /api/cutter/jobs/<id> for progress.
func (a *App) runCutterExport(job *CutterJob, abs, relPath string, req cutterExportRequest) {
	markers := append([]CutterMarker(nil), req.Markers...)
	sort.Slice(markers, func(i, j int) bool { return markers[i].OffsetSec < markers[j].OffsetSec })

	dur, err := probeMediaDuration(abs)
	if err != nil {
		job.logf("could not probe source duration: %s", err)
		job.finish(err)
		return
	}

	cfg := a.snapshotConfig()
	root := filepath.Clean(cfg.Settings.FinishedDir)
	ext := filepath.Ext(abs)

	var eventByID map[string]LibraryEvent
	a.mu.RLock()
	eventByID = make(map[string]LibraryEvent, len(a.cfg.LibraryEvents))
	for _, e := range a.cfg.LibraryEvents {
		eventByID[e.ID] = e
	}
	a.mu.RUnlock()

	for i, m := range markers {
		start := m.OffsetSec
		end := dur.Seconds()
		if i+1 < len(markers) {
			end = markers[i+1].OffsetSec
		}
		if end <= start {
			job.logf("skipping marker %d: end offset is not after start offset", i)
			continue
		}

		var ev *LibraryEvent
		if e, ok := eventByID[m.EventID]; ok {
			ev = &e
		}
		outRel := cutterExportPath(relPath, m, ev, ext)
		outAbs := filepath.Join(root, filepath.FromSlash(outRel))
		if err := os.MkdirAll(filepath.Dir(outAbs), 0o755); err != nil {
			job.logf("segment %d: %s", i, err)
			continue
		}

		args := []string{"-hide_banner", "-loglevel", "error", "-y",
			"-ss", strconv.FormatFloat(start, 'f', 3, 64),
			"-to", strconv.FormatFloat(end, 'f', 3, 64),
			"-i", abs}
		if req.Precise {
			args = append(args, "-c:v", "copy", "-c:a", "aac", "-b:a", "192k")
		} else {
			args = append(args, "-c", "copy")
		}
		args = append(args, outAbs)
		cmd := exec.Command("ffmpeg", args...)
		if out, err := cmd.CombinedOutput(); err != nil {
			job.logf("segment %d (%s) failed: %s: %s", i, outRel, err, strings.TrimSpace(string(out)))
			continue
		}

		meta := RecordingMeta{EventID: m.EventID, Channel: m.Channel, SetID: m.SetID, Artist: m.Artist, Start: m.Start, End: m.End, Tracklist: m.Tracklist}
		a.mu.Lock()
		if a.cfg.RecordingMeta == nil {
			a.cfg.RecordingMeta = map[string]RecordingMeta{}
		}
		a.cfg.RecordingMeta[outRel] = meta
		newCfg := a.cfg
		a.mu.Unlock()
		_ = a.persist(newCfg)

		go a.generateThumbnail(outAbs, outRel, isAudioOnlyExt(ext))
		job.progress(outRel)
		job.logf("exported %s", outRel)
	}
	job.finish(nil)
}
