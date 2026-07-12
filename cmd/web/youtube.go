package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ============================================================================
// YouTube auto-upload: after a finished recording, optionally push it to
// YouTube as a private or unlisted video (per source, defaulting to
// unlisted). Authenticates with a long-lived OAuth2 refresh token pasted
// into Settings (paired with a Google Cloud OAuth client ID/secret) rather
// than a full interactive consent flow - the admin generates the refresh
// token once (e.g. via Google's OAuth playground) and pastes all three
// values in. Mirrors a.backup()'s shape: best-effort, event-logged, never
// blocks or fails the recording itself.
// ============================================================================

// youtubeTokenURL and youtubeUploadURL are package-level vars (not consts)
// purely so a test can point them at a local httptest.Server instead of the
// real Google/YouTube endpoints.
var (
	youtubeTokenURL  = "https://oauth2.googleapis.com/token"
	youtubeUploadURL = "https://www.googleapis.com/upload/youtube/v3/videos?uploadType=resumable&part=snippet,status"
)

var youtubeAPIClient = &http.Client{Timeout: 30 * time.Second}

// youtubeConfigured reports whether enough YouTube OAuth credentials are
// present to attempt an upload.
func youtubeConfigured(cfg YouTubeConfig) bool {
	return cfg.Enabled && cfg.ClientID != "" && cfg.ClientSecret != "" && cfg.RefreshToken != ""
}

// youtubePrivacyOrDefault normalizes a source's configured privacy status,
// defaulting to "unlisted" for anything unset or unrecognized.
func youtubePrivacyOrDefault(p string) string {
	switch p {
	case "private", "public":
		return p
	default:
		return "unlisted"
	}
}

// youtubeAccessToken exchanges the configured long-lived refresh token for a
// short-lived access token via Google's standard OAuth2 refresh flow.
func youtubeAccessToken(cfg YouTubeConfig) (string, error) {
	form := url.Values{
		"client_id":     {cfg.ClientID},
		"client_secret": {cfg.ClientSecret},
		"refresh_token": {cfg.RefreshToken},
		"grant_type":    {"refresh_token"},
	}
	resp, err := youtubeAPIClient.PostForm(youtubeTokenURL, form)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
		ErrorDesc   string `json:"error_description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("could not parse token response: %w", err)
	}
	if resp.StatusCode != http.StatusOK || out.AccessToken == "" {
		if out.Error != "" {
			return "", fmt.Errorf("%s: %s", out.Error, out.ErrorDesc)
		}
		return "", fmt.Errorf("token refresh returned HTTP %d", resp.StatusCode)
	}
	return out.AccessToken, nil
}

// uploadYouTube uploads a finished recording as a private/unlisted/public
// YouTube video (per the source's configured privacy) if the source has
// auto-upload enabled and YouTube credentials are configured. Always call
// via `go a.uploadYouTube(rec)` from a recording-lifecycle path.
func (a *App) uploadYouTube(rec *recording) {
	cfg := a.snapshotConfig()
	if !rec.source.YouTubeUpload || !youtubeConfigured(cfg.Settings.YouTube) {
		return
	}
	if rec.source.AudioOnly {
		a.event("warn", fmt.Sprintf("[%s] skipped YouTube upload: audio-only recordings aren't supported yet", rec.source.Name))
		return
	}

	token, err := youtubeAccessToken(cfg.Settings.YouTube)
	if err != nil {
		a.event("error", fmt.Sprintf("[%s] YouTube upload failed: could not refresh access token: %s", rec.source.Name, err))
		return
	}

	title := rec.source.Name
	description := fmt.Sprintf("Recorded by MutiRec from %s\nSource: %s", rec.source.Type, rec.source.URL)
	videoURL, err := youtubeUploadFile(token, rec.finalPath, title, description, youtubePrivacyOrDefault(rec.source.YouTubePrivacy))
	if err != nil {
		a.event("error", fmt.Sprintf("[%s] YouTube upload failed: %s", rec.source.Name, err))
		return
	}
	a.event("info", fmt.Sprintf("[%s] uploaded to YouTube (%s): %s", rec.source.Name, rec.source.YouTubePrivacy, videoURL))
}

// youtubeUploadFile performs the two-step resumable upload: initiate a
// session (returns a per-upload URL in the Location header), then PUT the
// whole file to it in one shot. Generic over any file path so it covers
// both the auto-upload-after-recording path (uploadYouTube above) and a
// one-off manual upload of an already-finished recording (see
// handleRecordingYouTubeUpload in youtubemanual.go).
func youtubeUploadFile(accessToken, path, title, description, privacy string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	if len(title) > 100 {
		title = title[:100]
	}
	meta := map[string]any{
		"snippet": map[string]any{"title": title, "description": description},
		"status":  map[string]any{"privacyStatus": privacy, "selfDeclaredMadeForKids": false},
	}
	body, err := json.Marshal(meta)
	if err != nil {
		return "", err
	}

	contentType := mime.TypeByExtension(filepath.Ext(path))
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	initReq, err := http.NewRequest(http.MethodPost, youtubeUploadURL, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	initReq.Header.Set("Authorization", "Bearer "+accessToken)
	initReq.Header.Set("Content-Type", "application/json; charset=UTF-8")
	initReq.Header.Set("X-Upload-Content-Type", contentType)
	initReq.Header.Set("X-Upload-Content-Length", strconv.FormatInt(info.Size(), 10))

	initResp, err := youtubeAPIClient.Do(initReq)
	if err != nil {
		return "", fmt.Errorf("could not start the upload session: %w", err)
	}
	initBody, _ := io.ReadAll(initResp.Body)
	initResp.Body.Close()
	if initResp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("upload session request returned HTTP %d: %s", initResp.StatusCode, strings.TrimSpace(string(initBody)))
	}
	sessionURL := initResp.Header.Get("Location")
	if sessionURL == "" {
		return "", fmt.Errorf("upload session response had no Location header")
	}

	// No overall timeout here - unlike the metadata calls above, a large
	// recording's upload can legitimately take a long time.
	uploadClient := &http.Client{Transport: &http.Transport{ResponseHeaderTimeout: responseHeaderTimeout}}
	putReq, err := http.NewRequest(http.MethodPut, sessionURL, f)
	if err != nil {
		return "", err
	}
	putReq.Header.Set("Content-Type", contentType)
	putReq.ContentLength = info.Size()

	putResp, err := uploadClient.Do(putReq)
	if err != nil {
		return "", fmt.Errorf("upload failed: %w", err)
	}
	defer putResp.Body.Close()
	putBody, _ := io.ReadAll(putResp.Body)
	if putResp.StatusCode != http.StatusOK && putResp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("upload returned HTTP %d: %s", putResp.StatusCode, strings.TrimSpace(string(putBody)))
	}
	var result struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(putBody, &result); err != nil || result.ID == "" {
		return "", fmt.Errorf("could not parse upload response: %s", strings.TrimSpace(string(putBody)))
	}
	return "https://www.youtube.com/watch?v=" + result.ID, nil
}
