package main

import (
	"errors"
	"fmt"
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
