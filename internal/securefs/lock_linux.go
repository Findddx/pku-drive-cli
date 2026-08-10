//go:build linux

package securefs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
)

// WithLock runs fn while holding an exclusive advisory lock at path.
func WithLock(ctx context.Context, path string, fn func() error) error {
	parent, name, err := splitAbsoluteEntryPath(path)
	if err != nil {
		return err
	}
	dir, err := OpenPrivateDir(parent)
	if err != nil {
		return err
	}
	defer dir.Close()
	return WithLockAt(ctx, dir, name, fn)
}

// WithLockAt runs fn while holding an exclusive advisory lock named by name
// relative to dir.
func WithLockAt(ctx context.Context, dir *Dir, name string, fn func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateEntryName(name); err != nil {
		return err
	}
	f, err := openLock0600At(dir, name)
	if err != nil {
		return err
	}
	defer f.Close()

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
			return fn()
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			return fmt.Errorf("lock file: %w", err)
		}
		timer := time.NewTimer(25 * time.Millisecond)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func openLock0600At(dir *Dir, name string) (*os.File, error) {
	flags := syscall.O_RDWR | syscall.O_CREAT | syscall.O_EXCL | syscall.O_NOFOLLOW | syscall.O_CLOEXEC
	fd, err := syscall.Openat(dir.fd, name, flags, 0o600)
	created := err == nil
	if errors.Is(err, syscall.EEXIST) {
		fd, err = syscall.Openat(dir.fd, name, syscall.O_RDWR|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	}
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}
	if created {
		if err := syscall.Fchmod(fd, 0o600); err != nil {
			_ = syscall.Close(fd)
			return nil, fmt.Errorf("chmod lock file: %w", err)
		}
	}
	f := os.NewFile(uintptr(fd), name)
	if f == nil {
		_ = syscall.Close(fd)
		return nil, fmt.Errorf("open lock file: could not create file handle")
	}
	var stat syscall.Stat_t
	if err := syscall.Fstat(fd, &stat); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("inspect lock file: %w", err)
	}
	if err := validateRegular0600(name, &stat); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}
