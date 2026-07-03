//go:build !windows

package disk

import "golang.org/x/sys/unix"

// freeSpace reports the total and free bytes of the volume holding path.
func freeSpace(path string) (total, free uint64) {
	var s unix.Statfs_t
	if err := unix.Statfs(path, &s); err != nil {
		return 0, 0
	}
	total = uint64(s.Blocks) * uint64(s.Bsize)
	free = uint64(s.Bfree) * uint64(s.Bsize)
	return total, free
}
