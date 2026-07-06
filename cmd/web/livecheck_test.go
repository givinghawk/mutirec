package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestIsSourceLiveHTTPType(t *testing.T) {
	okSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer okSrv.Close()
	downSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer downSrv.Close()

	a := &App{}
	cfg := AppConfig{}

	if !a.isSourceLive(Source{Type: "http", URL: okSrv.URL}, cfg) {
		t.Error("expected a 200-responding http source to be reported live")
	}
	if a.isSourceLive(Source{Type: "http", URL: downSrv.URL}, cfg) {
		t.Error("expected a 404-responding http source to be reported not live")
	}
	if a.isSourceLive(Source{Type: "http", URL: "http://127.0.0.1:1"}, cfg) {
		t.Error("expected an unreachable http source to be reported not live")
	}
}

// fakeStreamlink writes a stub "streamlink" executable into a temp dir that
// exits 0 and prints outputIfLive when it sees "--stream-url" (only if
// outputIfLive is non-empty), else exits 1 - standing in for a real
// streamlink binary succeeding or failing to resolve a channel.
func fakeStreamlink(t *testing.T, outputIfLive string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell script stub not supported on windows")
	}
	dir := t.TempDir()
	script := "#!/bin/sh\n"
	if outputIfLive != "" {
		script += "echo '" + outputIfLive + "'\nexit 0\n"
	} else {
		script += "echo 'error: No playable streams found' 1>&2\nexit 1\n"
	}
	path := filepath.Join(dir, "streamlink")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake streamlink: %v", err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
}

func TestIsSourceLiveStreamlinkTypeWhenLive(t *testing.T) {
	fakeStreamlink(t, "https://example.com/resolved.m3u8")
	a := &App{}
	cfg := AppConfig{}
	if !a.isSourceLive(Source{Type: "twitch", URL: "https://twitch.tv/example"}, cfg) {
		t.Error("expected a resolvable channel to be reported live")
	}
}

func TestIsSourceLiveStreamlinkTypeWhenOffline(t *testing.T) {
	fakeStreamlink(t, "")
	a := &App{}
	cfg := AppConfig{}
	if a.isSourceLive(Source{Type: "twitch", URL: "https://twitch.tv/example"}, cfg) {
		t.Error("expected an unresolvable channel to be reported not live")
	}
}
