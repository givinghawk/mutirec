package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ============================================================================
// Peer-to-peer set sharing
//
// One MutiRec instance ("sender") that's reachable at a public URL can bundle
// a selection of recordings — individual sets, whole events, or whole stages —
// plus their sidecar metadata into a share. Sharing produces a short share
// code: base64url(JSON{u: publicURL, t: token}). A second instance
// ("receiver") pastes that code, previews what's on offer, and pulls the files
// (and metadata) straight from the sender over HTTP. The token in the code is
// an unguessable bearer credential scoped to exactly the files in that share.
//
// Setup is required and checked first: the sender must set a public URL and
// pass a reachability check (a nonce round-trip back to itself over that URL)
// before sharing is enabled.
// ============================================================================

// SharingConfig holds the sender-side setup: the public URL other instances
// reach this one at, whether sharing is enabled, when the URL last passed the
// reachability check (or Forced if that check was deliberately skipped), and
// an optional outbound proxy for all sharing-related network calls (the
// self-verification ping, manifest preview, and downloads).
type SharingConfig struct {
	Enabled    bool   `json:"enabled"`
	PublicURL  string `json:"publicUrl"`
	VerifiedAt string `json:"verifiedAt,omitempty"`
	Forced     bool   `json:"forced,omitempty"`
	ProxyURL   string `json:"proxyUrl,omitempty"`
}

// Share is one published bundle: an unguessable token plus the concrete list
// of recording paths (relative to FinishedDir) it exposes. Events/stages
// selected at creation time are resolved to their member files up front, so a
// share is always a fixed file list.
type Share struct {
	Token     string   `json:"token"`
	Name      string   `json:"name,omitempty"`
	Paths     []string `json:"paths"`
	CreatedAt string   `json:"createdAt"`
}

// ShareItem is one downloadable recording in a share manifest, carrying the
// same denormalized-by-name library metadata as a matchfile entry (names, not
// local IDs) so a receiver can reconstruct the event/festival grouping.
type ShareItem struct {
	Index        int    `json:"index"`
	Name         string `json:"name"`
	Channel      string `json:"channel,omitempty"`
	Size         int64  `json:"size"`
	Hash         string `json:"hash,omitempty"`
	HasNFO       bool   `json:"hasNfo"`
	Artist       string `json:"artist,omitempty"`
	Start        string `json:"start,omitempty"`
	End          string `json:"end,omitempty"`
	Tracklist    string `json:"tracklist,omitempty"`
	EventName    string `json:"eventName,omitempty"`
	FestivalName string `json:"festivalName,omitempty"`
}

// ShareManifest is what a receiver fetches to preview a share before pulling.
type ShareManifest struct {
	Name  string      `json:"name"`
	Items []ShareItem `json:"items"`
}

const shareNonceTTL = 30 * time.Second

// shortToken returns a compact, URL-safe, unguessable token (~20 chars from
// 15 random bytes) - short enough to keep share codes reasonable while still
// being infeasible to guess.
func shortToken() string {
	var b [15]byte
	_, _ = rand.Read(b[:])
	return base64.RawURLEncoding.EncodeToString(b[:])
}

// shareCodePayload is the minimal thing a share code carries: the sender's
// public URL and the share token. Keys are single letters to keep it short.
type shareCodePayload struct {
	U string `json:"u"`
	T string `json:"t"`
}

func encodeShareCode(publicURL, token string) string {
	data, _ := json.Marshal(shareCodePayload{U: strings.TrimRight(publicURL, "/"), T: token})
	return base64.RawURLEncoding.EncodeToString(data)
}

func decodeShareCode(code string) (shareCodePayload, error) {
	var p shareCodePayload
	code = strings.TrimSpace(code)
	data, err := base64.RawURLEncoding.DecodeString(code)
	if err != nil {
		// Tolerate a padded/standard-base64 paste too.
		data, err = base64.StdEncoding.DecodeString(code)
		if err != nil {
			return p, fmt.Errorf("that share code isn't valid")
		}
	}
	if err := json.Unmarshal(data, &p); err != nil {
		return p, fmt.Errorf("that share code isn't valid")
	}
	if p.U == "" || p.T == "" {
		return p, fmt.Errorf("that share code is missing a URL or token")
	}
	if u, err := url.Parse(p.U); err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return p, fmt.Errorf("that share code has an invalid URL")
	}
	return p, nil
}

// looksPublicHost reports whether a URL's host looks like a genuinely public
// address rather than a loopback/private one, so the reachability check can
// warn when a URL that verifies is probably only reachable on the LAN.
func looksPublicHost(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	host := u.Hostname()
	if host == "" || strings.EqualFold(host, "localhost") || strings.HasSuffix(strings.ToLower(host), ".local") {
		return false
	}
	if ip := net.ParseIP(host); ip != nil {
		return !(ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified())
	}
	// A bare hostname with a dot (a real domain) is assumed public; a
	// single-label name (no dot) is treated as LAN-only.
	return strings.Contains(host, ".")
}

func (a *App) putShareNonce(nonce string) {
	a.shareNonceMu.Lock()
	defer a.shareNonceMu.Unlock()
	now := time.Now()
	for k, exp := range a.shareNonces {
		if now.After(exp) {
			delete(a.shareNonces, k)
		}
	}
	a.shareNonces[nonce] = now.Add(shareNonceTTL)
}

// consumeShareNonce reports whether a nonce is currently pending and, if so,
// removes it so it can't be replayed.
func (a *App) consumeShareNonce(nonce string) bool {
	a.shareNonceMu.Lock()
	defer a.shareNonceMu.Unlock()
	exp, ok := a.shareNonces[nonce]
	if ok {
		delete(a.shareNonces, nonce)
	}
	return ok && time.Now().Before(exp)
}

func (a *App) isAdminReq(r *http.Request) bool {
	u, ok := userFromContext(r)
	return ok && u.Role == RoleAdmin
}

// handleSharePing echoes a pending verification nonce back. It's public: the
// reachability check works by this instance fetching its own configured public
// URL and confirming the nonce it just generated comes back - which only
// happens if that URL actually routes to this same instance.
func (a *App) handleSharePing(w http.ResponseWriter, r *http.Request) {
	nonce := r.URL.Query().Get("nonce")
	if nonce != "" && a.consumeShareNonce(nonce) {
		writeJSON(w, map[string]any{"ok": true, "nonce": nonce, "app": "mutirec"})
		return
	}
	writeJSON(w, map[string]any{"ok": false, "app": "mutirec"})
}

// handleShareVerify checks that a candidate public URL routes back to this
// instance (and is reachable over the network) before saving it and enabling
// sharing. It generates a nonce and fetches {url}/api/share/ping?nonce=... -
// success means the URL is correctly pointed at this instance. Force skips
// that self-check entirely: the check only proves the sender can reach
// itself, which can pass even when the real problem is that outside clients
// can't (e.g. a firewall/VPN setup that only routes internal traffic) - an
// admin who has already confirmed reachability some other way needs a way
// past a check that can't see that class of problem.
func (a *App) handleShareVerify(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !a.isAdminReq(r) {
		http.Error(w, "admin only", http.StatusForbidden)
		return
	}
	var req struct {
		PublicURL string `json:"publicUrl"`
		ProxyURL  string `json:"proxyUrl"`
		Force     bool   `json:"force"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	base := strings.TrimRight(strings.TrimSpace(req.PublicURL), "/")
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		writeJSON(w, map[string]any{"ok": false, "error": "Enter a full URL including http:// or https://"})
		return
	}
	proxyURL := strings.TrimSpace(req.ProxyURL)
	if proxyURL != "" {
		if _, err := proxyTransport(proxyURL); err != nil {
			writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
			return
		}
	}

	if !req.Force {
		nonce := shortToken()
		a.putShareNonce(nonce)
		pingURL := base + "/api/share/ping?nonce=" + url.QueryEscape(nonce)

		client, err := shareHTTPClient(proxyURL)
		if err != nil {
			writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		client.Timeout = 10 * time.Second
		resp, err := client.Get(pingURL)
		if err != nil {
			writeJSON(w, map[string]any{"ok": false, "error": "Could not reach that URL: " + err.Error() + " - if you're sure the URL is correct and reachable from outside (e.g. a VPN-only firewall is blocking this instance's own check but not real visitors), tick \"Skip verification\" to enable it anyway."})
			return
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		var pong struct {
			OK    bool   `json:"ok"`
			Nonce string `json:"nonce"`
			App   string `json:"app"`
		}
		_ = json.Unmarshal(body, &pong)
		if !pong.OK || pong.Nonce != nonce {
			if pong.App == "mutirec" {
				writeJSON(w, map[string]any{"ok": false, "error": "That URL reached a MutiRec instance, but not this one - double-check it points here."})
				return
			}
			writeJSON(w, map[string]any{"ok": false, "error": "That URL is reachable but didn't respond as this instance (got HTTP " + strconv.Itoa(resp.StatusCode) + ")."})
			return
		}
	}

	a.mu.Lock()
	a.cfg.Settings.Sharing = SharingConfig{Enabled: true, PublicURL: base, ProxyURL: proxyURL, Forced: req.Force}
	if !req.Force {
		a.cfg.Settings.Sharing.VerifiedAt = time.Now().Format(time.RFC3339)
	}
	cfg := a.cfg
	a.mu.Unlock()
	_ = a.persist(cfg)
	if req.Force {
		a.event("warn", "Peer sharing enabled at "+base+" WITHOUT verification (forced by an admin)")
	} else {
		a.event("info", "Peer sharing enabled and verified at "+base)
	}
	writeJSON(w, map[string]any{"ok": true, "publicUrl": base, "public": looksPublicHost(base), "verified": !req.Force, "forced": req.Force})
}

// handleShareConfig returns the current sharing setup, (POST) toggles
// enabled, and/or updates the outbound proxy - the proxy is just a routing
// setting, not something that needs re-verification, so it can be changed
// independently of the enabled flag.
func (a *App) handleShareConfig(w http.ResponseWriter, r *http.Request) {
	if !a.isAdminReq(r) {
		http.Error(w, "admin only", http.StatusForbidden)
		return
	}
	switch r.Method {
	case http.MethodGet:
		s := a.snapshotConfig().Settings.Sharing
		writeJSON(w, map[string]any{
			"enabled":    s.Enabled,
			"publicUrl":  s.PublicURL,
			"verifiedAt": s.VerifiedAt,
			"forced":     s.Forced,
			"proxyUrl":   s.ProxyURL,
			"public":     s.PublicURL != "" && looksPublicHost(s.PublicURL),
		})
	case http.MethodPost:
		var req struct {
			Enabled  bool    `json:"enabled"`
			ProxyURL *string `json:"proxyUrl"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		if req.ProxyURL != nil && *req.ProxyURL != "" {
			if _, err := proxyTransport(*req.ProxyURL); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
		}
		a.mu.Lock()
		a.cfg.Settings.Sharing.Enabled = req.Enabled
		if req.ProxyURL != nil {
			a.cfg.Settings.Sharing.ProxyURL = strings.TrimSpace(*req.ProxyURL)
		}
		cfg := a.cfg
		a.mu.Unlock()
		_ = a.persist(cfg)
		writeJSON(w, map[string]any{"enabled": req.Enabled, "proxyUrl": cfg.Settings.Sharing.ProxyURL})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// resolveSharePaths turns a create-share request (explicit paths + selected
// event IDs + selected stage/channel names) into a deduplicated, validated
// list of recording paths relative to FinishedDir.
func (a *App) resolveSharePaths(cfg AppConfig, explicit, eventIDs, stages []string) []string {
	wantPath := map[string]bool{}
	for _, p := range explicit {
		wantPath[filepath.ToSlash(p)] = true
	}
	wantEvent := map[string]bool{}
	for _, e := range eventIDs {
		wantEvent[e] = true
	}
	wantStage := map[string]bool{}
	for _, s := range stages {
		wantStage[strings.ToLower(s)] = true
	}

	selected := map[string]bool{}
	root := filepath.Clean(cfg.Settings.FinishedDir)
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || strings.HasSuffix(strings.ToLower(p), ".nfo") {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		channel := ""
		if parts := strings.SplitN(rel, "/", 2); len(parts) > 1 {
			channel = parts[0]
		}
		meta := cfg.RecordingMeta[rel]
		if meta.Channel != "" {
			channel = meta.Channel
		}
		if wantPath[rel] || (meta.EventID != "" && wantEvent[meta.EventID]) || (channel != "" && wantStage[strings.ToLower(channel)]) {
			selected[rel] = true
		}
		return nil
	})

	out := make([]string, 0, len(selected))
	for p := range selected {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// handleShares lists this instance's shares or (POST) creates a new one.
func (a *App) handleShares(w http.ResponseWriter, r *http.Request) {
	if !a.isAdminReq(r) {
		http.Error(w, "admin only", http.StatusForbidden)
		return
	}
	switch r.Method {
	case http.MethodGet:
		cfg := a.snapshotConfig()
		type shareView struct {
			Share
			Code  string `json:"code"`
			Count int    `json:"count"`
		}
		out := []shareView{}
		for _, s := range cfg.Shares {
			out = append(out, shareView{Share: s, Code: encodeShareCode(cfg.Settings.Sharing.PublicURL, s.Token), Count: len(s.Paths)})
		}
		writeJSON(w, out)
	case http.MethodPost:
		cfg := a.snapshotConfig()
		if !cfg.Settings.Sharing.Enabled || cfg.Settings.Sharing.PublicURL == "" {
			http.Error(w, "set up and verify a public URL first (Settings → Peer Sharing)", http.StatusPreconditionFailed)
			return
		}
		var req struct {
			Name     string   `json:"name"`
			Paths    []string `json:"paths"`
			EventIDs []string `json:"eventIds"`
			Stages   []string `json:"stages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		paths := a.resolveSharePaths(cfg, req.Paths, req.EventIDs, req.Stages)
		if len(paths) == 0 {
			http.Error(w, "no matching recordings to share", http.StatusBadRequest)
			return
		}
		share := Share{Token: shortToken(), Name: strings.TrimSpace(req.Name), Paths: paths, CreatedAt: time.Now().Format(time.RFC3339)}
		a.mu.Lock()
		a.cfg.Shares = append(a.cfg.Shares, share)
		newCfg := a.cfg
		a.mu.Unlock()
		_ = a.persist(newCfg)
		a.event("info", fmt.Sprintf("Created share %q with %d recording(s)", share.Name, len(paths)))
		writeJSON(w, map[string]any{"share": share, "code": encodeShareCode(newCfg.Settings.Sharing.PublicURL, share.Token), "count": len(paths)})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleShareItem deletes a share by token (revoking the code).
func (a *App) handleShareItem(w http.ResponseWriter, r *http.Request) {
	if !a.isAdminReq(r) {
		http.Error(w, "admin only", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodDelete {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	token := strings.TrimPrefix(r.URL.Path, "/api/shares/")
	a.mu.Lock()
	kept := a.cfg.Shares[:0:0]
	found := false
	for _, s := range a.cfg.Shares {
		if s.Token == token {
			found = true
			continue
		}
		kept = append(kept, s)
	}
	a.cfg.Shares = kept
	newCfg := a.cfg
	a.mu.Unlock()
	if !found {
		http.NotFound(w, r)
		return
	}
	_ = a.persist(newCfg)
	writeJSON(w, map[string]string{"status": "revoked"})
}

func findShare(cfg AppConfig, token string) (Share, bool) {
	for _, s := range cfg.Shares {
		if s.Token == token {
			return s, true
		}
	}
	return Share{}, false
}

func nfoPathFor(abs string) string {
	return strings.TrimSuffix(abs, filepath.Ext(abs)) + ".nfo"
}

// buildShareManifest describes every file in a share, enriched with hashes and
// denormalized library metadata for the receiver.
func (a *App) buildShareManifest(cfg AppConfig, share Share) ShareManifest {
	root := filepath.Clean(cfg.Settings.FinishedDir)
	eventByID := map[string]LibraryEvent{}
	for _, e := range cfg.LibraryEvents {
		eventByID[e.ID] = e
	}
	festByID := map[string]Festival{}
	for _, f := range cfg.Festivals {
		festByID[f.ID] = f
	}
	man := ShareManifest{Name: share.Name, Items: []ShareItem{}}
	for i, rel := range share.Paths {
		abs := filepath.Join(root, filepath.FromSlash(rel))
		info, err := os.Stat(abs)
		if err != nil || info.IsDir() {
			continue
		}
		meta := cfg.RecordingMeta[rel]
		channel := meta.Channel
		if channel == "" {
			if parts := strings.SplitN(rel, "/", 2); len(parts) > 1 {
				channel = parts[0]
			}
		}
		item := ShareItem{
			Index:     i,
			Name:      filepath.Base(rel),
			Channel:   channel,
			Size:      info.Size(),
			Artist:    meta.Artist,
			Start:     meta.Start,
			End:       meta.End,
			Tracklist: meta.Tracklist,
		}
		if h, err := a.fileHash(abs, rel, info.Size(), info.ModTime()); err == nil {
			item.Hash = h
		}
		if _, err := os.Stat(nfoPathFor(abs)); err == nil {
			item.HasNFO = true
		}
		if ev, ok := eventByID[meta.EventID]; ok {
			item.EventName = ev.Name
			if f, ok := festByID[ev.FestivalID]; ok {
				item.FestivalName = f.Name
			}
		}
		man.Items = append(man.Items, item)
	}
	return man
}

// handleShareGet is the public download surface, authenticated purely by the
// unguessable token in the path:
//
//	/api/share/get/{token}            -> the manifest (JSON)
//	/api/share/get/{token}/f/{index}  -> the recording file
//	/api/share/get/{token}/nfo/{index}-> the NFO sidecar (if any)
func (a *App) handleShareGet(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/share/get/")
	parts := strings.Split(rest, "/")
	token := parts[0]
	if token == "" {
		http.NotFound(w, r)
		return
	}
	cfg := a.snapshotConfig()
	if !cfg.Settings.Sharing.Enabled {
		http.Error(w, "sharing is disabled", http.StatusForbidden)
		return
	}
	share, ok := findShare(cfg, token)
	if !ok {
		http.Error(w, "no such share", http.StatusNotFound)
		return
	}
	root := filepath.Clean(cfg.Settings.FinishedDir)

	// Manifest.
	if len(parts) == 1 {
		writeJSON(w, a.buildShareManifest(cfg, share))
		return
	}
	// File or NFO by index.
	if len(parts) != 3 || (parts[1] != "f" && parts[1] != "nfo") {
		http.NotFound(w, r)
		return
	}
	idx, err := strconv.Atoi(parts[2])
	if err != nil || idx < 0 || idx >= len(share.Paths) {
		http.NotFound(w, r)
		return
	}
	abs := filepath.Clean(filepath.Join(root, filepath.FromSlash(share.Paths[idx])))
	if !strings.HasPrefix(abs, root) {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}
	if parts[1] == "nfo" {
		abs = nfoPathFor(abs)
	}
	if _, err := os.Stat(abs); err != nil {
		http.NotFound(w, r)
		return
	}
	http.ServeFile(w, r, abs)
}

// fetchShareManifest pulls and parses a share manifest from a share code,
// optionally routed through proxyURL (see SharingConfig.ProxyURL).
func fetchShareManifest(code, proxyURL string) (shareCodePayload, ShareManifest, error) {
	p, err := decodeShareCode(code)
	if err != nil {
		return p, ShareManifest{}, err
	}
	client, err := shareHTTPClient(proxyURL)
	if err != nil {
		return p, ShareManifest{}, err
	}
	resp, err := client.Get(p.U + "/api/share/get/" + url.PathEscape(p.T))
	if err != nil {
		return p, ShareManifest{}, fmt.Errorf("could not reach the sender: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return p, ShareManifest{}, fmt.Errorf("the sender returned HTTP %d (the share may have been revoked)", resp.StatusCode)
	}
	var man ShareManifest
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&man); err != nil {
		return p, ShareManifest{}, fmt.Errorf("the sender's response wasn't a valid manifest")
	}
	return p, man, nil
}

// handleSharePreview fetches a remote manifest so the receiver can see what a
// share code offers before importing.
func (a *App) handleSharePreview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !a.isAdminReq(r) {
		http.Error(w, "admin only", http.StatusForbidden)
		return
	}
	var req struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	proxyURL := a.snapshotConfig().Settings.Sharing.ProxyURL
	_, man, err := fetchShareManifest(req.Code, proxyURL)
	if err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, map[string]any{"ok": true, "manifest": man})
}

// shareImportDest builds a safe destination path for a downloaded item,
// sanitizing the sender-provided channel and filename so nothing can be
// written outside FinishedDir.
func shareImportDest(root, channel, name string) (string, string, error) {
	base := safeName(filepath.Base(name))
	if base == "" || base == "." {
		return "", "", fmt.Errorf("bad filename")
	}
	ch := "shared"
	if strings.TrimSpace(channel) != "" {
		ch = safeName(channel)
	}
	rel := ch + "/" + base
	abs := filepath.Clean(filepath.Join(root, ch, base))
	if abs != filepath.Clean(root+string(os.PathSeparator)+ch+string(os.PathSeparator)+base) {
		return "", "", fmt.Errorf("bad path")
	}
	if !strings.HasPrefix(abs, filepath.Clean(root)+string(os.PathSeparator)) {
		return "", "", fmt.Errorf("bad path")
	}
	return abs, rel, nil
}

// ============================================================================
// Import jobs: a share import runs as a background goroutine so a large
// transfer doesn't need a browser tab left open for hours. The HTTP handler
// only validates the code, fetches the manifest, and starts the job; progress
// (current file, bytes transferred, transfer speed, a running log, and a
// final done/error status) is polled from a separate endpoint and survives
// the requesting page being closed or navigated away from - only an app
// restart clears it, since jobs are kept in memory only.
// ============================================================================

// ShareJobLogLine is one timestamped line in a job's live log.
type ShareJobLogLine struct {
	Time string `json:"time"`
	Text string `json:"text"`
}

// ShareJob tracks one in-progress or finished share import. All fields are
// guarded by mu since the background goroutine writes them while HTTP
// handlers read them concurrently; use view() to get a safe JSON-able copy.
type ShareJob struct {
	mu sync.Mutex

	id         string
	shareName  string
	senderURL  string
	status     string // "running" | "done" | "error"
	startedAt  time.Time
	finishedAt time.Time

	totalFiles, doneFiles, skippedFiles, failedFiles int
	totalBytes, transferredBytes                     int64
	speedBps                                         float64
	currentFile                                      string
	currentFileBytes, currentFileTotal               int64
	errMsg                                           string
	log                                              []ShareJobLogLine

	lastSampleAt    time.Time
	lastSampleBytes int64
}

// ShareJobView is the JSON-safe snapshot of a ShareJob served to the client.
type ShareJobView struct {
	ID               string            `json:"id"`
	ShareName        string            `json:"shareName,omitempty"`
	SenderURL        string            `json:"senderUrl"`
	Status           string            `json:"status"`
	StartedAt        time.Time         `json:"startedAt"`
	FinishedAt       *time.Time        `json:"finishedAt,omitempty"`
	TotalFiles       int               `json:"totalFiles"`
	DoneFiles        int               `json:"doneFiles"`
	SkippedFiles     int               `json:"skippedFiles"`
	FailedFiles      int               `json:"failedFiles"`
	TotalBytes       int64             `json:"totalBytes"`
	TransferredBytes int64             `json:"transferredBytes"`
	SpeedBps         float64           `json:"speedBps"`
	CurrentFile      string            `json:"currentFile,omitempty"`
	CurrentFileBytes int64             `json:"currentFileBytes"`
	CurrentFileTotal int64             `json:"currentFileTotal"`
	Error            string            `json:"error,omitempty"`
	Log              []ShareJobLogLine `json:"log"`
}

func (j *ShareJob) view() ShareJobView {
	j.mu.Lock()
	defer j.mu.Unlock()
	v := ShareJobView{
		ID: j.id, ShareName: j.shareName, SenderURL: j.senderURL, Status: j.status,
		StartedAt: j.startedAt, TotalFiles: j.totalFiles, DoneFiles: j.doneFiles,
		SkippedFiles: j.skippedFiles, FailedFiles: j.failedFiles,
		TotalBytes: j.totalBytes, TransferredBytes: j.transferredBytes, SpeedBps: j.speedBps,
		CurrentFile: j.currentFile, CurrentFileBytes: j.currentFileBytes, CurrentFileTotal: j.currentFileTotal,
		Error: j.errMsg, Log: append([]ShareJobLogLine(nil), j.log...),
	}
	if !j.finishedAt.IsZero() {
		f := j.finishedAt
		v.FinishedAt = &f
	}
	return v
}

func (j *ShareJob) logf(format string, args ...any) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.log = append(j.log, ShareJobLogLine{Time: time.Now().Format(time.RFC3339), Text: fmt.Sprintf(format, args...)})
	if len(j.log) > 500 {
		j.log = j.log[len(j.log)-500:]
	}
}

func (j *ShareJob) startFile(name string, total int64) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.currentFile = name
	j.currentFileBytes = 0
	j.currentFileTotal = total
}

// addBytes records transferred bytes for both the current file and the
// running total, and re-samples transfer speed at most every ~250ms so the
// reported rate reflects recent throughput rather than the cumulative
// average from the very start (which reads misleadingly low after a slow
// first file).
func (j *ShareJob) addBytes(n int64) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.currentFileBytes += n
	j.transferredBytes += n
	now := time.Now()
	if j.lastSampleAt.IsZero() {
		j.lastSampleAt = now
		j.lastSampleBytes = j.transferredBytes
		return
	}
	if elapsed := now.Sub(j.lastSampleAt); elapsed >= 250*time.Millisecond {
		j.speedBps = float64(j.transferredBytes-j.lastSampleBytes) / elapsed.Seconds()
		j.lastSampleAt = now
		j.lastSampleBytes = j.transferredBytes
	}
}

func (j *ShareJob) finishFile(outcome string) {
	j.mu.Lock()
	switch outcome {
	case "done":
		j.doneFiles++
	case "skipped":
		j.skippedFiles++
	case "failed":
		j.failedFiles++
	}
	j.mu.Unlock()
}

func (j *ShareJob) finish(err error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.finishedAt = time.Now()
	j.speedBps = 0
	if err != nil {
		j.status = "error"
		j.errMsg = err.Error()
	} else {
		j.status = "done"
	}
}

func (a *App) putShareJob(job *ShareJob) {
	a.shareJobsMu.Lock()
	defer a.shareJobsMu.Unlock()
	// Cap retained job history so a long-running instance doesn't accumulate
	// unbounded memory from years of transfers - keep the most recent 50.
	if len(a.shareJobs) > 50 {
		oldest, oldestID := time.Now(), ""
		for id, j := range a.shareJobs {
			j.mu.Lock()
			t := j.startedAt
			j.mu.Unlock()
			if t.Before(oldest) {
				oldest, oldestID = t, id
			}
		}
		if oldestID != "" {
			delete(a.shareJobs, oldestID)
		}
	}
	a.shareJobs[job.id] = job
}

func (a *App) getShareJob(id string) (*ShareJob, bool) {
	a.shareJobsMu.Lock()
	defer a.shareJobsMu.Unlock()
	j, ok := a.shareJobs[id]
	return j, ok
}

// handleShareJobs lists all import jobs (running and finished) this instance
// knows about, most recent first, so the Receive view can show live progress
// and past transfer history without needing to keep the tab that started an
// import open.
func (a *App) handleShareJobs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !a.isAdminReq(r) {
		http.Error(w, "admin only", http.StatusForbidden)
		return
	}
	a.shareJobsMu.Lock()
	views := make([]ShareJobView, 0, len(a.shareJobs))
	for _, j := range a.shareJobs {
		views = append(views, j.view())
	}
	a.shareJobsMu.Unlock()
	sort.Slice(views, func(i, k int) bool { return views[i].StartedAt.After(views[k].StartedAt) })
	writeJSON(w, views)
}

// handleShareJobItem returns one job's current status by ID, for polling.
func (a *App) handleShareJobItem(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !a.isAdminReq(r) {
		http.Error(w, "admin only", http.StatusForbidden)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/share/jobs/")
	job, ok := a.getShareJob(id)
	if !ok {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, job.view())
}

// handleShareImport validates the share code, fetches the manifest, and
// starts a background job to download the selected items - returning
// immediately with a job ID to poll rather than blocking the request for
// however long the transfer takes.
func (a *App) handleShareImport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !a.isAdminReq(r) {
		http.Error(w, "admin only", http.StatusForbidden)
		return
	}
	var req struct {
		Code    string `json:"code"`
		Indices []int  `json:"indices"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	payload, man, err := fetchShareManifest(req.Code, a.snapshotConfig().Settings.Sharing.ProxyURL)
	if err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	want := map[int]bool{}
	for _, i := range req.Indices {
		want[i] = true
	}
	all := len(req.Indices) == 0

	var items []ShareItem
	var totalBytes int64
	for _, item := range man.Items {
		if all || want[item.Index] {
			items = append(items, item)
			totalBytes += item.Size
		}
	}
	if len(items) == 0 {
		writeJSON(w, map[string]any{"ok": false, "error": "nothing selected to import"})
		return
	}

	job := &ShareJob{
		id: newID(), shareName: man.Name, senderURL: payload.U, status: "running",
		startedAt: time.Now(), totalFiles: len(items), totalBytes: totalBytes,
	}
	a.putShareJob(job)
	job.logf("Starting import of %d item(s), %s total, from %s", len(items), formatBytesGo(totalBytes), payload.U)
	a.event("info", fmt.Sprintf("Started peer import job %s (%d item(s) from %s)", job.id, len(items), payload.U))

	go a.runShareImportJob(job, payload, items)

	writeJSON(w, map[string]any{"ok": true, "jobId": job.id})
}

// formatBytesGo is a tiny human-readable byte formatter for log lines (the
// frontend does its own formatting for the UI; this is just for /api event
// log and job log text).
func formatBytesGo(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// runShareImportJob is the background worker started by handleShareImport.
// It downloads each selected item, verifies its content hash against the
// manifest (discarding and failing the item on mismatch - a corrupted or
// truncated transfer must never silently join the library), pulls the NFO
// sidecar if present, and finally applies library metadata once for
// everything that succeeded.
func (a *App) runShareImportJob(job *ShareJob, payload shareCodePayload, items []ShareItem) {
	cfg := a.snapshotConfig()
	root := filepath.Clean(cfg.Settings.FinishedDir)
	client, err := shareHTTPClient(cfg.Settings.Sharing.ProxyURL)
	if err != nil {
		job.logf("Cannot start: %s", err)
		job.finish(err)
		return
	}

	type applied struct {
		rel  string
		item ShareItem
	}
	var toApply []applied

	for _, item := range items {
		abs, rel, err := shareImportDest(root, item.Channel, item.Name)
		if err != nil {
			job.logf("Skipping %q: %s", item.Name, err)
			job.finishFile("failed")
			continue
		}
		if _, err := os.Stat(abs); err == nil {
			job.logf("Already have %q — skipped", item.Name)
			job.finishFile("skipped")
			continue
		}
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			job.logf("Could not create directory for %q: %s", item.Name, err)
			job.finishFile("failed")
			continue
		}

		job.startFile(item.Name, item.Size)
		job.logf("Downloading %q (%s)…", item.Name, formatBytesGo(item.Size))
		fileURL := payload.U + "/api/share/get/" + url.PathEscape(payload.T) + "/f/" + strconv.Itoa(item.Index)
		if err := downloadTo(client, fileURL, abs, job.addBytes); err != nil {
			job.logf("Download failed for %q: %s", item.Name, err)
			job.finishFile("failed")
			continue
		}

		if item.Hash != "" {
			info, statErr := os.Stat(abs)
			var gotHash string
			if statErr == nil {
				gotHash, err = a.fileHash(abs, rel, info.Size(), info.ModTime())
			} else {
				err = statErr
			}
			if err != nil || gotHash != item.Hash {
				job.logf("Hash mismatch for %q — discarding (expected %s, got %s)", item.Name, shortHash(item.Hash), shortHash(gotHash))
				_ = os.Remove(abs)
				job.finishFile("failed")
				continue
			}
			job.logf("Verified %q (sha256 %s)", item.Name, shortHash(gotHash))
		}

		if item.HasNFO {
			nfoURL := payload.U + "/api/share/get/" + url.PathEscape(payload.T) + "/nfo/" + strconv.Itoa(item.Index)
			if err := downloadTo(client, nfoURL, nfoPathFor(abs), job.addBytes); err != nil {
				job.logf("Note: could not fetch the .nfo sidecar for %q: %s", item.Name, err)
			}
		}

		toApply = append(toApply, applied{rel: rel, item: item})
		job.finishFile("done")
		go func(finalPath, relPath string) { a.generateThumbnail(finalPath, relPath, false) }(abs, rel)
	}

	if len(toApply) > 0 {
		a.mu.Lock()
		if a.cfg.RecordingMeta == nil {
			a.cfg.RecordingMeta = map[string]RecordingMeta{}
		}
		for _, ap := range toApply {
			eventID := ""
			if ap.item.EventName != "" {
				eventID = a.resolveOrCreateLibraryEventLocked(ap.item.EventName, ap.item.FestivalName)
			}
			a.cfg.RecordingMeta[ap.rel] = RecordingMeta{
				EventID:   eventID,
				Channel:   ap.item.Channel,
				Artist:    ap.item.Artist,
				Start:     ap.item.Start,
				End:       ap.item.End,
				Tracklist: ap.item.Tracklist,
			}
		}
		newCfg := a.cfg
		a.mu.Unlock()
		_ = a.persist(newCfg)
	}

	view := job.view()
	job.logf("Done: %d imported, %d skipped, %d failed", view.DoneFiles, view.SkippedFiles, view.FailedFiles)
	job.finish(nil)
	a.event("info", fmt.Sprintf("Peer import job %s finished: %d imported, %d skipped, %d failed", job.id, view.DoneFiles, view.SkippedFiles, view.FailedFiles))
}

func shortHash(h string) string {
	if len(h) <= 12 {
		return h
	}
	return h[:12]
}

// downloadTo streams a URL to a file via a .part temp file renamed on
// success, so a failed/partial transfer never leaves a truncated file in the
// library. onBytes (may be nil) is called after every chunk written, for
// live transfer-progress reporting.
func downloadTo(client *http.Client, url, dest string, onBytes func(int64)) error {
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	tmp := dest + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	var w io.Writer = f
	if onBytes != nil {
		w = io.MultiWriter(f, progressWriter(onBytes))
	}
	if _, err := io.Copy(w, resp.Body); err != nil {
		f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dest)
}

// progressWriter adapts a byte-count callback to an io.Writer so it can be
// tee'd alongside the real destination file via io.MultiWriter.
type progressWriter func(int64)

func (p progressWriter) Write(b []byte) (int, error) {
	p(int64(len(b)))
	return len(b), nil
}
