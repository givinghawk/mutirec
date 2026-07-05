package main

import (
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
		{"DJ_Isaac_BLUE_Thursday_25_06_2026_Defqon_1_Sacred_Oath_HardDance.mp3", 2026, 6, 25, true, false},
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
		{"DJ_Isaac_BLUE_Thursday_25_06_2026_Defqon_1_Sacred_Oath_HardDance.mp3", "BLUE", "DJ Isaac"},
		{"Adaro_B2B_Da_Tweekaz_RED_Friday_26_06_2026.mkv", "RED", "Adaro B2B Da Tweekaz"},
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
		{"DJ Isaac", "DJ Isaac", 0.99},
		{"DJ Isaac", "DJ Isaac B2B Adaro", 0.99}, // exact subset shouldn't be diluted
		{"Adaro B2B Da Tweekaz", "Adaro B2B Da Tweekaz", 0.99},
		{"DJ Isaac", "Wildstylez", 0},
	}
	for _, c := range cases {
		got := artistSimilarity(c.guessed, c.setName)
		if got < c.wantMin {
			t.Errorf("artistSimilarity(%q, %q) = %v, want >= %v", c.guessed, c.setName, got, c.wantMin)
		}
	}
}

// TestBestMatchSuggestion_UserExample exercises the full matching pipeline
// against the exact filename/scenario reported: "BLUE" is the real stage
// name (matching the recording's folder/channel), "Defqon_1_Sacred_Oath" is
// the festival name plus that year's edition/theme name (not a stage, and
// not parsed further), and the date is given day-first with no
// time-of-day - the channel match, guessed date, and guessed artist name
// together should be enough to confidently identify the set.
func TestBestMatchSuggestion_UserExample(t *testing.T) {
	cfg := AppConfig{
		LibraryEvents: []LibraryEvent{
			{
				ID:   "ev1",
				Name: "Defqon.1 - Sacred Oath (2026)",
				Timetable: []StageSchedule{
					{
						Stage: "BLUE",
						Sets: []ScheduleSet{
							{ID: "s1", Name: "DJ Isaac", Start: "2026-06-25T14:00:00Z", End: "2026-06-25T15:00:00Z"},
							{ID: "s2", Name: "Wildstylez", Start: "2026-06-25T15:00:00Z", End: "2026-06-25T16:00:00Z"},
						},
					},
				},
			},
		},
	}

	name := "DJ_Isaac_BLUE_Thursday_25_06_2026_Defqon_1_Sacred_Oath_HardDance.mp3"
	guessed, fromName, hasTOD := guessTimeFromName(name, time.Now())
	if !fromName {
		t.Fatal("expected a date to be parsed from the filename")
	}
	got := bestMatchSuggestion(cfg, "BLUE/"+name, name, "BLUE", guessed, fromName, hasTOD)
	if got.SetID != "s1" {
		t.Fatalf("expected match on set s1 (DJ Isaac), got SetID=%q Artist=%q Confidence=%q Reason=%q", got.SetID, got.Artist, got.Confidence, got.Reason)
	}
	if got.Confidence != "high" {
		t.Errorf("expected high confidence, got %q (reason: %s)", got.Confidence, got.Reason)
	}
	if got.GuessedArtist != "DJ Isaac" {
		t.Errorf("expected GuessedArtist = %q, got %q", "DJ Isaac", got.GuessedArtist)
	}
}
