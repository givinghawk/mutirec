//go:build linux

package main

import "golang.org/x/sys/unix"

// totalMemoryBytes reports total installed physical RAM via the Linux
// sysinfo() syscall - the Docker image this app ships in always runs Linux.
func totalMemoryBytes() uint64 {
	var info unix.Sysinfo_t
	if err := unix.Sysinfo(&info); err != nil {
		return 0
	}
	return uint64(info.Totalram) * uint64(info.Unit)
}
