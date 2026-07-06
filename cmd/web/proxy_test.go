package main

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"testing"
	"time"
)

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("url.Parse(%q): %v", raw, err)
	}
	return u
}

func TestProxyTransportHTTPScheme(t *testing.T) {
	transport, err := proxyTransport("http://user:pass@127.0.0.1:8888")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if transport.Proxy == nil {
		t.Fatal("expected an http/https proxy to set Transport.Proxy")
	}
	req, _ := http.NewRequest(http.MethodGet, "http://example.com/foo", nil)
	proxyURL, err := transport.Proxy(req)
	if err != nil || proxyURL == nil || proxyURL.Host != "127.0.0.1:8888" {
		t.Fatalf("unexpected proxy resolution: url=%v err=%v", proxyURL, err)
	}
}

func TestProxyTransportSocks5Scheme(t *testing.T) {
	transport, err := proxyTransport("socks5://user:pass@127.0.0.1:1080")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if transport.DialContext == nil {
		t.Fatal("expected a SOCKS5 proxy to set a custom DialContext")
	}
}

func TestProxyTransportSocks4Scheme(t *testing.T) {
	for _, scheme := range []string{"socks4", "socks4a"} {
		transport, err := proxyTransport(scheme + "://127.0.0.1:1080")
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", scheme, err)
		}
		if transport.DialContext == nil {
			t.Fatalf("%s: expected a custom DialContext", scheme)
		}
	}
}

func TestProxyTransportUnsupportedScheme(t *testing.T) {
	if _, err := proxyTransport("ftp://127.0.0.1:21"); err == nil {
		t.Fatal("expected an error for an unsupported proxy scheme")
	}
}

func TestProxyTransportMalformedURL(t *testing.T) {
	for _, bad := range []string{"", "not a url", "socks5://", "://missing-scheme"} {
		if _, err := proxyTransport(bad); err == nil {
			t.Errorf("expected an error for malformed proxy URL %q", bad)
		}
	}
}

func TestShareHTTPClientNoProxy(t *testing.T) {
	client, err := shareHTTPClient("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// No blanket client.Timeout - it would cover the entire body read and
	// kill a large in-progress download; only dial/header timeouts are set.
	if client.Timeout != 0 {
		t.Fatalf("expected no overall client timeout (would cut off large downloads), got %v", client.Timeout)
	}
	transport, ok := client.Transport.(*http.Transport)
	if !ok || transport == nil {
		t.Fatal("expected a configured *http.Transport even with no proxy")
	}
	if transport.ResponseHeaderTimeout == 0 {
		t.Fatal("expected a response-header timeout to bound an unresponsive server")
	}
}

func TestShareHTTPClientInvalidProxyPropagatesError(t *testing.T) {
	if _, err := shareHTTPClient("ftp://nope"); err == nil {
		t.Fatal("expected shareHTTPClient to reject an unsupported proxy scheme")
	}
}

// fakeSocks4Proxy starts a TCP listener that speaks just enough SOCKS4 to
// grant every CONNECT request, recording the raw request bytes it received
// so the test can assert on the wire format (IP-mode vs SOCKS4a hostname
// mode) without a real upstream target on the other end.
func fakeSocks4Proxy(t *testing.T) (addr string, received chan []byte) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	received = make(chan []byte, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, 512)
		n, _ := conn.Read(buf)
		received <- append([]byte(nil), buf[:n]...)
		conn.Write([]byte{0x00, 0x5a, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00})
	}()
	t.Cleanup(func() { ln.Close() })
	return ln.Addr().String(), received
}

func TestSocks4DialContextIPMode(t *testing.T) {
	addr, received := fakeSocks4Proxy(t)
	dial := socks4DialContext(mustParseURL(t, "socks4://"+addr), false)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn, err := dial(ctx, "tcp", "127.0.0.1:9999")
	if err != nil {
		t.Fatalf("unexpected dial error: %v", err)
	}
	defer conn.Close()
	req := <-received
	if req[0] != 0x04 || req[1] != 0x01 {
		t.Fatalf("unexpected SOCKS4 request header: %v", req[:2])
	}
	// IP mode: destination IP is 127.0.0.1, not the invalid-IP SOCKS4a marker.
	if req[4] == 0 && req[5] == 0 && req[6] == 0 && req[7] == 1 {
		t.Fatal("expected a real IPv4 address in IP mode, got the SOCKS4a marker")
	}
}

func TestSocks4aDialContextHostnameMode(t *testing.T) {
	addr, received := fakeSocks4Proxy(t)
	dial := socks4DialContext(mustParseURL(t, "socks4a://"+addr), true)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn, err := dial(ctx, "tcp", "example.invalid:443")
	if err != nil {
		t.Fatalf("unexpected dial error: %v", err)
	}
	defer conn.Close()
	req := <-received
	// SOCKS4a marker: 0.0.0.x with a non-zero last octet.
	if !(req[4] == 0 && req[5] == 0 && req[6] == 0 && req[7] != 0) {
		t.Fatalf("expected the SOCKS4a invalid-IP marker, got %v", req[4:8])
	}
	if !containsBytes(req, []byte("example.invalid")) {
		t.Fatal("expected the hostname to be embedded in the SOCKS4a request")
	}
}

func TestSocks4DialContextRejection(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, 512)
		conn.Read(buf)
		conn.Write([]byte{0x00, 0x5b, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}) // request rejected
	}()
	dial := socks4DialContext(mustParseURL(t, "socks4://"+ln.Addr().String()), false)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := dial(ctx, "tcp", "127.0.0.1:9999"); err == nil {
		t.Fatal("expected an error when the SOCKS4 proxy rejects the connection")
	}
}

func containsBytes(haystack, needle []byte) bool {
	return len(needle) == 0 || (len(haystack) >= len(needle) && func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if string(haystack[i:i+len(needle)]) == string(needle) {
				return true
			}
		}
		return false
	}())
}
