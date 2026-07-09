package main

import "testing"

func TestYoutubeConfigured(t *testing.T) {
	cases := []struct {
		name string
		cfg  YouTubeConfig
		want bool
	}{
		{"fully configured", YouTubeConfig{Enabled: true, ClientID: "id", ClientSecret: "secret", RefreshToken: "token"}, true},
		{"disabled", YouTubeConfig{Enabled: false, ClientID: "id", ClientSecret: "secret", RefreshToken: "token"}, false},
		{"missing refresh token", YouTubeConfig{Enabled: true, ClientID: "id", ClientSecret: "secret"}, false},
		{"missing everything", YouTubeConfig{}, false},
	}
	for _, c := range cases {
		if got := youtubeConfigured(c.cfg); got != c.want {
			t.Errorf("%s: youtubeConfigured() = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestYoutubePrivacyOrDefault(t *testing.T) {
	cases := map[string]string{
		"private":  "private",
		"unlisted": "unlisted",
		"public":   "public",
		"":         "unlisted",
		"bogus":    "unlisted",
	}
	for in, want := range cases {
		if got := youtubePrivacyOrDefault(in); got != want {
			t.Errorf("youtubePrivacyOrDefault(%q) = %q, want %q", in, got, want)
		}
	}
}
