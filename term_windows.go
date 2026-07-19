//go:build windows

package main

import (
	"os"

	"golang.org/x/sys/windows"
)

// termSize returns the console's column/row count for the given file
// descriptor. Returns ok=false if f is not a console.
func termSize(f *os.File) (cols, rows int, ok bool) {
	var info windows.ConsoleScreenBufferInfo
	if err := windows.GetConsoleScreenBufferInfo(windows.Handle(f.Fd()), &info); err != nil {
		return 0, 0, false
	}
	cols = int(info.Window.Right - info.Window.Left + 1)
	rows = int(info.Window.Bottom - info.Window.Top + 1)
	return cols, rows, true
}

// makeRaw puts the Windows console input into raw mode and disables
// processed output so escape sequences pass through to the ConPTY.
// Returns the previous console mode for restoreTerm.
func makeRaw(f *os.File) (any, error) {
	fd := windows.Handle(f.Fd())
	var oldMode uint32
	if err := windows.GetConsoleMode(fd, &oldMode); err != nil {
		return nil, err
	}
	// Input: disable ENABLE_ECHO_INPUT, ENABLE_LINE_INPUT,
	// ENABLE_PROCESSED_INPUT (so Ctrl-C is delivered as a byte, not a
	// signal). Keep ENABLE_VIRTUAL_TERMINAL_INPUT if available so
	// escape sequences (arrow keys etc.) are reported.
	const (
		ENABLE_ECHO_INPUT             = 0x0004
		ENABLE_LINE_INPUT             = 0x0002
		ENABLE_PROCESSED_INPUT        = 0x0001
		ENABLE_VIRTUAL_TERMINAL_INPUT = 0x0200
	)
	newMode := oldMode
	newMode &^= ENABLE_ECHO_INPUT | ENABLE_LINE_INPUT | ENABLE_PROCESSED_INPUT
	newMode |= ENABLE_VIRTUAL_TERMINAL_INPUT
	_ = windows.SetConsoleMode(fd, newMode)

	// Output console: enable virtual terminal processing so ANSI escape
	// codes from the pty are rendered.
	var outMode uint32
	outHandle := windows.Handle(os.Stdout.Fd())
	if err := windows.GetConsoleMode(outHandle, &outMode); err == nil {
		const ENABLE_VIRTUAL_TERMINAL_PROCESSING = 0x0004
		_ = windows.SetConsoleMode(outHandle, outMode|ENABLE_VIRTUAL_TERMINAL_PROCESSING)
	}
	return oldMode, nil
}

// restoreTerm restores the previous console input mode.
func restoreTerm(f *os.File, state any) {
	mode, ok := state.(uint32)
	if !ok {
		return
	}
	fd := windows.Handle(f.Fd())
	_ = windows.SetConsoleMode(fd, mode)
}
