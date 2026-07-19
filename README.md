# ish — Interactive Shell Helper

[简体中文](README.zh-CN.md) | English

`ish` manages multiple background shell contexts for AI-agent-driven
penetration testing. Each context is a long-running child process
(e.g. `nc -lvvp 4444`, `ssh host`, `evil-winrm ...`, a sudo prompt) whose
stdin/stdout/stderr the daemon buffers. A single-shot AI client can then
feed input to and read output from these contexts across separate
invocations — letting an agent drive interactive shells one command at a
time.

The daemon listens on a **Unix Domain Socket** (Linux/macOS) or a
**Named Pipe** (Windows), restricted to the local user.

---

## Why

When an AI agent automates a pentest, it typically executes one shell
command at a time. The moment a reverse shell arrives (`nc -lvvp 4444`
returns), or `ssh`/`evil-winrm` is started, the agent loses the ability
to interact with that session — it can only fire one-shot commands.

`ish` solves this by parking each session in a daemon-managed context.
The agent starts a context, sends input, reads output, and continues —
all from short-lived CLI calls. A human can also `attach` to any context
like `screen -r` for hands-on work.

---

## Build

Requires Go 1.22+.

```bash
# Current platform
go build -o bin/ish .

# Linux (cross-compile, CGO-free)
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o bin/ish-linux-amd64 .

# Windows (cross-compile)
GOOS=windows GOARCH=amd64 go build -o bin/ish.exe .

# macOS - build on a macOS host. Cross-compile from Linux/Windows is
# not supported because term_unix.go uses Linux termios constants
# (TCGETS/TCSETS); macOS needs TIOCGETA/TIOCSETA.
GOOS=darwin GOARCH=amd64 go build -o bin/ish-darwin-amd64 .
```

The binary is self-contained: the same executable is both the client
and the daemon (the daemon auto-starts on first use, or run
`ish daemon` explicitly).

### Dependencies

| Platform | PTY backend | IPC backend |
|----------|-------------|-------------|
| Linux / macOS | `github.com/creack/pty` | Unix Domain Socket |
| Windows | ConPTY via `golang.org/x/sys/windows` (Win10 1809+) | Named Pipe (`Microsoft/go-winio`) |

No third-party ConPTY wrapper is used on Windows; the code calls
`CreatePseudoConsole` / `STARTUPINFOEX` / `PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE`
directly.

---

## Quick start

```bash
# 1. Start a reverse-shell listener; capture its uid (printed to stdout)
ish new --name revshell -- nc -lvvp 4444
# -> 9a3f1c2d8e7b6a50

# 2. Once a shell connects back, send commands
ish input --uid revshell "id; whoami; uname -a"

# 3. Read new output (stream for up to 5s)
ish stdout --uid revshell --offset 0 --follow --timeout 5

# 4. Attach interactively when you want hands-on (detach with Ctrl-Q)
ish attach --uid revshell

# 5. Tear down
ish kill --uid revshell
ish rm   --uid revshell
```

---

## Commands

| Command | Purpose |
|---------|---------|
| `new`     | Start a new background context |
| `ls`      | List all contexts |
| `info`    | Show details of one context |
| `stdout`  | Read stdout (raw bytes + offset metadata) |
| `stderr`  | Read stderr (in pty mode, mirrors stdout) |
| `input`   | Send bytes to a context's stdin |
| `attach`  | Attach a terminal interactively (like `screen -r`) |
| `wait`    | Block until a context exits (or timeout) |
| `kill`    | Kill a context's process tree |
| `rm`      | Remove a context (kills if still running) |
| `clean`   | Remove exited contexts (`--all` for running ones too) |
| `daemon`  | Start the daemon explicitly |
| `shutdown`| Stop the daemon |
| `version` | Print version |
| `help`    | Show full help |

Run `ish help` or `ish <command> -h` for the authoritative flag list.

---

## PTY mode vs pipe mode

| Mode | Flag | Use when |
|------|------|----------|
| **Pipe** (default) | `--no-pty` | Reverse shells (`nc`), `evil-winrm`, anything that doesn't need a TTY. stdout and stderr are separate streams. |
| **PTY** | `--pty` | `sudo` (password prompt), `ssh -t`, `mysql -p`, full-screen TUIs (`vim`, `top`, `nmap -iL -`). stdout and stderr share the pty stream. |

PTY mode uses `creack/pty` on Unix and **ConPTY** on Windows
(Win10 1809+). The daemon reads the pty master directly with
`ReadFile`/`WriteFile` rather than routing through Go's netpoll, so
ConPTY output is delivered reliably.

### Resizing

The daemon supports a `resize` command with `{"cols": N, "rows": N}`.
`ish attach` calls it automatically on entry to sync the pty size with
the local terminal.

---

## Attach

`ish attach --uid <id>` connects the current terminal to a context:

- Local keystrokes are forwarded byte-for-byte (Ctrl-C, arrow keys,
  etc. are sent to the child, not interpreted locally).
- Context output is streamed to the local terminal via the length-
  prefixed JSON streaming protocol (continuous `data` frames until
  `exit`/`end`).
- Detach with **Ctrl-Q** (configurable via `--detach-key`, accepts a
  literal char, `\xNN` hex, or `C-x` notation).
- The local terminal is put into raw mode and the **alternate screen
  buffer** (DECSET 1049) is entered on attach, so the user's terminal
  history is preserved. Both are restored on detach.
- The pty is resized to match the local terminal on attach.
- A 500ms exit-status poller prints a notice when the child exits.

Best with `--pty` contexts: pipe-mode contexts won't echo keystrokes
or honour terminal control codes.

> **Limitation:** attaching to an already-running pty session may show
> misaligned output — `ish` buffers raw pty bytes and doesn't track the
> child's cursor state, so escape sequences from the child assume a
> cursor position that no longer matches the local terminal. For a
> clean attach, attach immediately after `ish new --pty`. AI agents
> should prefer `ish input` + `ish stdout --follow` over `attach`.

---

## Architecture

```
+----------------+        IPC (UDS / Named Pipe)        +------------------+
|  ish client    | <----------------------------------> |  ish daemon      |
|  (one-shot)    |                                      |  (long-lived)    |
+----------------+                                      |                  |
                                                       |  Context A       |
                                                       |   - cmd.Start    |
                                                       |   - RingBuffer x2|
                                                       |   - pty master   |
                                                       |  Context B       |
                                                       |   ...            |
                                                       +------------------+
```

- **Daemon**: long-lived, detached, auto-spawned on first call.
  Owns the `Manager` (set of contexts) and accepts length-prefixed
  JSON frames over the IPC channel.
- **Client**: thin, one-shot. Each `ish <cmd>` invocation dials the
  daemon, writes one `Request` frame, reads one or more `Response`
  frames, prints the result, exits.
- **Context**: a child process plus two ring buffers (stdout, stderr)
  with monotonic offsets. Clients read from an offset and receive only
  bytes written after that offset; `--follow` streams `data` frames
  until an `exit`/`end` frame.
- **State directory**: `~/.ish` by default (override with
  `ISH_STATE_DIR`). Contains:
  - `daemon.addr` — IPC address (UDS path or pipe name)
  - `daemon.pid`  — daemon PID
  - `daemon.log`  — daemon log (set `ISH_PTY_DEBUG=1` for pty traces)

### IPC

| Platform | Channel | Permissions |
|----------|---------|-------------|
| Linux / macOS | `~/.ish/daemon.sock` (UDS) | `0600`, owner-only |
| Windows | `\\.\pipe\ish-<sanitized-path>` (Named Pipe) | SDDL `D:P(A;;FA;;;OW)(A;;FA;;;BA)` — owner + admins full |

### Wire protocol

Every connection is one request -> one or more responses. Frames are
4-byte little-endian length prefix + JSON payload.

```
client -> daemon:  Request  {cmd, uid, ...}
daemon -> client:  Response {status, type, bytes, offset, ...}   (one or more)
```

| `cmd`     | Request fields                                            | Response                                                                  |
|-----------|-----------------------------------------------------------|---------------------------------------------------------------------------|
| `health`  | —                                                         | `{status:200, data:{ok:true, pid:N}}`                                     |
| `list`    | —                                                         | `{status:200, data:[ContextInfo,...]}`                                    |
| `create`  | `name, exec, args, cwd, env, pty, cols, rows`             | `{status:200, data:ContextInfo}`                                          |
| `info`    | `uid`                                                     | `{status:200, data:ContextInfo}`                                          |
| `delete`  | `uid`                                                     | `{status:200, data:{ok:true}}`                                            |
| `input`   | `uid, data, no_newline`                                   | `{status:200, data:{ok:true, bytes:N}}`                                   |
| `kill`    | `uid`                                                     | `{status:200, data:{ok:true}}`                                            |
| `resize`  | `uid, cols, rows`                                         | `{status:200, data:{ok:true}}`                                            |
| `stream`  | `uid, stream(stdout\|stderr), offset, follow, timeout`    | single `{status:200, bytes, offset, exited}` if `!follow`; else multiple `data` frames then `end`/`exit` |
| `wait`    | `uid, timeout`                                            | `{status:200, data:{exited:true, exitCode:N}}` or `{status:202, data:{exited:false}}` |
| `shutdown`| —                                                         | `{status:200, data:{ok:true}}`                                            |

`offset=-1` means "current" (skip existing data, only stream new
output). `timeout=0` in `stream`/`wait` means "forever".

---

## Environment variables

| Variable | Default | Purpose |
|----------|---------|---------|
| `ISH_STATE_DIR` | `~/.ish` | Daemon state directory (addr/pid/log files + UDS socket) |
| `ISH_PTY_DEBUG` | unset | Any non-empty value enables verbose pty logging in `daemon.log` |

---

## Exit codes

| Code | Meaning |
|------|---------|
| 0 | success |
| 1 | runtime error (daemon unreachable, context not found, child spawn failed, ...) |
| 2 | usage error (missing required flags, unknown command) |

---

## Examples

### Reverse shell handler

```bash
ish new --name revshell -- nc -lvvp 4444
# ... victim connects ...
ish input --uid revshell "id; whoami; uname -a"
ish stdout --uid revshell --offset 0 --follow --timeout 5
```

### sudo with password (needs pty)

```bash
ish new --name sudo --pty -- sudo -k ls /root
ish input --uid sudo "mySecretPassword"
ish stdout --uid sudo --follow --timeout 3
```

### Interactive ssh (needs pty)

```bash
ish new --name ssh1 --pty -- ssh -t user@10.0.0.5
ish input --uid ssh1 "ls -la /tmp"
ish stdout --uid ssh1 --follow --timeout 3
# ... or attach a human ...
ish attach --uid ssh1
# (detach with Ctrl-Q)
```

### evil-winrm-style runner (pipe mode)

```bash
ish new --name winrm -- evil-winrm -i 10.0.0.5 -u myuser -p 'myPass'
ish input --uid winrm "whoami /priv"
ish stdout --uid winrm --follow --timeout 3
```

### Pipe a binary payload

```bash
cat payload.bin | ish input --uid revshell --stdin --no-newline
```

### Long-running listener with multiple readers

```bash
# Agent A reads from offset 0
ish stdout --uid revshell --offset 0 --follow --timeout 30 > out_a.txt

# Later, agent B reads only what's new (use the offset A returned)
ish stdout --uid revshell --offset 12345 --follow --timeout 30 > out_b.txt
```

---

## Project layout

```
.
├── main.go            # CLI entry, usage/help dispatch
├── client.go          # CLI command implementations (frame-based one-shot client)
├── daemon.go          # Daemon: frame protocol, dispatcher, context manager, auto-start
├── context.go         # Context + RingBuffer + Manager
├── attach.go          # 'ish attach' interactive mode (alt screen + streaming)
├── flagutil.go        # flagSet wrapper (repeatable --env)
├── ipc_unix.go        # UDS listen/dial/cleanup (linux/darwin/bsd)
├── ipc_windows.go     # Named Pipe listen/dial/cleanup (windows)
├── pty_unix.go        # creack/pty integration + waitChild
├── pty_windows.go     # ConPTY (CreatePseudoConsole + STARTUPINFOEX)
├── term_unix.go       # termios raw mode + restore + termSize
├── term_windows.go    # console mode raw mode + restore + termSize
├── proc_unix.go       # detachedSysProcAttr, killProcessTree, processExists
├── proc_windows.go    # same for Windows
├── go.mod
└── bin/               # compiled binaries (separate from source)
    ├── ish.exe
    ├── ish-linux-amd64
    └── ish-darwin-amd64
```

Source and compiled binaries are kept in separate directories so a
release tarball can ship only `bin/`.

---

## Notes

- The daemon reaps exited contexts older than 1 hour so memory doesn't
  grow unbounded. Recently-exited contexts stay around long enough for
  clients to read their final output.
- `kill` kills the entire process tree (Unix: `kill -PGID`, Windows:
  `taskkill /T /F`).
- `attach` is best-effort on pipe-mode contexts: keystrokes are
  forwarded but the child won't echo them and terminal control codes
  won't be honoured. Use `--pty` for true interactive sessions.
- ConPTY requires Windows 10 1809+. On older Windows the `--pty` path
  will fail at `CreatePseudoConsole`.
