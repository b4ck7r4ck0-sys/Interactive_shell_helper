package main

import (
	"fmt"
	"os"
)

const usage = `ish - Interactive Shell Helper

Manage multiple background shell contexts for AI agents. Each context is a
long-running child process (e.g. 'nc -lvvp 4444', 'ssh host', 'evil-winrm ...')
whose stdin/stdout/stderr the daemon buffers. A single-shot AI client can then
feed input to and read output from these contexts across separate invocations.

The daemon auto-starts on first use. It listens on a Unix Domain Socket
(Linux/macOS) or a Named Pipe (Windows), restricted to the local user.

USAGE:
  ish <command> [flags] [-- CMD ARGS...]
  ish <command> -h        # show flags for one command
  ish help                # show this full help

COMMANDS:
  new       Start a new background context
              ish new [--name NAME] [--cwd PATH] [--env K=V]...
                      [--pty [--cols N --rows N] | --no-pty]
                      -- CMD [ARGS...]
              stdout: the new context's uid
              stderr: a human-readable summary line
              --pty runs the child in a pseudo-terminal, enabling sudo/ssh
              password prompts and full-screen TUIs. stdout and stderr then
              share the same pty stream. Default is --no-pty (plain pipes).

  ls        List all contexts
              ish ls [--json]

  info      Show details of one context
              ish info --uid <uid|name> [--json]

  stdout    Read stdout of a context
              ish stdout --uid <uid|name> [--offset N] [--follow]
                        [--timeout S] [--json]
              stdout: raw bytes (binary-safe)
              stderr: JSON line {offset, bytes, exited}

  stderr    Read stderr of a context (same flags as stdout)
              In --pty mode stderr mirrors stdout.

  input     Send a line to a context's stdin
              ish input --uid <uid|name> [--no-newline] [--stdin] "cmd..."
              Positional args are joined with spaces; a newline is appended
              unless --no-newline. With --no-newline bytes are forwarded
              verbatim (used by 'attach').

  attach    Attach to a context interactively (like 'screen -r')
              ish attach --uid <uid|name> [--detach-key \x11] [--from-start]
              Local keystrokes are forwarded to the context; output is
              streamed back. Detach with Ctrl-Q (or custom --detach-key).
              SIGINT and Ctrl-C are forwarded to the child, not interpreted
              locally. Best with --pty contexts.

  wait      Block until a context exits (or timeout)
              ish wait --uid <uid|name> [--timeout S] [--json]
              --timeout 0 means wait forever.

  kill      Kill a context's process tree
              ish kill --uid <uid|name>

  rm        Remove a context from the list (kills if still running)
              ish rm --uid <uid|name>

  clean     Remove exited contexts (--all to remove running ones too)
              ish clean [--all]

  daemon    Start the daemon explicitly (normally auto-started)
  shutdown  Stop the daemon
  version   Print version
  help      Show this help

FLAGS (per command - run 'ish <cmd> -h' for the authoritative list):
  --uid <id>        Context uid or name (most commands)
  --name <s>        Human-readable name for 'new'
  --cwd <path>      Working directory for 'new'
  --env K=V         Extra env var for 'new' (repeatable)
  --pty             Run child in a pseudo-terminal
  --no-pty          Force pipe mode (default)
  --cols N          Pty column count (default 120, with --pty)
  --rows N          Pty row count (default 40, with --pty)
  --offset N        Start reading at this byte offset
  --follow          Long-poll until new data arrives
  --timeout S       Follow/wait timeout in seconds
  --no-newline      Forward input bytes verbatim
  --stdin           Read input from stdin instead of args
  --detach-key <k>  Attach detach byte: literal, \xNN, or C-x (default \x11)
  --from-start      Stream from offset 0 instead of only new output
  --json            Machine-readable output
  --all             'clean': also remove running contexts

ENVIRONMENT:
  ISH_STATE_DIR    Override the daemon's state directory (default ~/.ish).
                    Useful for running multiple daemons or non-standard installs.
  ISH_PTY_DEBUG=1  Enable verbose pty logging in the daemon log file.

EXIT CODES:
  0   success
  1   runtime error (daemon unreachable, context not found, ...)
  2   usage error (missing flags, unknown command)

EXAMPLES:
  # Start a reverse-shell listener; capture its uid
  ish new --name revshell -- nc -lvvp 4444

  # Send a command to whatever connected back
  ish input --uid revshell "id; whoami; uname -a"

  # Read everything new since the last read (use --follow to long-poll)
  ish stdout --uid revshell --offset 0 --follow --timeout 5

  # Start an interactive ssh session IN A PTY (so password prompts work)
  ish new --name ssh1 --pty -- ssh -t user@10.0.0.5
  ish input --uid ssh1 "ls -la /tmp"
  ish stdout --uid ssh1 --follow --timeout 3

  # Attach a human to the ssh session: keystrokes + full pty output
  ish attach --uid ssh1
  # (detach with Ctrl-Q)

  # sudo with password prompt - requires --pty
  ish new --name sudo --pty -- sudo -k ls /root
  ish input --uid sudo "mySecretPassword"
  ish stdout --uid sudo --follow --timeout 3

  # Pipe a file into a context
  cat payload.bin | ish input --uid revshell --stdin --no-newline
`

func main() {
	if len(os.Args) < 2 {
		fmt.Print(usage)
		os.Exit(0)
	}
	cmd := os.Args[1]
	args := os.Args[2:]

	var code int
	switch cmd {
	case "new":
		code = cmdNew(args)
	case "ls", "list":
		code = cmdList(args)
	case "info":
		code = cmdInfo(args)
	case "stdout":
		code = cmdStdout(args)
	case "stderr":
		code = cmdStderr(args)
	case "input":
		code = cmdInput(args)
	case "wait":
		code = cmdWait(args)
	case "kill":
		code = cmdKill(args)
	case "attach":
		code = cmdAttach(args)
	case "rm", "remove":
		code = cmdRemove(args)
	case "clean":
		code = cmdClean(args)
	case "daemon":
		code = cmdDaemon(args)
	case "shutdown", "stop":
		code = cmdShutdown(args)
	case "help", "-h", "--help":
		fmt.Print(usage)
		code = 0
	case "version", "-v", "--version":
		fmt.Println("ish 0.1.0")
	default:
		fmt.Fprintf(os.Stderr, "ish: unknown command %q\n", cmd)
		fmt.Fprintln(os.Stderr, "Run 'ish help' for usage.")
		code = 2
	}
	os.Exit(code)
}
