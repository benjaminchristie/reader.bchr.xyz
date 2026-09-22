//go:build linux || darwin

package main

import (
	"syscall"
	"unsafe"
)

// makeRaw turns off line buffering, echo and signal keys on fd so the chat UI
// can redraw its input line when messages arrive. It returns a restore func,
// or ok=false if fd is not a terminal.
func makeRaw(fd int) (restore func(), ok bool) {
	var old syscall.Termios
	if ioctl(fd, ioctlGetTermios, unsafe.Pointer(&old)) != nil {
		return nil, false
	}
	raw := old
	raw.Lflag &^= syscall.ICANON | syscall.ECHO | syscall.ISIG | syscall.IEXTEN
	raw.Iflag &^= syscall.IXON | syscall.ICRNL
	raw.Cc[syscall.VMIN] = 1
	raw.Cc[syscall.VTIME] = 0
	if ioctl(fd, ioctlSetTermios, unsafe.Pointer(&raw)) != nil {
		return nil, false
	}
	return func() { ioctl(fd, ioctlSetTermios, unsafe.Pointer(&old)) }, true
}

func isTerminal(fd int) bool {
	var t syscall.Termios
	return ioctl(fd, ioctlGetTermios, unsafe.Pointer(&t)) == nil
}

func termWidth(fd int) int {
	var ws struct{ Row, Col, X, Y uint16 }
	if ioctl(fd, syscall.TIOCGWINSZ, unsafe.Pointer(&ws)) != nil || ws.Col == 0 {
		return 80
	}
	return int(ws.Col)
}

func ioctl(fd int, req uintptr, arg unsafe.Pointer) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), req, uintptr(arg))
	if errno != 0 {
		return errno
	}
	return nil
}
