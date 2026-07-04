// Package disk reports disk usage for the recordings directory: the total
// recorded bytes (with a per-stage breakdown) and the free space on the volume.
package disk

import (
	"io/fs"
	"path/filepath"
	"strings"
)

// Usage is a snapshot of recordings disk usage.
type Usage struct {
	Total       uint64            `json:"total"`       // total size of the recordings directory
	PerStage    map[string]uint64 `json:"perStage"`    // size per immediate subfolder (stage)
	VolumeTotal uint64            `json:"volumeTotal"` // total size of the holding volume
	VolumeFree  uint64            `json:"volumeFree"`  // free space on the holding volume
}

// Scan walks the recordings directory and queries the volume's free space.
func Scan(recordingsDir string) Usage {
	u := Usage{PerStage: map[string]uint64{}}
	u.Total, u.PerStage = scanDir(recordingsDir)
	u.VolumeTotal, u.VolumeFree = freeSpace(recordingsDir)
	return u
}

// scanDir sums all files under root and groups them by immediate subfolder
// (used to attribute per-stage split files to their stage).
func scanDir(root string) (uint64, map[string]uint64) {
	var total uint64
	perStage := map[string]uint64{}
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		sz := uint64(info.Size())
		total += sz
		if rel, err := filepath.Rel(root, p); err == nil {
			parts := strings.Split(filepath.ToSlash(rel), "/")
			if len(parts) > 1 {
				perStage[parts[0]] += sz
			}
		}
		return nil
	})
	return total, perStage
}
