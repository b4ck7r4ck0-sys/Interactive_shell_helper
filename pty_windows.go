//go:build windows

package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync/atomic"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ConPTY on Windows is implemented via the kernel32 pseudoconsole API
// (CreatePseudoConsole / ClosePseudoConsole) plus a STARTUPINFOEX that
// attributes the child process to the pseudoconsole.
//
// We use golang.org/x/sys/windows directly (no third-party dep). This
// requires Windows 10 1809+ (ConPTY was added there).

const (
	procThreadAttributePseudoConsole = 0x00020016
	extendedStartupinfoPresent      = 0x00080000
)

// conptyMaster is the io.ReadWriteCloser over a ConPTY's read/write pipe.
type conptyMaster struct {
	hpc        windows.Handle // pseudoconsole handle
	pipeIn     windows.Handle // daemon writes here -> child stdin
	pipeOut    windows.Handle // child stdout -> daemon reads here
	waitHandle atomic.Uintptr // child process handle for waitForProcess
}

// Read calls ReadFile directly so we get blocking semantics on the
// raw pipe handle. os.NewFile would route through Go's netpoll which
// doesn't wake up for ConPTY writes.
func (m *conptyMaster) Read(b []byte) (int, error) {
	var n uint32
	err := windows.ReadFile(m.pipeOut, b, &n, nil)
	if err != nil {
		return 0, err
	}
	if n == 0 {
		return 0, io.EOF
	}
	return int(n), nil
}

// Write calls WriteFile directly on the input pipe handle.
func (m *conptyMaster) Write(b []byte) (int, error) {
	var n uint32
	err := windows.WriteFile(m.pipeIn, b, &n, nil)
	if err != nil {
		return 0, err
	}
	return int(n), nil
}

func (m *conptyMaster) Close() error {
	// Closing the pseudoconsole signals EOF to the child.
	windows.ClosePseudoConsole(m.hpc)
	windows.CloseHandle(m.pipeIn)
	windows.CloseHandle(m.pipeOut)
	return nil
}

// startPtyChild starts cmd attached to a new Windows ConPTY. The returned
// master is what the caller reads/writes.
func startPtyChild(cmd *exec.Cmd, cols, rows int) (io.ReadWriteCloser, error) {
	if cols <= 0 || rows <= 0 {
		cols, rows = 120, 40
	}

	// Two pairs of anonymous pipes. CreatePseudoConsole takes the read
	// end of the input pipe (hInput - data the pty reads from) and the
	// write end of the output pipe (hOutput - data the pty writes to).
	// We use windows.CreatePipe directly (not os.Pipe) so the handles
	// stay in blocking mode - Go's runtime would otherwise mark them
	// non-blocking for netpoll, which ConPTY writes can't handle.
	// Pipes must be created inheritable; non-inheritable handles
	// result in ERROR_INVALID_HANDLE from CreatePseudoConsole.
	sa := &windows.SecurityAttributes{
		Length:             uint32(unsafe.Sizeof(windows.SecurityAttributes{})),
		InheritHandle:      1,
		SecurityDescriptor: nil,
	}
	var inRead, inWrite windows.Handle
	if err := windows.CreatePipe(&inRead, &inWrite, sa, 0); err != nil {
		return nil, fmt.Errorf("create input pipe: %w", err)
	}
	var outRead, outWrite windows.Handle
	if err := windows.CreatePipe(&outRead, &outWrite, sa, 0); err != nil {
		windows.CloseHandle(inRead)
		windows.CloseHandle(inWrite)
		return nil, fmt.Errorf("create output pipe: %w", err)
	}

	var hpc windows.Handle
	if err := windows.CreatePseudoConsole(
		windows.Coord{X: int16(cols), Y: int16(rows)},
		inRead,
		outWrite,
		0,
		&hpc,
	); err != nil {
		windows.CloseHandle(inRead)
		windows.CloseHandle(inWrite)
		windows.CloseHandle(outRead)
		windows.CloseHandle(outWrite)
		return nil, fmt.Errorf("CreatePseudoConsole: %w", err)
	}
	// The child's ends are now owned by the pty; close ours.
	windows.CloseHandle(inRead)
	windows.CloseHandle(outWrite)

	// Build the proc thread attribute list with the pseudoconsole handle.
	attrList, err := windows.NewProcThreadAttributeList(1)
	if err != nil {
		windows.ClosePseudoConsole(hpc)
		windows.CloseHandle(inWrite)
		windows.CloseHandle(outRead)
		return nil, fmt.Errorf("NewProcThreadAttributeList: %w", err)
	}
	defer attrList.Delete()
	if err := attrList.Update(
		procThreadAttributePseudoConsole,
		unsafe.Pointer(&hpc),
		unsafe.Sizeof(hpc),
	); err != nil {
		windows.ClosePseudoConsole(hpc)
		windows.CloseHandle(inWrite)
		windows.CloseHandle(outRead)
		return nil, fmt.Errorf("UpdateProcThreadAttribute: %w", err)
	}

	// STARTUPINFOEX: StartupInfo followed by the attribute list pointer.
	var si windows.StartupInfoEx
	si.StartupInfo.Cb = uint32(unsafe.Sizeof(si))
	si.StartupInfo.Flags = windows.STARTF_USESTDHANDLES | extendedStartupinfoPresent
	si.ProcThreadAttributeList = attrList.List()

	// Compose the command line as a single UTF16 string.
	cmdLine := composeWindowsCommandLine(cmd.Path, cmd.Args[1:])
	cmdLinePtr, _ := windows.UTF16PtrFromString(cmdLine)

	var cwdPtr *uint16
	if cmd.Dir != "" {
		cwdPtr, _ = windows.UTF16PtrFromString(cmd.Dir)
	}

	// Build the environment block if cmd.Env is set; otherwise inherit.
	var envBlock *uint16
	if len(cmd.Env) > 0 {
		envBlock = buildEnvBlock(cmd.Env)
	}

	var pi windows.ProcessInformation
	err = windows.CreateProcess(
		nil, cmdLinePtr, nil, nil, false,
		windows.CREATE_UNICODE_ENVIRONMENT|windows.CREATE_NO_WINDOW,
		envBlock, cwdPtr, &si.StartupInfo, &pi,
	)
	if err != nil {
		windows.ClosePseudoConsole(hpc)
		windows.CloseHandle(inWrite)
		windows.CloseHandle(outRead)
		return nil, fmt.Errorf("CreateProcess: %w", err)
	}
	windows.CloseHandle(pi.Thread)
	// Keep pi.Process so we can wait for the child to exit; store it on
	// the master so exec.Cmd.Wait (which uses os.FindProcess and would
	// fail) isn't needed. The daemon waits via waitChild below.
	proc, err := os.FindProcess(int(pi.ProcessId))
	if err != nil {
		windows.CloseHandle(pi.Process)
		windows.ClosePseudoConsole(hpc)
		windows.CloseHandle(inWrite)
		windows.CloseHandle(outRead)
		return nil, err
	}
	cmd.Process = proc
	cmd.ProcessState = nil

	m := &conptyMaster{
		hpc:     hpc,
		pipeIn:  inWrite,
		pipeOut: outRead,
	}
	m.waitHandle.Store(uintptr(pi.Process)) // kept for waitChild
	return m, nil
}

// waitChild waits for the child process to exit and returns (exitCode, exitErr).
// Pipe mode uses exec.Cmd.Wait(); pty (ConPTY) mode uses WaitForSingleObject
// because we hand-rolled CreateProcess and os.FindProcess().Wait() doesn't
// hold a real process handle.
func waitChild(c *Context, usePty bool) (int, string) {
	if !usePty {
		err := c.cmd.Wait()
		if err == nil {
			return 0, ""
		}
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode(), err.Error()
		}
		return -1, err.Error()
	}
	m, ok := c.master.(*conptyMaster)
	if !ok {
		return -1, "not a conpty master"
	}
	h := m.waitHandle.Load()
	if h == 0 {
		return -1, "no process handle"
	}
	defer windows.CloseHandle(windows.Handle(h))
	ev, err := windows.WaitForSingleObject(windows.Handle(h), windows.INFINITE)
	if err != nil {
		return -1, err.Error()
	}
	if ev != windows.WAIT_OBJECT_0 {
		return -1, fmt.Sprintf("WaitForSingleObject returned 0x%x", ev)
	}
	var code uint32
	if err := windows.GetExitCodeProcess(windows.Handle(h), &code); err != nil {
		return -1, err.Error()
	}
	return int(code), ""
}

// buildEnvBlock turns a list of "KEY=VAL" strings into a Windows-style
// environment block (UTF16, NUL-separated, double-NUL-terminated).
func buildEnvBlock(env []string) *uint16 {
	var buf []uint16
	for _, kv := range env {
		u := windows.StringToUTF16(kv)
		buf = append(buf, u...)
	}
	buf = append(buf, 0) // terminating double-NUL
	return &buf[0]
}

// composeWindowsCommandLine quotes args following the same rules as
// syscall.EscapeArg. The first element is the program path.
func composeWindowsCommandLine(exe string, args []string) string {
	var b []byte
	b = appendWindowsArg(b, exe)
	for _, a := range args {
		b = append(b, ' ')
		b = appendWindowsArg(b, a)
	}
	return string(b)
}

func appendWindowsArg(b []byte, arg string) []byte {
	if arg == "" {
		return append(b, '"', '"')
	}
	if !needsQuoting(arg) {
		return append(b, arg...)
	}
	b = append(b, '"')
	for i := 0; i < len(arg); i++ {
		c := arg[i]
		switch c {
		case '\\':
			j := i
			for j < len(arg) && arg[j] == '\\' {
				j++
			}
			n := j - i
			if j == len(arg) {
				for k := 0; k < n*2; k++ {
					b = append(b, '\\')
				}
			} else if arg[j] == '"' {
				for k := 0; k < n*2; k++ {
					b = append(b, '\\')
				}
				b = append(b, '"')
			} else {
				for k := 0; k < n; k++ {
					b = append(b, '\\')
				}
			}
			i = j - 1
		case '"':
			b = append(b, '\\', '"')
		default:
			b = append(b, c)
		}
	}
	b = append(b, '"')
	return b
}

func needsQuoting(s string) bool {
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case ' ', '\t', '"', '\\':
			return true
		}
	}
	return false
}

// resizePty resizes the ConPTY.
func resizePty(master io.Writer, cols, rows int) error {
	m, ok := master.(*conptyMaster)
	if !ok {
		return nil
	}
	return windows.ResizePseudoConsole(m.hpc,
		windows.Coord{X: int16(cols), Y: int16(rows)})
}
