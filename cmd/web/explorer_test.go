package main

import (
	"archive/zip"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestExplorerApp(t *testing.T) (*App, string) {
	t.Helper()
	root := t.TempDir()
	cfg := AppConfig{Settings: Settings{FileExplorerRoot: root}}
	a := &App{config: filepath.Join(t.TempDir(), "config.json")}
	a.cfg = cfg
	return a, root
}

func TestResolveExplorerPathRejectsTraversal(t *testing.T) {
	root := "/data/recordings"
	if _, err := resolveExplorerPath(root, "../../etc/passwd"); err == nil {
		t.Fatal("expected a traversal attempt to be rejected")
	}
	// A raw backslash isn't a path separator on this platform, so it can't
	// be used to escape root here - filepath.ToSlash only normalizes "\" on
	// Windows, where it's the native separator and matters.
	abs, err := resolveExplorerPath(root, "BLUE/set.mkv")
	if err != nil || abs != filepath.Join(root, "BLUE", "set.mkv") {
		t.Fatalf("unexpected resolution: abs=%q err=%v", abs, err)
	}
	// Empty path resolves to the root itself.
	abs, err = resolveExplorerPath(root, "")
	if err != nil || abs != filepath.Clean(root) {
		t.Fatalf("expected empty path to resolve to root, got abs=%q err=%v", abs, err)
	}
}

func TestSanitizeEntryNameRejectsSeparators(t *testing.T) {
	for _, bad := range []string{"", ".", "..", "a/b", "a\\b"} {
		if _, err := sanitizeEntryName(bad); err == nil {
			t.Errorf("expected %q to be rejected", bad)
		}
	}
	name, err := sanitizeEntryName("My Set (2026).mkv")
	if err != nil || name != "My Set (2026).mkv" {
		t.Fatalf("expected a normal filename to pass through, got %q err=%v", name, err)
	}
}

func TestHandleExplorerMkdirRenameDelete(t *testing.T) {
	a, root := newTestExplorerApp(t)

	mk := adminRequest(http.MethodPost, "/api/explorer/mkdir", `{"path":"","name":"BLUE"}`)
	rec := httptest.NewRecorder()
	a.handleExplorerMkdir(rec, mk)
	if rec.Code != 200 {
		t.Fatalf("mkdir failed: %d %s", rec.Code, rec.Body.String())
	}
	if info, err := os.Stat(filepath.Join(root, "BLUE")); err != nil || !info.IsDir() {
		t.Fatalf("expected BLUE dir to exist: %v", err)
	}

	ren := adminRequest(http.MethodPost, "/api/explorer/rename", `{"path":"BLUE","newName":"RED"}`)
	rec2 := httptest.NewRecorder()
	a.handleExplorerRename(rec2, ren)
	if rec2.Code != 200 {
		t.Fatalf("rename failed: %d %s", rec2.Code, rec2.Body.String())
	}
	if _, err := os.Stat(filepath.Join(root, "RED")); err != nil {
		t.Fatal("expected RED dir to exist after rename")
	}

	del := adminRequest(http.MethodPost, "/api/explorer/delete", `{"path":"RED"}`)
	rec3 := httptest.NewRecorder()
	a.handleExplorerDelete(rec3, del)
	if rec3.Code != 200 {
		t.Fatalf("delete failed: %d %s", rec3.Code, rec3.Body.String())
	}
	if _, err := os.Stat(filepath.Join(root, "RED")); !os.IsNotExist(err) {
		t.Fatal("expected RED dir to be gone after delete")
	}
}

func TestHandleExplorerDeleteRefusesRoot(t *testing.T) {
	a, _ := newTestExplorerApp(t)
	del := adminRequest(http.MethodPost, "/api/explorer/delete", `{"path":""}`)
	rec := httptest.NewRecorder()
	a.handleExplorerDelete(rec, del)
	if rec.Code == 200 {
		t.Fatal("expected deleting the explorer root itself to be refused")
	}
}

func TestHandleExplorerListSortsDirsFirst(t *testing.T) {
	a, root := newTestExplorerApp(t)
	os.WriteFile(filepath.Join(root, "b.txt"), []byte("x"), 0o644)
	os.Mkdir(filepath.Join(root, "a-dir"), 0o755)
	req := adminRequest(http.MethodGet, "/api/explorer/list?path=", "")
	rec := httptest.NewRecorder()
	a.handleExplorerList(rec, req)
	if rec.Code != 200 {
		t.Fatalf("list failed: %d %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Index(body, "a-dir") > strings.Index(body, "b.txt") {
		t.Fatalf("expected directories listed before files: %s", body)
	}
}

func TestExtractZipNeutralizesZipSlip(t *testing.T) {
	root := t.TempDir()
	zipPath := filepath.Join(root, "evil.zip")
	f, err := os.Create(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	w, err := zw.Create("../../etc/passwd")
	if err != nil {
		t.Fatal(err)
	}
	w.Write([]byte("pwned"))
	zw.Close()
	f.Close()

	// A path-traversal entry name is defused (its leading ".." segments are
	// dropped by rooting it before cleaning), not rejected outright - either
	// is an acceptable defense, but what must never happen is the file
	// landing outside destDir.
	destDir := filepath.Join(root, "extracted")
	grandparent := filepath.Dir(filepath.Dir(root))
	if err := extractZip(zipPath, destDir, root); err != nil {
		t.Fatalf("unexpected error extracting a defused entry: %v", err)
	}
	if _, err := os.Stat(filepath.Join(grandparent, "etc", "passwd")); err == nil {
		t.Fatal("zip-slip entry must not have been written outside the destination")
	}
	if _, err := os.Stat(filepath.Join(destDir, "etc", "passwd")); err != nil {
		t.Fatalf("expected the defused entry to land safely inside destDir: %v", err)
	}
}

func TestExtractZipRejectsEscapeViaAbsoluteRootMismatch(t *testing.T) {
	// If destDir itself is not actually under root (a caller bug), extractZip
	// must still refuse rather than write anywhere.
	root := t.TempDir()
	outsideRoot := t.TempDir()
	zipPath := filepath.Join(root, "x.zip")
	f, err := os.Create(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	w, _ := zw.Create("file.txt")
	w.Write([]byte("x"))
	zw.Close()
	f.Close()

	destDir := filepath.Join(outsideRoot, "dest")
	if err := extractZip(zipPath, destDir, root); err == nil {
		t.Fatal("expected extraction to a destDir outside root to be rejected")
	}
}

func TestExtractZipRoundTrip(t *testing.T) {
	root := t.TempDir()
	zipPath := filepath.Join(root, "good.zip")
	f, err := os.Create(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	w, _ := zw.Create("sub/hello.txt")
	w.Write([]byte("hello world"))
	zw.Close()
	f.Close()

	destDir := filepath.Join(root, "good")
	if err := extractZip(zipPath, destDir, root); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(destDir, "sub", "hello.txt"))
	if err != nil || string(data) != "hello world" {
		t.Fatalf("expected extracted file content to round-trip, got %q err=%v", data, err)
	}
}

func TestHandleExplorerZipAndUnzipRoundTrip(t *testing.T) {
	a, root := newTestExplorerApp(t)
	os.WriteFile(filepath.Join(root, "one.txt"), []byte("one"), 0o644)
	os.Mkdir(filepath.Join(root, "sub"), 0o755)
	os.WriteFile(filepath.Join(root, "sub", "two.txt"), []byte("two"), 0o644)

	zipReq := adminRequest(http.MethodPost, "/api/explorer/zip", `{"path":"","names":["one.txt","sub"],"zipName":"bundle"}`)
	rec := httptest.NewRecorder()
	a.handleExplorerZip(rec, zipReq)
	if rec.Code != 200 {
		t.Fatalf("zip failed: %d %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(root, "bundle.zip")); err != nil {
		t.Fatalf("expected bundle.zip to exist: %v", err)
	}

	// Remove the originals, then unzip should restore both.
	os.Remove(filepath.Join(root, "one.txt"))
	os.RemoveAll(filepath.Join(root, "sub"))

	unzipReq := adminRequest(http.MethodPost, "/api/explorer/unzip", `{"path":"bundle.zip"}`)
	rec2 := httptest.NewRecorder()
	a.handleExplorerUnzip(rec2, unzipReq)
	if rec2.Code != 200 {
		t.Fatalf("unzip failed: %d %s", rec2.Code, rec2.Body.String())
	}
	if data, err := os.ReadFile(filepath.Join(root, "bundle", "one.txt")); err != nil || string(data) != "one" {
		t.Fatalf("expected one.txt restored, got %q err=%v", data, err)
	}
	if data, err := os.ReadFile(filepath.Join(root, "bundle", "sub", "two.txt")); err != nil || string(data) != "two" {
		t.Fatalf("expected sub/two.txt restored, got %q err=%v", data, err)
	}
}

func TestUniqueSiblingDir(t *testing.T) {
	root := t.TempDir()
	base := filepath.Join(root, "name")
	if got := uniqueSiblingDir(base); got != base {
		t.Fatalf("expected the base name when nothing exists yet, got %q", got)
	}
	os.Mkdir(base, 0o755)
	got := uniqueSiblingDir(base)
	if got == base || !strings.HasPrefix(got, base+"-") {
		t.Fatalf("expected a disambiguated sibling name, got %q", got)
	}
}
