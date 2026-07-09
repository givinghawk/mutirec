package main

import (
	"net/http"
	"net/url"
	"testing"
)

func TestLooksLikeOwncloudShare(t *testing.T) {
	cases := []struct {
		raw       string
		wantToken string
		wantOK    bool
	}{
		{"https://stack.example.com/s/abc123", "abc123", true},
		{"https://stack.example.com/index.php/s/tok-en", "tok-en", true},
		{"https://stack.example.com/s/abc123/download", "abc123", true},
		{"https://example.com/some/random/path", "", false},
		{"https://example.com/direct/file.mkv", "", false},
	}
	for _, c := range cases {
		u, err := url.Parse(c.raw)
		if err != nil {
			t.Fatalf("url.Parse(%q): %v", c.raw, err)
		}
		token, ok := looksLikeOwncloudShare(u)
		if ok != c.wantOK || token != c.wantToken {
			t.Errorf("looksLikeOwncloudShare(%q) = (%q, %v), want (%q, %v)", c.raw, token, ok, c.wantToken, c.wantOK)
		}
	}
}

func TestParseStackFilesURL(t *testing.T) {
	cases := []struct {
		raw        string
		wantToken  string
		wantNodeID string
		wantOK     bool
	}{
		{"https://stack.example.com/s/O9LJsSmxNLOgggau/en/files/63722", "O9LJsSmxNLOgggau", "63722", true},
		{"https://stack.example.com/s/O9LJsSmxNLOgggau/files/63722", "O9LJsSmxNLOgggau", "63722", true},
		{"https://stack.example.com/s/abc123", "", "", false},
		{"https://example.com/some/random/path", "", "", false},
	}
	for _, c := range cases {
		u, err := url.Parse(c.raw)
		if err != nil {
			t.Fatalf("url.Parse(%q): %v", c.raw, err)
		}
		token, nodeID, ok := parseStackFilesURL(u)
		if ok != c.wantOK || token != c.wantToken || nodeID != c.wantNodeID {
			t.Errorf("parseStackFilesURL(%q) = (%q, %q, %v), want (%q, %q, %v)", c.raw, token, nodeID, ok, c.wantToken, c.wantNodeID, c.wantOK)
		}
	}
}

func TestSanitizeStackName(t *testing.T) {
	cases := map[string]string{
		"normal name.mp4":       "normal name.mp4",
		"has/slash.mp4":         "has-slash.mp4",
		"has\\backslash.mp4":    "has-backslash.mp4",
		"..":                    "file",
		"":                      "file",
		"Cryex @ Set, 2022.m4a": "Cryex @ Set, 2022.m4a",
	}
	for in, want := range cases {
		if got := sanitizeStackName(in); got != want {
			t.Errorf("sanitizeStackName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestURLDownloadDestPrefersContentDisposition(t *testing.T) {
	resp := &http.Response{Header: http.Header{}}
	resp.Header.Set("Content-Disposition", `attachment; filename="My Set.mkv"`)
	if got := urlDownloadDest(resp, "https://example.com/s/abc123/download"); got != "My Set.mkv" {
		t.Fatalf("expected the Content-Disposition filename, got %q", got)
	}
}

func TestURLDownloadDestFallsBackToURLPath(t *testing.T) {
	resp := &http.Response{Header: http.Header{}}
	if got := urlDownloadDest(resp, "https://example.com/files/artist-set.mp4"); got != "artist-set.mp4" {
		t.Fatalf("expected the URL basename, got %q", got)
	}
}

func TestURLDownloadDestFallsBackToTimestampWhenNothingUsable(t *testing.T) {
	resp := &http.Response{Header: http.Header{}}
	got := urlDownloadDest(resp, "https://example.com/s/abc123/download")
	if got == "" {
		t.Fatal("expected a non-empty fallback filename")
	}
}

func TestURLFetchJobViewIsSafeCopy(t *testing.T) {
	job := &URLFetchJob{id: "f1", sourceURL: "https://example.com/x.zip", status: "running"}
	job.logf("started")
	job.addBytes(100)
	view := job.view()
	if view.ID != "f1" || view.SourceURL != "https://example.com/x.zip" || view.Status != "running" {
		t.Fatalf("unexpected view: %+v", view)
	}
	if len(view.Log) != 1 || view.Log[0].Text != "started" {
		t.Fatalf("unexpected log: %+v", view.Log)
	}
	view.Log[0].Text = "mutated"
	if job.view().Log[0].Text != "started" {
		t.Fatal("view() log slice was not copied - mutation leaked back into the job")
	}
}

func TestURLFetchJobFinish(t *testing.T) {
	job := &URLFetchJob{id: "f1", status: "running"}
	job.finish(nil)
	if v := job.view(); v.Status != "done" || v.FinishedAt == nil {
		t.Fatalf("expected a successful finish to mark done: %+v", v)
	}
}
