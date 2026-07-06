package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/proxy"
)

// ============================================================================
// Outbound proxy support for peer-to-peer sharing.
//
// Every network call this instance makes as part of sharing - the sender's
// self-verification ping, a receiver's manifest preview, and the actual file
// downloads - goes through shareHTTPClient. Some networks can only reach a
// peer through an HTTP/SOCKS proxy (a VPN-gated firewall, a Tor/SOCKS
// tunnel, etc.), so SharingConfig.ProxyURL lets an admin route all of that
// traffic through one. Supported proxy URL schemes: http, https (standard
// net/http proxying), socks5/socks5h (via golang.org/x/net/proxy, hostname
// resolved by the proxy), and socks4/socks4a (hand-rolled below - the
// golang.org/x/net/proxy package doesn't support SOCKS4).
// ============================================================================

// dialTimeout bounds how long connecting to the proxy (or, with no proxy,
// the destination itself) may take; responseHeaderTimeout bounds how long a
// server may take to start responding once connected. Neither bounds how
// long *streaming the body* takes - large recordings can take many minutes
// to transfer, so nothing here uses http.Client.Timeout, which (unlike
// these) covers the entire response body read and would otherwise kill a
// large in-progress download for no reason other than having taken a while.
const (
	dialTimeout           = 15 * time.Second
	responseHeaderTimeout = 30 * time.Second
)

// shareHTTPClient builds the HTTP client used for one sharing-related
// request, optionally routed through proxyURL (empty means dial directly).
// Callers that need a short overall deadline (e.g. the self-verification
// ping, which talks to a single small endpoint) should set client.Timeout
// themselves after getting the client back.
func shareHTTPClient(proxyURL string) (*http.Client, error) {
	if strings.TrimSpace(proxyURL) == "" {
		return &http.Client{Transport: &http.Transport{
			DialContext:           (&net.Dialer{Timeout: dialTimeout}).DialContext,
			TLSHandshakeTimeout:   dialTimeout,
			ResponseHeaderTimeout: responseHeaderTimeout,
		}}, nil
	}
	transport, err := proxyTransport(proxyURL)
	if err != nil {
		return nil, err
	}
	return &http.Client{Transport: transport}, nil
}

// proxyTransport builds an http.RoundTripper that dials through the given
// proxy URL, returning an error for a malformed URL or unsupported scheme so
// callers can surface it immediately rather than failing confusingly on the
// first real request.
func proxyTransport(proxyURL string) (*http.Transport, error) {
	u, err := url.Parse(strings.TrimSpace(proxyURL))
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("invalid proxy URL - use scheme://[user:pass@]host:port")
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		return &http.Transport{
			Proxy:                 http.ProxyURL(u),
			ResponseHeaderTimeout: responseHeaderTimeout,
		}, nil
	case "socks5", "socks5h":
		var auth *proxy.Auth
		if u.User != nil {
			auth = &proxy.Auth{User: u.User.Username()}
			auth.Password, _ = u.User.Password()
		}
		dialer, err := proxy.SOCKS5("tcp", u.Host, auth, proxy.Direct)
		if err != nil {
			return nil, fmt.Errorf("invalid SOCKS5 proxy: %w", err)
		}
		return &http.Transport{
			DialContext:           contextDialerFunc(dialer),
			ResponseHeaderTimeout: responseHeaderTimeout,
		}, nil
	case "socks4", "socks4a":
		return &http.Transport{
			DialContext:           socks4DialContext(u, u.Scheme == "socks4a"),
			ResponseHeaderTimeout: responseHeaderTimeout,
		}, nil
	default:
		return nil, fmt.Errorf("unsupported proxy scheme %q - use http, https, socks5, socks4, or socks4a", u.Scheme)
	}
}

// contextDialerFunc adapts a proxy.Dialer to the DialContext signature
// http.Transport wants, using the dialer's own DialContext when available
// (true of golang.org/x/net/proxy's SOCKS5 dialer) and falling back to a
// goroutine-wrapped Dial otherwise.
func contextDialerFunc(d proxy.Dialer) func(ctx context.Context, network, addr string) (net.Conn, error) {
	if cd, ok := d.(proxy.ContextDialer); ok {
		return cd.DialContext
	}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		var conn net.Conn
		var err error
		done := make(chan struct{})
		go func() {
			conn, err = d.Dial(network, addr)
			close(done)
		}()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-done:
			return conn, err
		}
	}
}

// socks4DialContext dials addr through a SOCKS4 (or SOCKS4a, when
// useHostname is set) proxy at proxyURL.Host. SOCKS4 has no hostname
// resolution of its own - the client resolves the target first - while
// SOCKS4a extends it with a hostname field for proxies that should do their
// own resolution (useful when the proxy has DNS access the client doesn't).
func socks4DialContext(proxyURL *url.URL, useHostname bool) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, portStr, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		port, err := strconv.Atoi(portStr)
		if err != nil || port <= 0 || port > 65535 {
			return nil, fmt.Errorf("invalid port %q", portStr)
		}
		d := net.Dialer{Timeout: dialTimeout}
		conn, err := d.DialContext(ctx, "tcp", proxyURL.Host)
		if err != nil {
			return nil, err
		}
		if err := socks4Connect(conn, host, port, useHostname, proxyURL); err != nil {
			conn.Close()
			return nil, err
		}
		return conn, nil
	}
}

// socks4Connect performs the SOCKS4/4a CONNECT handshake on an already-open
// connection to the proxy. See https://www.openssh.com/txt/socks4.protocol
// and https://www.openssh.com/txt/socks4a.protocol.
func socks4Connect(conn net.Conn, host string, port int, useHostname bool, proxyURL *url.URL) error {
	var ip net.IP
	if !useHostname {
		ips, err := net.LookupIP(host)
		if err != nil || len(ips) == 0 {
			return fmt.Errorf("could not resolve %q for SOCKS4 (use socks4a to let the proxy resolve it): %w", host, err)
		}
		ip = ips[0].To4()
		if ip == nil {
			return fmt.Errorf("SOCKS4 requires an IPv4 target address, got %q (use socks4a for hostnames)", host)
		}
	}

	userID := ""
	if proxyURL.User != nil {
		userID = proxyURL.User.Username()
	}

	req := make([]byte, 0, 32+len(host))
	req = append(req, 0x04, 0x01, byte(port>>8), byte(port))
	if useHostname {
		req = append(req, 0, 0, 0, 1) // non-zero last octet signals SOCKS4a to the proxy
	} else {
		req = append(req, ip...)
	}
	req = append(req, []byte(userID)...)
	req = append(req, 0)
	if useHostname {
		req = append(req, []byte(host)...)
		req = append(req, 0)
	}
	if _, err := conn.Write(req); err != nil {
		return err
	}

	resp := make([]byte, 8)
	if _, err := io.ReadFull(conn, resp); err != nil {
		return fmt.Errorf("SOCKS4 proxy did not respond: %w", err)
	}
	if resp[0] != 0x00 {
		return fmt.Errorf("malformed SOCKS4 response")
	}
	switch resp[1] {
	case 0x5a:
		return nil
	case 0x5b:
		return fmt.Errorf("SOCKS4 proxy rejected the connection")
	case 0x5c:
		return fmt.Errorf("SOCKS4 proxy: client is not running identd (or not reachable from the proxy)")
	case 0x5d:
		return fmt.Errorf("SOCKS4 proxy: client's identd could not confirm the user ID")
	default:
		return fmt.Errorf("SOCKS4 proxy returned unknown status 0x%02x", resp[1])
	}
}
