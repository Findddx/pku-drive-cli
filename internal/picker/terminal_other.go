//go:build !linux

package picker

import "os"

func newTerminalSession(_, _ *os.File) (session, error) {
	return nil, ErrNotTerminal
}

func validateTerminalFiles(_, _ *os.File) error {
	return ErrNotTerminal
}
