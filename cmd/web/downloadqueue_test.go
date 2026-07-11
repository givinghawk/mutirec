package main

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestEnqueueDownloadRunsWithinConcurrencyLimit(t *testing.T) {
	a := &App{cfg: AppConfig{Settings: Settings{Downloads: DownloadsConfig{MaxConcurrent: 2}}}}

	var running int32
	var maxRunning int32
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		a.enqueueDownload(func() {
			defer wg.Done()
			n := atomic.AddInt32(&running, 1)
			for {
				cur := atomic.LoadInt32(&maxRunning)
				if n <= cur || atomic.CompareAndSwapInt32(&maxRunning, cur, n) {
					break
				}
			}
			time.Sleep(20 * time.Millisecond)
			atomic.AddInt32(&running, -1)
		})
	}
	wg.Wait()

	if maxRunning > 2 {
		t.Fatalf("expected at most 2 concurrent tasks, saw %d", maxRunning)
	}
	if maxRunning < 1 {
		t.Fatalf("expected at least one task to have run")
	}
}

func TestEnqueueDownloadDefaultsConcurrencyWhenUnset(t *testing.T) {
	a := &App{}
	done := make(chan struct{})
	a.enqueueDownload(func() { close(done) })
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("task never ran - queue did not start with a zero-value config")
	}
}
