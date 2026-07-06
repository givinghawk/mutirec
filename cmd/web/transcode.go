package main

import (
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
	args := []string{"-hide_banner", "-loglevel", "error", "-y"}
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

// transcodeOneFile re-encodes a single recording per opt, writing to a temp
// path first and only replacing/placing the final file after ffmpeg
// succeeds. Returns a result describing what happened - errors here are
// per-file and never abort the rest of the batch.
func (a *App) transcodeOneFile(abs, relPath string, opt TranscodeOptions) TranscodeFileResult {
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

	args := buildTranscodeArgs(abs, tempOut, containerName, opt)
	cmd := exec.Command("ffmpeg", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		_ = os.Remove(tempOut)
		return TranscodeFileResult{Path: relPath, Status: "error", Error: fmt.Sprintf("%s: %s", err, strings.TrimSpace(string(out)))}
	}

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
		result := a.transcodeOneFile(abs, relPath, opt)
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
