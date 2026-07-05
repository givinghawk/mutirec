package main

import (
	"testing"
	"time"
)

func newTestApp() *App {
	return &App{retry: map[string]*retryState{}}
}

// TestRecordFailureSilentWithoutWindow covers a source that has never gone
// live: every failed attempt should bump the backoff so the scheduler
// doesn't hammer it, but must never log anything or show up as
// "reconnecting" on the dashboard - that's just ordinary background polling,
// not an error worth surfacing.
func TestRecordFailureSilentWithoutWindow(t *testing.T) {
	a := newTestApp()
	for i := 0; i < 3; i++ {
		a.recordFailure("Some DJ", "src1")
	}
	if len(a.events) != 0 {
		t.Fatalf("expected no events logged for a source that's never gone live, got %v", a.events)
	}
	if _, _, ok := a.reconnectStatus("src1"); ok {
		t.Fatal("expected reconnectStatus to report not-reconnecting for a source that's never gone live")
	}
	if _, blocked := a.retryBlocked("src1"); !blocked {
		t.Fatal("expected the source to still be backed off (blocked) even though it's silent")
	}
}

// TestRecordFailureVisibleWithinWindow covers a source that *was* confirmed
// live and then stopped: retry attempts within reconnectVisibilityWindow of
// that should be logged and reported as "reconnecting".
func TestRecordFailureVisibleWithinWindow(t *testing.T) {
	a := newTestApp()
	a.startReconnectWindow("src1")
	a.recordFailure("Some DJ", "src1")
	if len(a.events) != 1 {
		t.Fatalf("expected exactly one visible retry event, got %v", a.events)
	}
	if a.events[0].Level != "warn" {
		t.Errorf("expected a warn-level event, got %q: %s", a.events[0].Level, a.events[0].Text)
	}
	attempts, _, ok := a.reconnectStatus("src1")
	if !ok {
		t.Fatal("expected reconnectStatus to report reconnecting within the visible window")
	}
	if attempts != 1 {
		t.Errorf("expected attempts = 1, got %d", attempts)
	}
}

// TestRecordFailureGivesUpAfterWindow covers the requested 10-minute cutoff:
// once the visible window has lapsed, exactly one "giving up" notice is
// logged and every attempt after that goes back to being silent, same as a
// source that's never gone live.
func TestRecordFailureGivesUpAfterWindow(t *testing.T) {
	a := newTestApp()
	a.retry["src1"] = &retryState{windowUntil: time.Now().Add(-time.Second)} // already lapsed
	a.recordFailure("Some DJ", "src1")
	if len(a.events) != 1 {
		t.Fatalf("expected exactly one 'giving up' event, got %v", a.events)
	}
	if _, _, ok := a.reconnectStatus("src1"); ok {
		t.Fatal("expected reconnectStatus to report not-reconnecting once the window has lapsed")
	}

	// A second failure after giving up must stay silent - no repeated
	// "giving up" spam.
	a.recordFailure("Some DJ", "src1")
	if len(a.events) != 1 {
		t.Fatalf("expected no additional events after giving up once, got %v", a.events)
	}
}

// TestClearRetryResetsWindow covers a manual stop/start: it must fully clear
// both the backoff and any visible window, so the dashboard immediately
// stops showing "reconnecting".
func TestClearRetryResetsWindow(t *testing.T) {
	a := newTestApp()
	a.startReconnectWindow("src1")
	a.recordFailure("Some DJ", "src1")
	a.clearRetry("src1")
	if _, _, ok := a.reconnectStatus("src1"); ok {
		t.Fatal("expected reconnectStatus to report not-reconnecting after clearRetry")
	}
	if _, blocked := a.retryBlocked("src1"); blocked {
		t.Fatal("expected retryBlocked to report unblocked after clearRetry")
	}
}
