//go:build !windows

package main

import (
	"net"
	"os"
	"path/filepath"
	"time"
)

// listenIPC creates a Unix Domain Socket inside the state directory and
// returns a listener. The socket file is chmod 0600 so only the owning
// user can connect. The returned addr is the socket file path.
func listenIPC(stateDirPath string) (net.Listener, string, error) {
	sockPath := filepath.Join(stateDirPath, "daemon.sock")
	// Remove a stale socket from a previous crash.
	_ = os.Remove(sockPath)
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		return nil, "", err
	}
	if err := os.Chmod(sockPath, 0o600); err != nil {
		ln.Close()
		return nil, "", err
	}
	return ln, sockPath, nil
}

// dialIPC dials the daemon's Unix Domain Socket at addr (a file path).
func dialIPC(addr string) (net.Conn, error) {
	return net.Dial("unix", addr)
}

// dialIPCTimeout dials with a deadline. Used by probe() so a stale
// socket doesn't block the auto-start loop.
func dialIPCTimeout(addr string, timeout time.Duration) (net.Conn, error) {
	return net.DialTimeout("unix", addr, timeout)
}

// cleanupIPC removes the socket file on daemon shutdown.
func cleanupIPC(addr string) {
	_ = os.Remove(addr)
}
