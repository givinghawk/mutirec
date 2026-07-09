package main

import (
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

// TestCombineDateTimeRollover covers the festival-day after-midnight
// convention: a set listed under "Thursday" at an early-morning wall-clock
// time is really the following calendar day (Friday). This is the bug where
// an after-party at 01:00 was landing 23 hours too early, on the same day as
// the afternoon it opened.
func TestCombineDateTimeRollover(t *testing.T) {
	loc := time.UTC
	// Thursday, 25 June 2026 is the festival "day" label.
	thu := time.Date(2026, 6, 25, 0, 0, 0, 0, loc)

	cases := []struct {
		hm        string
		wantDay   int // day-of-month expected
		wantHour  int
		wantOK    bool
		wantMonth time.Month
	}{
		{"14:00", 25, 14, true, time.June}, // afternoon stays on the label day
		{"23:30", 25, 23, true, time.June}, // late evening stays
		{"01:00", 26, 1, true, time.June},  // after-party rolls to next day
		{"03:30", 26, 3, true, time.June},  // small hours roll
		{"07:59", 26, 7, true, time.June},  // just before the rollover boundary
		{"08:00", 25, 8, true, time.June},  // at/after the boundary stays on the day
		{"25:00", 26, 1, true, time.June},  // hour>=24 convention normalizes to next day 01:00
		{"", 0, 0, false, 0},               // empty is not a time
		{"notatime", 0, 0, false, 0},       // garbage is rejected
	}
	for _, c := range cases {
		got, ok := combineDateTime(thu, c.hm, loc)
		if ok != c.wantOK {
			t.Errorf("combineDateTime(%q) ok = %v, want %v", c.hm, ok, c.wantOK)
			continue
		}
		if !ok {
			continue
		}
		if got.Day() != c.wantDay || got.Hour() != c.wantHour || got.Month() != c.wantMonth {
			t.Errorf("combineDateTime(%q) = %s, want day=%d hour=%d month=%s", c.hm, got.Format(time.RFC3339), c.wantDay, c.wantHour, c.wantMonth)
		}
	}
}

// TestCombineDateTimeAbsoluteTimestamp confirms a value that's already a full
// RFC3339 timestamp is taken as-is, with no festival-day rollover applied.
func TestCombineDateTimeAbsoluteTimestamp(t *testing.T) {
	thu := time.Date(2026, 6, 25, 0, 0, 0, 0, time.UTC)
	got, ok := combineDateTime(thu, "2026-06-25T01:00:00Z", time.UTC)
	if !ok {
		t.Fatal("expected an absolute RFC3339 timestamp to parse")
	}
	if got.Day() != 25 || got.Hour() != 1 {
		t.Errorf("absolute timestamp was altered: got %s, want 2026-06-25T01:00", got.Format(time.RFC3339))
	}
}

// TestConvertTimetableLolAfterMidnight exercises the end-to-end import path
// with a day whose program runs past midnight, confirming the late-night set
// ends up on the following calendar day and sorts after the evening sets.
func TestConvertTimetableLolAfterMidnight(t *testing.T) {
	payload := timetableLolPlannerData{
		TimeZone: "UTC",
		Data: map[string]timetableLolDay{
			"thu": {
				Date: "2026-06-25",
				Stages: map[string][]timetableLolSet{
					"BLUE": {
						{ID: "e1", Artist: "Headliner", Start: "22:00", End: "23:30"},
						{ID: "e2", Artist: "The Afterparty", Start: "01:00", End: "03:00"},
					},
				},
			},
		},
	}
	out, _, err := convertTimetableLolData(payload)
	if err != nil {
		t.Fatalf("convertTimetableLolData: %v", err)
	}
	if len(out) != 1 || out[0].Stage != "BLUE" || len(out[0].Sets) != 2 {
		t.Fatalf("unexpected shape: %+v", out)
	}
	// Sorted by start; the afterparty must come *after* the headliner, on the 26th.
	head := out[0].Sets[0]
	after := out[0].Sets[1]
	if head.Name != "Headliner" {
		t.Fatalf("expected the headliner first after sort, got %q then %q", head.Name, after.Name)
	}
	start, _ := time.Parse(time.RFC3339, after.Start)
	if start.Day() != 26 || start.Hour() != 1 {
		t.Errorf("afterparty start = %s, want 2026-06-26T01:00 (rolled to next day)", after.Start)
	}
	end, _ := time.Parse(time.RFC3339, after.End)
	if end.Day() != 26 || end.Hour() != 3 {
		t.Errorf("afterparty end = %s, want 2026-06-26T03:00", after.End)
	}
}

// TestParseAnyTimetableJSON confirms both accepted shapes route to a usable
// []StageSchedule: the app's own RFC3339 export format and the compact
// numeric-tuple community format.
func TestParseAnyTimetableJSON(t *testing.T) {
	rfc := []byte(`[{"stage":"RED","url":"https://x/live","sets":[{"start":"2026-06-26T13:00:00+02:00","end":"2026-06-26T14:00:00+02:00","name":"Opening"}]}]`)
	out, err := parseAnyTimetableJSON(rfc)
	if err != nil || len(out) != 1 || len(out[0].Sets) != 1 || out[0].Sets[0].Name != "Opening" {
		t.Fatalf("RFC3339 shape did not round-trip: %+v (err %v)", out, err)
	}
	if out[0].URL != "https://x/live" {
		t.Errorf("expected the stage URL to be preserved, got %q", out[0].URL)
	}

	compact := []byte(`[{"stage":"BLUE","sets":[[2026,6,26,23,30,"Encore"],[2026,6,27,1,0]]}]`)
	out, err = parseAnyTimetableJSON(compact)
	if err != nil || len(out) != 1 || len(out[0].Sets) != 1 {
		t.Fatalf("compact shape did not parse: %+v (err %v)", out, err)
	}
	// The trailing name-less row supplies the end time (01:00 next day).
	end, _ := time.Parse(time.RFC3339, out[0].Sets[0].End)
	if end.Day() != 27 || end.Hour() != 1 {
		t.Errorf("compact end time = %s, want 2026-06-27T01:00", out[0].Sets[0].End)
	}

	// Planner JSON (timetable.lol / local planner file format) - the third
	// shape parseAnyTimetableJSON tries.
	planner := []byte(`{
		"planType":"timetable",
		"timeZone":"UTC",
		"data":{
			"Friday":{
				"date":"2026-06-26",
				"stages":{
					"RED":[
						["x1","14:00","15:00","Opener"],
						["x2","23:00","01:00","Afterparty"]
					]
				}
			}
		},
		"festivalRange":{"Friday":{"date":"2026-06-26"}}
	}`)
	out, err = parseAnyTimetableJSON(planner)
	if err != nil || len(out) != 1 || out[0].Stage != "RED" || len(out[0].Sets) != 2 {
		t.Fatalf("planner shape did not parse: %+v (err %v)", out, err)
	}
	// Afterparty starts 23:00 Friday, ends 01:00 Saturday (rolled over).
	afterSet := out[0].Sets[1]
	afterEnd, _ := time.Parse(time.RFC3339, afterSet.End)
	if afterEnd.Day() != 27 || afterEnd.Hour() != 1 {
		t.Errorf("planner afterparty end = %s, want 2026-06-27T01:00", afterSet.End)
	}

	if _, err := parseAnyTimetableJSON([]byte(`{"not":"an array"}`)); err == nil {
		t.Error("expected an error for input that is neither timetable shape")
	}
}

// TestSnapshotTimetableCapsHistory confirms snapshotTimetable both records a
// new entry (with computed stage/set counts) and drops the oldest one once
// the cap is exceeded, so config.json doesn't grow forever across repeated
// imports.
func TestSnapshotTimetableCapsHistory(t *testing.T) {
	cfg := &AppConfig{}
	schedule := []StageSchedule{{Stage: "RED", Sets: []ScheduleSet{{Name: "A"}, {Name: "B"}}}}
	for i := 0; i < maxSavedTimetables+5; i++ {
		snapshotTimetable(cfg, "import", "file upload", schedule)
	}
	if len(cfg.SavedTimetables) != maxSavedTimetables {
		t.Fatalf("expected the saved-timetable list capped at %d, got %d", maxSavedTimetables, len(cfg.SavedTimetables))
	}
	last := cfg.SavedTimetables[len(cfg.SavedTimetables)-1]
	if last.Stages != 1 || last.Sets != 2 {
		t.Errorf("expected computed stages=1 sets=2, got stages=%d sets=%d", last.Stages, last.Sets)
	}
	if last.Source != "file upload" {
		t.Errorf("expected source %q, got %q", "file upload", last.Source)
	}
}

// TestHandleTimetableSavedItemActivateAndDelete covers switching the live
// timetable to a saved snapshot (preserving any per-stage URL already
// configured, same as every other import path) and deleting a snapshot.
func TestHandleTimetableSavedItemActivateAndDelete(t *testing.T) {
	dir := t.TempDir()
	a := &App{config: filepath.Join(dir, "config.json")}
	a.cfg = AppConfig{
		Timetable: []StageSchedule{{Stage: "RED", URL: "https://live/red"}},
		SavedTimetables: []SavedTimetable{
			{ID: "snap1", Name: "Old Import", Schedule: []StageSchedule{{Stage: "RED", Sets: []ScheduleSet{{Name: "Headliner"}}}}},
		},
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/timetable/saved/snap1/activate", nil)
	a.handleTimetableSavedItem(rec, req)
	if rec.Code != 200 {
		t.Fatalf("activate: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(a.cfg.Timetable) != 1 || a.cfg.Timetable[0].Stage != "RED" || len(a.cfg.Timetable[0].Sets) != 1 {
		t.Fatalf("activate did not switch the live timetable: %+v", a.cfg.Timetable)
	}
	if a.cfg.Timetable[0].URL != "https://live/red" {
		t.Errorf("expected the existing stage URL to be preserved, got %q", a.cfg.Timetable[0].URL)
	}

	delRec := httptest.NewRecorder()
	delReq := httptest.NewRequest("DELETE", "/api/timetable/saved/snap1", nil)
	a.handleTimetableSavedItem(delRec, delReq)
	if delRec.Code != 200 {
		t.Fatalf("delete: expected 200, got %d: %s", delRec.Code, delRec.Body.String())
	}
	if len(a.cfg.SavedTimetables) != 0 {
		t.Fatalf("expected the saved snapshot to be removed, got %+v", a.cfg.SavedTimetables)
	}
}

// TestFirstStringField confirms it tries keys in order and skips
// non-string/empty values, since timetable.lol's event list is passed
// through as an untyped map[string]any and we don't control its schema.
func TestFirstStringField(t *testing.T) {
	m := map[string]any{
		"date":      "",
		"startDate": "2026-06-25",
		"eventDate": 12345, // wrong type, must be skipped
	}
	if got := firstStringField(m, "startDate", "date", "eventDate"); got != "2026-06-25" {
		t.Errorf("got %q, want %q", got, "2026-06-25")
	}
	if got := firstStringField(m, "date", "eventDate"); got != "" {
		t.Errorf("expected empty when only empty/wrong-typed candidates remain, got %q", got)
	}
	if got := firstStringField(m, "missing"); got != "" {
		t.Errorf("expected empty for a missing key, got %q", got)
	}
}

// TestParseStageTimetableJSONHourOverflow confirms the compact-format parser
// normalizes an out-of-range hour (the "25:00" after-midnight convention)
// into a valid next-day instant rather than emitting an unparseable string.
func TestParseStageTimetableJSONHourOverflow(t *testing.T) {
	data := []byte(`[{"stage":"RED","sets":[[2026,6,26,25,0,"Late One"]]}]`)
	out, err := parseStageTimetableJSON(data)
	if err != nil {
		t.Fatalf("parseStageTimetableJSON: %v", err)
	}
	if len(out) != 1 || len(out[0].Sets) != 1 {
		t.Fatalf("unexpected shape: %+v", out)
	}
	start, err := time.Parse(time.RFC3339, out[0].Sets[0].Start)
	if err != nil {
		t.Fatalf("start was not valid RFC3339 (%q): %v", out[0].Sets[0].Start, err)
	}
	if start.Day() != 27 || start.Hour() != 1 {
		t.Errorf("start = %s, want 2026-06-27T01:00 (25:00 normalized to next day)", out[0].Sets[0].Start)
	}
}
