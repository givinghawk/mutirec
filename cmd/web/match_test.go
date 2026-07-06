package main

import (
	"strings"
	"testing"
	"time"
)

func TestGuessTimeFromName(t *testing.T) {
	mtime := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)

	cases := []struct {
		name                string
		wantY, wantM, wantD int
		wantFromName        bool
		wantTimeOfDay       bool
	}{
		{"DJ_Vertex_BLUE_Thursday_25_06_2026_Neonbeat_Prime_Directive_HardDance.mp3", 2026, 6, 25, true, false},
		{"recording.20260625-143000.mkv", 2026, 6, 25, true, true},
		{"Some_Set_2026-06-25.mp4", 2026, 6, 25, true, false},
		{"no_date_in_this_one.mp3", 2026, 8, 1, false, false}, // falls back to mtime
	}

	for _, c := range cases {
		got, fromName, hasTOD := guessTimeFromName(c.name, mtime)
		if fromName != c.wantFromName {
			t.Errorf("%s: fromName = %v, want %v", c.name, fromName, c.wantFromName)
			continue
		}
		if hasTOD != c.wantTimeOfDay {
			t.Errorf("%s: hasTimeOfDay = %v, want %v", c.name, hasTOD, c.wantTimeOfDay)
		}
		if got.Year() != c.wantY || int(got.Month()) != c.wantM || got.Day() != c.wantD {
			t.Errorf("%s: got %04d-%02d-%02d, want %04d-%02d-%02d", c.name, got.Year(), got.Month(), got.Day(), c.wantY, c.wantM, c.wantD)
		}
	}
}

func TestGuessTimeFromName_WeekdayDisambiguation(t *testing.T) {
	// "03_04_2026" is ambiguous (both <=12): 2026-04-03 is a Friday,
	// 2026-03-04 is a Wednesday. The filename says Friday, so day=3, month=4
	// should win over the naive day-first default.
	mtime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	got, fromName, _ := guessTimeFromName("Some_Artist_Friday_03_04_2026_Stage.mp3", mtime)
	if !fromName {
		t.Fatal("expected fromName = true")
	}
	if got.Month() != time.April || got.Day() != 3 {
		t.Errorf("got %04d-%02d-%02d, want 2026-04-03 (weekday-disambiguated)", got.Year(), got.Month(), got.Day())
	}
}

func TestGuessArtistFromName(t *testing.T) {
	cases := []struct {
		name, channel, want string
	}{
		{"DJ_Vertex_BLUE_Thursday_25_06_2026_Neonbeat_Prime_Directive_HardDance.mp3", "BLUE", "DJ Vertex"},
		{"Fenrix_B2B_Glitch_Bros_RED_Friday_26_06_2026.mkv", "RED", "Fenrix B2B Glitch Bros"},
		{"recording.20260625-143000.mkv", "BLUE", "recording"},
	}
	for _, c := range cases {
		got := guessArtistFromName(c.name, c.channel)
		if got != c.want {
			t.Errorf("guessArtistFromName(%q, %q) = %q, want %q", c.name, c.channel, got, c.want)
		}
	}
}

func TestArtistSimilarity(t *testing.T) {
	cases := []struct {
		guessed, setName string
		wantMin          float64
	}{
		{"DJ Vertex", "DJ Vertex", 0.99},
		{"DJ Vertex", "DJ Vertex B2B Fenrix", 0.99}, // exact subset shouldn't be diluted
		{"Fenrix B2B Glitch Bros", "Fenrix B2B Glitch Bros", 0.99},
		{"DJ Vertex", "Nightcaster", 0},
	}
	for _, c := range cases {
		got := artistSimilarity(c.guessed, c.setName)
		if got < c.wantMin {
			t.Errorf("artistSimilarity(%q, %q) = %v, want >= %v", c.guessed, c.setName, got, c.wantMin)
		}
	}
}

// TestBestMatchSuggestion_UserExample exercises the full matching pipeline
// against a filename shaped like a real reported convention (genericized
// here): "BLUE" is the real stage name (matching the recording's
// folder/channel), "Neonbeat_Prime_Directive" is the festival name plus that
// year's edition/theme name (not a stage, and not parsed further), and the
// date is given day-first with no time-of-day - the channel match, guessed
// date, and guessed artist name together should be enough to confidently
// identify the set.
func TestBestMatchSuggestion_UserExample(t *testing.T) {
	cfg := AppConfig{
		LibraryEvents: []LibraryEvent{
			{
				ID:   "ev1",
				Name: "Neonbeat - Prime Directive (2026)",
				Timetable: []StageSchedule{
					{
						Stage: "BLUE",
						Sets: []ScheduleSet{
							{ID: "s1", Name: "DJ Vertex", Start: "2026-06-25T14:00:00Z", End: "2026-06-25T15:00:00Z"},
							{ID: "s2", Name: "Nightcaster", Start: "2026-06-25T15:00:00Z", End: "2026-06-25T16:00:00Z"},
						},
					},
				},
			},
		},
	}

	name := "DJ_Vertex_BLUE_Thursday_25_06_2026_Neonbeat_Prime_Directive_HardDance.mp3"
	guessed, fromName, hasTOD := guessTimeFromName(name, time.Now())
	if !fromName {
		t.Fatal("expected a date to be parsed from the filename")
	}
	got := bestMatchSuggestion(cfg, "BLUE/"+name, name, "BLUE", guessed, fromName, hasTOD)
	if got.SetID != "s1" {
		t.Fatalf("expected match on set s1 (DJ Vertex), got SetID=%q Artist=%q Confidence=%q Reason=%q", got.SetID, got.Artist, got.Confidence, got.Reason)
	}
	if got.Confidence != "high" {
		t.Errorf("expected high confidence, got %q (reason: %s)", got.Confidence, got.Reason)
	}
	if got.GuessedArtist != "DJ Vertex" {
		t.Errorf("expected GuessedArtist = %q, got %q", "DJ Vertex", got.GuessedArtist)
	}
}

// TestBestMatchSuggestion_FestivalScoping covers two festival editions that
// happen to reuse the same stage name ("RED") and, in this case, the exact
// same artist name on the same calendar day - the kind of collision that can
// happen across a touring artist's appearances at similarly-run festivals.
// A source explicitly linked to one Festival (via Source.FestivalID) should
// prefer the matching edition's set over the other, and flag it if the best
// available candidate belongs to the *wrong* linked Festival.
func TestBestMatchSuggestion_FestivalScoping(t *testing.T) {
	cfg := AppConfig{
		Sources: []Source{
			{Name: "RED", FestivalID: "fest-a"},
		},
		LibraryEvents: []LibraryEvent{
			{
				ID: "ev-a", Name: "Festival A 2026", FestivalID: "fest-a",
				Timetable: []StageSchedule{{Stage: "RED", Sets: []ScheduleSet{
					{ID: "a1", Name: "Nightcaster", Start: "2026-06-25T14:00:00Z", End: "2026-06-25T15:00:00Z"},
				}}},
			},
			{
				ID: "ev-b", Name: "Festival B 2026", FestivalID: "fest-b",
				Timetable: []StageSchedule{{Stage: "RED", Sets: []ScheduleSet{
					{ID: "b1", Name: "Nightcaster", Start: "2026-06-25T14:00:00Z", End: "2026-06-25T15:00:00Z"},
				}}},
			},
		},
	}

	name := "Nightcaster_RED_Thursday_25_06_2026.mp3"
	guessed, fromName, hasTOD := guessTimeFromName(name, time.Now())
	got := bestMatchSuggestion(cfg, "RED/"+name, name, "RED", guessed, fromName, hasTOD)
	if got.EventID != "ev-a" {
		t.Fatalf("expected the source's linked Festival (ev-a) to win an otherwise-tied match, got EventID=%q Reason=%q", got.EventID, got.Reason)
	}

	// Now the source is linked to fest-b instead - the *other* candidate
	// should win, since it's the only one honoring the source's own link.
	cfg.Sources[0].FestivalID = "fest-b"
	got = bestMatchSuggestion(cfg, "RED/"+name, name, "RED", guessed, fromName, hasTOD)
	if got.EventID != "ev-b" {
		t.Fatalf("expected the source's linked Festival (ev-b) to win, got EventID=%q Reason=%q", got.EventID, got.Reason)
	}

	// A source with no Festival link at all shouldn't be penalized either
	// way - it just falls back to whichever candidate scores highest on the
	// other signals (a tie here, so either is acceptable, but it must not
	// come back empty).
	cfg.Sources[0].FestivalID = ""
	got = bestMatchSuggestion(cfg, "RED/"+name, name, "RED", guessed, fromName, hasTOD)
	if got.EventID == "" {
		t.Fatalf("expected a match even without a Festival link, got none (reason: %s)", got.Reason)
	}
}

// TestFlagSharedSetCandidates covers the scenario introduced by
// auto-reconnect: a dropped stream produces two separate recording files for
// what was originally one continuous set, and both independently match the
// same set. Both should be flagged so the user notices before organizing
// both onto the same set unknowingly.
func TestFlagSharedSetCandidates(t *testing.T) {
	suggestions := []MatchSuggestion{
		{Path: "a", EventID: "ev1", SetID: "s1", Reason: "first"},
		{Path: "b", EventID: "ev1", SetID: "s1", Reason: "second"},
		{Path: "c", EventID: "ev1", SetID: "s2", Reason: "unrelated"},
		{Path: "d", EventID: "", SetID: "", Reason: "no match"},
	}
	flagSharedSetCandidates(suggestions)
	if !strings.Contains(suggestions[0].Reason, "other recording") {
		t.Errorf("expected suggestion 0 to be flagged, got reason: %q", suggestions[0].Reason)
	}
	if !strings.Contains(suggestions[1].Reason, "other recording") {
		t.Errorf("expected suggestion 1 to be flagged, got reason: %q", suggestions[1].Reason)
	}
	if strings.Contains(suggestions[2].Reason, "other recording") {
		t.Errorf("suggestion 2 matches a different set and should not be flagged, got reason: %q", suggestions[2].Reason)
	}
	if strings.Contains(suggestions[3].Reason, "other recording") {
		t.Errorf("suggestion 3 has no set match and should not be flagged, got reason: %q", suggestions[3].Reason)
	}
}
