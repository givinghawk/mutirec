package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSendDiscordWebhookSuccess(t *testing.T) {
	var gotBody map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("expected application/json, got %q", ct)
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	if err := sendDiscordWebhook(srv.URL, "Subject", "Body"); err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if gotBody["content"] != "Subject\nBody" {
		t.Errorf("content = %q, want %q", gotBody["content"], "Subject\nBody")
	}
}

// A webhook that's been deleted, rate-limited, or misconfigured must surface
// as an error the caller can log - the old code silently swallowed this.
func TestSendDiscordWebhookFailureSurfacesError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "Unknown Webhook", http.StatusNotFound)
	}))
	defer srv.Close()

	err := sendDiscordWebhook(srv.URL, "Subject", "Body")
	if err == nil {
		t.Fatal("expected an error for a 404 response, got nil")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("expected the error to mention the status, got %v", err)
	}
}

// A very long body (e.g. a full ffmpeg error message) must be truncated to
// Discord's content limit rather than sent as-is and rejected outright.
func TestSendDiscordWebhookTruncatesLongContent(t *testing.T) {
	var gotBody map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	longBody := strings.Repeat("x", discordContentLimit*2)
	if err := sendDiscordWebhook(srv.URL, "Subject", longBody); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	gotRunes := []rune(gotBody["content"])
	if len(gotRunes) != discordContentLimit {
		t.Errorf("content rune count = %d, want exactly %d (the limit)", len(gotRunes), discordContentLimit)
	}
	if !strings.HasSuffix(gotBody["content"], "…") {
		t.Error("expected truncated content to end with an ellipsis marker")
	}
}

func TestHandleNotificationsTestNothingConfigured(t *testing.T) {
	a := &App{}
	req := adminRequest(http.MethodPost, "/api/notifications/test", `{}`)
	rec := httptest.NewRecorder()
	a.handleNotificationsTest(rec, req)
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["error"] == nil {
		t.Fatalf("expected an error when nothing is configured to test, got %+v", resp)
	}
}

func TestHandleNotificationsTestDiscordOnly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	a := &App{}
	body := `{"discordWebhook":"` + srv.URL + `"}`
	req := adminRequest(http.MethodPost, "/api/notifications/test", body)
	rec := httptest.NewRecorder()
	a.handleNotificationsTest(rec, req)

	var resp struct {
		Discord notificationTestResult `json:"discord"`
		SMTP    notificationTestResult `json:"smtp"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad response: %v (body=%s)", err, rec.Body.String())
	}
	if !resp.Discord.Tested || !resp.Discord.Ok {
		t.Fatalf("expected Discord to be tested and ok, got %+v", resp.Discord)
	}
	if resp.SMTP.Tested {
		t.Fatalf("expected SMTP not to be tested when it wasn't filled in, got %+v", resp.SMTP)
	}
}

func TestHandleNotificationsTestRequiresAdmin(t *testing.T) {
	a := &App{}
	req := viewerRequest(http.MethodPost, "/api/notifications/test", `{"discordWebhook":"http://example.invalid"}`)
	rec := httptest.NewRecorder()
	a.handleNotificationsTest(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for a non-admin, got %d", rec.Code)
	}
}
