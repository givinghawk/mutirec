package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ============================================================================
// Live Cut Sessions - crowdsourced live transition marking
//
// One instance ("host") ties a session to one of its own live sources. Other
// MutiRec instances ("guests") join with a short code - the exact same
// {u: publicURL, t: token} shape as a P2P share code (see sharing.go),
// reusing encodeShareCode/decodeShareCode as-is. Any authenticated user on a
// joined instance (not just admins) can then press "Mark Transition" - the
// host stamps every mark with its own wall-clock time regardless of which
// instance it came from, so cross-instance clock skew never enters the
// picture, and tags it with the submitting instance's InstanceID/name and
// username. Once the tied recording exists, the host can convert the
// collected marks into CutterMarker offsets for the Set Cutter.
//
// Like Share/ShareJob, sessions are deliberately in-memory only - never
// persisted to config.json - and cleared on restart.
// ============================================================================

// LiveCutEvent is one "mark" pressed by someone in a session, as the host
// recorded it. Seq is a per-session monotonically increasing cursor so
// clients only ask for what's new since their last poll.
type LiveCutEvent struct {
	Seq          int64  `json:"seq"`
	Ts           int64  `json:"ts"` // ms since epoch, the host's clock
	InstanceID   string `json:"instanceId"`
	InstanceName string `json:"instanceName,omitempty"`
	Username     string `json:"username,omitempty"`
}

// LiveCutSession is one crowdsourced live-cut session, hosted by this
// instance. Guarded by its own mutex (not AppConfig's) since it's a
// high-frequency, ephemeral structure - go vet's copylocks check means this
// must only ever be handled as *LiveCutSession, never copied by value (see
// view() below for the DTO used to cross the JSON boundary).
type LiveCutSession struct {
	mu          sync.Mutex
	Token       string
	SourceID    string
	SourceName  string
	StartedAtMs int64 // the tied recording's recording.startedAt, captured at creation
	CreatedAt   time.Time
	ClosedAt    time.Time
	Events      []LiveCutEvent
	nextSeq     int64
}

type LiveCutSessionView struct {
	Token      string         `json:"token"`
	Code       string         `json:"code"`
	SourceID   string         `json:"sourceId"`
	SourceName string         `json:"sourceName"`
	CreatedAt  time.Time      `json:"createdAt"`
	Closed     bool           `json:"closed"`
	Events     []LiveCutEvent `json:"events"`
}

func (s *LiveCutSession) view(code string) LiveCutSessionView {
	s.mu.Lock()
	defer s.mu.Unlock()
	events := make([]LiveCutEvent, len(s.Events))
	copy(events, s.Events)
	return LiveCutSessionView{
		Token: s.Token, Code: code, SourceID: s.SourceID, SourceName: s.SourceName,
		CreatedAt: s.CreatedAt, Closed: !s.ClosedAt.IsZero(), Events: events,
	}
}

func (s *LiveCutSession) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.ClosedAt.IsZero()
}

func (s *LiveCutSession) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ClosedAt.IsZero() {
		s.ClosedAt = time.Now()
	}
}

func (s *LiveCutSession) addEvent(instanceID, instanceName, username string) LiveCutEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextSeq++
	ev := LiveCutEvent{Seq: s.nextSeq, Ts: time.Now().UnixMilli(), InstanceID: instanceID, InstanceName: instanceName, Username: username}
	s.Events = append(s.Events, ev)
	return ev
}

// eventsSince returns every event with Seq > since, in order, plus whether
// the session has been closed.
func (s *LiveCutSession) eventsSince(since int64) (out []LiveCutEvent, closed bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ev := range s.Events {
		if ev.Seq > since {
			out = append(out, ev)
		}
	}
	return out, !s.ClosedAt.IsZero()
}

// joinedLiveCut is a session hosted *elsewhere* that this instance has
// joined - just enough to relay mark/feed calls to the host without making
// the guest paste the code again for every button press. Immutable after
// creation, so no mutex is needed on the struct itself (only on the map).
type joinedLiveCut struct {
	Token    string    `json:"token"`
	HostURL  string    `json:"hostUrl"`
	Name     string    `json:"name"`
	JoinedAt time.Time `json:"joinedAt"`
}

func (a *App) getLiveCutSession(token string) (*LiveCutSession, bool) {
	a.liveCutMu.Lock()
	defer a.liveCutMu.Unlock()
	s, ok := a.liveCutSessions[token]
	return s, ok
}

// handleLiveCutSessions lists (GET) this instance's own hosted sessions, or
// creates one (POST) tied to a currently-recording local source.
func (a *App) handleLiveCutSessions(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		cfg := a.snapshotConfig()
		a.liveCutMu.Lock()
		sessions := make([]*LiveCutSession, 0, len(a.liveCutSessions))
		for _, s := range a.liveCutSessions {
			sessions = append(sessions, s)
		}
		a.liveCutMu.Unlock()
		out := make([]LiveCutSessionView, 0, len(sessions))
		for _, s := range sessions {
			out = append(out, s.view(encodeShareCode(cfg.Settings.Sharing.PublicURL, s.Token)))
		}
		writeJSON(w, out)
	case http.MethodPost:
		if !a.isAdminReq(r) {
			http.Error(w, "admin only", http.StatusForbidden)
			return
		}
		cfg := a.snapshotConfig()
		if cfg.Settings.Sharing.PublicURL == "" {
			http.Error(w, "set up and verify a public URL first (Settings → Peer Sharing)", http.StatusPreconditionFailed)
			return
		}
		var req struct {
			SourceID string `json:"sourceId"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		var src Source
		found := false
		for _, s := range cfg.Sources {
			if s.ID == req.SourceID {
				src = s
				found = true
				break
			}
		}
		if !found {
			http.Error(w, "unknown source", http.StatusBadRequest)
			return
		}
		var startedAtMs int64
		a.mu.RLock()
		if rec, ok := a.active[req.SourceID]; ok {
			startedAtMs = rec.startedAt.UnixMilli()
		}
		a.mu.RUnlock()
		if startedAtMs == 0 {
			http.Error(w, "this source isn't actively recording right now", http.StatusPreconditionFailed)
			return
		}
		session := &LiveCutSession{Token: shortToken(), SourceID: src.ID, SourceName: src.Name, StartedAtMs: startedAtMs, CreatedAt: time.Now()}
		a.liveCutMu.Lock()
		a.liveCutSessions[session.Token] = session
		a.liveCutMu.Unlock()
		a.event("info", fmt.Sprintf("Started a Live Cut Session for %q", src.Name))
		writeJSON(w, session.view(encodeShareCode(cfg.Settings.Sharing.PublicURL, session.Token)))
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleLiveCutSessionItem handles everything addressed under
// /api/livecut/sessions/{token}[/mark|/feed|/import] - a bare DELETE closes
// the session, the three suffixes are the local (session-authed) equivalents
// of the public host endpoints below, used by this instance's own browser.
func (a *App) handleLiveCutSessionItem(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/livecut/sessions/")
	parts := strings.SplitN(rest, "/", 2)
	token := parts[0]
	action := ""
	if len(parts) == 2 {
		action = parts[1]
	}
	session, ok := a.getLiveCutSession(token)
	if !ok {
		http.NotFound(w, r)
		return
	}
	switch action {
	case "":
		if r.Method != http.MethodDelete {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !a.isAdminReq(r) {
			http.Error(w, "admin only", http.StatusForbidden)
			return
		}
		session.close()
		writeJSON(w, map[string]string{"status": "closed"})
	case "feed":
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
		events, closed := session.eventsSince(since)
		writeJSON(w, map[string]any{"events": events, "closed": closed})
	case "mark":
		// Any authenticated role can mark, not just admins - crowdsourcing the
		// button press is the whole point.
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if session.isClosed() {
			http.Error(w, "this session has been closed", http.StatusGone)
			return
		}
		u, _ := userFromContext(r)
		cfg := a.snapshotConfig()
		ev := session.addEvent(cfg.InstanceID, cfg.UI.AppName, u.Username)
		writeJSON(w, ev)
	case "import":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !a.isAdminReq(r) {
			http.Error(w, "admin only", http.StatusForbidden)
			return
		}
		a.handleLiveCutImport(w, session)
	default:
		http.NotFound(w, r)
	}
}

// handleLiveCutImport converts a session's collected marks into CutterMarker
// offsets (wall-clock ts minus the tied recording's startedAt) and merges
// them into that recording's marker sidecar, the same file
// handleCutterMarkers reads/writes.
func (a *App) handleLiveCutImport(w http.ResponseWriter, session *LiveCutSession) {
	session.mu.Lock()
	startedAtMs := session.StartedAtMs
	events := make([]LiveCutEvent, len(session.Events))
	copy(events, session.Events)
	sourceID := session.SourceID
	session.mu.Unlock()

	a.mu.RLock()
	finalPath, ok := a.lastFinished[sourceID]
	if !ok {
		if rec, active := a.active[sourceID]; active {
			finalPath = rec.finalPath
			ok = true
		}
	}
	a.mu.RUnlock()
	if !ok {
		http.Error(w, "no recording found for this source yet", http.StatusPreconditionFailed)
		return
	}

	cfg := a.snapshotConfig()
	rel, err := filepath.Rel(cfg.Settings.FinishedDir, finalPath)
	if err != nil {
		http.Error(w, "could not resolve the recording path", http.StatusInternalServerError)
		return
	}
	relPath := filepath.ToSlash(rel)
	abs, ok := a.resolveRecordingPath(relPath)
	if !ok {
		http.Error(w, "invalid recording path", http.StatusInternalServerError)
		return
	}

	var existing []CutterMarker
	if data, err := os.ReadFile(sidecarMarkersPath(abs)); err == nil {
		_ = json.Unmarshal(data, &existing)
	}
	added := make([]CutterMarker, 0, len(events))
	for _, ev := range events {
		offsetSec := float64(ev.Ts-startedAtMs) / 1000
		if offsetSec < 0 {
			continue
		}
		who := ev.InstanceName
		if who == "" {
			who = ev.InstanceID
		}
		if ev.Username != "" {
			who = fmt.Sprintf("%s (%s)", who, ev.Username)
		}
		added = append(added, CutterMarker{ID: newID(), OffsetSec: offsetSec, Name: fmt.Sprintf("Live cut - %s", who)})
	}
	merged := append(existing, added...)
	if err := writeSidecarJSON(sidecarMarkersPath(abs), merged); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"path": relPath, "added": len(added), "markers": merged})
}

// handleLiveCutHostMark lets a remote instance push a mark into a session
// this instance is hosting. Public/token-authed like /api/share/get/ - the
// token is the only credential, since the caller has no account here.
func (a *App) handleLiveCutHostMark(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Token        string `json:"token"`
		InstanceID   string `json:"instanceId"`
		InstanceName string `json:"instanceName"`
		Username     string `json:"username"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	session, ok := a.getLiveCutSession(req.Token)
	if !ok {
		http.Error(w, "unknown or expired session", http.StatusNotFound)
		return
	}
	if session.isClosed() {
		http.Error(w, "this session has been closed", http.StatusGone)
		return
	}
	ev := session.addEvent(req.InstanceID, req.InstanceName, req.Username)
	writeJSON(w, ev)
}

// handleLiveCutHostFeed is the public counterpart of the "feed" action on
// handleLiveCutSessionItem, reachable by token alone so a remote instance's
// backend can poll it directly without an account here.
func (a *App) handleLiveCutHostFeed(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	session, ok := a.getLiveCutSession(r.URL.Query().Get("token"))
	if !ok {
		http.Error(w, "unknown or expired session", http.StatusNotFound)
		return
	}
	since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
	events, closed := session.eventsSince(since)
	session.mu.Lock()
	name := session.SourceName
	session.mu.Unlock()
	writeJSON(w, map[string]any{"name": name, "events": events, "closed": closed})
}

// handleLiveCutJoin decodes a share-code-shaped join code (reusing
// decodeShareCode as-is - identical {u, t} payload) and, after confirming the
// remote session is actually reachable, remembers it locally so this
// instance's users don't need to paste the code again for every mark/feed
// call.
func (a *App) handleLiveCutJoin(w http.ResponseWriter, r *http.Request) {
	if !a.isAdminReq(r) {
		http.Error(w, "admin only", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	payload, err := decodeShareCode(req.Code)
	if err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	cfg := a.snapshotConfig()
	client, err := shareHTTPClient(cfg.Settings.Sharing.ProxyURL)
	if err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	client.Timeout = 10 * time.Second
	resp, err := client.Get(payload.U + "/api/livecut/host/feed?token=" + url.QueryEscape(payload.T))
	if err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": "could not reach that instance"})
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		writeJSON(w, map[string]any{"ok": false, "error": "that code isn't valid, or the session has ended"})
		return
	}
	var feed struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&feed); err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": "unexpected response from that instance"})
		return
	}
	joined := &joinedLiveCut{Token: payload.T, HostURL: payload.U, Name: feed.Name, JoinedAt: time.Now()}
	a.liveCutJoinedMu.Lock()
	a.liveCutJoined[joined.Token] = joined
	a.liveCutJoinedMu.Unlock()
	a.event("info", fmt.Sprintf("Joined Live Cut Session %q", feed.Name))
	writeJSON(w, map[string]any{"ok": true, "session": joined})
}

func (a *App) handleLiveCutJoinedList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	a.liveCutJoinedMu.Lock()
	out := make([]*joinedLiveCut, 0, len(a.liveCutJoined))
	for _, j := range a.liveCutJoined {
		out = append(out, j)
	}
	a.liveCutJoinedMu.Unlock()
	writeJSON(w, out)
}

// handleLiveCutJoinedItem relays feed/mark calls for a joined session to
// whichever instance is actually hosting it (a thin read/write-through proxy
// - no local caching, since polling is already cheap and this avoids a
// second place marks could get out of sync), or forgets the session (leave).
func (a *App) handleLiveCutJoinedItem(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/livecut/joined/")
	parts := strings.SplitN(rest, "/", 2)
	token := parts[0]
	action := ""
	if len(parts) == 2 {
		action = parts[1]
	}
	a.liveCutJoinedMu.Lock()
	joined, ok := a.liveCutJoined[token]
	a.liveCutJoinedMu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}

	if action == "" {
		if r.Method != http.MethodDelete {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !a.isAdminReq(r) {
			http.Error(w, "admin only", http.StatusForbidden)
			return
		}
		a.liveCutJoinedMu.Lock()
		delete(a.liveCutJoined, token)
		a.liveCutJoinedMu.Unlock()
		writeJSON(w, map[string]string{"status": "left"})
		return
	}

	cfg := a.snapshotConfig()
	client, err := shareHTTPClient(cfg.Settings.Sharing.ProxyURL)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	client.Timeout = 10 * time.Second

	switch action {
	case "feed":
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		since := r.URL.Query().Get("since")
		remote := joined.HostURL + "/api/livecut/host/feed?token=" + url.QueryEscape(joined.Token) + "&since=" + url.QueryEscape(since)
		resp, err := client.Get(remote)
		if err != nil {
			http.Error(w, "could not reach that instance", http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	case "mark":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		u, _ := userFromContext(r)
		body, _ := json.Marshal(map[string]string{
			"token": joined.Token, "instanceId": cfg.InstanceID, "instanceName": cfg.UI.AppName, "username": u.Username,
		})
		resp, err := client.Post(joined.HostURL+"/api/livecut/host/mark", "application/json", bytes.NewReader(body))
		if err != nil {
			http.Error(w, "could not reach that instance", http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	default:
		http.NotFound(w, r)
	}
}
