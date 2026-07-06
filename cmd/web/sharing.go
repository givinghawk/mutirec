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
// reach this one at, whether sharing is enabled, and when the URL last passed
// the reachability check.
type SharingConfig struct {
	Enabled    bool   `json:"enabled"`
	PublicURL  string `json:"publicUrl"`
	VerifiedAt string `json:"verifiedAt,omitempty"`
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

func shareHTTPClient() *http.Client {
	return &http.Client{Timeout: 30 * time.Second}
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
// success means the URL is correctly pointed at this instance.
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

	nonce := shortToken()
	a.putShareNonce(nonce)
	pingURL := base + "/api/share/ping?nonce=" + url.QueryEscape(nonce)

	client := shareHTTPClient()
	client.Timeout = 10 * time.Second
	resp, err := client.Get(pingURL)
	if err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": "Could not reach that URL: " + err.Error()})
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

	a.mu.Lock()
	a.cfg.Settings.Sharing = SharingConfig{Enabled: true, PublicURL: base, VerifiedAt: time.Now().Format(time.RFC3339)}
	cfg := a.cfg
	a.mu.Unlock()
	_ = a.persist(cfg)
	a.event("info", "Peer sharing enabled and verified at "+base)
	writeJSON(w, map[string]any{"ok": true, "publicUrl": base, "public": looksPublicHost(base)})
}

// handleShareConfig returns the current sharing setup, or (POST) disables it.
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
			"public":     s.PublicURL != "" && looksPublicHost(s.PublicURL),
		})
	case http.MethodPost:
		var req struct {
			Enabled bool `json:"enabled"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		a.mu.Lock()
		a.cfg.Settings.Sharing.Enabled = req.Enabled
		cfg := a.cfg
		a.mu.Unlock()
		_ = a.persist(cfg)
		writeJSON(w, map[string]any{"enabled": req.Enabled})
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

// fetchShareManifest pulls and parses a share manifest from a share code.
func fetchShareManifest(code string) (shareCodePayload, ShareManifest, error) {
	p, err := decodeShareCode(code)
	if err != nil {
		return p, ShareManifest{}, err
	}
	client := shareHTTPClient()
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
	_, man, err := fetchShareManifest(req.Code)
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

// handleShareImport downloads the selected items from a share into this
// instance's library and applies their metadata (resolving/creating the
// LibraryEvent/Festival by name, like matchfile import). Existing files are
// left untouched.
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
	payload, man, err := fetchShareManifest(req.Code)
	if err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	want := map[int]bool{}
	for _, i := range req.Indices {
		want[i] = true
	}
	all := len(req.Indices) == 0

	cfg := a.snapshotConfig()
	root := filepath.Clean(cfg.Settings.FinishedDir)
	client := shareHTTPClient()

	imported, skipped, failed := 0, 0, 0
	type applied struct {
		rel  string
		item ShareItem
	}
	var toApply []applied

	for _, item := range man.Items {
		if !all && !want[item.Index] {
			continue
		}
		abs, rel, err := shareImportDest(root, item.Channel, item.Name)
		if err != nil {
			failed++
			continue
		}
		if _, err := os.Stat(abs); err == nil {
			skipped++ // don't clobber an existing local file
			continue
		}
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			failed++
			continue
		}
		if err := downloadTo(client, payload.U+"/api/share/get/"+url.PathEscape(payload.T)+"/f/"+strconv.Itoa(item.Index), abs); err != nil {
			failed++
			continue
		}
		if item.HasNFO {
			_ = downloadTo(client, payload.U+"/api/share/get/"+url.PathEscape(payload.T)+"/nfo/"+strconv.Itoa(item.Index), nfoPathFor(abs))
		}
		toApply = append(toApply, applied{rel: rel, item: item})
		imported++
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
	a.event("info", fmt.Sprintf("Imported %d recording(s) from a peer share (%d skipped, %d failed)", imported, skipped, failed))
	writeJSON(w, map[string]any{"ok": true, "imported": imported, "skipped": skipped, "failed": failed})
}

// downloadTo streams a URL to a file via a .part temp file renamed on success,
// so a failed/partial transfer never leaves a truncated file in the library.
func downloadTo(client *http.Client, url, dest string) error {
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
	if _, err := io.Copy(f, resp.Body); err != nil {
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
