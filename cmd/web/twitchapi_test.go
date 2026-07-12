package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// withFakeTwitchAPI points twitchTokenURL/twitchStreamsURL at local test
// servers for the duration of the test, restoring the real URLs afterward.
func withFakeTwitchAPI(t *testing.T, tokenHandler, streamsHandler http.HandlerFunc) {
	t.Helper()
	tokenSrv := httptest.NewServer(tokenHandler)
	streamsSrv := httptest.NewServer(streamsHandler)
	t.Cleanup(func() {
		tokenSrv.Close()
		streamsSrv.Close()
	})
	origToken, origStreams := twitchTokenURL, twitchStreamsURL
	twitchTokenURL, twitchStreamsURL = tokenSrv.URL, streamsSrv.URL
	t.Cleanup(func() { twitchTokenURL, twitchStreamsURL = origToken, origStreams })
}

func TestFetchTwitchStreamTitleReturnsTitle(t *testing.T) {
	withFakeTwitchAPI(t,
		func(w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode(map[string]any{"access_token": "tok123", "expires_in": 3600})
		},
		func(w http.ResponseWriter, r *http.Request) {
			if got := r.Header.Get("Authorization"); got != "Bearer tok123" {
				t.Errorf("expected Authorization: Bearer tok123, got %q", got)
			}
			if got := r.URL.Query().Get("user_login"); got != "somechannel" {
				t.Errorf("expected user_login=somechannel, got %q", got)
			}
			json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"title": "Grinding ranked"}}})
		},
	)
	a := &App{cfg: AppConfig{Settings: Settings{Twitch: TwitchConfig{Enabled: true, ClientID: "cid", ClientSecret: "secret"}}}}
	got := a.fetchTwitchStreamTitle("somechannel")
	if got != "Grinding ranked" {
		t.Fatalf("got %q", got)
	}
}

func TestFetchTwitchStreamTitleEmptyWhenOffline(t *testing.T) {
	withFakeTwitchAPI(t,
		func(w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode(map[string]any{"access_token": "tok123", "expires_in": 3600})
		},
		func(w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{}})
		},
	)
	a := &App{cfg: AppConfig{Settings: Settings{Twitch: TwitchConfig{Enabled: true, ClientID: "cid", ClientSecret: "secret"}}}}
	if got := a.fetchTwitchStreamTitle("somechannel"); got != "" {
		t.Fatalf("expected empty title for an offline channel, got %q", got)
	}
}

func TestFetchTwitchStreamTitleEmptyWhenNotConfigured(t *testing.T) {
	a := &App{cfg: AppConfig{Settings: Settings{Twitch: TwitchConfig{Enabled: false}}}}
	if got := a.fetchTwitchStreamTitle("somechannel"); got != "" {
		t.Fatalf("expected empty title when Twitch isn't configured, got %q", got)
	}
}

func TestTwitchAccessTokenCachesUntilExpiry(t *testing.T) {
	calls := 0
	withFakeTwitchAPI(t,
		func(w http.ResponseWriter, r *http.Request) {
			calls++
			json.NewEncoder(w).Encode(map[string]any{"access_token": "tok123", "expires_in": 3600})
		},
		func(w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"title": "t"}}})
		},
	)
	a := &App{cfg: AppConfig{Settings: Settings{Twitch: TwitchConfig{Enabled: true, ClientID: "cid", ClientSecret: "secret"}}}}
	a.fetchTwitchStreamTitle("a")
	a.fetchTwitchStreamTitle("b")
	if calls != 1 {
		t.Fatalf("expected the token endpoint to be called once (cached after), got %d calls", calls)
	}
}
