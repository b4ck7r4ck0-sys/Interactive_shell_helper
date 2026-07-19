//go:build windows

package main

import (
	"net"
	"strings"
	"time"

	"github.com/Microsoft/go-winio"
)

// pipeName derives a stable named pipe path from the state directory.
// The pipe name must match between daemon and clients; we hash the
// state dir path so different users get different pipes.
func pipeName(stateDirPath string) string {
	// Named pipe names allow almost any character, but the path component
	// after \\.\pipe\ must not contain backslashes. Replace them.
	safe := strings.ReplaceAll(stateDirPath, "\\", "_")
	safe = strings.ReplaceAll(safe, ":", "_")
	return `\\.\pipe\ish-` + safe
}

// listenIPC creates a Named Pipe server. The SecurityDescriptor SDDL
// string restricts access to the owner (OW) and administrators (BA),
// so other users on the same host cannot connect.
func listenIPC(stateDirPath string) (net.Listener, string, error) {
	name := pipeName(stateDirPath)
	cfg := &winio.PipeConfig{
		SecurityDescriptor: "D:P(A;;FA;;;OW)(A;;FA;;;BA)", // owner + admins full
		InputBufferSize:    65536,
		OutputBufferSize:   65536,
	}
	ln, err := winio.ListenPipe(name, cfg)
	if err != nil {
		return nil, "", err
	}
	return ln, name, nil
}

// dialIPC dials the daemon's Named Pipe at addr (a \\.\pipe\... path).
func dialIPC(addr string) (net.Conn, error) {
	return winio.DialPipe(addr, nil)
}

// dialIPCTimeout dials with a deadline. Used by probe() so a stale
// pipe doesn't block the auto-start loop.
func dialIPCTimeout(addr string, timeout time.Duration) (net.Conn, error) {
	t := timeout
	return winio.DialPipe(addr, &t)
}

// cleanupIPC is a no-op on Windows; named pipes are cleaned up when the
// listening process exits.
func cleanupIPC(addr string) {}
