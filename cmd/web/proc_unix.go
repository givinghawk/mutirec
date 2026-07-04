//go:build !windows

package main

import (
	"os"
	"os/exec"
)

// prepareProcessGroup is a no-op on Unix, where os.Process.Signal(os.Interrupt)
// (SIGINT) already works directly on the child process.
func prepareProcessGroup(cmd *exec.Cmd) {}

// interruptProcess asks a process to shut down gracefully via SIGINT.
func interruptProcess(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Signal(os.Interrupt)
}
