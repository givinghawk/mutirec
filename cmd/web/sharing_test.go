package main

import (
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
