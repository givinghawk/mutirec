package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// ============================================================================
// Twitch Helix API: only used to look up a channel's current live stream
// title, so it can be used as the recording's NFO title instead of the
// source's own (often generic, e.g. just the channel name) display name.
// Everything read here is public channel/stream info, so authentication
// uses an app access token (client ID + secret, client-credentials grant) -
// no per-user OAuth consent needed, unlike the pasted-refresh-token flow
// YouTube auto-upload uses for something that actually needs a real user
// account.
// ============================================================================

// twitchTokenURL and twitchStreamsURL are package-level vars (not consts)
// purely so a test can point them at a local httptest.Server instead of the
// real Twitch endpoints.
var (
	twitchTokenURL   = "https://id.twitch.tv/oauth2/token"
	twitchStreamsURL = "https://api.twitch.tv/helix/streams"
)

// twitchAppToken caches the app access token so a title lookup doesn't
// re-authenticate every time - client-credentials tokens are typically
// valid for ~60 days. Guarded by its own mutex; never copied by value (see
// App.twitchToken).
type twitchAppToken struct {
	mu        sync.Mutex
	token     string
	expiresAt time.Time
}

// twitchAccessToken returns a cached app access token, fetching a new one
// via the client-credentials grant if there's none cached yet or the
// cached one is at (or near) expiry.
func (a *App) twitchAccessToken(cfg TwitchConfig) (string, error) {
	a.twitchToken.mu.Lock()
	defer a.twitchToken.mu.Unlock()
	if a.twitchToken.token != "" && time.Now().Before(a.twitchToken.expiresAt) {
		return a.twitchToken.token, nil
	}
	form := url.Values{
		"client_id":     {cfg.ClientID},
		"client_secret": {cfg.ClientSecret},
		"grant_type":    {"client_credentials"},
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.PostForm(twitchTokenURL, form)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if out.AccessToken == "" {
		return "", fmt.Errorf("Twitch did not return an access token (HTTP %d) - check the client ID/secret", resp.StatusCode)
	}
	a.twitchToken.token = out.AccessToken
	// Refresh a minute early so a request never fires with a token that
	// expires mid-flight.
	a.twitchToken.expiresAt = time.Now().Add(time.Duration(out.ExpiresIn)*time.Second - time.Minute)
	return out.AccessToken, nil
}

// fetchTwitchStreamTitle looks up channel's current live stream title via
// the Helix "Get Streams" endpoint. Best-effort: Twitch not configured, the
// channel being offline, or any request failure just returns "", and the
// caller falls back to the source's own display name - same behavior as
// before this existed.
func (a *App) fetchTwitchStreamTitle(channel string) string {
	cfg := a.snapshotConfig().Settings.Twitch
	if !cfg.Enabled || cfg.ClientID == "" || cfg.ClientSecret == "" || channel == "" {
		return ""
	}
	token, err := a.twitchAccessToken(cfg)
	if err != nil {
		return ""
	}
	req, err := http.NewRequest(http.MethodGet, twitchStreamsURL+"?user_login="+url.QueryEscape(channel), nil)
	if err != nil {
		return ""
	}
	req.Header.Set("Client-Id", cfg.ClientID)
	req.Header.Set("Authorization", "Bearer "+token)
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	var out struct {
		Data []struct {
			Title string `json:"title"`
		} `json:"data"`
	}
	if json.NewDecoder(resp.Body).Decode(&out) != nil || len(out.Data) == 0 {
		return ""
	}
	return strings.TrimSpace(out.Data[0].Title)
}
