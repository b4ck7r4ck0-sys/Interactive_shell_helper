//go:build windows

package main

import (
	"os/exec"
	"strconv"
	"syscall"
)

// detachedSysProcAttr configures a child process to run fully detached
// from the current console so it survives after the parent (the client)
// exits. Used when auto-spawning the daemon.
func detachedSysProcAttr() *syscall.SysProcAttr {
	const CREATE_NEW_PROCESS_GROUP = 0x00000200
	const DETACHED_PROCESS = 0x00000008
	return &syscall.SysProcAttr{
		CreationFlags: CREATE_NEW_PROCESS_GROUP | DETACHED_PROCESS,
	}
}

// killProcessTree kills the process tree rooted at pid. On Windows we use
// taskkill /T /F to take down the whole tree (e.g. nc + the shell it
// spawned from a reverse shell).
func killProcessTree(pid int) error {
	c := exec.Command("taskkill", "/PID", strconv.Itoa(pid), "/T", "/F")
	c.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x08000000} // CREATE_NO_WINDOW
	return c.Run()
}

// childSysProcAttr is applied to every spawned context process.
// CREATE_NO_WINDOW keeps nc/ssh/etc. from popping up console windows
// when the daemon itself has no visible console.
func childSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: 0x08000000} // CREATE_NO_WINDOW
}
