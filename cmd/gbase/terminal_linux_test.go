//go:build linux

package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

func testTerminal(t *testing.T) (*os.File, *os.File) {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { master.Close() })
	var unlock int32
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, master.Fd(), syscall.TIOCSPTLCK, uintptr(unsafe.Pointer(&unlock)))
	if errno != 0 {
		t.Fatal(errno)
	}
	var number uint32
	_, _, errno = syscall.Syscall(syscall.SYS_IOCTL, master.Fd(), syscall.TIOCGPTN, uintptr(unsafe.Pointer(&number)))
	if errno != 0 {
		t.Fatal(errno)
	}
	slave, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", number), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { slave.Close() })
	return master, slave
}

func waitForEditor(t *testing.T, file *os.File) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		settings, err := terminalSettings(file.Fd())
		if err != nil {
			t.Fatal(err)
		}
		if settings.Lflag&syscall.ICANON == 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("editor did not initialize")
}

func TestTerminalShellHistoryAndRestore(t *testing.T) {
	master, slave := testTerminal(t)
	original, err := terminalSettings(slave.Fd())
	if err != nil {
		t.Fatal(err)
	}
	var capture terminalCapture
	readDone := make(chan struct{})
	go func() { defer close(readDone); io.Copy(&capture, master) }()
	done := make(chan int, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	path := filepath.Join(t.TempDir(), "history.db")
	go func() { done <- run(ctx, []string{path}, slave, slave, slave, true) }()
	waitForEditor(t, slave)
	if _, err = io.WriteString(master, "SELECT 42 AS recalled;\r\x1b[A\r.exit\r"); err != nil {
		t.Fatal(err)
	}
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("shell failed: %d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("shell blocked")
	}
	restored, err := terminalSettings(slave.Fd())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(original, restored) {
		t.Fatal("terminal settings not restored after exit")
	}
	slave.Close()
	select {
	case <-readDone:
	case <-time.After(time.Second):
		master.Close()
		<-readDone
	}
	if count := capture.count("\r\n42\r\n"); count != 2 {
		t.Fatalf("query did not execute twice: count=%d output=%q", count, capture.text())
	}
}

func TestTerminalCancellationRestoresSettings(t *testing.T) {
	master, slave := testTerminal(t)
	original, err := terminalSettings(slave.Fd())
	if err != nil {
		t.Fatal(err)
	}
	// Drain redraw output so waiting for input cannot be confused with a full PTY.
	readDone := make(chan struct{})
	go func() { defer close(readDone); io.Copy(io.Discard, master) }()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan int, 1)
	go func() { done <- run(ctx, []string{filepath.Join(t.TempDir(), "cancel.db")}, slave, slave, slave, true) }()
	waitForEditor(t, slave)
	if _, err = io.WriteString(master, "unfinished"); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case code := <-done:
		if code == 0 {
			t.Fatal("canceled shell succeeded")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("terminal read ignored cancellation")
	}
	restored, err := terminalSettings(slave.Fd())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(original, restored) {
		t.Fatal("terminal settings not restored after cancellation")
	}
	slave.Close()
	select {
	case <-readDone:
	case <-time.After(time.Second):
		master.Close()
		<-readDone
	}
}

func TestNonTerminalDoesNotChangeScriptInput(t *testing.T) {
	file, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	editor, restore, err := attachTerminal(context.Background(), file, file)
	if err != nil || editor != nil || restore != nil {
		t.Fatalf("non-terminal attached: %v", err)
	}
}

type terminalCapture struct {
	mu   sync.Mutex
	data []byte
}

func (c *terminalCapture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.data = append(c.data, p...)
	return len(p), nil
}

func (c *terminalCapture) text() string            { c.mu.Lock(); defer c.mu.Unlock(); return string(c.data) }
func (c *terminalCapture) count(needle string) int { return strings.Count(c.text(), needle) }
