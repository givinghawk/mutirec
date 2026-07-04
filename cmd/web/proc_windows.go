//go:build windows

package main

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// prepareProcessGroup puts a child process in its own console process group
// on Windows so it can later be sent a CTRL_BREAK_EVENT independently of
// this process - os.Process.Signal(os.Interrupt) always returns "not
// supported by windows" otherwise, which left ffmpeg/streamlink with no way
// to be asked to shut down gracefully and finalize their output.
func prepareProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP}
}

// interruptProcess asks a process started via prepareProcessGroup to shut
// down gracefully.
func interruptProcess(pid int) error {
	return windows.GenerateConsoleCtrlEvent(windows.CTRL_BREAK_EVENT, uint32(pid))
}
