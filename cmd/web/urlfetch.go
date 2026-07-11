package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ============================================================================
// Fetch-from-URL: downloads a file (or a whole shared folder) straight into
// the file explorer tree from a direct link or a public share link, as a
// background job so a large download doesn't need a browser tab left open.
//
// Specifically supports the ownCloud/Nextcloud "public share" URL
// convention - TransIP Stack (like a number of other self-hosted file-share
// products) is built on it - where a share link such as
// https://host/s/<token> or https://host/index.php/s/<token> serves the
// share's landing page at that URL, but appending "/download" to it returns
// the actual file (or, for a shared folder, a zip of the whole thing) and,
// for a password-protected share, HTTP Basic auth with the share token as
// the username and the share password as the password. Any other URL is
// just downloaded directly (with optional HTTP Basic auth, which covers
// most other authenticated direct-download links generically).
// ============================================================================

// URLFetchJob tracks one in-progress or finished fetch-from-URL download.
// All fields are guarded by mu; use view() for a JSON-safe snapshot.
type URLFetchJob struct {
	mu sync.Mutex

	id         string
	sourceURL  string
	destName   string
	status     string // "running" | "done" | "error"
	startedAt  time.Time
	finishedAt time.Time

	// cookie, if set, is sent as the Cookie header on every request this job
	// makes - pasted in by the user from their own browser's session for a
	// share whose backend requires an authenticated session that a plain
	// anonymous request never gets (some Stack instances do this even for a
	// link that's otherwise a public share). Immutable after job creation,
	// so it's safe to read without holding mu.
	cookie string

	// username, if set, is used for HTTP Basic Auth alongside password (see
	// runURLFetchJob's password parameter) - only the WebDAV path uses this;
	// every other share convention this file supports has an implicit
	// username (the share token) and only needs a password.
	username string

	// proxyURL, if set, routes every request this job makes through the
	// given proxy (http/https/socks5/socks4/socks4a - see proxyTransport in
	// proxy.go). Empty means dial directly. Immutable after job creation.
	proxyURL string

	totalBytes, transferredBytes int64
	speedBps                     float64
	errMsg                       string
	log                          []ShareJobLogLine

	lastSampleAt    time.Time
	lastSampleBytes int64

	// Debug mode: when enabled, every HTTP request/response this job makes
	// (URLs, headers, and small textual bodies) is appended to debugFile, a
	// plain-text ".log" file dropped in the download's own destination
	// directory - meant to be handed back for troubleshooting a share
	// provider's API without having to reproduce the problem live.
	debugMu   sync.Mutex
	debugFile *os.File
	debugPath string
}

// URLFetchJobView is the JSON-safe snapshot of a URLFetchJob.
type URLFetchJobView struct {
	ID               string            `json:"id"`
	SourceURL        string            `json:"sourceUrl"`
	DestName         string            `json:"destName"`
	Status           string            `json:"status"`
	StartedAt        time.Time         `json:"startedAt"`
	FinishedAt       *time.Time        `json:"finishedAt,omitempty"`
	TotalBytes       int64             `json:"totalBytes"`
	TransferredBytes int64             `json:"transferredBytes"`
	SpeedBps         float64           `json:"speedBps"`
	Error            string            `json:"error,omitempty"`
	Log              []ShareJobLogLine `json:"log"`
}

func (j *URLFetchJob) view() URLFetchJobView {
	j.mu.Lock()
	defer j.mu.Unlock()
	v := URLFetchJobView{
		ID: j.id, SourceURL: j.sourceURL, DestName: j.destName, Status: j.status,
		StartedAt: j.startedAt, TotalBytes: j.totalBytes, TransferredBytes: j.transferredBytes,
		SpeedBps: j.speedBps, Error: j.errMsg, Log: append([]ShareJobLogLine(nil), j.log...),
	}
	if !j.finishedAt.IsZero() {
		v.FinishedAt = &j.finishedAt
	}
	return v
}

func (j *URLFetchJob) logf(format string, args ...any) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.log = append(j.log, ShareJobLogLine{Time: time.Now().Format("15:04:05"), Text: fmt.Sprintf(format, args...)})
	if len(j.log) > 500 {
		j.log = j.log[len(j.log)-500:]
	}
}

func (j *URLFetchJob) addBytes(n int64) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.transferredBytes += n
	now := time.Now()
	if j.lastSampleAt.IsZero() {
		j.lastSampleAt, j.lastSampleBytes = now, j.transferredBytes
		return
	}
	if elapsed := now.Sub(j.lastSampleAt); elapsed >= 250*time.Millisecond {
		j.speedBps = float64(j.transferredBytes-j.lastSampleBytes) / elapsed.Seconds()
		j.lastSampleAt, j.lastSampleBytes = now, j.transferredBytes
	}
}

func (j *URLFetchJob) finish(err error) {
	j.mu.Lock()
	j.finishedAt = time.Now()
	j.speedBps = 0
	if err != nil {
		j.status = "error"
		j.errMsg = err.Error()
	} else {
		j.status = "done"
	}
	j.mu.Unlock()

	j.debugMu.Lock()
	if j.debugFile != nil {
		j.debugFile.Close()
		j.debugFile = nil
	}
	j.debugMu.Unlock()
}

// enableDebug turns on request/response logging for this job, writing to a
// ".log" file in destDir (the same directory the download is being saved
// into) named after the job's ID so concurrent jobs targeting the same
// folder don't clobber each other's logs. Best-effort: a failure to create
// the file just means debug logging silently stays off.
func (j *URLFetchJob) enableDebug(destDir string) {
	path := filepath.Join(destDir, "mutirec-fetch-debug-"+j.id+".log")
	f, err := os.Create(path)
	if err != nil {
		j.logf("Debug mode requested but could not create %s: %s", path, err)
		return
	}
	j.debugMu.Lock()
	j.debugFile = f
	j.debugPath = path
	j.debugMu.Unlock()
	j.logf("Debug mode on - logging every HTTP request/response to %s", path)
}

// debugf appends one line to the debug log, if enabled; a no-op otherwise.
func (j *URLFetchJob) debugf(format string, args ...any) {
	j.debugMu.Lock()
	defer j.debugMu.Unlock()
	if j.debugFile == nil {
		return
	}
	fmt.Fprintf(j.debugFile, "[%s] "+format+"\n", append([]any{time.Now().Format("15:04:05.000")}, args...)...)
}

// httpTransport returns the base RoundTripper every HTTP client this job
// creates should use - wrapped in a request/response logger when debug mode
// is on, otherwise just the plain transport.
func (j *URLFetchJob) httpTransport() http.RoundTripper {
	var rt http.RoundTripper = &http.Transport{ResponseHeaderTimeout: responseHeaderTimeout}
	if j.proxyURL != "" {
		if t, err := proxyTransport(j.proxyURL); err == nil {
			rt = t
		} else {
			j.logf("Could not use the configured proxy (%s) - falling back to a direct connection", err)
		}
	}
	j.debugMu.Lock()
	enabled := j.debugFile != nil
	j.debugMu.Unlock()
	if enabled {
		rt = &debugRoundTripper{inner: rt, job: j}
	}
	// Cookie injection wraps outside the debug logger (not inside), so the
	// debug log shows the request as actually sent - including a redacted
	// Cookie line - rather than silently omitting it.
	if j.cookie != "" {
		rt = &cookieRoundTripper{inner: rt, cookie: j.cookie}
	}
	return rt
}

// cookieRoundTripper adds a fixed Cookie header to every request - used
// when the user pastes their own browser session cookie for a share whose
// backend requires an authenticated session that a plain anonymous request
// never gets (some Stack instances 401 an otherwise-public share's folder
// listing without one).
type cookieRoundTripper struct {
	inner  http.RoundTripper
	cookie string
}

func (c *cookieRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	if existing := req.Header.Get("Cookie"); existing != "" {
		req.Header.Set("Cookie", existing+"; "+c.cookie)
	} else {
		req.Header.Set("Cookie", c.cookie)
	}
	return c.inner.RoundTrip(req)
}

// debugBodyLogCap bounds how much of a textual response body gets copied
// into the debug log - plenty for an API's JSON/HTML response, small enough
// to never turn on a multi-gigabyte file download.
const debugBodyLogCap = 16 * 1024

// debugRoundTripper logs every request and response that passes through it
// to its job's debug log file, then delegates to inner unchanged.
type debugRoundTripper struct {
	inner http.RoundTripper
	job   *URLFetchJob
}

func (d *debugRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	d.job.debugf("--> %s %s", req.Method, req.URL.String())
	for k, v := range req.Header {
		if strings.EqualFold(k, "Authorization") || strings.EqualFold(k, "Cookie") {
			d.job.debugf("    %s: [redacted]", k)
			continue
		}
		d.job.debugf("    %s: %s", k, strings.Join(v, ", "))
	}

	resp, err := d.inner.RoundTrip(req)
	if err != nil {
		d.job.debugf("<-- error for %s %s: %s", req.Method, req.URL.String(), err)
		return resp, err
	}

	d.job.debugf("<-- %s %s", resp.Status, req.URL.String())
	for k, v := range resp.Header {
		d.job.debugf("    %s: %s", k, strings.Join(v, ", "))
	}
	if isTextualContentType(resp.Header.Get("Content-Type")) {
		head, _ := io.ReadAll(io.LimitReader(resp.Body, debugBodyLogCap))
		resp.Body = struct {
			io.Reader
			io.Closer
		}{io.MultiReader(bytes.NewReader(head), resp.Body), resp.Body}
		d.job.debugf("    body: %s", string(head))
	}
	return resp, nil
}

// isTextualContentType reports whether a response's Content-Type is worth
// copying into the debug log (JSON/HTML/XML/plain text), as opposed to a
// media file that would be pointlessly slow and huge to log.
func isTextualContentType(ct string) bool {
	ct = strings.ToLower(ct)
	return strings.HasPrefix(ct, "text/") || strings.Contains(ct, "json") || strings.Contains(ct, "xml")
}

func (a *App) putFetchJob(job *URLFetchJob) {
	a.fetchJobsMu.Lock()
	defer a.fetchJobsMu.Unlock()
	if len(a.fetchJobs) > 50 {
		oldest, oldestID := time.Now(), ""
		for id, j := range a.fetchJobs {
			j.mu.Lock()
			t := j.startedAt
			j.mu.Unlock()
			if t.Before(oldest) {
				oldest, oldestID = t, id
			}
		}
		if oldestID != "" {
			delete(a.fetchJobs, oldestID)
		}
	}
	a.fetchJobs[job.id] = job
}

func (a *App) getFetchJob(id string) (*URLFetchJob, bool) {
	a.fetchJobsMu.Lock()
	defer a.fetchJobsMu.Unlock()
	j, ok := a.fetchJobs[id]
	return j, ok
}

// handleExplorerFetchJobItem returns one fetch job's current status by ID,
// for polling.
func (a *App) handleExplorerFetchJobItem(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !a.isAdminReq(r) {
		http.Error(w, "admin only", http.StatusForbidden)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/explorer/fetch/jobs/")
	job, ok := a.getFetchJob(id)
	if !ok {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, job.view())
}

// looksLikeOwncloudShare reports whether a URL looks like an ownCloud/
// Nextcloud (and compatible - including TransIP Stack) public share link,
// i.e. its path contains an "/s/<token>" segment.
func looksLikeOwncloudShare(u *url.URL) (token string, ok bool) {
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	for i, p := range parts {
		if p == "s" && i+1 < len(parts) && parts[i+1] != "" {
			return parts[i+1], true
		}
	}
	return "", false
}

// handleExplorerFetchURL starts a background download of a direct link or a
// public share link (see looksLikeOwncloudShare) into a destination
// directory under the explorer root.
func (a *App) handleExplorerFetchURL(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !a.isAdminReq(r) {
		http.Error(w, "admin only", http.StatusForbidden)
		return
	}
	var req struct {
		URL      string `json:"url"`
		Username string `json:"username"`
		Password string `json:"password"`
		Path     string `json:"path"`
		Debug    bool   `json:"debug"`
		Cookie   string `json:"cookie"`
		UseProxy bool   `json:"useProxy"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	req.URL = strings.TrimSpace(req.URL)
	u, err := url.Parse(req.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		writeJSON(w, map[string]any{"ok": false, "error": "enter a full URL including http:// or https://"})
		return
	}
	root := a.explorerRoot(a.snapshotConfig())
	destDir, err := resolveExplorerPath(root, req.Path)
	if err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
		return
	}

	proxyURL := ""
	if req.UseProxy {
		proxyURL = a.snapshotConfig().Settings.Sharing.ProxyURL
		if proxyURL == "" {
			writeJSON(w, map[string]any{"ok": false, "error": "no proxy is configured - set one in Settings -> Peer Sharing (P2P) first"})
			return
		}
	}

	job := &URLFetchJob{
		id: newID(), sourceURL: req.URL, status: "queued", startedAt: time.Now(),
		cookie: strings.TrimSpace(req.Cookie), username: strings.TrimSpace(req.Username), proxyURL: proxyURL,
	}
	if req.Debug {
		job.enableDebug(destDir)
	}
	a.putFetchJob(job)
	a.event("info", "Queued URL fetch job "+job.id+" for "+req.URL)
	a.enqueueDownload(func() {
		job.mu.Lock()
		job.status = "running"
		job.startedAt = time.Now()
		job.mu.Unlock()
		a.runURLFetchJob(job, req.URL, req.Password, destDir, root)
	})
	writeJSON(w, map[string]any{"ok": true, "jobId": job.id})
}

// runURLFetchJob is the background worker started by handleExplorerFetchURL.
func (a *App) runURLFetchJob(job *URLFetchJob, rawURL, password, destDir, root string) {
	// A webdav://.../webdavs://... URL (the rclone/Cyberduck convention for
	// naming a WebDAV endpoint unambiguously, since a WebDAV server is
	// otherwise indistinguishable from a plain HTTP one by URL shape alone)
	// gets its own PROPFIND-based walker instead of a plain GET.
	if rewritten, isWebDAV := rewriteWebDAVScheme(rawURL); isWebDAV {
		a.runWebDAVDownload(job, rewritten, password, destDir, root)
		return
	}

	client := &http.Client{Transport: job.httpTransport()}

	// A Stack (stackstorage) share URL that already names a specific folder
	// (.../s/<token>/<locale>/files/<nodeID>) is unambiguous - go straight to
	// the Stack v2 API rather than the ownCloud/Nextcloud "/download" guess,
	// which just returns that share's single-page-app HTML shell on Stack.
	if u, err := url.Parse(rawURL); err == nil {
		if token, nodeID, ok := parseStackFilesURL(u); ok && nodeID != "" {
			a.runStackShareDownload(job, u.Scheme, u.Host, token, nodeID, destDir, root)
			return
		}
	}

	fetchURL := rawURL
	var basicUser, basicPass, shareToken string
	if u, err := url.Parse(rawURL); err == nil {
		if token, ok := looksLikeOwncloudShare(u); ok {
			shareToken = token
			job.logf("Detected an ownCloud/Nextcloud-style share link (token %s) - requesting the direct download", token)
			fetchURL = strings.TrimRight(rawURL, "/") + "/download"
			if password != "" {
				basicUser, basicPass = token, password
			}
		}
	}

	job.logf("Fetching %s", fetchURL)
	getReq, err := http.NewRequest(http.MethodGet, fetchURL, nil)
	if err != nil {
		job.logf("Invalid URL: %s", err)
		job.finish(err)
		return
	}
	if basicUser != "" {
		getReq.SetBasicAuth(basicUser, basicPass)
	}
	resp, err := client.Do(getReq)
	if err != nil {
		job.logf("Could not reach that URL: %s", err)
		job.finish(err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		err := fmt.Errorf("that link needs a password (or the one given was wrong) - HTTP %d", resp.StatusCode)
		job.logf("%s", err)
		job.finish(err)
		return
	}
	if resp.StatusCode != http.StatusOK {
		err := fmt.Errorf("the server returned HTTP %d", resp.StatusCode)
		job.logf("%s", err)
		job.finish(err)
		return
	}
	// Stack doesn't have a "/download" convenience URL like ownCloud/
	// Nextcloud - that path just serves the same single-page-app HTML as
	// the share page itself. Fall back to the Stack v2 listing API, which
	// works from just the share token (best-effort at the share's root,
	// since this bare "/s/<token>" URL didn't name a specific folder).
	if shareToken != "" && strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") {
		resp.Body.Close()
		u, _ := url.Parse(rawURL)
		a.runStackShareDownload(job, u.Scheme, u.Host, shareToken, "", destDir, root)
		return
	}

	name := urlDownloadDest(resp, fetchURL)
	job.mu.Lock()
	job.destName = name
	job.mu.Unlock()
	if resp.ContentLength > 0 {
		job.mu.Lock()
		job.totalBytes = resp.ContentLength
		job.mu.Unlock()
	}

	dest := filepath.Join(destDir, name)
	tmp := dest + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		job.logf("Could not create destination file: %s", err)
		job.finish(err)
		return
	}
	job.logf("Saving to %s", name)
	var w io.Writer = f
	w = io.MultiWriter(f, progressWriter(job.addBytes))
	_, copyErr := io.Copy(w, resp.Body)
	f.Close()
	if copyErr != nil {
		_ = os.Remove(tmp)
		job.logf("Download failed: %s", copyErr)
		job.finish(copyErr)
		return
	}
	if err := os.Rename(tmp, dest); err != nil {
		job.logf("Could not finalize download: %s", err)
		job.finish(err)
		return
	}
	job.logf("Saved %s (%s)", name, formatBytesGo(job.view().TransferredBytes))
	a.verifyDownloadHash(job.logf, dest)

	if strings.EqualFold(filepath.Ext(name), ".zip") {
		extractTo := uniqueSiblingDir(strings.TrimSuffix(dest, filepath.Ext(dest)))
		job.logf("Extracting %s...", name)
		if err := extractZip(dest, extractTo, root); err != nil {
			job.logf("Could not auto-extract the zip (left as-is): %s", err)
		} else {
			job.logf("Extracted into %s", filepath.Base(extractTo))
		}
	}

	job.finish(nil)
}

// ============================================================================
// Stack (stackstorage) v2 share API. Unlike the ownCloud/Nextcloud share
// convention above, Stack serves its share pages as a single-page app at
// /s/<token>[/<locale>/files/<nodeID>] and doesn't expose a "/download"
// shortcut - fetching that just returns the app's HTML shell. The actual
// data lives behind a JSON API: GET .../api/v2/share/<token>/nodes lists a
// folder's children (paginated, by parentID), and GET
// .../api/v2/share/<token>/files/<id>/download/<name> streams a given
// file's bytes. This mirrors that API to recursively walk and download a
// shared folder (or single file) into the explorer tree.
// ============================================================================

var stackFilesURLPattern = regexp.MustCompile(`^/s/([^/]+)(?:/[A-Za-z]{2}(?:-[A-Za-z]+)?)?/files/(\d+)`)

// parseStackFilesURL recognizes a Stack share URL that names a specific
// folder or file, e.g. "/s/<token>/en/files/<nodeID>" (the locale segment
// is optional and varies by the visitor's browser language).
func parseStackFilesURL(u *url.URL) (token, nodeID string, ok bool) {
	if m := stackFilesURLPattern.FindStringSubmatch(u.Path); m != nil {
		return m[1], m[2], true
	}
	return "", "", false
}

var stackCSRFTokenPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)csrf-?token["']?\s*[:=]\s*["']([A-Za-z0-9_-]+)["']`),
	regexp.MustCompile(`(?i)<meta[^>]+name=["']csrf-token["'][^>]+content=["']([^"']+)["']`),
}

// extractStackCSRFToken best-effort scrapes a CSRF token out of the share
// page's HTML/inline JS, needed by the file-download endpoint (the node
// listing endpoints work fine without it).
func extractStackCSRFToken(html string) string {
	for _, re := range stackCSRFTokenPatterns {
		if m := re.FindStringSubmatch(html); len(m) > 1 {
			return m[1]
		}
	}
	return ""
}

// stackNode is one entry (file or folder) from the Stack share nodes API.
type stackNode struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	Dir  bool   `json:"dir"`
	Size int64  `json:"size"`
}

type stackNodesResponse struct {
	Nodes []stackNode `json:"nodes"`
	Total int         `json:"total"`
}

// listStackNodes fetches every child of parentID (paginated 100 at a time).
func listStackNodes(client *http.Client, base, token, csrfToken, parentID string) ([]stackNode, error) {
	var all []stackNode
	for offset := 0; ; offset = len(all) {
		q := url.Values{}
		q.Set("limit", "100")
		q.Set("offset", strconv.Itoa(offset))
		q.Set("orderBy", "default")
		q.Set("reverse", "false")
		q.Set("parentID", parentID)
		q.Set("search", "")
		q.Set("mediaType", "all")
		if csrfToken != "" {
			q.Set("CSRF-Token", csrfToken)
		}
		reqURL := base + "/api/v2/share/" + url.PathEscape(token) + "/nodes?" + q.Encode()
		resp, err := client.Get(reqURL)
		if err != nil {
			return nil, err
		}
		var page stackNodesResponse
		decErr := json.NewDecoder(resp.Body).Decode(&page)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("listing folder contents returned HTTP %d", resp.StatusCode)
		}
		if decErr != nil {
			return nil, fmt.Errorf("could not parse the folder listing: %w", decErr)
		}
		all = append(all, page.Nodes...)
		if len(page.Nodes) == 0 || len(all) >= page.Total {
			break
		}
	}
	return all, nil
}

// sanitizeStackName strips path separators out of a name reported by the
// Stack API so it can't escape the destination directory it's joined into.
func sanitizeStackName(name string) string {
	name = strings.NewReplacer("/", "-", "\\", "-").Replace(strings.TrimSpace(name))
	if name == "" || name == "." || name == ".." {
		return "file"
	}
	return name
}

// stackFileToGet is one file discovered while walking a share, with the
// sanitized relative directory (under the download root) it belongs in.
type stackFileToGet struct {
	node   stackNode
	relDir string
}

// gatherStackFiles recursively lists parentID and everything under it,
// flattening the tree into a list of files to download.
func gatherStackFiles(client *http.Client, base, token, csrfToken, parentID, relDir string) ([]stackFileToGet, error) {
	nodes, err := listStackNodes(client, base, token, csrfToken, parentID)
	if err != nil {
		return nil, err
	}
	var files []stackFileToGet
	for _, n := range nodes {
		if n.Dir {
			sub, err := gatherStackFiles(client, base, token, csrfToken, strconv.FormatInt(n.ID, 10), filepath.Join(relDir, sanitizeStackName(n.Name)))
			if err != nil {
				return nil, err
			}
			files = append(files, sub...)
			continue
		}
		files = append(files, stackFileToGet{node: n, relDir: relDir})
	}
	return files, nil
}

// downloadStackFile streams a single Stack-hosted file to dest.
func downloadStackFile(client *http.Client, base, token, csrfToken string, node stackNode, dest string, job *URLFetchJob) error {
	q := url.Values{"contentDisposition": {"1"}}
	if csrfToken != "" {
		q.Set("CSRF-Token", csrfToken)
	}
	reqURL := base + "/api/v2/share/" + url.PathEscape(token) + "/files/" + strconv.FormatInt(node.ID, 10) + "/download/" + url.PathEscape(node.Name) + "?" + q.Encode()
	resp, err := client.Get(reqURL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") {
		return fmt.Errorf("server returned a web page instead of the file - the share may need a password, which isn't supported yet")
	}
	tmp := dest + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	w := io.MultiWriter(f, progressWriter(job.addBytes))
	_, copyErr := io.Copy(w, resp.Body)
	f.Close()
	if copyErr != nil {
		_ = os.Remove(tmp)
		return copyErr
	}
	return os.Rename(tmp, dest)
}

// runStackShareDownload downloads an entire Stack share (or the subfolder
// named by startNodeID) into destDir, mirroring its folder structure.
func (a *App) runStackShareDownload(job *URLFetchJob, scheme, host, token, startNodeID, destDir, root string) {
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, Transport: job.httpTransport()}
	base := scheme + "://" + host

	job.logf("Detected a Stack share (token %s) - using the Stack API to list and fetch files", token)

	csrfToken := ""
	if resp, err := client.Get(base + "/s/" + url.PathEscape(token)); err == nil {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		csrfToken = extractStackCSRFToken(string(body))
	}

	job.logf("Listing share contents...")
	files, err := gatherStackFiles(client, base, token, csrfToken, startNodeID, "")
	if err != nil {
		job.logf("Could not list share contents: %s", err)
		job.finish(err)
		return
	}
	if len(files) == 0 {
		err := fmt.Errorf("no files found in that share (it may be empty, password-protected, or this link format isn't supported yet)")
		job.logf("%s", err)
		job.finish(err)
		return
	}

	var total int64
	for _, f := range files {
		total += f.node.Size
	}
	job.mu.Lock()
	job.totalBytes = total
	job.destName = files[0].node.Name
	job.mu.Unlock()
	job.logf("Found %d file(s), %s total", len(files), formatBytesGo(total))

	for _, f := range files {
		destSubDir := filepath.Clean(filepath.Join(destDir, f.relDir))
		if destSubDir != destDir && !strings.HasPrefix(destSubDir, destDir+string(os.PathSeparator)) {
			job.logf("Skipping %s: path would escape the destination folder", f.node.Name)
			continue
		}
		if err := os.MkdirAll(destSubDir, 0o755); err != nil {
			job.logf("Could not create directory: %s", err)
			job.finish(err)
			return
		}
		dest := filepath.Join(destSubDir, sanitizeStackName(f.node.Name))
		if err := downloadStackFile(client, base, token, csrfToken, f.node, dest, job); err != nil {
			job.logf("Failed to download %s: %s", f.node.Name, err)
			job.finish(err)
			return
		}
		job.logf("Saved %s", f.node.Name)
		a.verifyDownloadHash(job.logf, dest)
	}

	job.finish(nil)
}
