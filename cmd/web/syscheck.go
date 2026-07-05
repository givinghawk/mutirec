package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"defqon-stream-recorder/internal/disk"
)

// CheckStatus is the verdict for one system check or hardware requirement row.
type CheckStatus string

const (
	StatusPass CheckStatus = "pass"
	StatusWarn CheckStatus = "warn"
	StatusFail CheckStatus = "fail"
)

// SystemCheckItem is one pass/warn/fail dependency check: a binary being
// installed, the network being reachable, a directory being writable, or a
// codec being available. There is no "recommended" tier here - it either
// works or it doesn't (a missing optional dependency like rclone is a warn,
// not a fail).
type SystemCheckItem struct {
	ID     string      `json:"id"`
	Label  string      `json:"label"`
	Status CheckStatus `json:"status"`
	Detail string      `json:"detail"`
}

// Requirement is one Steam-style "minimum vs recommended" hardware row: a
// detected value compared against two thresholds.
type Requirement struct {
	ID          string      `json:"id"`
	Label       string      `json:"label"`
	Value       string      `json:"value"`
	Min         string      `json:"min"`
	Recommended string      `json:"recommended"`
	Status      CheckStatus `json:"status"`
}

// SystemCheckReport is the full result served by /api/system-check.
type SystemCheckReport struct {
	Checks       []SystemCheckItem `json:"checks"`
	Requirements []Requirement     `json:"requirements"`
	OverallOK    bool              `json:"overallOk"`
}

// handleSystemCheck runs every dependency/hardware check fresh on each call -
// these all complete in well under a second (a few short-lived subprocesses
// and directory writes), so there's no need to cache the result.
func (a *App) handleSystemCheck(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, runSystemCheck(a.snapshotConfig()))
}

func runSystemCheck(cfg AppConfig) SystemCheckReport {
	var checks []SystemCheckItem
	ffmpegCheck := checkBinary("ffmpeg", "ffmpeg", "-version", true)
	checks = append(checks, ffmpegCheck)
	checks = append(checks, checkBinary("ffprobe", "ffprobe", "-version", true))
	checks = append(checks, checkBinary("streamlink", "streamlink", "--version", true))
	checks = append(checks, checkBinary("rclone", "rclone", "version", cfg.Settings.Backup.Enabled))

	if ffmpegCheck.Status != StatusFail {
		checks = append(checks, checkFFmpegCodecs()...)
		checks = append(checks, checkHardwareAccel())
	}

	checks = append(checks, checkInternet())
	checks = append(checks, checkStorageDirs(cfg)...)

	requirements := []Requirement{
		cpuRequirement(),
		memoryRequirement(),
		diskRequirement(cfg),
	}

	overall := true
	for _, c := range checks {
		if c.Status == StatusFail {
			overall = false
		}
	}
	for _, req := range requirements {
		if req.Status == StatusFail {
			overall = false
		}
	}
	return SystemCheckReport{Checks: checks, Requirements: requirements, OverallOK: overall}
}

// checkBinary confirms an external CLI tool this app shells out to is on
// PATH and runs cleanly, capturing its first output line (usually a version
// string). A missing optional tool (required=false, e.g. rclone when backups
// aren't enabled) is reported as a warning rather than a failure.
func checkBinary(id, name, versionArg string, required bool) SystemCheckItem {
	label := strings.ToUpper(id[:1]) + id[1:]
	path, err := exec.LookPath(name)
	if err != nil {
		if required {
			return SystemCheckItem{ID: id, Label: label, Status: StatusFail, Detail: fmt.Sprintf("%s was not found on PATH - required for recording to work.", name)}
		}
		return SystemCheckItem{ID: id, Label: label, Status: StatusWarn, Detail: fmt.Sprintf("%s was not found on PATH - only needed if you enable backups.", name)}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, versionArg).CombinedOutput()
	if err != nil {
		return SystemCheckItem{ID: id, Label: label, Status: StatusWarn, Detail: fmt.Sprintf("Found at %s but it did not run cleanly: %s", path, err)}
	}
	firstLine := strings.SplitN(strings.TrimSpace(string(out)), "\n", 2)[0]
	return SystemCheckItem{ID: id, Label: label, Status: StatusPass, Detail: firstLine}
}

// checkFFmpegCodecs confirms the two codecs every recording depends on are
// built into this ffmpeg: libx264 (the software fallback used whenever
// hardware acceleration isn't configured or available) and aac (used for
// every transcoded audio track, including the live-rewind HLS buffer).
func checkFFmpegCodecs() []SystemCheckItem {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ffmpeg", "-hide_banner", "-encoders").Output()
	if err != nil {
		return []SystemCheckItem{{ID: "codecs", Label: "Codecs", Status: StatusWarn, Detail: "Could not list ffmpeg's available encoders."}}
	}
	text := string(out)
	return []SystemCheckItem{
		codecCheck(text, "libx264", "H.264 encoder (libx264)", true),
		codecCheck(text, "aac", "AAC encoder", true),
	}
}

// codecCheck looks for an exact encoder name in `ffmpeg -encoders` output,
// whose lines look like " V..... libx264   H.264 / AVC ..." - the encoder
// name is always the second whitespace-separated field.
func codecCheck(encodersOutput, needle, label string, required bool) SystemCheckItem {
	for _, line := range strings.Split(encodersOutput, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[1] == needle {
			return SystemCheckItem{ID: "codec-" + needle, Label: label, Status: StatusPass, Detail: "Available in this ffmpeg build."}
		}
	}
	status := StatusWarn
	if required {
		status = StatusFail
	}
	return SystemCheckItem{ID: "codec-" + needle, Label: label, Status: status, Detail: "Not found in this ffmpeg build - it was likely compiled without it."}
}

// checkHardwareAccel is purely informational: hardware acceleration is
// always optional (software libx264 works everywhere), so its absence is a
// warning at most, never a failure.
func checkHardwareAccel() SystemCheckItem {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ffmpeg", "-hide_banner", "-hwaccels").Output()
	if err != nil {
		return SystemCheckItem{ID: "hwaccel", Label: "Hardware acceleration", Status: StatusWarn, Detail: "Could not query available hardware accelerators."}
	}
	text := strings.ToLower(string(out))
	var found []string
	for _, hw := range []string{"cuda", "qsv", "vaapi", "videotoolbox", "d3d11va"} {
		if strings.Contains(text, hw) {
			found = append(found, hw)
		}
	}
	if len(found) == 0 {
		return SystemCheckItem{ID: "hwaccel", Label: "Hardware acceleration", Status: StatusWarn, Detail: "None detected - transcoding will run on the CPU only. Fine for a handful of sources, but slow if you transcode many sources or use live rewind heavily."}
	}
	return SystemCheckItem{ID: "hwaccel", Label: "Hardware acceleration", Status: StatusPass, Detail: "Detected: " + strings.Join(found, ", ")}
}

// checkInternet confirms outbound HTTPS actually works, since every source
// type (YouTube/Twitch via streamlink, raw HTTP/HLS via ffmpeg) needs it -
// a check that only ran locally could pass on a machine with no route out.
func checkInternet() SystemCheckItem {
	targets := []string{"https://www.youtube.com", "https://www.twitch.tv", "https://api.timetable.lol"}
	client := &http.Client{Timeout: 5 * time.Second}
	var lastErr error
	for _, url := range targets {
		req, err := http.NewRequest(http.MethodHead, url, nil)
		if err != nil {
			lastErr = err
			continue
		}
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		resp.Body.Close()
		if resp.StatusCode < 500 {
			return SystemCheckItem{ID: "internet", Label: "Internet connection", Status: StatusPass, Detail: "Reached " + url}
		}
	}
	detail := "Could not reach YouTube, Twitch, or timetable.lol."
	if lastErr != nil {
		detail += " (" + lastErr.Error() + ")"
	}
	return SystemCheckItem{ID: "internet", Label: "Internet connection", Status: StatusFail, Detail: detail}
}

// checkStorageDirs confirms each configured directory exists (creating it if
// needed) and is actually writable, by writing and removing a small probe
// file - a directory that merely "exists" but is read-only (e.g. a
// misconfigured network mount) would otherwise pass silently and only fail
// much later, mid-recording.
func checkStorageDirs(cfg AppConfig) []SystemCheckItem {
	dirs := []struct{ id, label, path string }{
		{"finishedDir", "Recordings directory", cfg.Settings.FinishedDir},
		{"tempDir", "Temporary directory", cfg.Settings.TempDir},
		{"logDir", "Log directory", cfg.Settings.LogDir},
	}
	items := make([]SystemCheckItem, 0, len(dirs))
	for _, d := range dirs {
		items = append(items, checkDirWritable(d.id, d.label, d.path))
	}
	return items
}

func checkDirWritable(id, label, path string) SystemCheckItem {
	if strings.TrimSpace(path) == "" {
		return SystemCheckItem{ID: id, Label: label, Status: StatusFail, Detail: "Not configured."}
	}
	if err := os.MkdirAll(path, 0o755); err != nil {
		return SystemCheckItem{ID: id, Label: label, Status: StatusFail, Detail: fmt.Sprintf("%s: cannot create directory (%s)", path, err)}
	}
	probe := filepath.Join(path, ".defqon-write-test")
	if err := os.WriteFile(probe, []byte("ok"), 0o644); err != nil {
		return SystemCheckItem{ID: id, Label: label, Status: StatusFail, Detail: fmt.Sprintf("%s: not writable (%s)", path, err)}
	}
	_ = os.Remove(probe)
	return SystemCheckItem{ID: id, Label: label, Status: StatusPass, Detail: path + " is writable."}
}

// Hardware requirement thresholds. These are deliberately conservative: this
// app can run several simultaneous recordings plus software transcodes, so
// the "minimum" tier is "it'll work but don't push it" rather than bare
// survival, and "recommended" is comfortable headroom for a full multi-stage
// festival with live rewind enabled on more than one source.
const (
	minCPUCores = 2
	recCPUCores = 4
	minMemoryGB = 4
	recMemoryGB = 8
	minDiskGB   = 10
	recDiskGB   = 100
)

func gib(bytes uint64) float64 { return float64(bytes) / (1024 * 1024 * 1024) }

func cpuRequirement() Requirement {
	cores := runtime.NumCPU()
	status := StatusFail
	switch {
	case cores >= recCPUCores:
		status = StatusPass
	case cores >= minCPUCores:
		status = StatusWarn
	}
	return Requirement{
		ID: "cpu", Label: "CPU cores",
		Value:       fmt.Sprintf("%d", cores),
		Min:         fmt.Sprintf("%d cores", minCPUCores),
		Recommended: fmt.Sprintf("%d+ cores", recCPUCores),
		Status:      status,
	}
}

func memoryRequirement() Requirement {
	total := totalMemoryBytes()
	if total == 0 {
		return Requirement{
			ID: "memory", Label: "System memory",
			Value:       "Unknown",
			Min:         fmt.Sprintf("%d GB", minMemoryGB),
			Recommended: fmt.Sprintf("%d+ GB", recMemoryGB),
			Status:      StatusWarn,
		}
	}
	totalGB := gib(total)
	status := StatusFail
	switch {
	case totalGB >= recMemoryGB:
		status = StatusPass
	case totalGB >= minMemoryGB:
		status = StatusWarn
	}
	return Requirement{
		ID: "memory", Label: "System memory",
		Value:       fmt.Sprintf("%.1f GB", totalGB),
		Min:         fmt.Sprintf("%d GB", minMemoryGB),
		Recommended: fmt.Sprintf("%d+ GB", recMemoryGB),
		Status:      status,
	}
}

func diskRequirement(cfg AppConfig) Requirement {
	usage := disk.Scan(cfg.Settings.FinishedDir)
	freeGB := gib(usage.VolumeFree)
	status := StatusFail
	switch {
	case freeGB >= recDiskGB:
		status = StatusPass
	case freeGB >= minDiskGB:
		status = StatusWarn
	}
	return Requirement{
		ID: "disk", Label: "Free disk space",
		Value:       fmt.Sprintf("%.1f GB free", freeGB),
		Min:         fmt.Sprintf("%d GB", minDiskGB),
		Recommended: fmt.Sprintf("%d+ GB", recDiskGB),
		Status:      status,
	}
}
