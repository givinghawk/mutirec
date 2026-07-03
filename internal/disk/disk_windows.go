//go:build windows

package disk

import "golang.org/x/sys/windows"

// freeSpace reports the total and free bytes of the volume holding path.
func freeSpace(path string) (total, free uint64) {
	var freeBytes, totalBytes, totalFreeBytes uint64
	utf16, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, 0
	}
	if err := windows.GetDiskFreeSpaceEx(utf16, &freeBytes, &totalBytes, &totalFreeBytes); err != nil {
		return 0, 0
	}
	return totalBytes, freeBytes
}
