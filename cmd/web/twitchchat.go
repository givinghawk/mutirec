package main

import (
	"bufio"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"math/rand"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// ============================================================================
// Twitch chat archiving: for a source with ArchiveTwitchChat enabled, this
// connects anonymously (no OAuth needed - Twitch allows read-only "justinfan"
// logins for anyone) to Twitch's IRC-based chat server for the duration of
// the recording, and appends every message to a ".chat.jsonl" sidecar next
// to the recording (same "swap the extension" convention as .nfo/.timecode
// etc - see isSidecarPath), timestamped as seconds since the recording
// started so playback can line messages up against the video's own
// currentTime regardless of when the file is later watched.
// ============================================================================

// twitchChannelURLRe matches a twitch.tv/<channel> URL and captures the
// channel login name.
var twitchChannelURLRe = regexp.MustCompile(`(?i)twitch\.tv/([A-Za-z0-9_]+)`)

// twitchChannelFromURL extracts the channel login name from a source's
// Twitch URL, e.g. "https://twitch.tv/somechannel" -> "somechannel".
func twitchChannelFromURL(rawURL string) (string, bool) {
	m := twitchChannelURLRe.FindStringSubmatch(rawURL)
	if m == nil {
		return "", false
	}
	return strings.ToLower(m[1]), true
}

// TwitchChatMessage is one archived chat line.
type TwitchChatMessage struct {
	OffsetSeconds float64 `json:"t"`
	User          string  `json:"user"`
	Color         string  `json:"color,omitempty"`
	Text          string  `json:"text"`
}

// twitchChatSidecarPath derives the ".chat.jsonl" path for a recording, same
// "swap the extension" sidecar convention as .nfo/.timecode.json/etc.
func twitchChatSidecarPath(videoPath string) string {
	return strings.TrimSuffix(videoPath, filepath.Ext(videoPath)) + ".chat.jsonl"
}

// twitchIRCReconnectDelay bounds how quickly captureTwitchChat retries after
// a dropped chat connection - short enough that a brief network blip barely
// loses any messages, long enough not to hammer Twitch if it's rejecting
// connections outright (bad/no credentials aren't possible here since this
// is always an anonymous login, but rate limits still apply).
const twitchIRCReconnectDelay = 5 * time.Second

// captureTwitchChat runs for the lifetime of one recording (until rec.ctx is
// cancelled), (re)connecting to Twitch chat and appending every message to
// the sidecar JSONL file next to rec.tempPath. Best-effort: a failure to
// connect/reconnect is logged and retried, never fails the recording itself.
func (a *App) captureTwitchChat(rec *recording, channel string) {
	path := twitchChatSidecarPath(rec.tempPath)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		a.event("warn", fmt.Sprintf("[%s] could not open Twitch chat archive file: %s", rec.source.Name, err))
		return
	}
	defer f.Close()
	enc := json.NewEncoder(f)

	for rec.ctx.Err() == nil {
		if err := connectAndReadTwitchChat(rec, channel, enc); err != nil {
			a.event("warn", fmt.Sprintf("[%s] Twitch chat archive disconnected: %s - reconnecting", rec.source.Name, err))
		}
		select {
		case <-rec.ctx.Done():
			return
		case <-time.After(twitchIRCReconnectDelay):
		}
	}
}

// twitchPrivmsgRe matches one tagged PRIVMSG IRC line from Twitch chat, e.g.:
// @badge-info=;color=#FF0000;display-name=SomeUser;... :someuser!someuser@someuser.tmi.twitch.tv PRIVMSG #channel :hello world
var twitchPrivmsgRe = regexp.MustCompile(`^@(\S+) :([^!]+)!\S+ PRIVMSG #\S+ :(.*)$`)

// parseTwitchChatLine extracts the user, color, and message text from one
// raw IRC line, if it's a PRIVMSG (a chat message) - anything else (PING,
// JOIN/PART, NOTICE, ...) reports ok=false and is simply skipped by the
// caller.
func parseTwitchChatLine(line string) (user, color, text string, ok bool) {
	m := twitchPrivmsgRe.FindStringSubmatch(line)
	if m == nil {
		return "", "", "", false
	}
	tags, fallbackUser, msgText := m[1], m[2], m[3]
	tagColor, displayName := parseTwitchTags(tags)
	if displayName != "" {
		fallbackUser = displayName
	}
	return fallbackUser, tagColor, msgText, true
}

// parseTwitchTags pulls "color" and "display-name" out of an IRCv3 tags
// string (semicolon-separated key=value pairs) - the only two fields the
// chat archive cares about.
func parseTwitchTags(tags string) (color, displayName string) {
	for _, kv := range strings.Split(tags, ";") {
		k, v, found := strings.Cut(kv, "=")
		if !found {
			continue
		}
		switch k {
		case "color":
			color = v
		case "display-name":
			displayName = v
		}
	}
	return color, displayName
}

// connectAndReadTwitchChat opens one anonymous connection to Twitch chat,
// joins channel, and streams messages into enc until the connection drops
// or rec.ctx is cancelled. An anonymous "justinfan" login needs no real
// credentials - Twitch doesn't validate the password for these read-only
// logins, only that the nick matches the justinfan<digits> pattern.
func connectAndReadTwitchChat(rec *recording, channel string, enc *json.Encoder) error {
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 15 * time.Second}, "tcp", "irc.chat.twitch.tv:6697", &tls.Config{})
	if err != nil {
		return err
	}
	defer conn.Close()
	go func() {
		<-rec.ctx.Done()
		conn.Close()
	}()

	nick := fmt.Sprintf("justinfan%d", 10000+rand.Intn(90000))
	fmt.Fprintf(conn, "PASS anonymous\r\n")
	fmt.Fprintf(conn, "NICK %s\r\n", nick)
	fmt.Fprintf(conn, "CAP REQ :twitch.tv/tags\r\n")
	fmt.Fprintf(conn, "JOIN #%s\r\n", channel)

	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "PING") {
			fmt.Fprintf(conn, "PONG :tmi.twitch.tv\r\n")
			continue
		}
		user, color, text, ok := parseTwitchChatLine(line)
		if !ok {
			continue
		}
		_ = enc.Encode(TwitchChatMessage{
			OffsetSeconds: time.Since(rec.startedAt).Seconds(),
			User:          user,
			Color:         color,
			Text:          text,
		})
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return nil
}

// handleRecordingChat serves a recording's archived Twitch chat (if any) as
// a JSON array, for the player to replay alongside the video.
func (a *App) handleRecordingChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	relPath := filepath.ToSlash(strings.TrimSpace(r.URL.Query().Get("path")))
	if relPath == "" || strings.Contains(relPath, "..") {
		http.Error(w, "path is required", http.StatusBadRequest)
		return
	}
	abs, ok := a.resolveRecordingPath(relPath)
	if !ok {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}
	data, err := os.ReadFile(twitchChatSidecarPath(abs))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	messages := parseTwitchChatArchive(data)
	writeJSON(w, messages)
}

// parseTwitchChatArchive decodes a ".chat.jsonl" sidecar's contents (one
// JSON object per line) into a slice, skipping any blank or malformed line
// rather than failing the whole read.
func parseTwitchChatArchive(data []byte) []TwitchChatMessage {
	var messages []TwitchChatMessage
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var m TwitchChatMessage
		if json.Unmarshal([]byte(line), &m) == nil {
			messages = append(messages, m)
		}
	}
	return messages
}
