package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// cmdAttach connects the current terminal to a context, like 'screen -r'.
// Local keystrokes are forwarded to the context's input; the context's
// stdout is written to os.Stdout. The pty (if any) handles echo and
// terminal control. Detach with Ctrl-Q (or --detach-key). The local
// terminal enters the alternate screen buffer so the user's scrollback
// is preserved on detach.
func cmdAttach(args []string) int {
	fs := newFlagSet("ish attach")
	uid := fs.String("uid", "", "context uid or name (required)")
	detachKey := fs.String("detach-key", "\x11", "byte that detaches (default Ctrl-Q = 0x11); use \\xNN hex escape")
	offsetFollow := fs.Bool("from-start", false, "stream from offset 0 instead of only new output")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *uid == "" {
		fmt.Fprintln(os.Stderr, "ish attach: --uid is required")
		return 2
	}

	detachByte, err := parseDetachKey(*detachKey)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ish attach:", err)
		return 2
	}

	c, err := dialDaemon()
	if err != nil {
		fmt.Fprintln(os.Stderr, "ish:", err)
		return 1
	}

	// Sync the context's pty size to the local terminal. No-op for
	// pipe-mode contexts.
	if cols, rows, ok := termSize(os.Stdin); ok {
		_, _ = c.call(&Request{Cmd: "resize", UID: *uid, Cols: cols, Rows: rows})
	}

	// Raw mode so keystrokes (Ctrl-C, arrow keys, etc.) are forwarded
	// byte-for-byte rather than interpreted by the local shell.
	oldState, err := makeRaw(os.Stdin)
	isTty := err == nil
	if !isTty {
		fmt.Fprintln(os.Stderr, "ish: stdin not a tty, attaching in line mode")
	}

	// Enter the alternate screen buffer, hide the cursor, and clear the
	// screen so the pty's first output lands on a clean canvas. Restored
	// on detach.
	if isTty {
		fmt.Fprint(os.Stdout, "\033[?1049h\033[?25l\033[2J\033[H")
	}

	stopCh := make(chan struct{})
	var once sync.Once
	stop := func() { once.Do(func() { close(stopCh) }) }

	// Forward local stdin -> context input. The detach byte detaches
	// rather than being sent to the child.
	go func() {
		buf := make([]byte, 256)
		for {
			n, err := os.Stdin.Read(buf)
			if err != nil {
				stop()
				return
			}
			for i := 0; i < n; i++ {
				if buf[i] == detachByte {
					stop()
					return
				}
			}
			if n > 0 {
				if err := sendInput(c, *uid, buf[:n], true); err != nil {
					stop()
					return
				}
			}
		}
	}()

	// -1 means "current": only stream new output. --from-start reads
	// from the beginning.
	startOffset := int64(-1)
	if *offsetFollow {
		startOffset = 0
	}

	// In raw mode SIGINT is still delivered as a signal; intercept it
	// so the user detaches cleanly. Ctrl-C inside the pty is forwarded
	// as a byte by the stdin goroutine (raw mode disables ISIG).
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		stop()
	}()

	// Exit-status poller: print a notice and detach when the child exits.
	exitMsg := ""
	go func() {
		t := time.NewTicker(500 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				if info, ok := getContextInfo(c, *uid); ok && info.Exited {
					exitMsg = "ish: context exited, code=" + strconv.Itoa(info.ExitCode)
					stop()
					return
				}
			case <-stopCh:
				return
			}
		}
	}()

	streamOutput(c, *uid, startOffset, stopCh, os.Stdout)

	// Exit the alternate screen, restore the terminal, then print the
	// notice. Order matters: the notice must land on the user's main
	// screen, not inside the alt buffer being torn down.
	if isTty {
		fmt.Fprint(os.Stdout, "\033[?25h\033[?1049l")
		restoreTerm(os.Stdin, oldState)
	}
	if exitMsg != "" {
		fmt.Fprintln(os.Stderr, exitMsg)
	}
	fmt.Fprintln(os.Stderr, "ish: detached")
	return 0
}

// sendInput sends bytes to the context's input. With noNewline=true the
// bytes are forwarded verbatim (used by attach).
func sendInput(c *Client, uid string, data []byte, noNewline bool) error {
	resp, err := c.call(&Request{
		Cmd:       "input",
		UID:       uid,
		Data:      data,
		NoNewline: noNewline,
	})
	if err != nil {
		return err
	}
	return errFromResp(resp)
}

func getContextInfo(c *Client, uid string) (ContextInfo, bool) {
	resp, err := c.call(&Request{Cmd: "info", UID: uid})
	if err != nil {
		return ContextInfo{}, false
	}
	if errFromResp(resp) != nil {
		return ContextInfo{}, false
	}
	var info ContextInfo
	if err := json.Unmarshal(resp.Data, &info); err != nil {
		return ContextInfo{}, false
	}
	return info, true
}

// streamOutput reads the context's stdout via the streaming protocol
// and writes it to w until stopCh closes or the stream ends (exit/end
// frame). A reader goroutine pumps frames into a channel; the main
// loop selects between stopCh (user detached) and the channel.
func streamOutput(c *Client, uid string, startOffset int64, stopCh chan struct{}, w io.Writer) {
	conn, err := dialIPC(c.addr)
	if err != nil {
		return
	}
	defer conn.Close()

	req := &Request{
		Cmd:     "stream",
		UID:     uid,
		Stream:  "stdout",
		Offset:  startOffset,
		Follow:  true,
		Timeout: 0, // follow forever
	}
	if err := writeFrame(conn, req); err != nil {
		return
	}

	type frameOrErr struct {
		resp *Response
		err  error
	}
	ch := make(chan frameOrErr, 8)
	go func() {
		for {
			var resp Response
			if err := readFrame(conn, &resp); err != nil {
				ch <- frameOrErr{err: err}
				return
			}
			ch <- frameOrErr{resp: &resp}
			if resp.Type == "exit" || resp.Type == "end" {
				return
			}
		}
	}()

	for {
		select {
		case <-stopCh:
			return
		case fe := <-ch:
			if fe.err != nil {
				return
			}
			if len(fe.resp.Bytes) > 0 {
				w.Write(fe.resp.Bytes)
			}
			if fe.resp.Type == "exit" || fe.resp.Type == "end" {
				return
			}
		}
	}
}

// parseDetachKey accepts a single literal char, a \xNN hex escape (e.g.
// "\x11" for Ctrl-Q, "\x1d" for Ctrl-]), or C-x notation.
func parseDetachKey(s string) (byte, error) {
	if s == "" {
		return 0, fmt.Errorf("empty detach key")
	}
	if strings.HasPrefix(s, `\x`) && len(s) == 4 {
		n, err := strconv.ParseUint(s[2:], 16, 8)
		if err != nil {
			return 0, fmt.Errorf("invalid hex escape %q: %v", s, err)
		}
		return byte(n), nil
	}
	if len(s) == 1 {
		return s[0], nil
	}
	if strings.HasPrefix(s, "C-") && len(s) == 3 {
		c := s[2]
		if c >= 'a' && c <= 'z' {
			c -= 96 // 'a' (97) -> 1 (Ctrl-A)
			return c, nil
		}
		if c >= '@' && c <= '_' {
			return c - '@', nil
		}
	}
	return 0, fmt.Errorf("invalid detach key %q (use a single char, \\xNN, or C-x)", s)
}
