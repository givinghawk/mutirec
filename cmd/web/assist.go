package main

import (
	"encoding/json"
	"fmt"
	"math"
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
// Set Cutter, part 3: assisted mode.
//
// "Auto-detect cuts" proposes a refined cut point for every timetable set
// boundary in a recording, using only short windows around each expected
// boundary rather than processing the whole file:
//
//  1. Silence detection (ffmpeg silencedetect) always runs - no extra deps.
//  2. Whisper transcription (if a whisper binary is on PATH) optionally
//     looks for the next set's artist name or a generic MC handoff phrase.
//
// Both signals are scoped to a 20-minute window (10 min either side of the
// expected boundary) - this is a hard requirement, not a nice-to-have, so
// Whisper in particular is never run against more than that per boundary.
// ============================================================================

// DetectOptions is the POST /api/cutter/detect body's "options" object.
type DetectOptions struct {
	SilenceThresholdDB    float64 `json:"silenceThresholdDb"`    // 0 = default (-50)
	SilenceMinDurationSec float64 `json:"silenceMinDurationSec"` // 0 = default (2)
	UseWhisper            bool    `json:"useWhisper"`
	WhisperLanguage       string  `json:"whisperLanguage"` // "" or "auto" = Whisper's own auto-detect
}

// DetectedMarker is one proposed cut point from an assisted-detection run.
// The client reviews these and accepts individual ones (or all) into its
// working marker list - nothing here is written to the markers sidecar
// automatically.
type DetectedMarker struct {
	OffsetSec  float64 `json:"offsetSec"`
	Name       string  `json:"name"`
	Artist     string  `json:"artist"`
	Channel    string  `json:"channel"`
	EventID    string  `json:"eventId,omitempty"`
	SetID      string  `json:"setId,omitempty"`
	Start      string  `json:"start,omitempty"`
	End        string  `json:"end,omitempty"`
	Confidence string  `json:"confidence"` // "high" | "medium" | "low"
	Source     string  `json:"source"`     // "combined" | "whisper" | "silence" | "timetable-only"
}

// DetectJob tracks one in-progress or finished assisted-detection run.
type DetectJob struct {
	mu sync.Mutex

	id         string
	status     string // "running" | "done" | "error"
	startedAt  time.Time
	finishedAt time.Time

	proposals []DetectedMarker
	errMsg    string
	log       []ShareJobLogLine
}

// DetectJobView is the JSON-safe snapshot of a DetectJob.
type DetectJobView struct {
	ID         string            `json:"id"`
	Status     string            `json:"status"`
	StartedAt  time.Time         `json:"startedAt"`
	FinishedAt *time.Time        `json:"finishedAt,omitempty"`
	Proposals  []DetectedMarker  `json:"proposals"`
	Error      string            `json:"error,omitempty"`
	Log        []ShareJobLogLine `json:"log"`
}

func (j *DetectJob) view() DetectJobView {
	j.mu.Lock()
	defer j.mu.Unlock()
	v := DetectJobView{
		ID: j.id, Status: j.status, StartedAt: j.startedAt,
		Proposals: append([]DetectedMarker(nil), j.proposals...),
		Error:     j.errMsg, Log: append([]ShareJobLogLine(nil), j.log...),
	}
	if !j.finishedAt.IsZero() {
		v.FinishedAt = &j.finishedAt
	}
	return v
}

func (j *DetectJob) logf(format string, args ...any) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.log = append(j.log, ShareJobLogLine{Time: time.Now().Format("15:04:05"), Text: fmt.Sprintf(format, args...)})
}

func (j *DetectJob) addProposal(m DetectedMarker) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.proposals = append(j.proposals, m)
}

func (j *DetectJob) finish(err error) {
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

func (a *App) putDetectJob(job *DetectJob) {
	a.detectJobsMu.Lock()
	defer a.detectJobsMu.Unlock()
	if len(a.detectJobs) > 50 {
		oldest, oldestID := time.Now(), ""
		for id, j := range a.detectJobs {
			j.mu.Lock()
			t := j.startedAt
			j.mu.Unlock()
			if t.Before(oldest) {
				oldest, oldestID = t, id
			}
		}
		if oldestID != "" {
			delete(a.detectJobs, oldestID)
		}
	}
	a.detectJobs[job.id] = job
}

func (a *App) getDetectJob(id string) (*DetectJob, bool) {
	a.detectJobsMu.Lock()
	defer a.detectJobsMu.Unlock()
	j, ok := a.detectJobs[id]
	return j, ok
}

// detectWindowSeconds is how far either side of an expected timetable
// boundary silence/Whisper detection looks - 20 minutes total per boundary,
// never the whole file.
const detectWindowSeconds = 600.0

// defaultSilenceThresholdDB and defaultSilenceMinDurationSec are the
// conservative defaults from the design plan; the UI exposes both as
// sliders under Advanced options since some stages have continuous crowd
// noise with no true silence.
const (
	defaultSilenceThresholdDB    = -50.0
	defaultSilenceMinDurationSec = 2.0
)

// handleCutterDetect validates the request (the recording needs a timecode
// sidecar with a wall-clock anchor, plus an assigned event+channel with an
// archived timetable to compare against) and starts a background
// DetectJob, returning its ID immediately for the client to poll.
func (a *App) handleCutterDetect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !a.isAdminReq(r) {
		http.Error(w, "admin only", http.StatusForbidden)
		return
	}
	var req struct {
		Path    string        `json:"path"`
		Options DetectOptions `json:"options"`
	}
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

	scData, err := os.ReadFile(sidecarTimecodePath(abs))
	if err != nil {
		http.Error(w, "this recording has no timecode sidecar yet - use \"Backfill timecodes\" in Settings first", http.StatusBadRequest)
		return
	}
	var sc RecordingSidecar
	if err := json.Unmarshal(scData, &sc); err != nil || sc.StartedAt.IsZero() {
		http.Error(w, "this recording's timecode sidecar has no wall-clock start time", http.StatusBadRequest)
		return
	}

	a.mu.RLock()
	meta := a.cfg.RecordingMeta[relPath]
	var ev *LibraryEvent
	for _, e := range a.cfg.LibraryEvents {
		if e.ID == meta.EventID {
			evCopy := e
			ev = &evCopy
			break
		}
	}
	a.mu.RUnlock()
	if ev == nil {
		http.Error(w, "organize this recording (assign it to an event) before running auto-detect", http.StatusBadRequest)
		return
	}
	channel := meta.Channel
	if channel == "" {
		channel = channelFromPath(relPath)
	}
	var sets []ScheduleSet
	for _, st := range ev.Timetable {
		if strings.EqualFold(st.Stage, channel) {
			sets = append(sets, st.Sets...)
			break
		}
	}
	if len(sets) == 0 {
		http.Error(w, fmt.Sprintf("no archived timetable found for channel %q on %s", channel, ev.Name), http.StatusBadRequest)
		return
	}

	job := &DetectJob{id: newID(), status: "running", startedAt: time.Now()}
	a.putDetectJob(job)
	go a.runDetectJob(job, abs, sc.StartedAt, sets, channel, meta.EventID, req.Options)
	writeJSON(w, map[string]string{"jobId": job.id})
}

// handleCutterDetectJobItem returns one detect job's current status by ID.
func (a *App) handleCutterDetectJobItem(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/cutter/detect/jobs/")
	job, ok := a.getDetectJob(id)
	if !ok {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, job.view())
}

// runDetectJob walks every timetable set boundary (one set's end = the next
// set's start) that falls within the recording's span, maps it to a file
// offset via startedAt, and proposes a refined cut point for the *next*
// set's start using silence detection (always) and Whisper (if enabled and
// installed).
func (a *App) runDetectJob(job *DetectJob, abs string, startedAt time.Time, sets []ScheduleSet, channel, eventID string, opt DetectOptions) {
	dur, err := probeMediaDuration(abs)
	if err != nil {
		job.logf("could not probe recording duration: %s", err)
		job.finish(err)
		return
	}
	sort.Slice(sets, func(i, j int) bool { return sets[i].Start < sets[j].Start })

	threshold := opt.SilenceThresholdDB
	if threshold == 0 {
		threshold = defaultSilenceThresholdDB
	}
	minDur := opt.SilenceMinDurationSec
	if minDur <= 0 {
		minDur = defaultSilenceMinDurationSec
	}

	for i := 0; i+1 < len(sets); i++ {
		next := sets[i+1]
		setEnd, err := time.Parse(time.RFC3339, sets[i].End)
		if err != nil {
			continue
		}
		boundaryOffset := setEnd.Sub(startedAt).Seconds()
		if boundaryOffset < 0 || boundaryOffset > dur.Seconds() {
			continue // this boundary falls outside the recording's own span
		}

		windowStart := math.Max(0, boundaryOffset-detectWindowSeconds)
		windowDur := math.Min(2*detectWindowSeconds, dur.Seconds()-windowStart)

		job.logf("checking boundary at %.0fs (before %q)", boundaryOffset, next.Name)
		silenceOffset, silenceFound := detectSilenceNear(abs, windowStart, windowDur, boundaryOffset, threshold, minDur)

		var whisperOffset float64
		var whisperFound bool
		if opt.UseWhisper {
			whisperOffset, whisperFound = a.detectWhisperNear(abs, windowStart, windowDur, next.Name, opt.WhisperLanguage, job)
		}

		chosen, confidence, source := boundaryOffset, "low", "timetable-only"
		switch {
		case silenceFound && whisperFound && math.Abs(silenceOffset-whisperOffset) < 30:
			chosen, confidence, source = (silenceOffset+whisperOffset)/2, "high", "combined"
		case whisperFound:
			chosen, confidence, source = whisperOffset, "medium", "whisper"
		case silenceFound:
			chosen, confidence, source = silenceOffset, "medium", "silence"
		}

		job.addProposal(DetectedMarker{
			OffsetSec: chosen, Name: next.Name, Artist: next.Name, Channel: channel,
			EventID: eventID, SetID: next.ID, Start: next.Start, End: next.End,
			Confidence: confidence, Source: source,
		})
	}
	job.finish(nil)
}

// silenceGap is one silence interval parsed from ffmpeg's silencedetect
// output, with its end time already converted to an absolute file offset.
type silenceGap struct{ end, dur float64 }

// parseSilenceLog extracts every "silence_end: <t> | silence_duration: <d>"
// line from ffmpeg's silencedetect stderr output (which it writes regardless
// of exit status), converting each relative end time to an absolute file
// offset via windowStart. Split out from detectSilenceNear so the parsing
// itself is testable without invoking ffmpeg.
func parseSilenceLog(output string, windowStart float64) []silenceGap {
	var candidates []silenceGap
	for _, line := range strings.Split(output, "\n") {
		idx := strings.Index(line, "silence_end:")
		if idx < 0 {
			continue
		}
		fields := strings.Fields(line[idx:])
		if len(fields) < 5 {
			continue
		}
		endRel, err1 := strconv.ParseFloat(fields[1], 64)
		d, err2 := strconv.ParseFloat(fields[4], 64)
		if err1 != nil || err2 != nil {
			continue
		}
		candidates = append(candidates, silenceGap{end: windowStart + endRel, dur: d})
	}
	return candidates
}

// pickClosestSilence returns the candidate silence gap whose end lands
// closest to boundaryOffset, breaking ties by picking the longer silence.
func pickClosestSilence(candidates []silenceGap, boundaryOffset float64) (float64, bool) {
	if len(candidates) == 0 {
		return 0, false
	}
	best := candidates[0]
	bestDist := math.Abs(best.end - boundaryOffset)
	for _, c := range candidates[1:] {
		dist := math.Abs(c.end - boundaryOffset)
		if dist < bestDist || (dist == bestDist && c.dur > best.dur) {
			best, bestDist = c, dist
		}
	}
	return best.end, true
}

// detectSilenceNear runs ffmpeg's silencedetect filter over a bounded window
// and returns the silence gap whose end lands closest to boundaryOffset, or
// false if none was found.
func detectSilenceNear(abs string, windowStart, windowDur, boundaryOffset, thresholdDB, minDurSec float64) (float64, bool) {
	cmd := exec.Command("ffmpeg", "-hide_banner",
		"-ss", strconv.FormatFloat(windowStart, 'f', 2, 64),
		"-t", strconv.FormatFloat(windowDur, 'f', 2, 64),
		"-i", abs,
		"-af", fmt.Sprintf("silencedetect=n=%gdB:d=%g", thresholdDB, minDurSec),
		"-f", "null", "-")
	// ffmpeg writes silencedetect's analysis to stderr regardless of exit
	// status; CombinedOutput captures it even if the command itself errors.
	out, _ := cmd.CombinedOutput()
	return pickClosestSilence(parseSilenceLog(string(out), windowStart), boundaryOffset)
}

// whisperBinary reports the first available Whisper CLI on PATH - the
// vanilla openai/whisper CLI and the common whisper.cpp/faster-whisper
// binary names all accept a similar enough invocation for this use.
func whisperBinary() (string, bool) {
	for _, name := range []string{"whisper", "whisper-cli", "faster-whisper"} {
		if p, err := exec.LookPath(name); err == nil {
			return p, true
		}
	}
	return "", false
}

// whisperTranscript is the subset of Whisper's --output_format json shape
// this app reads.
type whisperTranscript struct {
	Segments []struct {
		Start float64 `json:"start"`
		Text  string  `json:"text"`
	} `json:"segments"`
}

// mcHandoffPhrases are generic cues (English and Dutch, since hardstyle-style
// festival MCs commonly mix both) that a set change is happening, used when
// the next artist's own name isn't picked up cleanly by the transcription.
var mcHandoffPhrases = []string{
	"next up", "give it up for", "please welcome", "make some noise",
	"volgende", "nu op",
}

// detectWhisperNear extracts a short WAV window and runs Whisper over it
// looking for the next set's artist name, falling back to a generic MC
// handoff phrase. Never processes more than the given window - this is a
// hard requirement, not a nice-to-have. Returns false (not an error) if no
// Whisper binary is installed, so silence detection still works standalone.
func (a *App) detectWhisperNear(abs string, windowStart, windowDur float64, nextArtist, language string, job *DetectJob) (float64, bool) {
	bin, ok := whisperBinary()
	if !ok {
		job.logf("whisper not installed - skipping speech-based detection for this boundary")
		return 0, false
	}

	tmpDir, err := os.MkdirTemp("", "mutirec-whisper-*")
	if err != nil {
		return 0, false
	}
	defer os.RemoveAll(tmpDir)

	wav := filepath.Join(tmpDir, "window.wav")
	extract := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
		"-ss", strconv.FormatFloat(windowStart, 'f', 2, 64),
		"-t", strconv.FormatFloat(windowDur, 'f', 2, 64),
		"-i", abs, "-ar", "16000", "-ac", "1", wav)
	if err := extract.Run(); err != nil {
		job.logf("whisper: could not extract the audio window: %s", err)
		return 0, false
	}

	args := []string{"--model", "tiny", "--output_format", "json", "--output_dir", tmpDir}
	if language != "" && language != "auto" {
		args = append(args, "--language", language)
	}
	args = append(args, wav)
	if err := exec.Command(bin, args...).Run(); err != nil {
		job.logf("whisper: transcription failed: %s", err)
		return 0, false
	}

	data, err := os.ReadFile(strings.TrimSuffix(wav, filepath.Ext(wav)) + ".json")
	if err != nil {
		return 0, false
	}
	var transcript whisperTranscript
	if err := json.Unmarshal(data, &transcript); err != nil {
		return 0, false
	}
	return matchWhisperTranscript(transcript, windowStart, nextArtist)
}

// matchWhisperTranscript scans a parsed Whisper transcript for the next
// set's artist name first, falling back to a generic MC handoff phrase -
// split out from detectWhisperNear so the matching logic is testable
// without invoking Whisper or ffmpeg.
func matchWhisperTranscript(transcript whisperTranscript, windowStart float64, nextArtist string) (float64, bool) {
	needle := strings.ToLower(strings.TrimSpace(nextArtist))
	for _, seg := range transcript.Segments {
		text := strings.ToLower(seg.Text)
		if needle != "" && strings.Contains(text, needle) {
			return windowStart + seg.Start, true
		}
	}
	for _, seg := range transcript.Segments {
		text := strings.ToLower(seg.Text)
		for _, phrase := range mcHandoffPhrases {
			if strings.Contains(text, phrase) {
				return windowStart + seg.Start, true
			}
		}
	}
	return 0, false
}
