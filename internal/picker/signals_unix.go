//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package picker

import (
	"os"
	"syscall"
)

func pickerSignals() []os.Signal {
	return []os.Signal{
		os.Interrupt,
		syscall.SIGHUP,
		syscall.SIGTERM,
		syscall.SIGQUIT,
		syscall.SIGTSTP,
	}
}
