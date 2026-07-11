package main

import (
	"os"
	"path/filepath"
)

// verifyDownloadHash computes the sha256 hash of a freshly downloaded file
// (via the same cached fileHash used for matchfile export, so re-checking
// an unchanged file later is free), logs it so it can be cross-checked
// against a hash published elsewhere, and warns if another file already in
// the same destination folder has identical content - the two questions
// most worth automating about any download: "did this come through
// intact" and "do I already have this". Best-effort: any error here is
// logged and swallowed, never fails the download itself.
func (a *App) verifyDownloadHash(logf func(string, ...any), absPath string) {
	info, err := os.Stat(absPath)
	if err != nil {
		return
	}
	root := a.explorerRoot(a.snapshotConfig())
	relPath, err := filepath.Rel(root, absPath)
	if err != nil {
		relPath = absPath
	}
	hash, err := a.fileHash(absPath, relPath, info.Size(), info.ModTime())
	if err != nil {
		logf("Could not compute a hash for %s: %s", filepath.Base(absPath), err)
		return
	}
	logf("sha256 %s  %s", hash, filepath.Base(absPath))

	dir := filepath.Dir(absPath)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	base := filepath.Base(absPath)
	for _, e := range entries {
		if e.IsDir() || e.Name() == base {
			continue
		}
		siblingAbs := filepath.Join(dir, e.Name())
		si, err := e.Info()
		if err != nil {
			continue
		}
		siblingRel, err := filepath.Rel(root, siblingAbs)
		if err != nil {
			continue
		}
		siblingHash, err := a.fileHash(siblingAbs, siblingRel, si.Size(), si.ModTime())
		if err != nil {
			continue
		}
		if siblingHash == hash {
			logf("Note: this file's content is identical to the existing \"%s\" - you may already have this one", e.Name())
			return
		}
	}
}
