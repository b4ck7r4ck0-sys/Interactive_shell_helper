//go:build !windows

package main

import (
	"os"
	"syscall"
	"unsafe"
)

// winsize matches struct winsize from <termios.h>.
type winsize struct {
	Row    uint16
	Col    uint16
	Xpixel uint16
	Ypixel uint16
}

// termSize returns the terminal's column/row count for the given file
// descriptor. Returns ok=false if f is not a terminal.
func termSize(f *os.File) (cols, rows int, ok bool) {
	var ws winsize
	_, _, errno := syscall.Syscall(
		syscall.SYS_IOCTL,
		f.Fd(),
		uintptr(syscall.TIOCGWINSZ),
		uintptr(unsafe.Pointer(&ws)),
	)
	if errno != 0 {
		return 0, 0, false
	}
	return int(ws.Col), int(ws.Row), true
}

// makeRaw puts the given terminal into raw mode (no echo, no line
// buffering, no signal generation from special chars). Returns the
// previous state to pass to restoreTerm. Returns an error if fd is not
// a terminal.
func makeRaw(f *os.File) (any, error) {
	fd := int(f.Fd())
	var oldState syscall.Termios
	if _, _, err := syscall.Syscall6(syscall.SYS_IOCTL, uintptr(fd), uintptr(syscall.TCGETS), uintptr(unsafe.Pointer(&oldState)), 0, 0, 0); err != 0 {
		return nil, err
	}
	raw := oldState
	// Disable input modes: BRKINT, ICRNL, INPCK, ISTRIP, IXON.
	raw.Iflag &^= syscall.IGNBRK | syscall.BRKINT | syscall.PARMRK | syscall.ISTRIP | syscall.INLCR | syscall.IGNCR | syscall.ICRNL | syscall.IXON
	// Disable output processing.
	raw.Oflag &^= syscall.OPOST
	// Disable echo and canonical mode, set 8-bit chars.
	raw.Lflag &^= syscall.ECHO | syscall.ECHONL | syscall.ICANON | syscall.ISIG | syscall.IEXTEN
	raw.Cflag |= syscall.CS8
	// Minimum input = 1 byte, no inter-byte timer.
	raw.Cc[syscall.VMIN] = 1
	raw.Cc[syscall.VTIME] = 0
	if _, _, err := syscall.Syscall6(syscall.SYS_IOCTL, uintptr(fd), uintptr(syscall.TCSETS), uintptr(unsafe.Pointer(&raw)), 0, 0, 0); err != 0 {
		return nil, err
	}
	return &oldState, nil
}

// restoreTerm restores the previously-saved terminal state.
func restoreTerm(f *os.File, state any) {
	if state == nil {
		return
	}
	tc, ok := state.(*syscall.Termios)
	if !ok {
		return
	}
	fd := int(f.Fd())
	_, _, _ = syscall.Syscall6(syscall.SYS_IOCTL, uintptr(fd), uintptr(syscall.TCSETS), uintptr(unsafe.Pointer(tc)), 0, 0, 0)
}
