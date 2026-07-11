package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ============================================================================
// Mass transcode: bulk re-encode a batch of recordings (video or audio) to a
// chosen container/codec/quality as a single background job, following the
// same shape as every other long-running operation in this app
// (URLFetchJob/ShareJob/CutterJob) - a handler validates input, starts a
// goroutine, and returns a job ID immediately for the client to poll.
// ============================================================================

// TranscodeOptions is the POST /api/transcode/start body's "options" object.
type TranscodeOptions struct {
	Container        string `json:"container"`        // target container/extension, e.g. "mkv", "mp4", "mp3"; "" keeps the source's own
	VideoCodec       string `json:"videoCodec"`       // "copy" | "h264" | "h265" | "none" (strip video, audio-only extraction)
	AudioCodec       string `json:"audioCodec"`       // "copy" | "aac" | "opus" | "mp3" | "flac"
	CRF              int    `json:"crf"`              // video quality (x264/x265 -crf); 0 = default (23)
	AudioBitrateKbps int    `json:"audioBitrateKbps"` // 0 = default (192)
	HardwareAccel    string `json:"hardwareAccel"`    // "", "cuda", "qsv", "vaapi" - same values as a Source's HardwareAccel
	Replace          bool   `json:"replace"`          // overwrite the original recording in place vs. write a new "-transcoded" file alongside it
}

// TranscodeFileResult is one input file's outcome within a transcode job.
type TranscodeFileResult struct {
	Path   string `json:"path"`
	Output string `json:"output,omitempty"`
	Status string `json:"status"` // "done" | "error"
	Error  string `json:"error,omitempty"`
}

// TranscodeJob tracks one in-progress or finished mass-transcode run.
type TranscodeJob struct {
	mu sync.Mutex

	id         string
	status     string // "running" | "done"
	startedAt  time.Time
	finishedAt time.Time

	totalFiles int
	results    []TranscodeFileResult
	log        []ShareJobLogLine
}

// TranscodeJobView is the JSON-safe snapshot of a TranscodeJob.
type TranscodeJobView struct {
	ID         string                `json:"id"`
	Status     string                `json:"status"`
	StartedAt  time.Time             `json:"startedAt"`
	FinishedAt *time.Time            `json:"finishedAt,omitempty"`
	TotalFiles int                   `json:"totalFiles"`
	Done       int                   `json:"done"`
	Failed     int                   `json:"failed"`
	Results    []TranscodeFileResult `json:"results"`
	Log        []ShareJobLogLine     `json:"log"`
}

func (j *TranscodeJob) view() TranscodeJobView {
	j.mu.Lock()
	defer j.mu.Unlock()
	done, failed := 0, 0
	for _, r := range j.results {
		if r.Status == "error" {
			failed++
		} else {
			done++
		}
	}
	v := TranscodeJobView{
		ID: j.id, Status: j.status, StartedAt: j.startedAt, TotalFiles: j.totalFiles,
		Done: done, Failed: failed,
		Results: append([]TranscodeFileResult(nil), j.results...),
		Log:     append([]ShareJobLogLine(nil), j.log...),
	}
	if !j.finishedAt.IsZero() {
		v.FinishedAt = &j.finishedAt
	}
	return v
}

func (j *TranscodeJob) logf(format string, args ...any) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.log = append(j.log, ShareJobLogLine{Time: time.Now().Format("15:04:05"), Text: fmt.Sprintf(format, args...)})
}

func (j *TranscodeJob) addResult(r TranscodeFileResult) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.results = append(j.results, r)
}

func (j *TranscodeJob) finish() {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.status = "done"
	j.finishedAt = time.Now()
}

func (a *App) putTranscodeJob(job *TranscodeJob) {
	a.transcodeJobsMu.Lock()
	defer a.transcodeJobsMu.Unlock()
	if len(a.transcodeJobs) > 50 {
		oldest, oldestID := time.Now(), ""
		for id, j := range a.transcodeJobs {
			j.mu.Lock()
			t := j.startedAt
			j.mu.Unlock()
			if t.Before(oldest) {
				oldest, oldestID = t, id
			}
		}
		if oldestID != "" {
			delete(a.transcodeJobs, oldestID)
		}
	}
	a.transcodeJobs[job.id] = job
}

func (a *App) getTranscodeJob(id string) (*TranscodeJob, bool) {
	a.transcodeJobsMu.Lock()
	defer a.transcodeJobsMu.Unlock()
	j, ok := a.transcodeJobs[id]
	return j, ok
}

// transcodeAudioCodecName maps a short codec name to the ffmpeg encoder name.
func transcodeAudioCodecName(codec string) string {
	switch codec {
	case "opus":
		return "libopus"
	case "mp3":
		return "libmp3lame"
	case "flac":
		return "flac"
	default: // "aac" or unrecognized
		return "aac"
	}
}

// transcodeVideoEncoder picks the ffmpeg video encoder for the requested
// codec, honoring the same hardware-accel values a Source's HardwareAccel
// field accepts. h264 reuses the existing videoEncoder() helper; h265/hevc
// gets its own hardware-aware mapping.
func transcodeVideoEncoder(codec, hw string) string {
	if codec != "h265" && codec != "hevc" {
		return videoEncoder(hw)
	}
	switch hw {
	case "cuda", "nvdec", "nvidia":
		return "hevc_nvenc"
	case "qsv":
		return "hevc_qsv"
	case "vaapi":
		return "hevc_vaapi"
	default:
		return "libx265"
	}
}

// containerExt normalizes a container name into a file extension (with the
// leading dot), so a client can send either the short "mkv" style name or
// the ffmpeg muxer's own "matroska" name interchangeably.
func containerExt(name string) string {
	if name == "" {
		return ""
	}
	if strings.EqualFold(name, "matroska") {
		return ".mkv"
	}
	return "." + strings.ToLower(name)
}

// buildTranscodeArgs constructs the ffmpeg command line for one file. Video
// is stream-copied by default (fast, lossless); "none" strips it entirely
// for audio-only extraction from a video source; anything else re-encodes
// with the requested codec/CRF. Audio follows the same copy-by-default
// pattern with a requested bitrate when re-encoding.
func buildTranscodeArgs(input, output, containerName string, opt TranscodeOptions) []string {
	// -progress pipe:1 makes ffmpeg emit machine-readable "key=value"
	// progress lines (out_time, speed, fps, ...) to stdout on top of
	// whatever -loglevel prints to stderr - transcodeOneFile reads these to
	// log periodic progress instead of going silent until the file finishes.
	args := []string{"-hide_banner", "-loglevel", "error", "-progress", "pipe:1", "-y"}
	if opt.HardwareAccel != "" && opt.HardwareAccel != "none" {
		args = append(args, "-hwaccel", opt.HardwareAccel)
	}
	args = append(args, "-i", input)

	switch opt.VideoCodec {
	case "none":
		args = append(args, "-vn")
	case "copy", "":
		args = append(args, "-c:v", "copy")
	default:
		crf := opt.CRF
		if crf <= 0 {
			crf = 23
		}
		args = append(args, "-c:v", transcodeVideoEncoder(opt.VideoCodec, opt.HardwareAccel), "-crf", strconv.Itoa(crf))
	}

	switch opt.AudioCodec {
	case "copy", "":
		args = append(args, "-c:a", "copy")
	default:
		kbps := opt.AudioBitrateKbps
		if kbps <= 0 {
			kbps = 192
		}
		args = append(args, "-c:a", transcodeAudioCodecName(opt.AudioCodec), "-b:a", fmt.Sprintf("%dk", kbps))
	}

	args = append(args, "-f", containerFormat(containerName), output)
	return args
}

// rewriteSidecarsAfterTranscode regenerates a recording's timecode/waveform
// sidecar after a transcode. Reuses whatever wall-clock/source metadata the
// old sidecar had (the recording's real start time doesn't change just
// because it was re-encoded) and re-probes media info + waveform against
// the new file. When the base filename is unchanged (the common "replace,
// same container" case) oldAbs and newAbs share the exact same
// ".timecode.json" path already - see sidecarTimecodePath, which derives the
// sidecar path by stripping the extension, so this is naturally a no-op
// rename in that case and only the content gets refreshed.
func (a *App) rewriteSidecarsAfterTranscode(oldAbs, newAbs, newRelPath string) {
	sc := RecordingSidecar{Version: 1, TimeSource: "unknown"}
	if data, err := os.ReadFile(sidecarTimecodePath(oldAbs)); err == nil {
		_ = json.Unmarshal(data, &sc)
	}
	sc.Transcoded = true
	sc.FinishedAt = time.Now().UTC()
	if probe, err := probeMediaInfo(newAbs); err == nil {
		buildSidecarFromProbe(&sc, probe)
	}
	sc.WaveformAvailable = a.generateWaveform(newAbs, newRelPath)
	_ = writeSidecarJSON(sidecarTimecodePath(newAbs), sc)
}

// migrateRecordingMeta moves a RecordingMeta entry from oldRelPath to
// newRelPath (a transcode that changes container also changes the
// recording's path, since it's still keyed by exact path) - a no-op if
// nothing was assigned yet.
func (a *App) migrateRecordingMeta(oldRelPath, newRelPath string) {
	if oldRelPath == newRelPath {
		return
	}
	a.mu.Lock()
	meta, ok := a.cfg.RecordingMeta[oldRelPath]
	if ok {
		delete(a.cfg.RecordingMeta, oldRelPath)
		if a.cfg.RecordingMeta == nil {
			a.cfg.RecordingMeta = map[string]RecordingMeta{}
		}
		a.cfg.RecordingMeta[newRelPath] = meta
	}
	cfg := a.cfg
	a.mu.Unlock()
	if ok {
		_ = a.persist(cfg)
	}
}

// transcodeProgressLogInterval bounds how often a single ffmpeg run's
// periodic progress line gets logged - frequent enough to show it's alive
// on a long file, not so frequent it floods the job log.
const transcodeProgressLogInterval = 5 * time.Second

// parseFFmpegTimestamp parses ffmpeg's "-progress" out_time value
// ("HH:MM:SS.ffffff") into seconds. Deliberately not using out_time_ms/
// out_time_us - ffmpeg has a long-standing (kept for compatibility) quirk
// where out_time_ms is actually microseconds, not milliseconds, which the
// human-readable out_time field sidesteps entirely.
func parseFFmpegTimestamp(s string) (float64, bool) {
	parts := strings.SplitN(s, ":", 3)
	if len(parts) != 3 {
		return 0, false
	}
	h, err1 := strconv.ParseFloat(parts[0], 64)
	m, err2 := strconv.ParseFloat(parts[1], 64)
	sec, err3 := strconv.ParseFloat(parts[2], 64)
	if err1 != nil || err2 != nil || err3 != nil {
		return 0, false
	}
	return h*3600 + m*60 + sec, true
}

// formatTranscodeProgress renders one ffmpeg "-progress" update (a
// key=value snapshot) as a short human-readable status line.
func formatTranscodeProgress(fields map[string]string, totalDurationSec float64) string {
	var parts []string
	if elapsed, ok := parseFFmpegTimestamp(fields["out_time"]); ok && totalDurationSec > 0 {
		pct := elapsed / totalDurationSec * 100
		if pct > 100 {
			pct = 100
		}
		parts = append(parts, fmt.Sprintf("%.0f%%", pct))
	}
	if speed := strings.TrimSuffix(fields["speed"], "x"); speed != "" && speed != "0" && speed != "N/A" {
		parts = append(parts, speed+"x speed")
	}
	if fps := fields["fps"]; fps != "" && fps != "0" {
		parts = append(parts, fps+" fps")
	}
	if len(parts) == 0 {
		return "in progress"
	}
	return strings.Join(parts, ", ")
}

// runFFmpegTranscode runs ffmpeg for one file, logging a periodic progress
// line (parsed from the "-progress pipe:1" stream buildTranscodeArgs adds)
// into job's log instead of going silent until the whole file finishes.
// ffmpeg's own diagnostic output (loglevel "error") is captured separately
// and returned verbatim on failure, same detail CombinedOutput used to give.
func runFFmpegTranscode(job *TranscodeJob, relPath string, args []string, totalDurationSec float64) error {
	cmd := exec.Command("ffmpeg", args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf
	if err := cmd.Start(); err != nil {
		return err
	}

	fields := map[string]string{}
	lastLog := time.Now()
	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		key, val, ok := strings.Cut(scanner.Text(), "=")
		if !ok {
			continue
		}
		fields[key] = strings.TrimSpace(val)
		if key != "progress" || val == "end" || time.Since(lastLog) < transcodeProgressLogInterval {
			continue
		}
		lastLog = time.Now()
		job.logf("%s: %s", relPath, formatTranscodeProgress(fields, totalDurationSec))
	}

	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("%s: %s", err, strings.TrimSpace(stderrBuf.String()))
	}
	return nil
}

// transcodeOneFile re-encodes a single recording per opt, writing to a temp
// path first and only replacing/placing the final file after ffmpeg
// succeeds. Returns a result describing what happened - errors here are
// per-file and never abort the rest of the batch.
func (a *App) transcodeOneFile(job *TranscodeJob, abs, relPath string, opt TranscodeOptions) TranscodeFileResult {
	containerName := opt.Container
	ext := containerExt(containerName)
	if ext == "" {
		containerName = strings.TrimPrefix(filepath.Ext(abs), ".")
		ext = filepath.Ext(abs)
	}

	dir := filepath.Dir(abs)
	base := strings.TrimSuffix(filepath.Base(abs), filepath.Ext(abs))
	var outAbs string
	if opt.Replace {
		outAbs = filepath.Join(dir, base+ext)
	} else {
		outAbs = filepath.Join(dir, base+"-transcoded"+ext)
	}
	tempOut := outAbs + ".transcoding.tmp"
	_ = os.Remove(tempOut)

	inInfo, statErr := os.Stat(abs)
	var totalDurationSec float64
	if probe, err := probeMediaInfo(abs); err == nil {
		totalDurationSec, _ = strconv.ParseFloat(strings.TrimSpace(probe.Format.Duration), 64)
		var vCodec, aCodec string
		for _, s := range probe.Streams {
			switch s.CodecType {
			case "video":
				if vCodec == "" {
					vCodec = s.CodecName
				}
			case "audio":
				if aCodec == "" {
					aCodec = s.CodecName
				}
			}
		}
		job.logf("%s: input %s/%s, %.0fs, re-encoding video=%s audio=%s -> %s", relPath, vCodec, aCodec, totalDurationSec, opt.VideoCodec, opt.AudioCodec, containerName)
	}

	started := time.Now()
	args := buildTranscodeArgs(abs, tempOut, containerName, opt)
	if err := runFFmpegTranscode(job, relPath, args, totalDurationSec); err != nil {
		_ = os.Remove(tempOut)
		return TranscodeFileResult{Path: relPath, Status: "error", Error: err.Error()}
	}
	elapsed := time.Since(started)

	root := filepath.Clean(a.snapshotConfig().Settings.FinishedDir)
	newRel, relErr := filepath.Rel(root, outAbs)
	if relErr != nil {
		_ = os.Remove(tempOut)
		return TranscodeFileResult{Path: relPath, Status: "error", Error: "output path resolved outside the recordings root"}
	}
	newRel = filepath.ToSlash(newRel)

	if opt.Replace && outAbs != abs {
		// Container changed - drop the old file and carry its library
		// assignment over to the new path.
		_ = os.Remove(abs)
		a.migrateRecordingMeta(relPath, newRel)
	}
	if err := os.Rename(tempOut, outAbs); err != nil {
		if cpErr := copyFile(tempOut, outAbs); cpErr != nil {
			_ = os.Remove(tempOut)
			return TranscodeFileResult{Path: relPath, Status: "error", Error: fmt.Sprintf("could not place transcoded output: %s", cpErr)}
		}
		_ = os.Remove(tempOut)
	}

	if outInfo, err := os.Stat(outAbs); err == nil && statErr == nil {
		ratio := 100.0
		if inInfo.Size() > 0 {
			ratio = float64(outInfo.Size()) / float64(inInfo.Size()) * 100
		}
		job.logf("%s: done in %s (%s -> %s, %.0f%% of original size)", relPath, elapsed.Round(time.Second), formatBytesGo(inInfo.Size()), formatBytesGo(outInfo.Size()), ratio)
	}

	a.rewriteSidecarsAfterTranscode(abs, outAbs, newRel)
	go a.generateThumbnail(outAbs, newRel, opt.VideoCodec == "none" || isAudioOnlyExt(ext))

	return TranscodeFileResult{Path: relPath, Output: newRel, Status: "done"}
}

// runTranscodeJob processes every requested path sequentially (not in
// parallel) - ffmpeg re-encoding is CPU/GPU-bound, and running a batch of
// them concurrently would just make each one slower rather than finishing
// the batch faster, on the same hardware this app already shares with any
// concurrent live recordings.
func (a *App) runTranscodeJob(job *TranscodeJob, paths []string, opt TranscodeOptions) {
	for _, relPath := range paths {
		abs, ok := a.resolveRecordingPath(relPath)
		if !ok {
			job.addResult(TranscodeFileResult{Path: relPath, Status: "error", Error: "invalid path"})
			continue
		}
		if _, err := os.Stat(abs); err != nil {
			job.addResult(TranscodeFileResult{Path: relPath, Status: "error", Error: "file not found"})
			continue
		}
		job.logf("transcoding %s", relPath)
		result := a.transcodeOneFile(job, abs, relPath, opt)
		if result.Status == "error" {
			job.logf("%s failed: %s", relPath, result.Error)
		} else {
			job.logf("%s -> %s", relPath, result.Output)
		}
		job.addResult(result)
	}
	job.finish()
}

// handleTranscodeStart validates the request and starts a background
// TranscodeJob, returning its ID immediately for the client to poll.
func (a *App) handleTranscodeStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !a.isAdminReq(r) {
		http.Error(w, "admin only", http.StatusForbidden)
		return
	}
	var req struct {
		Paths   []string         `json:"paths"`
		Options TranscodeOptions `json:"options"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if len(req.Paths) == 0 {
		http.Error(w, "at least one recording path is required", http.StatusBadRequest)
		return
	}
	var paths []string
	for _, p := range req.Paths {
		p = filepath.ToSlash(strings.TrimSpace(p))
		if p == "" || strings.Contains(p, "..") {
			continue
		}
		paths = append(paths, p)
	}
	if len(paths) == 0 {
		http.Error(w, "no valid recording paths were given", http.StatusBadRequest)
		return
	}

	job := &TranscodeJob{id: newID(), status: "running", startedAt: time.Now(), totalFiles: len(paths)}
	a.putTranscodeJob(job)
	go a.runTranscodeJob(job, paths, req.Options)
	writeJSON(w, map[string]string{"jobId": job.id})
}

// handleTranscodeJobItem returns one transcode job's current status by ID,
// for polling from the mass-transcode progress toast.
func (a *App) handleTranscodeJobItem(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/transcode/jobs/")
	job, ok := a.getTranscodeJob(id)
	if !ok {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, job.view())
}

// ============================================================================
// Transcode presets: a named bundle of TranscodeOptions, so a common target
// (e.g. "H.264 CRF 23 MP4") can be picked from a dropdown in the mass-
// transcode start form, or referenced by a TranscodeRule, instead of
// re-entering the same options by hand every time.
// ============================================================================

// TranscodePreset is one saved, named set of transcode options.
type TranscodePreset struct {
	ID      string           `json:"id"`
	Name    string           `json:"name"`
	Options TranscodeOptions `json:"options"`
}

// findTranscodePreset looks up a preset by ID, returning nil if not found.
func findTranscodePreset(presets []TranscodePreset, id string) *TranscodePreset {
	for i := range presets {
		if presets[i].ID == id {
			return &presets[i]
		}
	}
	return nil
}

// handleTranscodePresets handles the collection endpoint: GET lists every
// saved preset, POST creates a new one.
func (a *App) handleTranscodePresets(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, a.snapshotConfig().Settings.TranscodePresets)
	case http.MethodPost:
		var req struct {
			Name    string           `json:"name"`
			Options TranscodeOptions `json:"options"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Name) == "" {
			http.Error(w, "name is required", http.StatusBadRequest)
			return
		}
		preset := TranscodePreset{ID: newID(), Name: strings.TrimSpace(req.Name), Options: req.Options}
		a.mu.Lock()
		a.cfg.Settings.TranscodePresets = append(a.cfg.Settings.TranscodePresets, preset)
		cfg := a.cfg
		a.mu.Unlock()
		_ = a.persist(cfg)
		writeJSON(w, preset)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleTranscodePresetItem handles /api/transcode/presets/{id}: PUT
// replaces a preset's name/options, DELETE removes it.
func (a *App) handleTranscodePresetItem(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/transcode/presets/")
	if id == "" {
		http.NotFound(w, r)
		return
	}
	switch r.Method {
	case http.MethodPut:
		var req struct {
			Name    string           `json:"name"`
			Options TranscodeOptions `json:"options"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Name) == "" {
			http.Error(w, "name is required", http.StatusBadRequest)
			return
		}
		a.mu.Lock()
		preset := findTranscodePreset(a.cfg.Settings.TranscodePresets, id)
		if preset == nil {
			a.mu.Unlock()
			http.NotFound(w, r)
			return
		}
		preset.Name = strings.TrimSpace(req.Name)
		preset.Options = req.Options
		updated := *preset
		cfg := a.cfg
		a.mu.Unlock()
		_ = a.persist(cfg)
		writeJSON(w, updated)
	case http.MethodDelete:
		a.mu.Lock()
		out := a.cfg.Settings.TranscodePresets[:0]
		for _, p := range a.cfg.Settings.TranscodePresets {
			if p.ID != id {
				out = append(out, p)
			}
		}
		a.cfg.Settings.TranscodePresets = out
		// A rule referencing a deleted preset would otherwise silently stop
		// working with no indication why - disable it instead of leaving a
		// dangling reference.
		for i := range a.cfg.Settings.TranscodeRules {
			if a.cfg.Settings.TranscodeRules[i].PresetID == id {
				a.cfg.Settings.TranscodeRules[i].Enabled = false
			}
		}
		cfg := a.cfg
		a.mu.Unlock()
		_ = a.persist(cfg)
		writeJSON(w, map[string]string{"status": "deleted"})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// ============================================================================
// Auto-transcode rules: automatically transcode a just-finished recording
// when it matches, using one of the presets above. Evaluated once per
// recording (see a.autoTranscode, called synchronously from runRecording
// right after the recording's file is placed) - rules are tried in order
// and the first enabled match wins, so a recording is never transcoded by
// more than one rule.
// ============================================================================

// TranscodeRule automatically applies a preset to a finished recording when
// every one of its match conditions holds. An empty/nil condition means
// "any" - a rule with no conditions at all matches every recording.
type TranscodeRule struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Enabled  bool   `json:"enabled"`
	PresetID string `json:"presetId"`

	MatchContainer  []string `json:"matchContainer,omitempty"`  // e.g. ["ts","flv"] - the recording's own container/extension
	MatchSourceType []string `json:"matchSourceType,omitempty"` // e.g. ["twitch","youtube","http"]
	MatchAudioOnly  *bool    `json:"matchAudioOnly,omitempty"`
	MinSizeBytes    int64    `json:"minSizeBytes,omitempty"`
}

// matchesTranscodeRule reports whether rule applies to a just-finished
// recording described by container/sourceType/audioOnly/sizeBytes.
func matchesTranscodeRule(rule TranscodeRule, container, sourceType string, audioOnly bool, sizeBytes int64) bool {
	if !rule.Enabled {
		return false
	}
	if len(rule.MatchContainer) > 0 && !containsFold(rule.MatchContainer, container) {
		return false
	}
	if len(rule.MatchSourceType) > 0 && !containsFold(rule.MatchSourceType, sourceType) {
		return false
	}
	if rule.MatchAudioOnly != nil && *rule.MatchAudioOnly != audioOnly {
		return false
	}
	if rule.MinSizeBytes > 0 && sizeBytes < rule.MinSizeBytes {
		return false
	}
	return true
}

// containsFold reports whether val is in list, case-insensitively.
func containsFold(list []string, val string) bool {
	for _, v := range list {
		if strings.EqualFold(v, val) {
			return true
		}
	}
	return false
}

// autoTranscode evaluates Settings.TranscodeRules against a just-finished
// recording and, on the first match, runs that rule's preset as a
// single-file transcode job - mirrors a.backup()'s "best-effort,
// event-logged" shape, but runs synchronously (not `go`) and updates
// rec.finalPath in place when the rule's preset changes the file's path
// (e.g. a container change), so every step after this one in runRecording
// (NFO, backup, YouTube upload, thumbnail, sidecars) sees the real final
// file rather than the pre-transcode one.
func (a *App) autoTranscode(rec *recording) {
	cfg := a.snapshotConfig()
	if len(cfg.Settings.TranscodeRules) == 0 {
		return
	}
	info, err := os.Stat(rec.finalPath)
	if err != nil {
		return
	}
	root := filepath.Clean(cfg.Settings.FinishedDir)
	relPath, relErr := filepath.Rel(root, rec.finalPath)
	if relErr != nil {
		return
	}
	relPath = filepath.ToSlash(relPath)
	container := strings.TrimPrefix(filepath.Ext(rec.finalPath), ".")

	for _, rule := range cfg.Settings.TranscodeRules {
		if !matchesTranscodeRule(rule, container, rec.source.Type, rec.source.AudioOnly, info.Size()) {
			continue
		}
		preset := findTranscodePreset(cfg.Settings.TranscodePresets, rule.PresetID)
		if preset == nil {
			a.event("error", fmt.Sprintf("[%s] auto-transcode rule %q references a missing preset", rec.source.Name, rule.Name))
			return
		}

		job := &TranscodeJob{id: newID(), status: "running", startedAt: time.Now(), totalFiles: 1}
		a.putTranscodeJob(job)
		a.event("info", fmt.Sprintf("[%s] auto-transcode rule %q matched (preset %q)", rec.source.Name, rule.Name, preset.Name))
		job.logf("transcoding %s (auto-transcode rule %q)", relPath, rule.Name)
		result := a.transcodeOneFile(job, rec.finalPath, relPath, preset.Options)
		job.addResult(result)
		job.finish()

		if result.Status == "error" {
			a.event("error", fmt.Sprintf("[%s] auto-transcode failed: %s", rec.source.Name, result.Error))
			return
		}
		a.event("info", fmt.Sprintf("[%s] auto-transcode complete: %s", rec.source.Name, result.Output))
		if result.Output != "" {
			rec.finalPath = filepath.Join(root, filepath.FromSlash(result.Output))
		}
		return // first matching rule wins
	}
}

// handleTranscodeRules handles the collection endpoint: GET lists every
// rule, POST creates a new one.
func (a *App) handleTranscodeRules(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, a.snapshotConfig().Settings.TranscodeRules)
	case http.MethodPost:
		var rule TranscodeRule
		if err := json.NewDecoder(r.Body).Decode(&rule); err != nil || strings.TrimSpace(rule.Name) == "" {
			http.Error(w, "name is required", http.StatusBadRequest)
			return
		}
		rule.ID = newID()
		a.mu.Lock()
		a.cfg.Settings.TranscodeRules = append(a.cfg.Settings.TranscodeRules, rule)
		cfg := a.cfg
		a.mu.Unlock()
		_ = a.persist(cfg)
		writeJSON(w, rule)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleTranscodeRuleItem handles /api/transcode/rules/{id}: PUT replaces a
// rule, DELETE removes it.
func (a *App) handleTranscodeRuleItem(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/transcode/rules/")
	if id == "" {
		http.NotFound(w, r)
		return
	}
	switch r.Method {
	case http.MethodPut:
		var rule TranscodeRule
		if err := json.NewDecoder(r.Body).Decode(&rule); err != nil || strings.TrimSpace(rule.Name) == "" {
			http.Error(w, "name is required", http.StatusBadRequest)
			return
		}
		rule.ID = id
		a.mu.Lock()
		found := false
		for i := range a.cfg.Settings.TranscodeRules {
			if a.cfg.Settings.TranscodeRules[i].ID == id {
				a.cfg.Settings.TranscodeRules[i] = rule
				found = true
				break
			}
		}
		if !found {
			a.mu.Unlock()
			http.NotFound(w, r)
			return
		}
		cfg := a.cfg
		a.mu.Unlock()
		_ = a.persist(cfg)
		writeJSON(w, rule)
	case http.MethodDelete:
		a.mu.Lock()
		out := a.cfg.Settings.TranscodeRules[:0]
		for _, ru := range a.cfg.Settings.TranscodeRules {
			if ru.ID != id {
				out = append(out, ru)
			}
		}
		a.cfg.Settings.TranscodeRules = out
		cfg := a.cfg
		a.mu.Unlock()
		_ = a.persist(cfg)
		writeJSON(w, map[string]string{"status": "deleted"})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}
