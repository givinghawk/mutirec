package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestComputeStorageForecastNoActiveRecordings(t *testing.T) {
	got := computeStorageForecast(map[string]*recording{}, 100*1024*1024*1024)
	if got.Applicable {
		t.Fatalf("expected not applicable with no active recordings, got %+v", got)
	}
	if got.ActiveRecordings != 0 {
		t.Errorf("ActiveRecordings = %d, want 0", got.ActiveRecordings)
	}
}

func TestComputeStorageForecastIgnoresJustStartedRecordings(t *testing.T) {
	dir := t.TempDir()
	tmp := filepath.Join(dir, "rec.mkv.part")
	if err := os.WriteFile(tmp, make([]byte, 1024), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	active := map[string]*recording{
		"s1": {tempPath: tmp, startedAt: time.Now()}, // just started - below minForecastSampleSeconds
	}
	got := computeStorageForecast(active, 100*1024*1024*1024)
	if got.Applicable {
		t.Fatalf("expected a just-started recording to be ignored, got %+v", got)
	}
}

func TestComputeStorageForecastProjectsHoursRemaining(t *testing.T) {
	dir := t.TempDir()
	tmp := filepath.Join(dir, "rec.mkv.part")
	// 100 MB written over a 100s elapsed recording => 1 MB/s.
	if err := os.WriteFile(tmp, make([]byte, 100*1024*1024), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	active := map[string]*recording{
		"s1": {tempPath: tmp, startedAt: time.Now().Add(-100 * time.Second)},
	}
	freeBytes := uint64(3600 * 1024 * 1024) // 3600 MB free => 3600s = 1h at 1 MB/s
	got := computeStorageForecast(active, freeBytes)
	if !got.Applicable {
		t.Fatal("expected the forecast to be applicable")
	}
	if got.ActiveRecordings != 1 {
		t.Errorf("ActiveRecordings = %d, want 1", got.ActiveRecordings)
	}
	wantBps := 1024.0 * 1024.0
	if diff := got.BytesPerSecond - wantBps; diff > 1 || diff < -1 {
		t.Errorf("BytesPerSecond = %v, want ~%v", got.BytesPerSecond, wantBps)
	}
	if diff := got.HoursRemaining - 1.0; diff > 0.05 || diff < -0.05 {
		t.Errorf("HoursRemaining = %v, want ~1.0", got.HoursRemaining)
	}
}

func TestComputeStorageForecastSumsMultipleRecordings(t *testing.T) {
	dir := t.TempDir()
	tmp1 := filepath.Join(dir, "a.mkv.part")
	tmp2 := filepath.Join(dir, "b.mkv.part")
	if err := os.WriteFile(tmp1, make([]byte, 50*1024*1024), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.WriteFile(tmp2, make([]byte, 50*1024*1024), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	active := map[string]*recording{
		"s1": {tempPath: tmp1, startedAt: time.Now().Add(-100 * time.Second)},
		"s2": {tempPath: tmp2, startedAt: time.Now().Add(-100 * time.Second)},
	}
	got := computeStorageForecast(active, 1024*1024*1024)
	if got.ActiveRecordings != 2 {
		t.Errorf("ActiveRecordings = %d, want 2", got.ActiveRecordings)
	}
	wantBps := 1024.0 * 1024.0 // 50MB/100s * 2 recordings = ~1MB/s combined
	if diff := got.BytesPerSecond - wantBps; diff > 1 || diff < -1 {
		t.Errorf("combined BytesPerSecond = %v, want ~%v", got.BytesPerSecond, wantBps)
	}
}

func TestComputeStorageForecastMissingTempFileIsIgnored(t *testing.T) {
	active := map[string]*recording{
		"s1": {tempPath: "/does/not/exist.part", startedAt: time.Now().Add(-100 * time.Second)},
	}
	got := computeStorageForecast(active, 1024*1024*1024)
	if got.Applicable {
		t.Fatalf("expected a missing temp file to be skipped, got %+v", got)
	}
}
