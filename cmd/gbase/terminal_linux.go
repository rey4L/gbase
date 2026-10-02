//go:build linux

package main

import (
	"context"
	"errors"
	"io"
	"os"
	"syscall"
	"unsafe"
)

func terminalSettings(fd uintptr) (syscall.Termios, error) {
	var settings syscall.Termios
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TCGETS, uintptr(unsafe.Pointer(&settings)))
	if errno != 0 {
		return settings, errno
	}
	return settings, nil
}

func setTerminalSettings(fd uintptr, settings *syscall.Termios) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TCSETS, uintptr(unsafe.Pointer(settings)))
	if errno != 0 {
		return errno
	}
	return nil
}

func attachTerminal(ctx context.Context, input io.Reader, output io.Writer) (*lineEditor, func() error, error) {
	in, ok := input.(*os.File)
	if !ok {
		return nil, nil, nil
	}
	out, ok := output.(*os.File)
	if !ok {
		return nil, nil, nil
	}
	original, err := terminalSettings(in.Fd())
	if errors.Is(err, syscall.ENOTTY) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	if _, err = terminalSettings(out.Fd()); errors.Is(err, syscall.ENOTTY) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}

	// Cbreak mode delivers keystrokes without the kernel's line editing or echo.
	// Keep signal handling and output newline translation; the editor supplies echo.
	edited := original
	edited.Lflag &^= syscall.ICANON | syscall.ECHO | syscall.ECHONL
	edited.Lflag |= syscall.ISIG
	edited.Iflag &^= syscall.IXON
	// Timed reads allow cancellation without closing the caller-owned stdin file.
	// syscall.Read is used because os.File.Read converts a zero-byte timeout to EOF.
	edited.Cc[syscall.VMIN] = 0
	edited.Cc[syscall.VTIME] = 1
	if err = setTerminalSettings(in.Fd(), &edited); err != nil {
		return nil, nil, err
	}
	editor := newLineEditor(ctx, terminalInput{ctx: ctx, fd: int(in.Fd())}, output, func() int { return terminalColumns(out.Fd()) })
	restore := func() error { return setTerminalSettings(in.Fd(), &original) }
	return editor, restore, nil
}

type terminalInput struct {
	ctx context.Context
	fd  int
}

func (in terminalInput) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for {
		if err := in.ctx.Err(); err != nil {
			return 0, err
		}
		n, err := syscall.Read(in.fd, p)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if n > 0 || err != nil {
			return n, err
		}
	}
}

func terminalColumns(fd uintptr) int {
	var size struct{ rows, columns, xpixel, ypixel uint16 }
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TIOCGWINSZ, uintptr(unsafe.Pointer(&size)))
	if errno != 0 || size.columns == 0 {
		return 80
	}
	return int(size.columns)
}
