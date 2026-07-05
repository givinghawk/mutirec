//go:build windows

package main

import (
	"syscall"
	"unsafe"
)

// memoryStatusEx mirrors the Win32 MEMORYSTATUSEX struct used by
// GlobalMemoryStatusEx. golang.org/x/sys/windows doesn't expose this call,
// so it's declared directly against kernel32 via the standard syscall
// package instead of pulling in a new dependency.
type memoryStatusEx struct {
	dwLength                uint32
	dwMemoryLoad            uint32
	ullTotalPhys            uint64
	ullAvailPhys            uint64
	ullTotalPageFile        uint64
	ullAvailPageFile        uint64
	ullTotalVirtual         uint64
	ullAvailVirtual         uint64
	ullAvailExtendedVirtual uint64
}

var (
	modkernel32              = syscall.NewLazyDLL("kernel32.dll")
	procGlobalMemoryStatusEx = modkernel32.NewProc("GlobalMemoryStatusEx")
)

// totalMemoryBytes reports total installed physical RAM.
func totalMemoryBytes() uint64 {
	var m memoryStatusEx
	m.dwLength = uint32(unsafe.Sizeof(m))
	ret, _, _ := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&m)))
	if ret == 0 {
		return 0
	}
	return m.ullTotalPhys
}
