//go:build linux

package picker

import (
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
	defaultTerminalWidth  = 80
	defaultTerminalHeight = 24
	inputPollInterval     = 100 * time.Millisecond
	escapeSequenceWait    = 20 * time.Millisecond
)

type terminalSession struct {
	input       *os.File
	output      *os.File
	original    syscall.Termios
	pending     []byte
	closed      bool
	screenReady bool
}

type windowSize struct {
	rows    uint16
	columns uint16
	xpixels uint16
	ypixels uint16
}

func newTerminalSession(input, output *os.File) (*terminalSession, error) {
	if err := validateTerminalFiles(input, output); err != nil {
		return nil, err
	}
	original, err := readTermios(input.Fd())
	if err != nil {
		return nil, fmt.Errorf("read terminal mode: %w", err)
	}

	raw := original
	raw.Iflag &^= syscall.IGNBRK | syscall.BRKINT | syscall.PARMRK | syscall.ISTRIP |
		syscall.INLCR | syscall.IGNCR | syscall.ICRNL | syscall.IXON
	raw.Oflag &^= syscall.OPOST
	raw.Lflag &^= syscall.ECHO | syscall.ECHONL | syscall.ICANON | syscall.ISIG | syscall.IEXTEN
	raw.Cflag &^= syscall.CSIZE | syscall.PARENB
	raw.Cflag |= syscall.CS8
	raw.Cc[syscall.VMIN] = 1
	raw.Cc[syscall.VTIME] = 0
	if err := writeTermios(input.Fd(), &raw); err != nil {
		return nil, fmt.Errorf("enable terminal raw mode: %w", err)
	}

	session := &terminalSession{input: input, output: output, original: original, screenReady: true}
	if _, err := io.WriteString(output, "\x1b[?1049h\x1b[?25l"); err != nil {
		return nil, errors.Join(fmt.Errorf("initialize picker screen: %w", err), session.Close())
	}
	return session, nil
}

func validateTerminalFiles(input, output *os.File) error {
	if input == nil || output == nil {
		return ErrNotTerminal
	}
	if _, err := readTermios(input.Fd()); err != nil {
		return fmt.Errorf("%w: input: %v", ErrNotTerminal, err)
	}
	if _, err := readTermios(output.Fd()); err != nil {
		return fmt.Errorf("%w: output: %v", ErrNotTerminal, err)
	}
	return nil
}

func (s *terminalSession) Size() (int, int) {
	var size windowSize
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, s.output.Fd(), syscall.TIOCGWINSZ, uintptr(unsafe.Pointer(&size)))
	if errno != 0 || size.columns == 0 || size.rows == 0 {
		return defaultTerminalWidth, defaultTerminalHeight
	}
	return int(size.columns), int(size.rows)
}

func (s *terminalSession) Draw(screen string) error {
	_, err := io.WriteString(s.output, screen)
	return err
}

func (s *terminalSession) ReadKey(ctx context.Context) (key, error) {
	for {
		if parsed, consumed, needMore := parseKey(s.pending); !needMore {
			s.pending = s.pending[consumed:]
			return parsed, nil
		}

		wait := inputPollInterval
		if len(s.pending) > 0 && s.pending[0] == 0x1b {
			wait = escapeSequenceWait
		}
		ready, err := waitReadable(ctx, int(s.input.Fd()), wait)
		if err != nil {
			return keyUnknown, err
		}
		if !ready {
			if len(s.pending) > 0 && s.pending[0] == 0x1b {
				s.pending = s.pending[1:]
				return keyEscape, nil
			}
			continue
		}

		var buffer [64]byte
		count, err := syscall.Read(int(s.input.Fd()), buffer[:])
		if err != nil {
			if err == syscall.EINTR || err == syscall.EAGAIN {
				continue
			}
			return keyUnknown, err
		}
		if count == 0 {
			return keyUnknown, io.EOF
		}
		s.pending = append(s.pending, buffer[:count]...)
	}
}

func (s *terminalSession) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	var screenErr error
	if s.screenReady {
		_, screenErr = io.WriteString(s.output, "\x1b[0m\x1b[?25h\x1b[?1049l")
	}
	return errors.Join(screenErr, s.restoreTermios())
}

func (s *terminalSession) restoreTermios() error {
	if err := writeTermios(s.input.Fd(), &s.original); err != nil {
		return fmt.Errorf("restore terminal mode: %w", err)
	}
	return nil
}

func parseKey(input []byte) (pressed key, consumed int, needMore bool) {
	if len(input) == 0 {
		return keyUnknown, 0, true
	}
	switch input[0] {
	case 0x03:
		return keyInterrupt, 1, false
	case 0x08, 0x7f:
		return keyLeft, 1, false
	case '\r', '\n':
		return keyEnter, 1, false
	case ' ':
		return keySpace, 1, false
	case 'j':
		return keyDown, 1, false
	case 'k':
		return keyUp, 1, false
	case 'l':
		return keyRight, 1, false
	case 'h':
		return keyLeft, 1, false
	case 'q':
		return keyQuit, 1, false
	case 0x1b:
		if len(input) == 1 {
			return keyUnknown, 0, true
		}
		if input[1] != '[' && input[1] != 'O' {
			return keyEscape, 1, false
		}
		if len(input) < 3 {
			return keyUnknown, 0, true
		}
		switch input[2] {
		case 'A':
			return keyUp, 3, false
		case 'B':
			return keyDown, 3, false
		case 'C':
			return keyRight, 3, false
		case 'D':
			return keyLeft, 3, false
		default:
			return keyEscape, 1, false
		}
	default:
		return keyUnknown, 1, false
	}
}

func waitReadable(ctx context.Context, fd int, timeout time.Duration) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	var set syscall.FdSet
	wordBits := int(unsafe.Sizeof(set.Bits[0]) * 8)
	if fd < 0 || fd >= len(set.Bits)*wordBits {
		return false, fmt.Errorf("terminal file descriptor %d exceeds select capacity", fd)
	}
	fdSet(fd, &set)
	timeval := syscall.NsecToTimeval(timeout.Nanoseconds())
	count, err := syscall.Select(fd+1, &set, nil, nil, &timeval)
	if err != nil {
		if err == syscall.EINTR {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return false, ctxErr
			}
			return false, nil
		}
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return count > 0 && fdIsSet(fd, &set), nil
}

func fdSet(fd int, set *syscall.FdSet) {
	wordBits := int(unsafe.Sizeof(set.Bits[0]) * 8)
	set.Bits[fd/wordBits] |= 1 << (uint(fd) % uint(wordBits))
}

func fdIsSet(fd int, set *syscall.FdSet) bool {
	wordBits := int(unsafe.Sizeof(set.Bits[0]) * 8)
	return set.Bits[fd/wordBits]&(1<<(uint(fd)%uint(wordBits))) != 0
}

func readTermios(fd uintptr) (syscall.Termios, error) {
	var value syscall.Termios
	_, _, errno := syscall.Syscall6(syscall.SYS_IOCTL, fd, syscall.TCGETS, uintptr(unsafe.Pointer(&value)), 0, 0, 0)
	if errno != 0 {
		return syscall.Termios{}, errno
	}
	return value, nil
}

func writeTermios(fd uintptr, value *syscall.Termios) error {
	_, _, errno := syscall.Syscall6(syscall.SYS_IOCTL, fd, syscall.TCSETS, uintptr(unsafe.Pointer(value)), 0, 0, 0)
	if errno != 0 {
		return errno
	}
	return nil
}
