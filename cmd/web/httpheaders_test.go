package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseHTTPHeaderLines(t *testing.T) {
	lines := []string{
		"Authorization: Bearer abc123",
		"",
		"# a comment, ignored",
		"X-Custom-Header: some value with: a colon in it",
		"not a valid header line",
		"  Cookie:   session=xyz  ",
	}
	got := parseHTTPHeaderLines(lines)
	want := [][2]string{
		{"Authorization", "Bearer abc123"},
		{"X-Custom-Header", "some value with: a colon in it"},
		{"Cookie", "session=xyz"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d pairs, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("pair %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestParseHTTPHeaderLinesEmpty(t *testing.T) {
	if got := parseHTTPHeaderLines(nil); len(got) != 0 {
		t.Errorf("expected no pairs for nil input, got %+v", got)
	}
	if got := parseHTTPHeaderLines([]string{"", "  ", "# just a comment"}); len(got) != 0 {
		t.Errorf("expected no pairs for all-blank/comment input, got %+v", got)
	}
}

func TestFfmpegHeadersArg(t *testing.T) {
	pairs := [][2]string{{"Authorization", "Bearer abc"}, {"X-Token", "xyz"}}
	got := ffmpegHeadersArg(pairs)
	want := "Authorization: Bearer abc\r\nX-Token: xyz\r\n"
	if got != want {
		t.Errorf("ffmpegHeadersArg = %q, want %q", got, want)
	}
}

func TestFfmpegArgsIncludesHeadersForHTTPURL(t *testing.T) {
	src := Source{HTTPHeaders: []string{"Authorization: Bearer abc123"}}
	args := ffmpegArgs(src, "https://example.com/stream.m3u8", "/tmp/out.mkv.part", "", 0)
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "-headers Authorization: Bearer abc123\r\n") {
		t.Errorf("expected -headers with the Authorization line, got: %q", joined)
	}
	// -headers must come before -i for ffmpeg to apply it to that input.
	headersIdx := strings.Index(joined, "-headers")
	inputIdx := strings.Index(joined, "-i ")
	if headersIdx < 0 || inputIdx < 0 || headersIdx > inputIdx {
		t.Errorf("-headers must precede -i, got: %q", joined)
	}
}

func TestFfmpegArgsOmitsHeadersForPipeInput(t *testing.T) {
	src := Source{HTTPHeaders: []string{"Authorization: Bearer abc123"}}
	args := ffmpegArgs(src, "pipe:0", "/tmp/out.mkv.part", "", 0)
	if strings.Contains(strings.Join(args, " "), "-headers") {
		t.Error("did not expect -headers for a streamlink pipe:0 input")
	}
}

func TestFfmpegArgsOmitsHeadersWhenNoneConfigured(t *testing.T) {
	src := Source{}
	args := ffmpegArgs(src, "https://example.com/stream.m3u8", "/tmp/out.mkv.part", "", 0)
	if strings.Contains(strings.Join(args, " "), "-headers") {
		t.Error("did not expect -headers when the source has none configured")
	}
}

func TestIsSourceLiveSendsConfiguredHeaders(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		if gotAuth == "Bearer secret-token" {
			w.WriteHeader(http.StatusOK)
		} else {
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	defer srv.Close()

	app := &App{}
	src := Source{Type: "http", URL: srv.URL, HTTPHeaders: []string{"Authorization: Bearer secret-token"}}
	if !app.isSourceLive(src, AppConfig{}) {
		t.Fatalf("expected isSourceLive to succeed with the configured header sent, got auth header %q", gotAuth)
	}
}

func TestIsSourceLiveFailsWithoutRequiredHeader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer secret-token" {
			w.WriteHeader(http.StatusOK)
		} else {
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	defer srv.Close()

	app := &App{}
	src := Source{Type: "http", URL: srv.URL} // no HTTPHeaders configured
	if app.isSourceLive(src, AppConfig{}) {
		t.Fatal("expected isSourceLive to fail when the required header isn't sent")
	}
}

func TestProxyLiveHTTPForwardsHeadersAndBody(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Token") != "abc" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "video/mp2t")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("fake stream bytes"))
	}))
	defer upstream.Close()

	app := &App{}
	src := Source{Type: "http", URL: upstream.URL, HTTPHeaders: []string{"X-Token: abc"}}
	req := httptest.NewRequest(http.MethodGet, "/api/live/src1", nil)
	w := httptest.NewRecorder()
	app.proxyLiveHTTP(w, req, src)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "video/mp2t" {
		t.Errorf("Content-Type = %q, want video/mp2t", ct)
	}
	if w.Body.String() != "fake stream bytes" {
		t.Errorf("body = %q, want the upstream's fake stream bytes", w.Body.String())
	}
}
