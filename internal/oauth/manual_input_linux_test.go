//go:build linux

package oauth

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

type callbackReadResult struct {
	line string
	err  error
}

type promptNotifier struct {
	buffer bytes.Buffer
	ready  chan struct{}
}

func (writer *promptNotifier) Write(data []byte) (int, error) {
	written, err := writer.buffer.Write(data)
	select {
	case <-writer.ready:
	default:
		close(writer.ready)
	}
	return written, err
}

func TestCallbackLineReaderHidesTTYInputAndRestoresDescriptor(t *testing.T) {
	master, slave := openOAuthPTY(t)
	defer master.Close()
	defer slave.Close()

	originalTermios, err := getTermios(int(slave.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	originalFlags, err := getFileFlags(int(slave.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	notice := &promptNotifier{ready: make(chan struct{})}
	result := make(chan callbackReadResult, 1)
	reader := newCallbackLineReader(slave)
	go func() {
		line, readErr := reader.ReadLine(context.Background(), notice, "hidden: ", "visible: ")
		result <- callbackReadResult{line: line, err: readErr}
	}()

	select {
	case <-notice.ready:
	case <-time.After(time.Second):
		t.Fatal("callback prompt was not written")
	}
	hidden, err := getTermios(int(slave.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	if hidden.Lflag&(syscall.ECHO|syscall.ECHONL) != 0 || hidden.Cc[syscall.VSUSP] != 0 || hidden.Cc[syscall.VQUIT] != 0 {
		t.Fatalf("hidden termios did not suppress echo/job-control characters: %+v", hidden)
	}
	const secretLine = "http://127.0.0.1:43123/callback?code=hidden-code&state=hidden-state"
	if _, err := io.WriteString(master, secretLine+"\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-result:
		if got.err != nil || got.line != secretLine {
			t.Fatalf("line=%q err=%v", got.line, got.err)
		}
	case <-time.After(time.Second):
		t.Fatal("callback input did not complete")
	}

	restoredTermios, err := getTermios(int(slave.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	restoredFlags, err := getFileFlags(int(slave.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(restoredTermios, originalTermios) || restoredFlags != originalFlags {
		t.Fatalf("terminal was not restored: termios equal=%v flags=%#x want=%#x", reflect.DeepEqual(restoredTermios, originalTermios), restoredFlags, originalFlags)
	}
	if bytes.Contains(notice.buffer.Bytes(), []byte(secretLine)) {
		t.Fatalf("callback URL was copied to notice output: %q", notice.buffer.String())
	}
}

func TestCallbackLineReaderCancellationRestoresPipeFlags(t *testing.T) {
	input, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	defer writer.Close()
	originalFlags, err := getFileFlags(int(input.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	var notice bytes.Buffer
	_, err = newCallbackLineReader(input).ReadLine(ctx, &notice, "hidden: ", "visible: ")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ReadLine() error=%v", err)
	}
	restoredFlags, err := getFileFlags(int(input.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	if restoredFlags != originalFlags || notice.String() != "visible: \n" {
		t.Fatalf("flags=%#x want=%#x notice=%q", restoredFlags, originalFlags, notice.String())
	}
}

func TestCallbackLineReaderCancellationRestoresTTY(t *testing.T) {
	master, slave := openOAuthPTY(t)
	defer master.Close()
	defer slave.Close()
	original, err := getTermios(int(slave.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	notice := &promptNotifier{ready: make(chan struct{})}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err = newCallbackLineReader(slave).ReadLine(ctx, notice, "hidden: ", "visible: ")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ReadLine() error=%v", err)
	}
	restored, err := getTermios(int(slave.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(restored, original) {
		t.Fatal("cancellation left terminal settings changed")
	}
}

func TestCallbackLineReaderPromptFailureRestoresTTY(t *testing.T) {
	master, slave := openOAuthPTY(t)
	defer master.Close()
	defer slave.Close()
	original, err := getTermios(int(slave.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := newCallbackLineReader(slave).ReadLine(context.Background(), failingCallbackWriter{}, "hidden: ", "visible: "); err == nil {
		t.Fatal("ReadLine() error = nil")
	}
	restored, err := getTermios(int(slave.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(restored, original) {
		t.Fatal("prompt failure left terminal settings changed")
	}
}

func TestBufferedCallbackLinePreservesEmbeddedCarriageReturn(t *testing.T) {
	line, err := readBoundedBufferedLine(bufio.NewReader(strings.NewReader("co\rde\r\n")))
	if err != nil || line != "co\rde" {
		t.Fatalf("line=%q err=%v", line, err)
	}
}

type failingCallbackWriter struct{}

func (failingCallbackWriter) Write([]byte) (int, error) {
	return 0, errors.New("synthetic write failure")
}

func openOAuthPTY(t *testing.T) (*os.File, *os.File) {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("PTY unavailable: %v", err)
	}
	unlock := int32(0)
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, master.Fd(), uintptr(syscall.TIOCSPTLCK), uintptr(unsafe.Pointer(&unlock))); errno != 0 {
		master.Close()
		t.Skipf("cannot unlock PTY: %v", errno)
	}
	var number uint32
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, master.Fd(), uintptr(syscall.TIOCGPTN), uintptr(unsafe.Pointer(&number))); errno != 0 {
		master.Close()
		t.Skipf("cannot identify PTY: %v", errno)
	}
	slave, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", number), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		master.Close()
		t.Skipf("cannot open PTY slave: %v", err)
	}
	return master, slave
}
