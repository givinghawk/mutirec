package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRewriteWebDAVScheme(t *testing.T) {
	cases := []struct {
		raw    string
		want   string
		wantOK bool
	}{
		{"webdav://example.com/dav/", "http://example.com/dav/", true},
		{"webdavs://example.com/dav/", "https://example.com/dav/", true},
		{"WEBDAV://example.com/dav/", "http://example.com/dav/", true},
		{"https://example.com/s/abc123", "https://example.com/s/abc123", false},
	}
	for _, c := range cases {
		got, ok := rewriteWebDAVScheme(c.raw)
		if got != c.want || ok != c.wantOK {
			t.Errorf("rewriteWebDAVScheme(%q) = (%q, %v), want (%q, %v)", c.raw, got, ok, c.want, c.wantOK)
		}
	}
}

// davMultistatusFixture uses the "D:" namespace prefix real WebDAV servers
// (Apache mod_dav, Nextcloud) commonly use, to confirm the namespace-agnostic
// struct tags in webdav.go actually match regardless of prefix.
const davMultistatusFixture = `<?xml version="1.0" encoding="utf-8"?>
<D:multistatus xmlns:D="DAV:">
  <D:response>
    <D:href>/dav/Folder/</D:href>
    <D:propstat>
      <D:prop>
        <D:resourcetype><D:collection/></D:resourcetype>
      </D:prop>
      <D:status>HTTP/1.1 200 OK</D:status>
    </D:propstat>
  </D:response>
  <D:response>
    <D:href>/dav/Folder/Sub/</D:href>
    <D:propstat>
      <D:prop>
        <D:resourcetype><D:collection/></D:resourcetype>
      </D:prop>
      <D:status>HTTP/1.1 200 OK</D:status>
    </D:propstat>
  </D:response>
  <D:response>
    <D:href>/dav/Folder/Set%20A.mp3</D:href>
    <D:propstat>
      <D:prop>
        <D:resourcetype/>
        <D:getcontentlength>1234</D:getcontentlength>
      </D:prop>
      <D:status>HTTP/1.1 200 OK</D:status>
    </D:propstat>
  </D:response>
</D:multistatus>`

func TestWebdavChildrenParsesNamespacedXMLAndSkipsSelf(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "PROPFIND" {
			t.Errorf("expected PROPFIND, got %s", r.Method)
		}
		if r.Header.Get("Depth") != "1" {
			t.Errorf("expected Depth: 1, got %q", r.Header.Get("Depth"))
		}
		w.WriteHeader(207)
		w.Write([]byte(davMultistatusFixture))
	}))
	defer srv.Close()

	u, _ := url.Parse(srv.URL + "/dav/Folder/")
	entries, err := webdavChildren(srv.Client(), u, "", "")
	if err != nil {
		t.Fatalf("webdavChildren: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 children (self excluded), got %d: %+v", len(entries), entries)
	}
	var sawDir, sawFile bool
	for _, e := range entries {
		if e.isDir && strings.Contains(e.href, "Sub") {
			sawDir = true
		}
		if !e.isDir && strings.Contains(e.href, "Set%20A.mp3") {
			sawFile = true
			if e.size != 1234 {
				t.Errorf("expected size 1234, got %d", e.size)
			}
		}
	}
	if !sawDir || !sawFile {
		t.Fatalf("missing expected entries: %+v", entries)
	}
}

func TestGatherWebDAVFilesRecursesAndDecodesNames(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/dav/Folder/", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Depth") != "1" {
			http.Error(w, "expected Depth:1", 400)
			return
		}
		w.WriteHeader(207)
		w.Write([]byte(davMultistatusFixture))
	})
	mux.HandleFunc("/dav/Folder/Sub/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(207)
		w.Write([]byte(`<?xml version="1.0"?><multistatus xmlns="DAV:">
			<response><href>/dav/Folder/Sub/</href><propstat><prop><resourcetype><collection/></resourcetype></prop></propstat></response>
			<response><href>/dav/Folder/Sub/Inner.txt</href><propstat><prop><resourcetype/><getcontentlength>7</getcontentlength></prop></propstat></response>
		</multistatus>`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	u, _ := url.Parse(srv.URL + "/dav/Folder/")
	files, err := gatherWebDAVFiles(srv.Client(), u, "", "", "")
	if err != nil {
		t.Fatalf("gatherWebDAVFiles: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("expected 2 files (one top-level, one nested), got %d: %+v", len(files), files)
	}
	foundTop, foundNested := false, false
	for _, f := range files {
		if f.name == "Set A.mp3" && f.relDir == "" {
			foundTop = true
		}
		if f.name == "Inner.txt" && f.relDir == "Sub" {
			foundNested = true
		}
	}
	if !foundTop {
		t.Errorf("expected top-level file 'Set A.mp3' (URL-decoded), got %+v", files)
	}
	if !foundNested {
		t.Errorf("expected nested file 'Sub/Inner.txt', got %+v", files)
	}
}

func TestDownloadWebDAVFileSendsBasicAuth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != "alice" || pass != "hunter2" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Write([]byte("file contents"))
	}))
	defer srv.Close()

	dir := t.TempDir()
	dest := filepath.Join(dir, "out.txt")
	u, _ := url.Parse(srv.URL + "/file.txt")
	job := &URLFetchJob{id: "webdavtest", status: "running"}
	if err := downloadWebDAVFile(srv.Client(), u, "alice", "hunter2", dest, job); err != nil {
		t.Fatalf("downloadWebDAVFile: %v", err)
	}
	data, err := os.ReadFile(dest)
	if err != nil || string(data) != "file contents" {
		t.Fatalf("expected downloaded file contents, got %q (err %v)", data, err)
	}
}

func TestDownloadWebDAVFileRejectsBadAuth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	dir := t.TempDir()
	u, _ := url.Parse(srv.URL + "/file.txt")
	job := &URLFetchJob{id: "webdavtest2", status: "running"}
	err := downloadWebDAVFile(srv.Client(), u, "wrong", "wrong", filepath.Join(dir, "out.txt"), job)
	if err == nil {
		t.Fatal("expected an error for a 401 response")
	}
}

// TestBackupWebDAVCreatesFoldersAndUploads confirms backupWebDAV MKCOLs each
// intermediate directory before PUTting the file to its full relative path,
// and sends Basic Auth on both.
func TestBackupWebDAVCreatesFoldersAndUploads(t *testing.T) {
	var mkcolPaths []string
	var putPath, putBody string
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != "bob" || pass != "secret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.Method {
		case "MKCOL":
			mkcolPaths = append(mkcolPaths, r.URL.Path)
			w.WriteHeader(http.StatusCreated)
		case http.MethodPut:
			putPath = r.URL.Path
			body, _ := io.ReadAll(r.Body)
			putBody = string(body)
			w.WriteHeader(http.StatusCreated)
		default:
			http.Error(w, "unexpected method", 400)
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	finishedDir := t.TempDir()
	relDir := filepath.Join(finishedDir, "BLUE", "2026")
	if err := os.MkdirAll(relDir, 0o755); err != nil {
		t.Fatal(err)
	}
	finalPath := filepath.Join(relDir, "set.mkv")
	if err := os.WriteFile(finalPath, []byte("recording bytes"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := AppConfig{Settings: Settings{
		FinishedDir: finishedDir,
		Backup: BackupConfig{
			Method: "webdav",
			WebDAV: WebDAVBackup{URL: srv.URL, Username: "bob", Password: "secret"},
		},
	}}
	a := &App{}
	rec := &recording{finalPath: finalPath}
	if err := a.backupWebDAV(rec, cfg); err != nil {
		t.Fatalf("backupWebDAV: %v", err)
	}

	if len(mkcolPaths) != 2 || mkcolPaths[0] != "/BLUE/" || mkcolPaths[1] != "/BLUE/2026/" {
		t.Errorf("expected MKCOL for /BLUE/ then /BLUE/2026/, got %v", mkcolPaths)
	}
	if putPath != "/BLUE/2026/set.mkv" {
		t.Errorf("expected PUT to /BLUE/2026/set.mkv, got %q", putPath)
	}
	if putBody != "recording bytes" {
		t.Errorf("expected the file's contents to be uploaded, got %q", putBody)
	}
}

func TestWebdavMkdirAllToleratesAlreadyExists(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusMethodNotAllowed) // "already exists" per RFC 4918
	}))
	defer srv.Close()
	base, _ := url.Parse(srv.URL)
	if err := webdavMkdirAll(srv.Client(), base, "", "", "already/there"); err != nil {
		t.Fatalf("expected 405 (already exists) to be tolerated, got error: %v", err)
	}
}
