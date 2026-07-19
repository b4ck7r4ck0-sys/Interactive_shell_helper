package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"
)

var errProcessExited = errors.New("process has exited")

// ptyDebugf is a debug logger for pty-related events; writes to the
// daemon log file. No-op when debugPTY is false. Toggle by setting
// ISH_PTY_DEBUG=1 in the daemon environment.
var debugPTY = false
var daemonLogFile *os.File

func ptyDebugf(format string, args ...any) {
	if !debugPTY || daemonLogFile == nil {
		return
	}
	ts := time.Now().Format("15:04:05.000")
	fmt.Fprintf(daemonLogFile, "[%s][pty] "+format+"\n", append([]any{ts}, args...)...)
}

// ptyReadErr stores the last error from the pty read goroutine so the
// 'info' command can surface it for debugging.
var ptyReadErr atomic.Value

// maxBufferSize is the per-stream (stdout/stderr) ring buffer cap.
// Older bytes are dropped once this size is exceeded, but the global
// offset keeps increasing so clients can always request "new data
// since offset N" without ambiguity.
const maxBufferSize = 16 * 1024 * 1024 // 16 MiB per stream

// RingBuffer is a byte buffer that keeps at most maxSize recent bytes
// while monotonically increasing a "total written" offset. Readers use
// the offset to fetch only the data they have not consumed yet.
type RingBuffer struct {
	mu      sync.Mutex
	data    []byte
	maxSize int
	total   int64
	notify  chan struct{} // closed & recreated on every write
}

func NewRingBuffer() *RingBuffer {
	return &RingBuffer{
		maxSize: maxBufferSize,
		notify:  make(chan struct{}),
	}
}

func (r *RingBuffer) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.data = append(r.data, p...)
	if len(r.data) > r.maxSize {
		drop := len(r.data) - r.maxSize
		r.data = r.data[drop:]
	}
	r.total += int64(len(p))
	close(r.notify)
	r.notify = make(chan struct{})
	return len(p), nil
}

// Read returns bytes written after the given offset. If offset is older
// than the available window, it is clamped to the oldest available byte.
// Returns (data, newOffset) where newOffset is the next byte to read.
func (r *RingBuffer) Read(offset int64) ([]byte, int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	oldest := r.total - int64(len(r.data))
	if oldest < 0 {
		oldest = 0
	}
	if offset < oldest {
		offset = oldest
	}
	if offset >= r.total {
		return nil, r.total
	}
	start := offset - oldest
	out := make([]byte, r.total-offset)
	copy(out, r.data[start:])
	return out, r.total
}

// Size returns the current total bytes written.
func (r *RingBuffer) Size() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.total
}

// Wait blocks until new data beyond offset is available or the timeout
// elapses. Returns true if new data is available.
func (r *RingBuffer) Wait(offset int64, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		r.mu.Lock()
		if r.total > offset {
			r.mu.Unlock()
			return true
		}
		ch := r.notify
		r.mu.Unlock()

		remaining := time.Until(deadline)
		if remaining <= 0 {
			return false
		}
		timer := time.NewTimer(remaining)
		select {
		case <-ch:
			timer.Stop()
			continue
		case <-timer.C:
			return false
		}
	}
}

// ContextInfo is the JSON-serialisable snapshot of a context.
type ContextInfo struct {
	UID       string    `json:"uid"`
	Name      string    `json:"name"`
	Cmd       string    `json:"cmd"`
	Args      []string  `json:"args,omitempty"`
	Cwd       string    `json:"cwd,omitempty"`
	Created   time.Time `json:"created"`
	PID       int       `json:"pid"`
	Exited    bool      `json:"exited"`
	ExitCode  int       `json:"exitCode,omitempty"`
	ExitErr   string    `json:"exitErr,omitempty"`
	StdoutLen int64     `json:"stdoutLen"`
	StderrLen int64     `json:"stderrLen"`
	Pty       bool      `json:"pty"`
	Cols      int       `json:"cols,omitempty"`
	Rows      int       `json:"rows,omitempty"`
}

// Context wraps a long-running child process with captured stdio.
type Context struct {
	info   ContextInfo
	mu     sync.Mutex
	cmd    *exec.Cmd
	stdin  io.WriteCloser   // pipe mode: stdin pipe; pty mode: nil
	master io.ReadWriteCloser // pty mode: master end; pipe mode: nil
	stdout *RingBuffer
	stderr *RingBuffer
	done   chan struct{}
}

func (c *Context) Info() ContextInfo {
	c.mu.Lock()
	defer c.mu.Unlock()
	info := c.info
	info.StdoutLen = c.stdout.Size()
	info.StderrLen = c.stderr.Size()
	return info
}

func (c *Context) IsExited() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.info.Exited
}

func (c *Context) Stdout() *RingBuffer { return c.stdout }
func (c *Context) Stderr() *RingBuffer { return c.stderr }

// WriteInput sends bytes to the child process stdin (pipe mode) or to
// the pty master (pty mode).
func (c *Context) WriteInput(p []byte) error {
	c.mu.Lock()
	w := io.Writer(c.stdin)
	if w == nil {
		w = c.master
	}
	c.mu.Unlock()
	if w == nil {
		return errProcessExited
	}
	_, err := w.Write(p)
	return err
}

// WaitExit blocks until the process exits or timeout. Returns (exitCode, ok).
func (c *Context) WaitExit(timeout time.Duration) (int, bool) {
	if timeout <= 0 {
		select {
		case <-c.done:
		default:
			return 0, false
		}
	} else {
		select {
		case <-c.done:
		case <-time.After(timeout):
			return 0, false
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.info.ExitCode, true
}

func (c *Context) markExited(code int, exitErr string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.info.Exited = true
	c.info.ExitCode = code
	c.info.ExitErr = exitErr
	if c.stdin != nil {
		c.stdin.Close()
		c.stdin = nil
	}
	// Don't close c.master here: the pty read goroutine may still be
	// draining output the child wrote before exiting. The goroutine
	// closes master itself when Read returns EOF/error.
}

// Manager holds the set of live contexts.
type Manager struct {
	mu       sync.RWMutex
	contexts map[string]*Context
	byName   map[string]string // name -> uid
}

func NewManager() *Manager {
	return &Manager{
		contexts: make(map[string]*Context),
		byName:   make(map[string]string),
	}
}

func (m *Manager) Get(uid string) (*Context, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	c, ok := m.contexts[uid]
	return c, ok
}

func (m *Manager) GetByName(name string) (*Context, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	uid, ok := m.byName[name]
	if !ok {
		return nil, false
	}
	c, ok := m.contexts[uid]
	return c, ok
}

// Resolve accepts either a uid or a name and returns the context.
func (m *Manager) Resolve(id string) (*Context, bool) {
	if c, ok := m.Get(id); ok {
		return c, true
	}
	return m.GetByName(id)
}

func (m *Manager) List() []ContextInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]ContextInfo, 0, len(m.contexts))
	for _, c := range m.contexts {
		out = append(out, c.Info())
	}
	return out
}

func (m *Manager) Delete(uid string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.contexts[uid]
	if !ok {
		return false
	}
	if c.info.Name != "" {
		delete(m.byName, c.info.Name)
	}
	delete(m.contexts, uid)
	return true
}

// Create spawns a new context. The caller-provided cmdName/args/cwd/env
// are used to exec the child. name is optional; if empty a short random
// uid is also used as the name. The returned uid is always a fresh random
// value so it is unique even if names collide.
//
// If usePty is true, the child is started attached to a pseudo-terminal
// (Unix pty / Windows ConPTY). In pty mode stdout and stderr are merged
// into the single pty stream; both ring buffers receive the same bytes.
func (m *Manager) Create(name, cmdName string, args []string, cwd string, env []string, usePty bool, cols, rows int) (*Context, error) {
	uid := newUID()
	if name == "" {
		name = uid
	}

	cmd := exec.Command(cmdName, args...)
	if cwd != "" {
		cmd.Dir = cwd
	}
	if len(env) > 0 {
		cmd.Env = env
	}
	// In pty mode, creack/pty.StartWithSize takes over SysProcAttr
	// (sets Setsid + Setctty). Don't pre-set anything that could
	// conflict (e.g. Setpgid causes EPERM on WSL2 when combined with
	// Setctty). For pipe mode, set Setpgid so killProcessTree works.
	if !usePty {
		if attr := childSysProcAttr(); attr != nil {
			cmd.SysProcAttr = attr
		}
	}

	ctx := &Context{
		info: ContextInfo{
			UID:     uid,
			Name:    name,
			Cmd:     cmdName,
			Args:    args,
			Cwd:     cwd,
			Created: time.Now(),
			Pty:     usePty,
			Cols:    cols,
			Rows:    rows,
		},
		cmd:    cmd,
		stdout: NewRingBuffer(),
		stderr: NewRingBuffer(),
		done:   make(chan struct{}),
	}

	if usePty {
		master, err := startPtyChild(cmd, cols, rows)
		if err != nil {
			return nil, err
		}
		ctx.master = master
		ctx.info.PID = cmd.Process.Pid

		// pty has a single stream; mirror it into both ring buffers so
		// clients using 'stderr' still get output (it'll equal stdout).
		go func() {
			ptyDebugf("pty read goroutine started for uid=%s", uid)
			buf := make([]byte, 4096)
			for {
				n, err := master.Read(buf)
				ptyDebugf("pty read returned n=%d err=%v", n, err)
				if n > 0 {
					data := make([]byte, n)
					copy(data, buf[:n])
					ctx.stdout.Write(data)
					ctx.stderr.Write(data)
				}
				if err != nil {
					ptyReadErr.Store(err.Error())
					master.Close()
					break
				}
			}
		}()
	} else {
		stdin, err := cmd.StdinPipe()
		if err != nil {
			return nil, err
		}
		stdoutPipe, err := cmd.StdoutPipe()
		if err != nil {
			stdin.Close()
			return nil, err
		}
		stderrPipe, err := cmd.StderrPipe()
		if err != nil {
			stdin.Close()
			stdoutPipe.Close()
			return nil, err
		}

		if err := cmd.Start(); err != nil {
			return nil, err
		}
		ctx.stdin = stdin
		ctx.info.PID = cmd.Process.Pid

		go func() { _, _ = io.Copy(ctx.stdout, stdoutPipe) }()
		go func() { _, _ = io.Copy(ctx.stderr, stderrPipe) }()
	}

	// Wait for process exit, then mark exited.
	go func() {
		exitCode, exitErr := waitChild(ctx, usePty)
		ctx.markExited(exitCode, exitErr)
	}()

	m.mu.Lock()
	m.contexts[uid] = ctx
	m.byName[name] = uid
	m.mu.Unlock()

	return ctx, nil
}

// ResizePty resizes the context's pty. No-op for pipe contexts.
func (c *Context) ResizePty(cols, rows int) error {
	c.mu.Lock()
	master := c.master
	c.mu.Unlock()
	if master == nil {
		return nil
	}
	return resizePty(master, cols, rows)
}

func newUID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
