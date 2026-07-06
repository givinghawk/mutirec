package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestShareCodeRoundTrip(t *testing.T) {
	cases := []struct{ url, token string }{
		{"https://recorder.example.com", "abc123XYZ_-"},
		{"http://192.0.2.10:8080", "tok"},
		{"https://x.example.com/", "withslash"},
	}
	for _, c := range cases {
		code := encodeShareCode(c.url, c.token)
		if strings.ContainsAny(code, "+/=") {
			t.Errorf("share code %q is not URL-safe/unpadded", code)
		}
		p, err := decodeShareCode(code)
		if err != nil {
			t.Fatalf("decode(%q) error: %v", code, err)
		}
		if p.T != c.token {
			t.Errorf("token round-trip: got %q want %q", p.T, c.token)
		}
		wantURL := strings.TrimRight(c.url, "/")
		if p.U != wantURL {
			t.Errorf("url round-trip: got %q want %q", p.U, wantURL)
		}
	}
}

func TestDecodeShareCodeRejectsGarbage(t *testing.T) {
	for _, bad := range []string{"", "not base64 %%%", encodeShareCodeMissing()} {
		if _, err := decodeShareCode(bad); err == nil {
			t.Errorf("expected decode to reject %q", bad)
		}
	}
}

// encodeShareCodeMissing builds a syntactically-valid-but-incomplete code
// (missing the token) to confirm decode rejects it.
func encodeShareCodeMissing() string {
	return encodeShareCode("https://x.example.com", "")
}

func TestLooksPublicHost(t *testing.T) {
	public := []string{"https://recorder.example.com", "http://mutirec.example.org:8080", "https://203.0.113.5"}
	private := []string{"http://localhost:8080", "http://127.0.0.1", "http://192.168.1.10", "http://10.0.0.5", "http://myhost", "https://box.local"}
	for _, u := range public {
		if !looksPublicHost(u) {
			t.Errorf("expected %q to look public", u)
		}
	}
	for _, u := range private {
		if looksPublicHost(u) {
			t.Errorf("expected %q to look non-public", u)
		}
	}
}

func TestShareImportDestSafety(t *testing.T) {
	root := "/data/recordings"
	// Normal case: channel + file land under root.
	abs, rel, err := shareImportDest(root, "BLUE", "DJ Set.mkv")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasPrefix(abs, root+"/") || !strings.HasSuffix(rel, "DJSet.mkv") {
		t.Errorf("unexpected dest abs=%q rel=%q", abs, rel)
	}
	// Path-traversal attempts in either field must not escape root.
	for _, tc := range []struct{ ch, name string }{
		{"../../etc", "passwd"},
		{"BLUE", "../../../etc/passwd"},
		{"..", "x.mkv"},
	} {
		abs, _, err := shareImportDest(root, tc.ch, tc.name)
		if err == nil && !strings.HasPrefix(abs, root+"/") {
			t.Errorf("traversal (%q,%q) escaped root: %q", tc.ch, tc.name, abs)
		}
	}
	// Empty channel falls back to a "shared" folder.
	abs, _, err = shareImportDest(root, "", "file.mp3")
	if err != nil || !strings.HasPrefix(abs, root+"/shared/") {
		t.Errorf("empty channel should use shared/: abs=%q err=%v", abs, err)
	}
}

func TestShareJobViewIsSafeCopy(t *testing.T) {
	job := &ShareJob{id: "j1", shareName: "Test Share", status: "running", startedAt: time.Now(), totalFiles: 3, totalBytes: 300}
	job.logf("hello %d", 1)
	job.startFile("a.mkv", 100)
	job.addBytes(50)
	view := job.view()
	if view.ID != "j1" || view.ShareName != "Test Share" || view.Status != "running" {
		t.Fatalf("unexpected view: %+v", view)
	}
	if len(view.Log) != 1 || view.Log[0].Text != "hello 1" {
		t.Fatalf("unexpected log: %+v", view.Log)
	}
	if view.CurrentFile != "a.mkv" || view.CurrentFileBytes != 50 || view.CurrentFileTotal != 100 {
		t.Fatalf("unexpected current-file fields: %+v", view)
	}
	// Mutating the returned view must not affect the job's internal log slice.
	view.Log[0].Text = "mutated"
	if job.view().Log[0].Text != "hello 1" {
		t.Fatal("view() log slice was not copied - mutation leaked back into the job")
	}
}

func TestShareJobFinishFileOutcomes(t *testing.T) {
	job := &ShareJob{id: "j1"}
	job.finishFile("done")
	job.finishFile("skipped")
	job.finishFile("failed")
	view := job.view()
	if view.DoneFiles != 1 || view.SkippedFiles != 1 || view.FailedFiles != 1 {
		t.Fatalf("unexpected file counters: %+v", view)
	}
}

func TestShareJobFinish(t *testing.T) {
	job := &ShareJob{id: "j1", status: "running"}
	job.finish(nil)
	if v := job.view(); v.Status != "done" || v.FinishedAt == nil {
		t.Fatalf("expected a successful finish to mark done with a FinishedAt: %+v", v)
	}
	errJob := &ShareJob{id: "j2", status: "running"}
	errJob.finish(errors.New("boom"))
	if v := errJob.view(); v.Status != "error" || v.Error != "boom" {
		t.Fatalf("expected a failing finish to mark error with the error's message: %+v", v)
	}
}

func TestPutShareJobEvictsOldest(t *testing.T) {
	a := &App{shareJobs: map[string]*ShareJob{}}
	const n = 60
	// Timestamps must all land in the past (eviction picks the minimum
	// startedAt versus time.Now()), so count seconds backward from an
	// already-past base rather than forward from it.
	base := time.Now().Add(-time.Hour)
	for i := 0; i < n; i++ {
		a.putShareJob(&ShareJob{id: fmt.Sprintf("job-%02d", i), startedAt: base.Add(time.Duration(i) * time.Second)})
	}
	a.shareJobsMu.Lock()
	got := len(a.shareJobs)
	a.shareJobsMu.Unlock()
	if got > 51 {
		t.Fatalf("expected job history to stay bounded, got %d entries", got)
	}
	// The earliest jobs (lowest startedAt) should have been evicted first.
	if _, ok := a.getShareJob("job-00"); ok {
		t.Fatal("expected the oldest job to be evicted")
	}
	if _, ok := a.getShareJob(fmt.Sprintf("job-%02d", n-1)); !ok {
		t.Fatal("expected the most recent job to still be present")
	}
}

func newTestShareApp(t *testing.T) *App {
	t.Helper()
	return &App{
		config:      filepath.Join(t.TempDir(), "config.json"),
		shareNonces: map[string]time.Time{},
	}
}

func adminRequest(method, path string, body string) *http.Request {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	admin := User{ID: "admin", Username: "admin", Role: RoleAdmin}
	return req.WithContext(context.WithValue(req.Context(), userContextKey{}, admin))
}

func TestHandleShareVerifyForceSkipsReachabilityCheck(t *testing.T) {
	a := newTestShareApp(t)
	// No fake sender is running at this address at all - a normal verify
	// would fail to connect. Force must skip that check entirely.
	body := `{"publicUrl":"http://127.0.0.1:1","force":true}`
	req := adminRequest(http.MethodPost, "/api/share/verify", body)
	rec := httptest.NewRecorder()
	a.handleShareVerify(rec, req)

	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad JSON response: %v (body=%s)", err, rec.Body.String())
	}
	if ok, _ := resp["ok"].(bool); !ok {
		t.Fatalf("expected force verify to succeed, got %+v", resp)
	}
	if v, _ := resp["verified"].(bool); v {
		t.Fatal("expected verified=false for a forced enable")
	}
	if f, _ := resp["forced"].(bool); !f {
		t.Fatal("expected forced=true in the response")
	}

	cfg := a.snapshotConfig()
	if !cfg.Settings.Sharing.Enabled || !cfg.Settings.Sharing.Forced {
		t.Fatalf("expected sharing to be enabled and marked forced: %+v", cfg.Settings.Sharing)
	}
	if cfg.Settings.Sharing.VerifiedAt != "" {
		t.Fatal("a forced enable must not claim a VerifiedAt timestamp")
	}
}

func TestHandleShareVerifyNormalPathStillChecksReachability(t *testing.T) {
	a := newTestShareApp(t)
	body := `{"publicUrl":"http://127.0.0.1:1","force":false}`
	req := adminRequest(http.MethodPost, "/api/share/verify", body)
	rec := httptest.NewRecorder()
	a.handleShareVerify(rec, req)

	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad JSON response: %v", err)
	}
	if ok, _ := resp["ok"].(bool); ok {
		t.Fatal("expected the unreachable URL to fail verification without force")
	}
	cfg := a.snapshotConfig()
	if cfg.Settings.Sharing.Enabled {
		t.Fatal("a failed verification must not enable sharing")
	}
}

func TestHandleShareVerifyRejectsBadProxyURL(t *testing.T) {
	a := newTestShareApp(t)
	body := `{"publicUrl":"http://example.com","proxyUrl":"ftp://nope","force":true}`
	req := adminRequest(http.MethodPost, "/api/share/verify", body)
	rec := httptest.NewRecorder()
	a.handleShareVerify(rec, req)

	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad JSON response: %v", err)
	}
	if ok, _ := resp["ok"].(bool); ok {
		t.Fatal("expected an unsupported proxy scheme to be rejected even with force")
	}
	if a.snapshotConfig().Settings.Sharing.Enabled {
		t.Fatal("sharing must not be enabled when the proxy URL is invalid")
	}
}

func TestHandleShareConfigProxyUpdateDoesNotClobberEnabled(t *testing.T) {
	a := newTestShareApp(t)
	a.mu.Lock()
	a.cfg.Settings.Sharing = SharingConfig{Enabled: true, PublicURL: "http://example.com", VerifiedAt: "sometime"}
	a.mu.Unlock()

	body := `{"enabled":true,"proxyUrl":"socks5://127.0.0.1:1080"}`
	req := adminRequest(http.MethodPost, "/api/share/config", body)
	rec := httptest.NewRecorder()
	a.handleShareConfig(rec, req)

	cfg := a.snapshotConfig()
	if !cfg.Settings.Sharing.Enabled {
		t.Fatal("expected sharing to remain enabled")
	}
	if cfg.Settings.Sharing.ProxyURL != "socks5://127.0.0.1:1080" {
		t.Fatalf("expected the proxy URL to be saved, got %q", cfg.Settings.Sharing.ProxyURL)
	}

	// Disabling via {enabled:false} with no proxyUrl key must not wipe the
	// saved proxy - the frontend's Disable button only sends {enabled:false}.
	req2 := adminRequest(http.MethodPost, "/api/share/config", `{"enabled":false}`)
	rec2 := httptest.NewRecorder()
	a.handleShareConfig(rec2, req2)
	cfg2 := a.snapshotConfig()
	if cfg2.Settings.Sharing.Enabled {
		t.Fatal("expected sharing to now be disabled")
	}
	if cfg2.Settings.Sharing.ProxyURL != "socks5://127.0.0.1:1080" {
		t.Fatalf("expected the proxy URL to survive a disable-only request, got %q", cfg2.Settings.Sharing.ProxyURL)
	}
}

func TestShareNonceOneTimeUse(t *testing.T) {
	a := &App{shareNonces: map[string]time.Time{}}
	a.putShareNonce("n1")
	if !a.consumeShareNonce("n1") {
		t.Fatal("expected the pending nonce to verify")
	}
	if a.consumeShareNonce("n1") {
		t.Fatal("expected a nonce to be consumable only once")
	}
	if a.consumeShareNonce("never-issued") {
		t.Fatal("expected an unknown nonce to fail")
	}
}
