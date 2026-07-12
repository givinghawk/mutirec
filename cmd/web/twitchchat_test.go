package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTwitchChannelFromURL(t *testing.T) {
	cases := []struct {
		in     string
		want   string
		wantOK bool
	}{
		{"https://www.twitch.tv/somechannel", "somechannel", true},
		{"https://twitch.tv/SomeChannel/videos", "somechannel", true},
		{"http://twitch.tv/another_one", "another_one", true},
		{"https://youtube.com/watch?v=abc", "", false},
	}
	for _, c := range cases {
		got, ok := twitchChannelFromURL(c.in)
		if got != c.want || ok != c.wantOK {
			t.Errorf("twitchChannelFromURL(%q) = (%q, %v), want (%q, %v)", c.in, got, ok, c.want, c.wantOK)
		}
	}
}

func TestTwitchChatSidecarPath(t *testing.T) {
	got := twitchChatSidecarPath("/data/recordings/Foo/Foo.20260101-120000.mkv")
	want := "/data/recordings/Foo/Foo.20260101-120000.chat.jsonl"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestParseTwitchTags(t *testing.T) {
	color, display := parseTwitchTags("badge-info=;color=#FF0000;display-name=SomeUser;emotes=;id=abc")
	if color != "#FF0000" || display != "SomeUser" {
		t.Fatalf("got color=%q display=%q", color, display)
	}
}

func TestParseTwitchChatLine(t *testing.T) {
	line := `@badge-info=;color=#FF0000;display-name=SomeUser;emotes=;id=abc :someuser!someuser@someuser.tmi.twitch.tv PRIVMSG #channel :hello world`
	user, color, text, ok := parseTwitchChatLine(line)
	if !ok {
		t.Fatal("expected ok=true for a PRIVMSG line")
	}
	if user != "SomeUser" || color != "#FF0000" || text != "hello world" {
		t.Fatalf("got user=%q color=%q text=%q", user, color, text)
	}

	// display-name absent - falls back to the raw IRC nick.
	line2 := `@color=;emotes= :otheruser!otheruser@otheruser.tmi.twitch.tv PRIVMSG #channel :gg`
	user2, _, text2, ok2 := parseTwitchChatLine(line2)
	if !ok2 || user2 != "otheruser" || text2 != "gg" {
		t.Fatalf("got user=%q text=%q ok=%v", user2, text2, ok2)
	}

	// Not a PRIVMSG at all.
	if _, _, _, ok3 := parseTwitchChatLine("PING :tmi.twitch.tv"); ok3 {
		t.Fatal("expected ok=false for a PING line")
	}
}

func TestParseTwitchChatArchive(t *testing.T) {
	data := []byte("{\"t\":1.5,\"user\":\"A\",\"text\":\"hi\"}\n\n{\"t\":2,\"user\":\"B\",\"text\":\"yo\"}\nnot json\n")
	messages := parseTwitchChatArchive(data)
	if len(messages) != 2 {
		t.Fatalf("expected 2 valid messages, got %d: %+v", len(messages), messages)
	}
	if messages[0].User != "A" || messages[1].User != "B" {
		t.Fatalf("unexpected messages: %+v", messages)
	}
}

func TestHandleRecordingChatServesSidecar(t *testing.T) {
	dir := t.TempDir()
	a := &App{config: filepath.Join(dir, "config.json"), cfg: AppConfig{Settings: Settings{FinishedDir: dir}}}

	videoPath := filepath.Join(dir, "rec.mkv")
	if err := os.WriteFile(videoPath, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	chatPath := twitchChatSidecarPath(videoPath)
	if err := os.WriteFile(chatPath, []byte(`{"t":1,"user":"A","text":"hi"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest(http.MethodGet, "/api/recordings/chat?path=rec.mkv", nil)
	w := httptest.NewRecorder()
	a.handleRecordingChat(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"user":"A"`) {
		t.Fatalf("expected the archived message in the response, got %s", w.Body.String())
	}
}

func TestWriteNFOPrefersTwitchStreamTitle(t *testing.T) {
	dir := t.TempDir()
	a := &App{config: filepath.Join(dir, "config.json"), cfg: AppConfig{Settings: Settings{FinishedDir: dir, EnableNFO: true}}}

	finalPath := filepath.Join(dir, "rec.mkv")
	rec := &recording{source: Source{Name: "somechannel", Type: "twitch", URL: "https://twitch.tv/somechannel"}, finalPath: finalPath, streamTitle: "A Really Descriptive Stream Title"}
	a.writeNFO(rec)

	data, err := os.ReadFile(strings.TrimSuffix(finalPath, filepath.Ext(finalPath)) + ".nfo")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "Title: A Really Descriptive Stream Title") {
		t.Fatalf("expected the stream title in the NFO, got:\n%s", data)
	}
}

func TestWriteNFOFallsBackToSourceNameWithoutStreamTitle(t *testing.T) {
	dir := t.TempDir()
	a := &App{config: filepath.Join(dir, "config.json"), cfg: AppConfig{Settings: Settings{FinishedDir: dir, EnableNFO: true}}}

	finalPath := filepath.Join(dir, "rec.mkv")
	rec := &recording{source: Source{Name: "somechannel", Type: "twitch", URL: "https://twitch.tv/somechannel"}, finalPath: finalPath}
	a.writeNFO(rec)

	data, err := os.ReadFile(strings.TrimSuffix(finalPath, filepath.Ext(finalPath)) + ".nfo")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "Title: somechannel") {
		t.Fatalf("expected the source name as a fallback title, got:\n%s", data)
	}
}

func TestHandleRecordingChatNotFoundWithoutSidecar(t *testing.T) {
	dir := t.TempDir()
	a := &App{config: filepath.Join(dir, "config.json"), cfg: AppConfig{Settings: Settings{FinishedDir: dir}}}
	if err := os.WriteFile(filepath.Join(dir, "rec.mkv"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest(http.MethodGet, "/api/recordings/chat?path=rec.mkv", nil)
	w := httptest.NewRecorder()
	a.handleRecordingChat(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 when no chat sidecar exists, got %d", w.Code)
	}
}
