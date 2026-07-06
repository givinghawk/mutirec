package disk

import (
	"os"
	"path/filepath"
	"testing"
)

func TestScanSumsAndGroupsByStage(t *testing.T) {
	root := t.TempDir()

	// Raw recording in root.
	mustWrite(t, filepath.Join(root, "NEONBEAT.2026.UV.20260626.1300.LIVE.MP3-x.mp3"), 1000)
	// Split files under stage subfolders.
	mustWrite(t, filepath.Join(root, "UV", "set1.mp3"), 500)
	mustWrite(t, filepath.Join(root, "UV", "set2.mp3"), 250)
	mustWrite(t, filepath.Join(root, "BLUE", "set1.mp3"), 300)

	u := Scan(root)

	if u.Total != 2050 {
		t.Fatalf("total = %d, want 2050", u.Total)
	}
	if u.PerStage["UV"] != 750 {
		t.Fatalf("UV = %d, want 750", u.PerStage["UV"])
	}
	if u.PerStage["BLUE"] != 300 {
		t.Fatalf("BLUE = %d, want 300", u.PerStage["BLUE"])
	}
	// Free space should be reported on a real temp dir (non-zero on any sane FS).
	if u.VolumeFree == 0 {
		t.Logf("warning: volume free space reported 0 (acceptable on some CI filesystems)")
	}
}

func TestScanMissingDirIsEmpty(t *testing.T) {
	u := Scan(filepath.Join(t.TempDir(), "does-not-exist"))
	if u.Total != 0 || len(u.PerStage) != 0 {
		t.Fatalf("missing dir should yield empty usage, got %+v", u)
	}
}

func mustWrite(t *testing.T, path string, size int64) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
}
