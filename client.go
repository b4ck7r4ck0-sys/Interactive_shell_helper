package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

// Client holds the daemon's IPC address.
type Client struct {
	addr string
}

func dialDaemon() (*Client, error) {
	if err := ensureDaemon(); err != nil {
		return nil, err
	}
	addr, err := readAddrFile()
	if err != nil {
		return nil, err
	}
	return &Client{addr: addr}, nil
}

// call sends a Request and reads a single Response.
func (c *Client) call(req *Request) (*Response, error) {
	conn, err := dialIPC(c.addr)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if err := writeFrame(conn, req); err != nil {
		return nil, err
	}
	var resp Response
	if err := readFrame(conn, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// callStream sends a Request and reads Response frames until cb returns
// false, a terminating frame (end/exit) arrives, or a read fails.
func (c *Client) callStream(req *Request, cb func(*Response) bool) error {
	conn, err := dialIPC(c.addr)
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := writeFrame(conn, req); err != nil {
		return err
	}
	for {
		var resp Response
		if err := readFrame(conn, &resp); err != nil {
			return err
		}
		if !cb(&resp) {
			return nil
		}
		if resp.Type == "exit" || resp.Type == "end" {
			return nil
		}
	}
}

// errFromResp converts a status>=400 Response to an error.
func errFromResp(r *Response) error {
	if r == nil {
		return fmt.Errorf("nil response")
	}
	if r.Status >= 400 {
		if r.Error != "" {
			return fmt.Errorf("%s", r.Error)
		}
		return fmt.Errorf("status %d", r.Status)
	}
	return nil
}

// ---------- CLI command implementations ----------

func cmdNew(args []string) int {
	fs := newFlagSet("ish new")
	name := fs.String("name", "", "human-readable name for the context")
	cwd := fs.String("cwd", "", "working directory")
	jsonOut := fs.Bool("json", false, "output full info as JSON")
	envs := fs.multiString("env", "additional env var (KEY=VAL, repeatable)")
	usePty := fs.Bool("pty", false, "run the child in a pseudo-terminal (enables sudo/ssh password prompts, TUIs)")
	noPty := fs.Bool("no-pty", false, "explicitly disable pty (default)")
	cols := fs.Int("cols", 120, "pty column count (only with --pty)")
	rows := fs.Int("rows", 40, "pty row count (only with --pty)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	rest := fs.Args()
	if len(rest) == 0 {
		fmt.Fprintln(os.Stderr, "ish new: missing command. Usage: ish new [flags] -- CMD [ARGS...]")
		return 2
	}
	cmdName := rest[0]
	cmdArgs := rest[1:]

	ptyMode := *usePty && !*noPty
	c, err := dialDaemon()
	if err != nil {
		fmt.Fprintln(os.Stderr, "ish:", err)
		return 1
	}
	req := &Request{
		Cmd:  "create",
		Name: *name,
		Exec: cmdName,
		Args: cmdArgs,
		Cwd:  *cwd,
		Env:  *envs,
		Pty:  ptyMode,
		Cols: *cols,
		Rows: *rows,
	}
	resp, err := c.call(req)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ish:", err)
		return 1
	}
	if err := errFromResp(resp); err != nil {
		fmt.Fprintln(os.Stderr, "ish:", err)
		return 1
	}
	var info ContextInfo
	if err := json.Unmarshal(resp.Data, &info); err != nil {
		fmt.Fprintln(os.Stderr, "ish: bad response:", err)
		return 1
	}
	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(info)
		return 0
	}
	// Script-friendly: stdout gets only the uid (easy to capture),
	// stderr gets the human-readable summary.
	fmt.Fprintln(os.Stdout, info.UID)
	mode := "pipe"
	if info.Pty {
		mode = "pty"
	}
	fmt.Fprintf(os.Stderr, "created context name=%s uid=%s pid=%d mode=%s cmd=%s\n", info.Name, info.UID, info.PID, mode, info.Cmd)
	return 0
}

func cmdList(args []string) int {
	fs := newFlagSet("ish ls")
	jsonOut := fs.Bool("json", false, "output JSON array")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	c, err := dialDaemon()
	if err != nil {
		fmt.Fprintln(os.Stderr, "ish:", err)
		return 1
	}
	resp, err := c.call(&Request{Cmd: "list"})
	if err != nil {
		fmt.Fprintln(os.Stderr, "ish:", err)
		return 1
	}
	if err := errFromResp(resp); err != nil {
		fmt.Fprintln(os.Stderr, "ish:", err)
		return 1
	}
	var list []ContextInfo
	if err := json.Unmarshal(resp.Data, &list); err != nil {
		fmt.Fprintln(os.Stderr, "ish: bad response:", err)
		return 1
	}
	if *jsonOut {
		_ = json.NewEncoder(os.Stdout).Encode(list)
		return 0
	}
	if len(list) == 0 {
		fmt.Println("(no contexts)")
		return 0
	}
	fmt.Printf("%-16s %-16s %-8s %-8s %-20s %s\n", "UID", "NAME", "PID", "STATUS", "CREATED", "CMD")
	for _, ci := range list {
		status := "running"
		if ci.Exited {
			status = "exit=" + strconv.Itoa(ci.ExitCode)
		}
		fmt.Printf("%-16s %-16s %-8d %-8s %-20s %s\n",
			ci.UID, ci.Name, ci.PID, status, ci.Created.Format("2006-01-02 15:04:05"), ci.Cmd)
	}
	return 0
}

func cmdInfo(args []string) int {
	fs := newFlagSet("ish info")
	uid := fs.String("uid", "", "context uid or name (required)")
	jsonOut := fs.Bool("json", false, "output JSON")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *uid == "" {
		fmt.Fprintln(os.Stderr, "ish info: --uid is required")
		return 2
	}
	c, err := dialDaemon()
	if err != nil {
		fmt.Fprintln(os.Stderr, "ish:", err)
		return 1
	}
	resp, err := c.call(&Request{Cmd: "info", UID: *uid})
	if err != nil {
		fmt.Fprintln(os.Stderr, "ish:", err)
		return 1
	}
	if err := errFromResp(resp); err != nil {
		fmt.Fprintln(os.Stderr, "ish:", err)
		return 1
	}
	var info ContextInfo
	if err := json.Unmarshal(resp.Data, &info); err != nil {
		fmt.Fprintln(os.Stderr, "ish: bad response:", err)
		return 1
	}
	if *jsonOut {
		_ = json.NewEncoder(os.Stdout).Encode(info)
		return 0
	}
	fmt.Printf("uid:      %s\n", info.UID)
	fmt.Printf("name:     %s\n", info.Name)
	fmt.Printf("pid:      %d\n", info.PID)
	fmt.Printf("cmd:      %s %s\n", info.Cmd, strings.Join(info.Args, " "))
	if info.Cwd != "" {
		fmt.Printf("cwd:      %s\n", info.Cwd)
	}
	fmt.Printf("created:  %s\n", info.Created.Format(time.RFC3339))
	fmt.Printf("status:   ")
	if info.Exited {
		fmt.Printf("exited (code=%d)\n", info.ExitCode)
		if info.ExitErr != "" {
			fmt.Printf("exitErr:  %s\n", info.ExitErr)
		}
	} else {
		fmt.Println("running")
	}
	fmt.Printf("stdout:   %d bytes\n", info.StdoutLen)
	fmt.Printf("stderr:   %d bytes\n", info.StderrLen)
	return 0
}

func cmdStdout(args []string) int { return cmdStream("stdout", args) }
func cmdStderr(args []string) int { return cmdStream("stderr", args) }

func cmdStream(which string, args []string) int {
	fs := newFlagSet("ish " + which)
	uid := fs.String("uid", "", "context uid or name (required)")
	offset := fs.Int64("offset", 0, "only return bytes after this offset")
	follow := fs.Bool("follow", false, "long-poll until new data arrives")
	timeout := fs.Int("timeout", 30, "follow timeout in seconds (0 = forever)")
	jsonOut := fs.Bool("json", false, "output JSON with metadata + data as string")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *uid == "" {
		fmt.Fprintln(os.Stderr, "ish "+which+": --uid is required")
		return 2
	}
	c, err := dialDaemon()
	if err != nil {
		fmt.Fprintln(os.Stderr, "ish:", err)
		return 1
	}
	req := &Request{
		Cmd:     "stream",
		UID:     *uid,
		Stream:  which,
		Offset:  *offset,
		Follow:  *follow,
		Timeout: *timeout,
	}

	if !*follow {
		// Single-shot read: one response frame.
		resp, err := c.call(req)
		if err != nil {
			fmt.Fprintln(os.Stderr, "ish:", err)
			return 1
		}
		if err := errFromResp(resp); err != nil {
			fmt.Fprintln(os.Stderr, "ish:", err)
			return 1
		}
		emitStreamResult(resp, *jsonOut)
		return 0
	}

	// Follow mode: read frames continuously until end/exit.
	var lastOffset int64 = *offset
	var exited bool
	var exitCode int
	var totalBytes int
	err = c.callStream(req, func(r *Response) bool {
		if r.Status >= 400 {
			fmt.Fprintf(os.Stderr, "ish: stream error: %s\n", r.Error)
			return false
		}
		if len(r.Bytes) > 0 {
			os.Stdout.Write(r.Bytes)
			totalBytes += len(r.Bytes)
		}
		if r.Offset > 0 {
			lastOffset = r.Offset
		}
		if r.Exited {
			exited = true
		}
		if r.Type == "exit" {
			exitCode = r.Code
		}
		return true
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "ish:", err)
		return 1
	}
	if *jsonOut {
		_ = json.NewEncoder(os.Stderr).Encode(map[string]any{
			"offset":  lastOffset,
			"bytes":   totalBytes,
			"exited":  exited,
			"exitCode": exitCode,
		})
	} else {
		fmt.Fprintf(os.Stderr, "{\"offset\":%d,\"bytes\":%d,\"exited\":%t}\n", lastOffset, totalBytes, exited)
	}
	return 0
}

func emitStreamResult(r *Response, jsonOut bool) {
	if jsonOut {
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{
			"offset": r.Offset,
			"exited": r.Exited,
			"bytes":  len(r.Bytes),
			"data":   string(r.Bytes),
		})
		return
	}
	if len(r.Bytes) > 0 {
		os.Stdout.Write(r.Bytes)
	}
	fmt.Fprintf(os.Stderr, "{\"offset\":%d,\"bytes\":%d,\"exited\":%t}\n", r.Offset, len(r.Bytes), r.Exited)
}

func cmdInput(args []string) int {
	fs := newFlagSet("ish input")
	uid := fs.String("uid", "", "context uid or name (required)")
	noNewline := fs.Bool("no-newline", false, "do not append a newline")
	stdin := fs.Bool("stdin", false, "read input from stdin instead of positional arg")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *uid == "" {
		fmt.Fprintln(os.Stderr, "ish input: --uid is required")
		return 2
	}
	var data []byte
	var err error
	if *stdin {
		data, err = io.ReadAll(os.Stdin)
		if err != nil {
			fmt.Fprintln(os.Stderr, "ish:", err)
			return 1
		}
	} else {
		rest := fs.Args()
		if len(rest) == 0 {
			fmt.Fprintln(os.Stderr, "ish input: provide a string argument or --stdin")
			return 2
		}
		// Join with spaces so the AI can pass "whoami; id; uname -a"
		// without quoting pain.
		data = []byte(strings.Join(rest, " "))
	}

	c, err := dialDaemon()
	if err != nil {
		fmt.Fprintln(os.Stderr, "ish:", err)
		return 1
	}
	resp, err := c.call(&Request{
		Cmd:       "input",
		UID:       *uid,
		Data:      data,
		NoNewline: *noNewline,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "ish:", err)
		return 1
	}
	if err := errFromResp(resp); err != nil {
		fmt.Fprintln(os.Stderr, "ish:", err)
		return 1
	}
	fmt.Println("ok")
	return 0
}

func cmdKill(args []string) int {
	fs := newFlagSet("ish kill")
	uid := fs.String("uid", "", "context uid or name (required)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *uid == "" {
		fmt.Fprintln(os.Stderr, "ish kill: --uid is required")
		return 2
	}
	c, err := dialDaemon()
	if err != nil {
		fmt.Fprintln(os.Stderr, "ish:", err)
		return 1
	}
	resp, err := c.call(&Request{Cmd: "kill", UID: *uid})
	if err != nil {
		fmt.Fprintln(os.Stderr, "ish:", err)
		return 1
	}
	if err := errFromResp(resp); err != nil {
		fmt.Fprintln(os.Stderr, "ish:", err)
		return 1
	}
	fmt.Println("ok")
	return 0
}

func cmdRemove(args []string) int {
	fs := newFlagSet("ish rm")
	uid := fs.String("uid", "", "context uid or name (required)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *uid == "" {
		fmt.Fprintln(os.Stderr, "ish rm: --uid is required")
		return 2
	}
	c, err := dialDaemon()
	if err != nil {
		fmt.Fprintln(os.Stderr, "ish:", err)
		return 1
	}
	resp, err := c.call(&Request{Cmd: "delete", UID: *uid})
	if err != nil {
		fmt.Fprintln(os.Stderr, "ish:", err)
		return 1
	}
	if err := errFromResp(resp); err != nil {
		fmt.Fprintln(os.Stderr, "ish:", err)
		return 1
	}
	fmt.Println("ok")
	return 0
}

func cmdClean(args []string) int {
	fs := newFlagSet("ish clean")
	all := fs.Bool("all", false, "remove all contexts including running ones")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	c, err := dialDaemon()
	if err != nil {
		fmt.Fprintln(os.Stderr, "ish:", err)
		return 1
	}
	resp, err := c.call(&Request{Cmd: "list"})
	if err != nil {
		fmt.Fprintln(os.Stderr, "ish:", err)
		return 1
	}
	if err := errFromResp(resp); err != nil {
		fmt.Fprintln(os.Stderr, "ish:", err)
		return 1
	}
	var list []ContextInfo
	if err := json.Unmarshal(resp.Data, &list); err != nil {
		fmt.Fprintln(os.Stderr, "ish: bad response:", err)
		return 1
	}
	removed := 0
	for _, ci := range list {
		if !ci.Exited && !*all {
			continue
		}
		if *all && !ci.Exited {
			_, _ = c.call(&Request{Cmd: "kill", UID: ci.UID})
		}
		if r, err := c.call(&Request{Cmd: "delete", UID: ci.UID}); err == nil && r.Status < 400 {
			removed++
		}
	}
	fmt.Printf("removed %d contexts\n", removed)
	return 0
}

func cmdWait(args []string) int {
	fs := newFlagSet("ish wait")
	uid := fs.String("uid", "", "context uid or name (required)")
	timeout := fs.Int("timeout", 0, "seconds to wait (0 = forever)")
	jsonOut := fs.Bool("json", false, "output JSON")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *uid == "" {
		fmt.Fprintln(os.Stderr, "ish wait: --uid is required")
		return 2
	}
	c, err := dialDaemon()
	if err != nil {
		fmt.Fprintln(os.Stderr, "ish:", err)
		return 1
	}
	resp, err := c.call(&Request{Cmd: "wait", UID: *uid, Timeout: *timeout})
	if err != nil {
		fmt.Fprintln(os.Stderr, "ish:", err)
		return 1
	}
	if err := errFromResp(resp); err != nil && resp.Status != 202 {
		fmt.Fprintln(os.Stderr, "ish:", err)
		return 1
	}
	var out map[string]any
	_ = json.Unmarshal(resp.Data, &out)
	if *jsonOut {
		_ = json.NewEncoder(os.Stdout).Encode(out)
		return 0
	}
	if resp.Status == 202 {
		fmt.Println("timeout")
		return 0
	}
	if exited, _ := out["exited"].(bool); exited {
		code, _ := out["exitCode"].(float64)
		fmt.Printf("exited code=%d\n", int(code))
		return 0
	}
	fmt.Println("timeout")
	return 0
}

func cmdDaemon(args []string) int {
	// Explicit daemon start. Useful for debugging or service setup.
	return runDaemon()
}

func cmdShutdown(args []string) int {
	c, err := dialDaemon()
	if err != nil {
		fmt.Fprintln(os.Stderr, "ish:", err)
		return 1
	}
	_, _ = c.call(&Request{Cmd: "shutdown"})
	fmt.Println("daemon stopped")
	return 0
}
