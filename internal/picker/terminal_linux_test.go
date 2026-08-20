//go:build linux

package picker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

func TestParseKey(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		want     key
		consumed int
		more     bool
	}{
		{name: "empty", more: true},
		{name: "up", input: "\x1b[A", want: keyUp, consumed: 3},
		{name: "down application mode", input: "\x1bOB", want: keyDown, consumed: 3},
		{name: "right", input: "\x1b[C", want: keyRight, consumed: 3},
		{name: "left", input: "\x1b[D", want: keyLeft, consumed: 3},
		{name: "partial escape", input: "\x1b[", more: true},
		{name: "escape before text", input: "\x1bx", want: keyEscape, consumed: 1},
		{name: "space", input: " ", want: keySpace, consumed: 1},
		{name: "enter", input: "\r", want: keyEnter, consumed: 1},
		{name: "backspace", input: "\x7f", want: keyLeft, consumed: 1},
		{name: "ctrl-c", input: "\x03", want: keyInterrupt, consumed: 1},
		{name: "unknown", input: "x", want: keyUnknown, consumed: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, consumed, more := parseKey([]byte(test.input))
			if got != test.want || consumed != test.consumed || more != test.more {
				t.Fatalf("parseKey(%q) = (%v, %d, %v), want (%v, %d, %v)", test.input, got, consumed, more, test.want, test.consumed, test.more)
			}
		})
	}
}

func TestRunRestoresRealTermiosOnCancellation(t *testing.T) {
	master, slave := openPTY(t)
	defer master.Close()
	defer slave.Close()

	before, err := readTermios(slave.Fd())
	if err != nil {
		t.Fatalf("read original termios: %v", err)
	}
	source := &fakeSource{entries: map[string][]Entry{"": nil}}
	done := make(chan error, 1)
	go func() {
		_, err := Run(context.Background(), source, Options{Input: slave, Output: slave, Title: "test"})
		done <- err
	}()

	waitForRawMode(t, slave, before)
	if _, err := master.Write([]byte{'q'}); err != nil {
		t.Fatalf("write cancel key: %v", err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, ErrCanceled) {
			t.Fatalf("Run error = %v, want ErrCanceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return")
	}

	after, err := readTermios(slave.Fd())
	if err != nil {
		t.Fatalf("read restored termios: %v", err)
	}
	if after != before {
		t.Fatalf("termios was not restored\nbefore: %#v\nafter:  %#v", before, after)
	}
}

func TestRunRestoresRealTermiosOnContextCancellation(t *testing.T) {
	master, slave := openPTY(t)
	defer master.Close()
	defer slave.Close()

	before, err := readTermios(slave.Fd())
	if err != nil {
		t.Fatalf("read original termios: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	source := &fakeSource{entries: map[string][]Entry{"": nil}}
	done := make(chan error, 1)
	go func() {
		_, err := Run(ctx, source, Options{Input: slave, Output: slave})
		done <- err
	}()

	waitForRawMode(t, slave, before)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run error = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return")
	}

	after, err := readTermios(slave.Fd())
	if err != nil {
		t.Fatalf("read restored termios: %v", err)
	}
	if after != before {
		t.Fatalf("termios was not restored after context cancellation")
	}
}

func TestRunRestoresRealTermiosOnSignal(t *testing.T) {
	master, slave := openPTY(t)
	defer master.Close()
	defer slave.Close()

	before, err := readTermios(slave.Fd())
	if err != nil {
		t.Fatalf("read original termios: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := Run(context.Background(), &fakeSource{}, Options{Input: slave, Output: slave})
		done <- err
	}()

	waitForRawMode(t, slave, before)
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatalf("send SIGTERM: %v", err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run error = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after SIGTERM")
	}

	after, err := readTermios(slave.Fd())
	if err != nil {
		t.Fatalf("read restored termios: %v", err)
	}
	if after != before {
		t.Fatalf("termios was not restored after signal")
	}
}

func TestRunRejectsNonTTY(t *testing.T) {
	input, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	output, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()

	source := &fakeSource{}
	_, err = Run(context.Background(), source, Options{Input: input, Output: output})
	if !errors.Is(err, ErrNotTerminal) {
		t.Fatalf("Run error = %v, want ErrNotTerminal", err)
	}
	if calls := source.listed(); len(calls) != 0 {
		t.Fatalf("source called before TTY validation: %v", calls)
	}
}

func waitForRawMode(t *testing.T, slave *os.File, original syscall.Termios) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		current, err := readTermios(slave.Fd())
		if err != nil {
			t.Fatalf("read termios while waiting for raw mode: %v", err)
		}
		if current != original {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("terminal did not enter raw mode")
}

func openPTY(t *testing.T) (*os.File, *os.File) {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("open /dev/ptmx: %v", err)
	}

	locked := int32(0)
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, master.Fd(), syscall.TIOCSPTLCK, uintptr(unsafe.Pointer(&locked))); errno != 0 {
		master.Close()
		t.Fatalf("unlock PTY: %v", errno)
	}
	var number uint32
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, master.Fd(), syscall.TIOCGPTN, uintptr(unsafe.Pointer(&number))); errno != 0 {
		master.Close()
		t.Fatalf("get PTY number: %v", errno)
	}
	slave, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", number), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		master.Close()
		t.Fatalf("open PTY slave: %v", err)
	}
	return master, slave
}
