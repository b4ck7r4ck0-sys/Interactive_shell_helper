//go:build !windows

package main

import (
	"syscall"
)

// detachedSysProcAttr configures the daemon process itself to start in
// its own session so it survives the parent shell's exit.
func detachedSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}

// killProcessTree sends SIGKILL to the whole process group. The daemon
// starts each context's child with setpgid so we can kill the entire
// group (e.g. nc + the reverse shell it spawned).
func killProcessTree(pid int) error {
	// Negative pid = kill the process group.
	pgid, err := syscall.Getpgid(pid)
	if err == nil {
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
	}
	return syscall.Kill(pid, syscall.SIGKILL)
}

// childSysProcAttr puts each spawned context in its own process group
// so killProcessTree can take down the whole tree (e.g. nc + the reverse
// shell it spawned).
func childSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}
