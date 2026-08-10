//go:build linux

package oauth

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"
	"time"
	"unsafe"
)

const (
	ioctlReadTermios  = 0x5401
	ioctlWriteTermios = 0x5402
)

var errCallbackInputRestore = errors.New("callback input restore failed")

type callbackLineReader struct {
	input    io.Reader
	buffered *bufio.Reader
}

func newCallbackLineReader(input io.Reader) *callbackLineReader {
	if input == nil {
		return nil
	}
	return &callbackLineReader{input: input, buffered: bufio.NewReader(input)}
}

func (r *callbackLineReader) ReadLine(ctx context.Context, notice io.Writer, hiddenPrompt, visiblePrompt string) (string, error) {
	if r == nil || r.input == nil {
		return "", io.EOF
	}
	if file, ok := r.input.(*os.File); ok {
		return readFileCallbackLine(ctx, file, notice, hiddenPrompt, visiblePrompt)
	}
	if _, err := fmt.Fprint(notice, visiblePrompt); err != nil {
		return "", err
	}
	type result struct {
		line string
		err  error
	}
	resultCh := make(chan result, 1)
	go func() {
		line, err := readBoundedBufferedLine(r.buffered)
		resultCh <- result{line: line, err: err}
	}()
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case got := <-resultCh:
		if _, err := fmt.Fprintln(notice); err != nil && got.err == nil {
			got.err = err
		}
		return got.line, got.err
	}
}

func readFileCallbackLine(ctx context.Context, file *os.File, notice io.Writer, hiddenPrompt, visiblePrompt string) (string, error) {
	fd := int(file.Fd())
	var descriptorSet syscall.FdSet
	if fd < 0 || fd >= len(descriptorSet.Bits)*64 {
		return "", errors.New("callback input file descriptor is unsupported")
	}
	originalFlags, err := getFileFlags(fd)
	if err != nil {
		return "", err
	}
	if err := setFileFlags(fd, originalFlags|syscall.O_NONBLOCK); err != nil {
		return "", err
	}
	originalTermios, termiosErr := getTermios(fd)
	echoHidden := termiosErr == nil
	if termiosErr != nil && !errors.Is(termiosErr, syscall.ENOTTY) {
		if restoreErr := setFileFlags(fd, originalFlags); restoreErr != nil {
			return "", errCallbackInputRestore
		}
		return "", termiosErr
	}
	if echoHidden {
		hidden := *originalTermios
		hidden.Lflag &^= syscall.ECHO | syscall.ECHONL
		// Keep Ctrl-C available to cancel the command, but disable the terminal
		// characters that would suspend or quit before echo can be restored.
		hidden.Cc[syscall.VSUSP] = 0
		hidden.Cc[syscall.VQUIT] = 0
		if err := setTermios(fd, &hidden); err != nil {
			if restoreErr := setFileFlags(fd, originalFlags); restoreErr != nil {
				return "", errCallbackInputRestore
			}
			return "", err
		}
	}
	prompt := visiblePrompt
	if echoHidden {
		prompt = hiddenPrompt
	}
	if _, err := fmt.Fprint(notice, prompt); err != nil {
		var restoreErr error
		if echoHidden {
			restoreErr = setTermios(fd, originalTermios)
		}
		if flagsErr := setFileFlags(fd, originalFlags); restoreErr == nil {
			restoreErr = flagsErr
		}
		if restoreErr != nil {
			return "", errCallbackInputRestore
		}
		return "", err
	}

	line, readErr := readNonblockingCallbackLine(ctx, fd, &descriptorSet)
	var restoreErr error
	if echoHidden {
		restoreErr = setTermios(fd, originalTermios)
	}
	if err := setFileFlags(fd, originalFlags); restoreErr == nil && err != nil {
		restoreErr = err
	}
	if _, err := fmt.Fprintln(notice); restoreErr == nil && err != nil {
		restoreErr = err
	}
	if restoreErr != nil {
		return "", errCallbackInputRestore
	}
	if readErr != nil {
		return "", readErr
	}
	return line, nil
}

func readNonblockingCallbackLine(ctx context.Context, fd int, descriptorSet *syscall.FdSet) (string, error) {
	line := make([]byte, 0, 512)
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		*descriptorSet = syscall.FdSet{}
		descriptorSet.Bits[fd/64] |= int64(1) << uint(fd%64)
		timeout := syscall.NsecToTimeval((100 * time.Millisecond).Nanoseconds())
		ready, err := syscall.Select(fd+1, descriptorSet, nil, nil, &timeout)
		if errors.Is(err, syscall.EINTR) || errors.Is(err, syscall.EAGAIN) {
			continue
		}
		if err != nil {
			return "", err
		}
		if ready == 0 {
			continue
		}
		var one [1]byte
		count, err := syscall.Read(fd, one[:])
		if errors.Is(err, syscall.EINTR) || errors.Is(err, syscall.EAGAIN) {
			continue
		}
		if err != nil {
			return "", err
		}
		if count == 0 {
			return "", io.EOF
		}
		if one[0] == '\n' {
			if len(line) > 0 && line[len(line)-1] == '\r' {
				line = line[:len(line)-1]
			}
			return string(line), nil
		}
		line = append(line, one[0])
		if len(line) > maximumPastedCallbackBytes {
			return "", invalidPastedCallback()
		}
	}
}

func readBoundedBufferedLine(reader *bufio.Reader) (string, error) {
	line := make([]byte, 0, 512)
	for {
		one, err := reader.ReadByte()
		if err != nil {
			if errors.Is(err, io.EOF) && len(line) > 0 {
				if line[len(line)-1] == '\r' {
					line = line[:len(line)-1]
				}
				return string(line), nil
			}
			return "", err
		}
		if one == '\n' {
			if len(line) > 0 && line[len(line)-1] == '\r' {
				line = line[:len(line)-1]
			}
			return string(line), nil
		}
		line = append(line, one)
		if len(line) > maximumPastedCallbackBytes {
			return "", invalidPastedCallback()
		}
	}
}

func getTermios(fd int) (*syscall.Termios, error) {
	termios := new(syscall.Termios)
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), ioctlReadTermios, uintptr(unsafe.Pointer(termios)))
	if errno != 0 {
		return nil, errno
	}
	return termios, nil
}

func setTermios(fd int, termios *syscall.Termios) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), ioctlWriteTermios, uintptr(unsafe.Pointer(termios)))
	if errno != 0 {
		return errno
	}
	return nil
}

func getFileFlags(fd int) (int, error) {
	flags, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), syscall.F_GETFL, 0)
	if errno != 0 {
		return 0, errno
	}
	return int(flags), nil
}

func setFileFlags(fd, flags int) error {
	_, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), syscall.F_SETFL, uintptr(flags))
	if errno != 0 {
		return errno
	}
	return nil
}
