package main

import (
	"strings"
	"testing"
)

// TestLoadSourcePresetsWellFormed guards the bundled presets/presets.json:
// every preset must have an id/name and at least one source with a type
// this app actually knows how to record, and every source's URL must at
// least look right for its declared type (catches a copy/paste mistake
// before it ships, e.g. a Twitch URL tagged as "youtube").
func TestLoadSourcePresetsWellFormed(t *testing.T) {
	presets := loadSourcePresets()
	if len(presets) == 0 {
		t.Fatal("expected at least one bundled preset")
	}
	seenID := map[string]bool{}
	for _, p := range presets {
		if p.ID == "" {
			t.Errorf("preset %q has no id", p.Name)
		}
		if seenID[p.ID] {
			t.Errorf("duplicate preset id %q", p.ID)
		}
		seenID[p.ID] = true
		if p.Name == "" {
			t.Errorf("preset %q has no name", p.ID)
		}
		if len(p.Sources) == 0 {
			t.Errorf("preset %q has no sources", p.ID)
		}
		for _, src := range p.Sources {
			if err := validateSource(src); err != nil {
				t.Errorf("preset %q source %q fails validation: %s", p.ID, src.Name, err)
			}
			if src.Type == "twitch" && !strings.Contains(src.URL, "twitch.tv/") {
				t.Errorf("preset %q source %q is typed twitch but URL doesn't look like one: %s", p.ID, src.Name, src.URL)
			}
		}
	}
}
