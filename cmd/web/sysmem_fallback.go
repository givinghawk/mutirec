//go:build !windows && !linux

package main

// totalMemoryBytes has no implementation for this OS (only Windows dev
// environments and the Linux Docker image are supported targets) - the
// system check reports memory as "Unknown" rather than failing outright.
func totalMemoryBytes() uint64 { return 0 }
