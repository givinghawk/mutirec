package main

import (
	"strings"
	"testing"
)

func hasSeq(args []string, seq ...string) bool {
	for i := 0; i+len(seq) <= len(args); i++ {
		match := true
		for j, s := range seq {
			if args[i+j] != s {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

func TestFFmpegArgsLoudnessNormalize(t *testing.T) {
	// Stream copy video/audio, no loudness normalization: audio is copied,
	// no filter is applied.
	src := Source{Container: "mkv"}
	args := ffmpegArgs(src, "pipe:0", "/tmp/out.mkv.part", "", 0)
	if !hasSeq(args, "-c:v", "copy") || !hasSeq(args, "-c:a", "copy") {
		t.Fatalf("expected stream copy for both video and audio, got %v", args)
	}
	if hasSeq(args, "-af") {
		t.Fatalf("did not expect a loudnorm filter without LoudnessNormalize, got %v", args)
	}

	// Loudness normalization on a stream-copy source: video stays copied,
	// audio must be re-encoded with the loudnorm filter applied.
	src.LoudnessNormalize = true
	args = ffmpegArgs(src, "pipe:0", "/tmp/out.mkv.part", "", 0)
	if !hasSeq(args, "-c:v", "copy") {
		t.Fatalf("expected video to stay stream-copied, got %v", args)
	}
	if !hasSeq(args, "-c:a", "aac") {
		t.Fatalf("expected audio to be re-encoded to aac, got %v", args)
	}
	if !hasSeq(args, "-af", loudnormFilter) {
		t.Fatalf("expected the loudnorm filter to be applied, got %v", args)
	}

	// Loudness normalization combined with full transcode: video still
	// re-encodes via the normal transcode path, audio still gets loudnorm.
	src.Transcode = true
	args = ffmpegArgs(src, "pipe:0", "/tmp/out.mkv.part", "", 0)
	if !hasSeq(args, "-c:v", "libx264") {
		t.Fatalf("expected libx264 video encoder, got %v", args)
	}
	if !hasSeq(args, "-af", loudnormFilter) {
		t.Fatalf("expected the loudnorm filter to still apply under transcode, got %v", args)
	}

	// Audio-only + loudness normalize: no video stream at all, audio re-encoded.
	src = Source{Container: "m4a", AudioOnly: true, LoudnessNormalize: true}
	args = ffmpegArgs(src, "pipe:0", "/tmp/out.m4a.part", "", 0)
	if !hasSeq(args, "-vn") {
		t.Fatalf("expected -vn for audio-only source, got %v", args)
	}
	if !hasSeq(args, "-af", loudnormFilter) {
		t.Fatalf("expected the loudnorm filter for audio-only, got %v", args)
	}
	if strings.Contains(strings.Join(args, " "), "-c:v") {
		t.Fatalf("did not expect any video codec flag for an audio-only source, got %v", args)
	}
}

func TestReconnectDelayBackoffCapped(t *testing.T) {
	if reconnectDelay(1) != reconnectBaseDelay {
		t.Fatalf("expected first attempt to use the base delay, got %v", reconnectDelay(1))
	}
	if reconnectDelay(2) <= reconnectDelay(1) {
		t.Fatalf("expected backoff to grow between attempts 1 and 2")
	}
	if reconnectDelay(50) != reconnectMaxDelay {
		t.Fatalf("expected backoff to be capped at reconnectMaxDelay, got %v", reconnectDelay(50))
	}
}
