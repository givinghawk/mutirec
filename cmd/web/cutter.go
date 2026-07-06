package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
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
	for _, suffix := range []string{".nfo", ".timecode.json", ".markers.json"} {
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
