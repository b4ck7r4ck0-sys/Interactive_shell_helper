//go:build !windows

package main

import (
	"io"
	"os"
	"os/exec"

	"github.com/creack/pty"
)

// startPtyChild starts cmd attached to a new pseudo-terminal. Returns
// the master end (which the caller reads/writes) and the started cmd.
// On Unix, pty.Start already execs the command with the slave as its
// controlling terminal, so we don't call cmd.Start ourselves.
func startPtyChild(cmd *exec.Cmd, cols, rows int) (master io.ReadWriteCloser, err error) {
	if cols <= 0 || rows <= 0 {
		cols, rows = 120, 40
	}
	ws := &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)}
	return pty.StartWithSize(cmd, ws)
}

// resizePty resizes the pty master to the given dimensions.
func resizePty(master io.Writer, cols, rows int) error {
	if f, ok := master.(*os.File); ok {
		return pty.Setsize(f, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
	}
	return nil
}

// waitChild waits for the child process to exit and returns (exitCode, exitErr).
// On Unix, both pipe and pty mode use exec.Cmd.Wait() because pty.Start
// dispatches through cmd.Start internally.
func waitChild(c *Context, usePty bool) (int, string) {
	err := c.cmd.Wait()
	if err == nil {
		return 0, ""
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode(), err.Error()
	}
	return -1, err.Error()
}
