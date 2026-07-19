package main

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Frame protocol: 4-byte little-endian length prefix + JSON payload.
// Non-streaming commands return a single Response; streaming commands
// (stream with follow) return multiple data frames terminated by an
// end or exit frame.

const maxFrameSize = 16 * 1024 * 1024

// Request is a client->daemon frame.
type Request struct {
	Cmd       string   `json:"cmd"`                  // health|list|create|info|delete|input|kill|resize|stream|wait|shutdown
	UID       string   `json:"uid,omitempty"`        // context uid or name
	Name      string   `json:"name,omitempty"`       // create: human-readable name
	Exec      string   `json:"exec,omitempty"`       // create: executable path
	Args      []string `json:"args,omitempty"`       // create: args
	Cwd       string   `json:"cwd,omitempty"`        // create: working directory
	Env       []string `json:"env,omitempty"`        // create: extra env vars
	Pty       bool     `json:"pty,omitempty"`        // create: use pty
	Cols      int      `json:"cols,omitempty"`       // create/resize: column count
	Rows      int      `json:"rows,omitempty"`       // create/resize: row count
	Offset    int64    `json:"offset,omitempty"`     // stream: starting offset
	Follow    bool     `json:"follow,omitempty"`     // stream: follow mode
	Timeout   int      `json:"timeout,omitempty"`    // stream/wait: seconds; 0 = forever
	Stream    string   `json:"stream,omitempty"`     // stream: "stdout" or "stderr"
	NoNewline bool     `json:"no_newline,omitempty"` // input: forward bytes verbatim
	Data      []byte   `json:"data,omitempty"`       // input: bytes to send
}

// Response is a daemon->client frame. Type distinguishes streaming
// data frames (data) from terminating frames (end, exit).
type Response struct {
	Status int             `json:"status"`           // 200/202/400/404/500
	Error  string          `json:"error,omitempty"`  // status>=400
	Type   string          `json:"type,omitempty"`   // data|end|exit
	Bytes  []byte          `json:"bytes,omitempty"`  // data frame payload
	Offset int64           `json:"offset,omitempty"` // current read offset
	Exited bool            `json:"exited,omitempty"` // child has exited
	Code   int             `json:"code,omitempty"`   // exit frame exit code
	Data   json.RawMessage `json:"data,omitempty"`   // non-streaming JSON payload
}

func writeFrame(w io.Writer, v any) error {
	payload, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if len(payload) > maxFrameSize {
		return fmt.Errorf("frame too large: %d bytes", len(payload))
	}
	var hdr [4]byte
	binary.LittleEndian.PutUint32(hdr[:], uint32(len(payload)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err = w.Write(payload)
	return err
}

func readFrame(r io.Reader, v any) error {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return err
	}
	n := binary.LittleEndian.Uint32(hdr[:])
	if n == 0 {
		return io.ErrUnexpectedEOF
	}
	if n > maxFrameSize {
		return fmt.Errorf("frame too large: %d bytes", n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return err
	}
	return json.Unmarshal(buf, v)
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// Daemon owns the listener and context manager.
type Daemon struct {
	addr     string
	listener net.Listener
	manager  *Manager
	logFile  *os.File
	stopCh   chan struct{}
	stopped  bool
	mu       sync.Mutex
}

func stateDir() (string, error) {
	if dir := os.Getenv("ISH_STATE_DIR"); dir != "" {
		return dir, os.MkdirAll(dir, 0o700)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".ish")
	return dir, os.MkdirAll(dir, 0o700)
}

func stateFile(name string) string {
	dir, err := stateDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, name)
}

func readAddrFile() (string, error) {
	data, err := os.ReadFile(stateFile("daemon.addr"))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

func writeAddrFile(addr string) error {
	return os.WriteFile(stateFile("daemon.addr"), []byte(addr), 0o600)
}

// NewDaemon creates the IPC listener (UDS on Unix, Named Pipe on
// Windows) and returns a new daemon.
func NewDaemon() (*Daemon, error) {
	dir, err := stateDir()
	if err != nil {
		return nil, err
	}
	logPath := filepath.Join(dir, "daemon.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	daemonLogFile = logFile

	ln, addr, err := listenIPC(dir)
	if err != nil {
		logFile.Close()
		return nil, err
	}

	d := &Daemon{
		addr:     addr,
		listener: ln,
		manager:  NewManager(),
		logFile:  logFile,
		stopCh:   make(chan struct{}),
	}
	return d, nil
}

func (d *Daemon) Printf(format string, args ...any) {
	ts := time.Now().Format("2006-01-02 15:04:05")
	fmt.Fprintf(d.logFile, "[%s] "+format, append([]any{ts}, args...)...)
}

func (d *Daemon) Serve() error {
	if err := writeAddrFile(d.addr); err != nil {
		return err
	}
	if pid := os.Getpid(); pid > 0 {
		_ = os.WriteFile(stateFile("daemon.pid"), []byte(strconv.Itoa(pid)), 0o600)
	}
	d.Printf("daemon started on %s pid=%d\n", d.addr, os.Getpid())

	go d.reaper()

	for {
		conn, err := d.listener.Accept()
		if err != nil {
			d.mu.Lock()
			stopped := d.stopped
			d.mu.Unlock()
			cleanupIPC(d.addr)
			if stopped {
				return nil
			}
			return err
		}
		go d.handleConn(conn)
	}
}

// reaper drops contexts that exited more than an hour ago.
func (d *Daemon) reaper() {
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	for range t.C {
		cutoff := time.Now().Add(-1 * time.Hour)
		for _, info := range d.manager.List() {
			if info.Exited && info.Created.Before(cutoff) {
				d.manager.Delete(info.UID)
				d.Printf("reaped exited context %s (%s)\n", info.UID, info.Name)
			}
		}
	}
}

// handleConn reads one Request frame and dispatches it.
func (d *Daemon) handleConn(conn net.Conn) {
	defer conn.Close()
	var req Request
	if err := readFrame(conn, &req); err != nil {
		return
	}
	switch req.Cmd {
	case "health":
		d.reply(conn, &Response{Status: 200, Data: mustJSON(map[string]any{"ok": true, "pid": os.Getpid()})})
	case "list":
		d.reply(conn, &Response{Status: 200, Data: mustJSON(d.manager.List())})
	case "create":
		d.handleCreate(conn, &req)
	case "info":
		d.handleInfo(conn, &req)
	case "delete":
		d.handleDelete(conn, &req)
	case "input":
		d.handleInput(conn, &req)
	case "kill":
		d.handleKill(conn, &req)
	case "resize":
		d.handleResize(conn, &req)
	case "stream":
		d.handleStream(conn, &req)
	case "wait":
		d.handleWait(conn, &req)
	case "shutdown":
		d.handleShutdown(conn, &req)
	default:
		d.reply(conn, &Response{Status: 400, Error: "unknown command: " + req.Cmd})
	}
}

func (d *Daemon) reply(w io.Writer, resp *Response) error {
	return writeFrame(w, resp)
}

func (d *Daemon) handleCreate(conn net.Conn, req *Request) {
	if req.Exec == "" {
		d.reply(conn, &Response{Status: 400, Error: "exec is required"})
		return
	}
	ctx, err := d.manager.Create(req.Name, req.Exec, req.Args, req.Cwd, req.Env, req.Pty, req.Cols, req.Rows)
	if err != nil {
		d.Printf("create %q failed: %v\n", req.Exec, err)
		d.reply(conn, &Response{Status: 500, Error: err.Error()})
		return
	}
	info := ctx.Info()
	mode := "pipe"
	if info.Pty {
		mode = "pty"
	}
	d.Printf("created context uid=%s name=%s pid=%d mode=%s cmd=%s\n", info.UID, info.Name, info.PID, mode, info.Cmd)
	d.reply(conn, &Response{Status: 200, Data: mustJSON(info)})
}

func (d *Daemon) handleInfo(conn net.Conn, req *Request) {
	ctx, ok := d.manager.Resolve(req.UID)
	if !ok {
		d.reply(conn, &Response{Status: 404, Error: "context not found"})
		return
	}
	d.reply(conn, &Response{Status: 200, Data: mustJSON(ctx.Info())})
}

func (d *Daemon) handleDelete(conn net.Conn, req *Request) {
	ctx, ok := d.manager.Resolve(req.UID)
	if !ok {
		d.reply(conn, &Response{Status: 404, Error: "context not found"})
		return
	}
	if !ctx.IsExited() {
		_ = killProcessTree(ctx.Info().PID)
	}
	d.manager.Delete(ctx.Info().UID)
	d.reply(conn, &Response{Status: 200, Data: mustJSON(map[string]bool{"ok": true})})
}

func (d *Daemon) handleInput(conn net.Conn, req *Request) {
	ctx, ok := d.manager.Resolve(req.UID)
	if !ok {
		d.reply(conn, &Response{Status: 404, Error: "context not found"})
		return
	}
	data := req.Data
	if !req.NoNewline {
		data = append(data, '\n')
	}
	if err := ctx.WriteInput(data); err != nil {
		d.reply(conn, &Response{Status: 500, Error: err.Error()})
		return
	}
	d.reply(conn, &Response{Status: 200, Data: mustJSON(map[string]any{"ok": true, "bytes": len(data)})})
}

func (d *Daemon) handleKill(conn net.Conn, req *Request) {
	ctx, ok := d.manager.Resolve(req.UID)
	if !ok {
		d.reply(conn, &Response{Status: 404, Error: "context not found"})
		return
	}
	if err := killProcessTree(ctx.Info().PID); err != nil {
		d.reply(conn, &Response{Status: 500, Error: err.Error()})
		return
	}
	d.reply(conn, &Response{Status: 200, Data: mustJSON(map[string]bool{"ok": true})})
}

func (d *Daemon) handleResize(conn net.Conn, req *Request) {
	ctx, ok := d.manager.Resolve(req.UID)
	if !ok {
		d.reply(conn, &Response{Status: 404, Error: "context not found"})
		return
	}
	if req.Cols <= 0 || req.Rows <= 0 {
		d.reply(conn, &Response{Status: 400, Error: "cols and rows must be positive"})
		return
	}
	if err := ctx.ResizePty(req.Cols, req.Rows); err != nil {
		d.reply(conn, &Response{Status: 500, Error: err.Error()})
		return
	}
	d.reply(conn, &Response{Status: 200, Data: mustJSON(map[string]bool{"ok": true})})
}

// handleStream serves stdout/stderr reads. With Follow=true, streams
// data frames until timeout or child exit.
func (d *Daemon) handleStream(conn net.Conn, req *Request) {
	ctx, ok := d.manager.Resolve(req.UID)
	if !ok {
		d.reply(conn, &Response{Status: 404, Error: "context not found"})
		return
	}
	var rb *RingBuffer
	if req.Stream == "stderr" {
		rb = ctx.Stderr()
	} else {
		rb = ctx.Stdout()
	}

	offset := req.Offset
	if offset < 0 {
		offset = rb.Size()
	}

	if !req.Follow {
		data, newOffset := rb.Read(offset)
		d.reply(conn, &Response{
			Status: 200,
			Bytes:  data,
			Offset: newOffset,
			Exited: ctx.IsExited(),
		})
		return
	}

	var deadline time.Time
	if req.Timeout > 0 {
		deadline = time.Now().Add(time.Duration(req.Timeout) * time.Second)
	}
	for {
		var wait time.Duration
		if deadline.IsZero() {
			wait = 24 * time.Hour
		} else {
			wait = time.Until(deadline)
			if wait <= 0 {
				d.reply(conn, &Response{Type: "end", Offset: offset, Exited: ctx.IsExited()})
				return
			}
		}
		if !rb.Wait(offset, wait) && !deadline.IsZero() {
			d.reply(conn, &Response{Type: "end", Offset: offset, Exited: ctx.IsExited()})
			return
		}
		data, newOffset := rb.Read(offset)
		if len(data) > 0 {
			if err := d.reply(conn, &Response{
				Type:   "data",
				Bytes:  data,
				Offset: newOffset,
				Exited: ctx.IsExited(),
			}); err != nil {
				return
			}
			offset = newOffset
		}
		if ctx.IsExited() {
			info := ctx.Info()
			d.reply(conn, &Response{Type: "exit", Offset: offset, Exited: true, Code: info.ExitCode})
			return
		}
	}
}

func (d *Daemon) handleWait(conn net.Conn, req *Request) {
	ctx, ok := d.manager.Resolve(req.UID)
	if !ok {
		d.reply(conn, &Response{Status: 404, Error: "context not found"})
		return
	}
	var timeout time.Duration
	if req.Timeout > 0 {
		timeout = time.Duration(req.Timeout) * time.Second
	}
	code, exited := ctx.WaitExit(timeout)
	if !exited {
		d.reply(conn, &Response{Status: 202, Data: mustJSON(map[string]any{"exited": false})})
		return
	}
	d.reply(conn, &Response{Status: 200, Data: mustJSON(map[string]any{"exited": true, "exitCode": code})})
}

func (d *Daemon) handleShutdown(conn net.Conn, req *Request) {
	d.reply(conn, &Response{Status: 200, Data: mustJSON(map[string]bool{"ok": true})})
	go func() {
		time.Sleep(100 * time.Millisecond)
		d.mu.Lock()
		d.stopped = true
		d.mu.Unlock()
		close(d.stopCh)
		_ = d.listener.Close()
	}()
}

var startMu sync.Mutex

// ensureDaemon connects to a running daemon, starting one if needed.
func ensureDaemon() error {
	startMu.Lock()
	defer startMu.Unlock()

	if addr, err := readAddrFile(); err == nil {
		if probe(addr) {
			return nil
		}
	}
	if err := startDaemonProcess(); err != nil {
		return fmt.Errorf("failed to start daemon: %w", err)
	}
	for i := 0; i < 50; i++ {
		time.Sleep(100 * time.Millisecond)
		addr, err := readAddrFile()
		if err != nil {
			continue
		}
		if probe(addr) {
			return nil
		}
	}
	return errors.New("daemon did not become reachable")
}

// probe sends a health request to verify the daemon is reachable.
func probe(addr string) bool {
	conn, err := dialIPCTimeout(addr, 500*time.Millisecond)
	if err != nil {
		return false
	}
	defer conn.Close()
	if err := writeFrame(conn, &Request{Cmd: "health"}); err != nil {
		return false
	}
	var resp Response
	if err := readFrame(conn, &resp); err != nil {
		return false
	}
	return resp.Status == 200
}

func startDaemonProcess() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, "daemon")
	if attr := detachedSysProcAttr(); attr != nil {
		cmd.SysProcAttr = attr
	}
	return cmd.Start()
}

func runDaemon() int {
	if os.Getenv("ISH_PTY_DEBUG") != "" {
		debugPTY = true
	}
	d, err := NewDaemon()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ish: cannot start daemon: %v\n", err)
		return 1
	}
	if err := d.Serve(); err != nil {
		fmt.Fprintf(os.Stderr, "ish: daemon exited: %v\n", err)
		return 1
	}
	return 0
}
