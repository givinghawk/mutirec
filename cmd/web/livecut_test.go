package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestLiveCutApp(t *testing.T) *App {
	t.Helper()
	dir := t.TempDir()
	finishedDir := filepath.Join(dir, "finished")
	if err := os.MkdirAll(finishedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	a := &App{
		config:          filepath.Join(dir, "config.json"),
		active:          map[string]*recording{},
		lastFinished:    map[string]string{},
		liveCutSessions: map[string]*LiveCutSession{},
		liveCutJoined:   map[string]*joinedLiveCut{},
	}
	a.cfg.Settings.FinishedDir = finishedDir
	a.cfg.Settings.Sharing.PublicURL = "http://host.example"
	a.cfg.InstanceID = "host-instance"
	a.cfg.UI.AppName = "Host MutiRec"
	return a
}

func viewerRequest(method, path, body string) *http.Request {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	viewer := User{ID: "v1", Username: "viewer1", Role: RoleViewer}
	return req.WithContext(context.WithValue(req.Context(), userContextKey{}, viewer))
}

func TestLiveCutSessionEventsSinceOrderingAndCursor(t *testing.T) {
	s := &LiveCutSession{Token: "tok"}
	s.addEvent("inst-a", "Instance A", "alice")
	s.addEvent("inst-b", "Instance B", "bob")
	s.addEvent("inst-a", "Instance A", "alice")

	all, closed := s.eventsSince(0)
	if closed {
		t.Fatal("session should not be closed")
	}
	if len(all) != 3 {
		t.Fatalf("expected 3 events, got %d", len(all))
	}
	if all[0].Seq != 1 || all[1].Seq != 2 || all[2].Seq != 3 {
		t.Fatalf("expected sequential seqs 1,2,3, got %+v", all)
	}

	onlyLast, _ := s.eventsSince(2)
	if len(onlyLast) != 1 || onlyLast[0].InstanceID != "inst-a" {
		t.Fatalf("expected only the third event since seq 2, got %+v", onlyLast)
	}

	s.close()
	_, closed = s.eventsSince(0)
	if !closed {
		t.Fatal("expected closed=true after close()")
	}
}

func TestHandleLiveCutHostMarkAndFeed(t *testing.T) {
	a := newTestLiveCutApp(t)
	session := &LiveCutSession{Token: "tok123", SourceID: "src1", SourceName: "Mainstage"}
	a.liveCutSessions[session.Token] = session

	body := `{"token":"tok123","instanceId":"guest-1","instanceName":"Guest MutiRec","username":"carol"}`
	req := httptest.NewRequest(http.MethodPost, "/api/livecut/host/mark", strings.NewReader(body))
	rec := httptest.NewRecorder()
	a.handleLiveCutHostMark(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var ev LiveCutEvent
	if err := json.Unmarshal(rec.Body.Bytes(), &ev); err != nil {
		t.Fatal(err)
	}
	if ev.InstanceID != "guest-1" || ev.Username != "carol" || ev.Seq != 1 {
		t.Fatalf("unexpected event: %+v", ev)
	}
	if ev.Ts == 0 {
		t.Fatal("expected the host to stamp a wall-clock ts")
	}

	feedReq := httptest.NewRequest(http.MethodGet, "/api/livecut/host/feed?token=tok123&since=0", nil)
	feedRec := httptest.NewRecorder()
	a.handleLiveCutHostFeed(feedRec, feedReq)
	var feed struct {
		Name   string         `json:"name"`
		Events []LiveCutEvent `json:"events"`
		Closed bool           `json:"closed"`
	}
	if err := json.Unmarshal(feedRec.Body.Bytes(), &feed); err != nil {
		t.Fatal(err)
	}
	if feed.Name != "Mainstage" || len(feed.Events) != 1 || feed.Closed {
		t.Fatalf("unexpected feed: %+v", feed)
	}
}

func TestHandleLiveCutHostMarkUnknownToken(t *testing.T) {
	a := newTestLiveCutApp(t)
	req := httptest.NewRequest(http.MethodPost, "/api/livecut/host/mark", strings.NewReader(`{"token":"nope"}`))
	rec := httptest.NewRecorder()
	a.handleLiveCutHostMark(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for an unknown token, got %d", rec.Code)
	}
}

func TestHandleLiveCutHostMarkClosedSession(t *testing.T) {
	a := newTestLiveCutApp(t)
	session := &LiveCutSession{Token: "tok123"}
	session.close()
	a.liveCutSessions[session.Token] = session
	req := httptest.NewRequest(http.MethodPost, "/api/livecut/host/mark", strings.NewReader(`{"token":"tok123"}`))
	rec := httptest.NewRecorder()
	a.handleLiveCutHostMark(rec, req)
	if rec.Code != http.StatusGone {
		t.Fatalf("expected 410 for a closed session, got %d", rec.Code)
	}
}

// A viewer role (not admin) must still be able to mark - crowdsourcing the
// button press across every user, not just admins, is the point.
func TestHandleLiveCutSessionItemMarkAllowsViewerRole(t *testing.T) {
	a := newTestLiveCutApp(t)
	session := &LiveCutSession{Token: "tok123", SourceName: "Mainstage"}
	a.liveCutSessions[session.Token] = session

	if !rbacAllowed(http.MethodPost, "/api/livecut/sessions/tok123/mark", RoleViewer) {
		t.Fatal("expected rbacAllowed to permit a viewer to POST .../mark")
	}

	req := viewerRequest(http.MethodPost, "/api/livecut/sessions/tok123/mark", "")
	rec := httptest.NewRecorder()
	a.handleLiveCutSessionItem(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var ev LiveCutEvent
	if err := json.Unmarshal(rec.Body.Bytes(), &ev); err != nil {
		t.Fatal(err)
	}
	if ev.InstanceID != "host-instance" || ev.Username != "viewer1" {
		t.Fatalf("unexpected event: %+v", ev)
	}
}

func TestHandleLiveCutImportConvertsOffsetsAndWritesSidecar(t *testing.T) {
	a := newTestLiveCutApp(t)
	startedAt := time.Date(2026, 7, 6, 20, 0, 0, 0, time.UTC)
	finalPath := filepath.Join(a.cfg.Settings.FinishedDir, "stage1", "set.mkv")
	if err := os.MkdirAll(filepath.Dir(finalPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(finalPath, []byte("fake"), 0o644); err != nil {
		t.Fatal(err)
	}
	a.lastFinished["src1"] = finalPath

	session := &LiveCutSession{Token: "tok123", SourceID: "src1", SourceName: "Stage 1", StartedAtMs: startedAt.UnixMilli()}
	session.addEvent("inst-a", "Guest A", "carol") // Ts = now, definitely > startedAtMs
	a.liveCutSessions[session.Token] = session

	rec := httptest.NewRecorder()
	a.handleLiveCutImport(rec, session)
	if rec.Code != 0 && rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Path    string         `json:"path"`
		Added   int            `json:"added"`
		Markers []CutterMarker `json:"markers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad response: %v (body=%s)", err, rec.Body.String())
	}
	if resp.Added != 1 || len(resp.Markers) != 1 {
		t.Fatalf("expected exactly 1 imported marker, got %+v", resp)
	}
	wantOffset := float64(session.Events[0].Ts-startedAt.UnixMilli()) / 1000
	if resp.Markers[0].OffsetSec != wantOffset {
		t.Fatalf("offsetSec = %v, want %v", resp.Markers[0].OffsetSec, wantOffset)
	}
	if !strings.Contains(resp.Markers[0].Name, "Guest A") || !strings.Contains(resp.Markers[0].Name, "carol") {
		t.Fatalf("expected marker name to credit the submitting instance/user, got %q", resp.Markers[0].Name)
	}

	// The sidecar file on disk should match what the response reported.
	data, err := os.ReadFile(sidecarMarkersPath(finalPath))
	if err != nil {
		t.Fatalf("expected a markers sidecar to be written: %v", err)
	}
	var onDisk []CutterMarker
	if err := json.Unmarshal(data, &onDisk); err != nil {
		t.Fatal(err)
	}
	if len(onDisk) != 1 || onDisk[0].OffsetSec != wantOffset {
		t.Fatalf("sidecar on disk = %+v, want one marker with offset %v", onDisk, wantOffset)
	}
}

// TestHandleLiveCutJoinAndProxy exercises the full guest side against a real
// httptest server standing in for "the host": join decodes the code and
// confirms reachability, then feed/mark on the joined session proxy through
// to that host's public endpoints and relay the response back untouched.
func TestHandleLiveCutJoinAndProxy(t *testing.T) {
	hostApp := newTestLiveCutApp(t)
	hostSession := &LiveCutSession{Token: "host-tok", SourceID: "src1", SourceName: "Mainstage"}
	hostApp.liveCutSessions[hostSession.Token] = hostSession

	mux := http.NewServeMux()
	mux.HandleFunc("/api/livecut/host/mark", hostApp.handleLiveCutHostMark)
	mux.HandleFunc("/api/livecut/host/feed", hostApp.handleLiveCutHostFeed)
	hostServer := httptest.NewServer(mux)
	defer hostServer.Close()

	guest := newTestLiveCutApp(t)
	code := encodeShareCode(hostServer.URL, hostSession.Token)

	joinReq := adminRequest(http.MethodPost, "/api/livecut/join", `{"code":"`+code+`"}`)
	joinRec := httptest.NewRecorder()
	guest.handleLiveCutJoin(joinRec, joinReq)
	var joinResp struct {
		OK      bool          `json:"ok"`
		Error   string        `json:"error"`
		Session joinedLiveCut `json:"session"`
	}
	if err := json.Unmarshal(joinRec.Body.Bytes(), &joinResp); err != nil {
		t.Fatalf("bad join response: %v (body=%s)", err, joinRec.Body.String())
	}
	if !joinResp.OK {
		t.Fatalf("expected join to succeed, got error: %s", joinResp.Error)
	}
	if joinResp.Session.Name != "Mainstage" {
		t.Fatalf("expected joined session to pick up the host's session name, got %+v", joinResp.Session)
	}

	// Guest presses Mark - should be relayed to the host and stamped with
	// the guest's own instance identity by handleLiveCutJoinedItem, not the
	// host's.
	markReq := viewerRequest(http.MethodPost, "/api/livecut/joined/host-tok/mark", "")
	markRec := httptest.NewRecorder()
	guest.handleLiveCutJoinedItem(markRec, markReq)
	if markRec.Code != http.StatusOK {
		t.Fatalf("expected mark proxy to succeed, got %d: %s", markRec.Code, markRec.Body.String())
	}

	hostEvents, _ := hostSession.eventsSince(0)
	if len(hostEvents) != 1 {
		t.Fatalf("expected the host to have received exactly 1 mark, got %d", len(hostEvents))
	}
	if hostEvents[0].InstanceID != guest.cfg.InstanceID {
		t.Fatalf("expected the mark to be tagged with the guest's own InstanceID, got %q", hostEvents[0].InstanceID)
	}

	// Guest polls the feed - should proxy through and see that same mark.
	feedReq := viewerRequest(http.MethodGet, "/api/livecut/joined/host-tok/feed?since=0", "")
	feedRec := httptest.NewRecorder()
	guest.handleLiveCutJoinedItem(feedRec, feedReq)
	var feed struct {
		Events []LiveCutEvent `json:"events"`
	}
	if err := json.Unmarshal(feedRec.Body.Bytes(), &feed); err != nil {
		t.Fatalf("bad feed response: %v (body=%s)", err, feedRec.Body.String())
	}
	if len(feed.Events) != 1 || feed.Events[0].InstanceID != guest.cfg.InstanceID {
		t.Fatalf("expected the proxied feed to show the guest's own mark, got %+v", feed.Events)
	}
}
