package main

import (
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// ============================================================================
// WebDAV support for Fetch-from-URL: a URL prefixed with webdav:// or
// webdavs:// (the rclone/Cyberduck convention for naming a WebDAV endpoint
// unambiguously - a WebDAV server is otherwise indistinguishable from a
// plain HTTP one by URL shape alone) is rewritten to http/https and treated
// as a WebDAV collection: PROPFIND (Depth: 0) on the target itself decides
// whether it's a single file or a folder, PROPFIND (Depth: 1) walks a
// folder's children recursively, and GET downloads each file. Routes
// through the same proxy/cookie/debug-log machinery as every other
// Fetch-from-URL job (see URLFetchJob.httpTransport in urlfetch.go).
// ============================================================================

// rewriteWebDAVScheme rewrites a webdav://.../webdavs://... URL to the
// equivalent http/https URL, reporting whether rawURL used that scheme.
func rewriteWebDAVScheme(rawURL string) (string, bool) {
	lower := strings.ToLower(rawURL)
	switch {
	case strings.HasPrefix(lower, "webdav://"):
		return "http://" + rawURL[len("webdav://"):], true
	case strings.HasPrefix(lower, "webdavs://"):
		return "https://" + rawURL[len("webdavs://"):], true
	default:
		return rawURL, false
	}
}

// webdavMultiStatus/webdavResponse/webdavPropstat/webdavProp/
// webdavResourceType decode just enough of a PROPFIND response to tell a
// file from a folder and read its size. Struct tags deliberately omit a
// namespace (encoding/xml then matches by local name only), since WebDAV
// servers vary in which namespace prefix they use for the DAV: namespace
// ("D:", "d:", or none at all).
type webdavMultiStatus struct {
	Responses []webdavResponse `xml:"response"`
}
type webdavResponse struct {
	Href      string           `xml:"href"`
	PropStats []webdavPropstat `xml:"propstat"`
}
type webdavPropstat struct {
	Prop webdavProp `xml:"prop"`
}
type webdavProp struct {
	ResourceType  webdavResourceType `xml:"resourcetype"`
	ContentLength int64              `xml:"getcontentlength"`
}
type webdavResourceType struct {
	Collection *struct{} `xml:"collection"`
}

// webdavEntry is one child entry found in a PROPFIND (Depth: 1) response.
type webdavEntry struct {
	href  string
	isDir bool
	size  int64
}

// webdavPropfind issues a PROPFIND against u and returns the decoded
// multistatus response - depth "0" describes u itself, "1" also describes
// its immediate children.
func webdavPropfind(client *http.Client, u *url.URL, username, password, depth string) (*webdavMultiStatus, error) {
	body := `<?xml version="1.0" encoding="utf-8"?><propfind xmlns="DAV:"><prop><resourcetype/><getcontentlength/></prop></propfind>`
	req, err := http.NewRequest("PROPFIND", u.String(), strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Depth", depth)
	req.Header.Set("Content-Type", "application/xml; charset=utf-8")
	if username != "" || password != "" {
		req.SetBasicAuth(username, password)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("the WebDAV server rejected the credentials (HTTP %d) - check the username/password", resp.StatusCode)
	}
	if resp.StatusCode != 207 && resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("PROPFIND returned HTTP %d - is this actually a WebDAV URL?", resp.StatusCode)
	}
	var ms webdavMultiStatus
	if err := xml.NewDecoder(resp.Body).Decode(&ms); err != nil {
		return nil, fmt.Errorf("could not parse the WebDAV response: %w", err)
	}
	return &ms, nil
}

// webdavChildren runs a Depth:1 PROPFIND against dirURL and returns its
// children, excluding the collection's own self-referencing entry (WebDAV
// servers include the directory itself as the first <response> alongside
// its children).
func webdavChildren(client *http.Client, dirURL *url.URL, username, password string) ([]webdavEntry, error) {
	ms, err := webdavPropfind(client, dirURL, username, password, "1")
	if err != nil {
		return nil, err
	}
	selfPath := strings.TrimSuffix(dirURL.Path, "/")
	var entries []webdavEntry
	for _, r := range ms.Responses {
		if r.Href == "" || len(r.PropStats) == 0 {
			continue
		}
		hrefPath := strings.TrimSuffix(r.Href, "/")
		if p, err := url.Parse(hrefPath); err == nil {
			hrefPath = p.Path
		}
		if hrefPath == selfPath {
			continue
		}
		entries = append(entries, webdavEntry{
			href:  r.Href,
			isDir: r.PropStats[0].Prop.ResourceType.Collection != nil,
			size:  r.PropStats[0].Prop.ContentLength,
		})
	}
	return entries, nil
}

// webdavFileToGet is one file discovered while walking a WebDAV collection,
// with the sanitized relative directory (under the download root) it
// belongs in.
type webdavFileToGet struct {
	url    *url.URL
	name   string
	relDir string
	size   int64
}

// gatherWebDAVFiles recursively walks dirURL and everything under it,
// flattening the tree into a list of files to download.
func gatherWebDAVFiles(client *http.Client, dirURL *url.URL, username, password, relDir string) ([]webdavFileToGet, error) {
	entries, err := webdavChildren(client, dirURL, username, password)
	if err != nil {
		return nil, err
	}
	var files []webdavFileToGet
	for _, e := range entries {
		hrefURL, err := url.Parse(e.href)
		if err != nil {
			continue
		}
		childURL := dirURL.ResolveReference(hrefURL)
		name := sanitizeStackName(baseNameFromPath(childURL.Path))
		if e.isDir {
			sub, err := gatherWebDAVFiles(client, childURL, username, password, filepath.Join(relDir, name))
			if err != nil {
				return nil, err
			}
			files = append(files, sub...)
			continue
		}
		files = append(files, webdavFileToGet{url: childURL, name: name, relDir: relDir, size: e.size})
	}
	return files, nil
}

// baseNameFromPath returns the last path segment of a URL path, decoded
// from its percent-encoding, falling back to the raw segment if it doesn't
// decode cleanly.
func baseNameFromPath(p string) string {
	base := filepath.Base(strings.TrimSuffix(p, "/"))
	if decoded, err := url.PathUnescape(base); err == nil {
		return decoded
	}
	return base
}

// downloadWebDAVFile streams a single WebDAV-hosted file to dest.
func downloadWebDAVFile(client *http.Client, u *url.URL, username, password, dest string, job *URLFetchJob) error {
	req, err := http.NewRequest(http.MethodGet, u.String(), nil)
	if err != nil {
		return err
	}
	if username != "" || password != "" {
		req.SetBasicAuth(username, password)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
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

// runWebDAVDownload downloads a WebDAV URL (rewritten from webdav(s):// by
// the caller) into destDir - a single file if the target itself isn't a
// collection, or the whole tree (mirroring its folder structure) if it is.
func (a *App) runWebDAVDownload(job *URLFetchJob, rewrittenURL, password, destDir, root string) {
	client := &http.Client{Transport: job.httpTransport()}
	u, err := url.Parse(rewrittenURL)
	if err != nil {
		job.logf("Invalid WebDAV URL: %s", err)
		job.finish(err)
		return
	}

	job.logf("Detected a WebDAV URL - checking whether it's a file or a folder")
	ms, err := webdavPropfind(client, u, job.username, password, "0")
	if err != nil {
		job.logf("%s", err)
		job.finish(err)
		return
	}
	isDir := len(ms.Responses) > 0 && len(ms.Responses[0].PropStats) > 0 && ms.Responses[0].PropStats[0].Prop.ResourceType.Collection != nil

	if !isDir {
		name := sanitizeStackName(baseNameFromPath(u.Path))
		if name == "" || name == "file" {
			name = "download"
		}
		job.mu.Lock()
		job.destName = name
		if len(ms.Responses) > 0 && len(ms.Responses[0].PropStats) > 0 {
			job.totalBytes = ms.Responses[0].PropStats[0].Prop.ContentLength
		}
		job.mu.Unlock()
		dest := filepath.Join(destDir, name)
		job.logf("Downloading %s", name)
		if err := downloadWebDAVFile(client, u, job.username, password, dest, job); err != nil {
			job.logf("Download failed: %s", err)
			job.finish(err)
			return
		}
		job.logf("Saved %s", name)
		job.finish(nil)
		return
	}

	job.logf("Listing WebDAV folder contents...")
	files, err := gatherWebDAVFiles(client, u, job.username, password, "")
	if err != nil {
		job.logf("Could not list folder contents: %s", err)
		job.finish(err)
		return
	}
	if len(files) == 0 {
		err := fmt.Errorf("no files found at that WebDAV URL")
		job.logf("%s", err)
		job.finish(err)
		return
	}

	var total int64
	for _, f := range files {
		total += f.size
	}
	job.mu.Lock()
	job.totalBytes = total
	job.destName = files[0].name
	job.mu.Unlock()
	job.logf("Found %d file(s), %s total", len(files), formatBytesGo(total))

	for _, f := range files {
		destSubDir := filepath.Clean(filepath.Join(destDir, f.relDir))
		if destSubDir != destDir && !strings.HasPrefix(destSubDir, destDir+string(os.PathSeparator)) {
			job.logf("Skipping %s: path would escape the destination folder", f.name)
			continue
		}
		if err := os.MkdirAll(destSubDir, 0o755); err != nil {
			job.logf("Could not create directory: %s", err)
			job.finish(err)
			return
		}
		dest := filepath.Join(destSubDir, f.name)
		if err := downloadWebDAVFile(client, f.url, job.username, password, dest, job); err != nil {
			job.logf("Failed to download %s: %s", f.name, err)
			job.finish(err)
			return
		}
		job.logf("Saved %s", f.name)
	}

	job.finish(nil)
}
