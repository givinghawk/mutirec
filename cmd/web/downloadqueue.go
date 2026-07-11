package main

// ============================================================================
// Shared download queue: both Fetch-from-URL (urlfetch.go) and YouTube
// downloads (ytdownload.go) submit their actual work here instead of firing
// off a bare goroutine, so starting several downloads at once queues the
// extras behind Settings.Downloads.MaxConcurrent rather than running them
// all in parallel and saturating the box's network/disk. A job sits in
// "queued" status (set by its caller before submitting) until a worker
// actually picks it up.
// ============================================================================

// startDownloadWorkers lazily sizes and starts the fixed worker pool the
// first time a download is queued, from the concurrency configured at that
// moment. Changing Settings.Downloads.MaxConcurrent later takes effect on
// the next process restart, not live - simple, and consistent with the
// existing scheduler ticker (also fixed for the process's lifetime).
func (a *App) startDownloadWorkers() {
	n := a.snapshotConfig().Settings.Downloads.MaxConcurrent
	if n <= 0 {
		n = 2
	}
	a.downloadQueue = make(chan func(), 200)
	for i := 0; i < n; i++ {
		go func() {
			for task := range a.downloadQueue {
				task()
			}
		}()
	}
}

// enqueueDownload submits a download task to the shared worker pool. It
// never blocks the caller for long: the queue channel is generously
// buffered, so a handler can enqueue and return immediately.
func (a *App) enqueueDownload(task func()) {
	a.downloadQueueOnce.Do(a.startDownloadWorkers)
	a.downloadQueue <- task
}
