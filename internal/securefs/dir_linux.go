//go:build linux

package securefs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

const (
	oPath       = 0x200000
	atEmptyPath = 0x1000
)

type linuxOps struct {
	open     func(string, int, uint32) (int, error)
	openat   func(int, string, int, uint32) (int, error)
	mkdirat  func(int, string, uint32) error
	fstat    func(int, *syscall.Stat_t) error
	fchmodat func(int, string, uint32, int) error
	close    func(int) error
}

var hostLinuxOps = linuxOps{
	open:     syscall.Open,
	openat:   syscall.Openat,
	mkdirat:  syscall.Mkdirat,
	fstat:    syscall.Fstat,
	fchmodat: syscall.Fchmodat,
	close:    syscall.Close,
}

// Dir is an open directory descriptor. Entry operations remain bound to this
// directory even if its pathname is renamed or replaced.
type Dir struct {
	fd      int
	closeFD func(int) error
}

// Close releases the directory descriptor. Repeated calls are harmless.
func (d *Dir) Close() error {
	if d == nil || d.fd < 0 {
		return nil
	}
	fd := d.fd
	d.fd = -1
	closeFD := d.closeFD
	if closeFD == nil {
		closeFD = syscall.Close
	}
	if err := closeFD(fd); err != nil {
		return fmt.Errorf("close private directory: %w", err)
	}
	return nil
}

// OpenPrivateDir opens an existing tool-owned private directory.
func OpenPrivateDir(path string) (*Dir, error) {
	dir, err := openDirectoryTree(path, false)
	if err != nil {
		return nil, err
	}
	if err := validatePrivateDir(path, dir.fd, hostLinuxOps); err != nil {
		_ = dir.Close()
		return nil, err
	}
	return dir, nil
}

// EnsurePrivateDir creates path when needed and returns a validated descriptor
// for a private 0700 directory. Existing directories are never chmodded.
func EnsurePrivateDir(path string) (*Dir, error) {
	if err := validateAbsolutePath(path); err != nil {
		return nil, err
	}
	parent, err := openDirectoryTree(filepath.Dir(path), true)
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	return EnsurePrivateDirAt(parent, filepath.Base(path))
}

// EnsurePrivateDirAt creates name as a direct private child of parent when
// needed and returns a validated descriptor bound to that child.
func EnsurePrivateDirAt(parent *Dir, name string) (*Dir, error) {
	return ensurePrivateDirAtWithOps(parent, name, hostLinuxOps)
}

func ensurePrivateDirAtWithOps(parent *Dir, name string, ops linuxOps) (*Dir, error) {
	if parent == nil || parent.fd < 0 {
		return nil, fmt.Errorf("invalid parent directory descriptor")
	}
	if err := validateEntryName(name); err != nil {
		return nil, err
	}
	created := false
	if err := ops.mkdirat(parent.fd, name, 0); err == nil {
		created = true
	} else if !errors.Is(err, syscall.EEXIST) {
		return nil, fmt.Errorf("create private directory: %w", err)
	}

	pathFD, err := ops.openat(parent.fd, name, oPath|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open private directory path: %w", err)
	}
	keepPathFD := true
	defer func() {
		if keepPathFD {
			_ = ops.close(pathFD)
		}
	}()

	var pathStat syscall.Stat_t
	if err := ops.fstat(pathFD, &pathStat); err != nil {
		return nil, fmt.Errorf("inspect private directory: %w", err)
	}
	if created {
		if err := validateDirectoryStat(name, &pathStat, 0); err != nil {
			return nil, err
		}
		if err := ops.fchmodat(pathFD, "", 0o700, atEmptyPath); err != nil {
			if errors.Is(err, syscall.ENOSYS) || errors.Is(err, syscall.EOPNOTSUPP) {
				return nil, fmt.Errorf("promote private directory: Linux 6.6 or newer with fchmodat2(AT_EMPTY_PATH) is required: %w", err)
			}
			return nil, fmt.Errorf("promote private directory: %w", err)
		}
		if err := ops.fstat(pathFD, &pathStat); err != nil {
			return nil, fmt.Errorf("reinspect private directory: %w", err)
		}
	}
	if err := validateDirectoryStat(name, &pathStat, 0o700); err != nil {
		return nil, err
	}

	fd, err := ops.openat(pathFD, ".", syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open private directory: %w", err)
	}
	dir := &Dir{fd: fd, closeFD: ops.close}
	var openedStat syscall.Stat_t
	if err := ops.fstat(fd, &openedStat); err != nil {
		_ = dir.Close()
		return nil, fmt.Errorf("inspect opened private directory: %w", err)
	}
	if pathStat.Dev != openedStat.Dev || pathStat.Ino != openedStat.Ino {
		_ = dir.Close()
		return nil, fmt.Errorf("private directory changed while opening")
	}
	if err := validateDirectoryStat(name, &openedStat, 0o700); err != nil {
		_ = dir.Close()
		return nil, err
	}
	closeErr := ops.close(pathFD)
	keepPathFD = false
	if closeErr != nil {
		_ = dir.Close()
		return nil, fmt.Errorf("close private directory path: %w", closeErr)
	}
	return dir, nil
}

func openDirectoryTree(path string, createParents bool) (*Dir, error) {
	return walkDirWithOps(path, createParents, hostLinuxOps)
}

func walkDirWithOps(path string, createParents bool, ops linuxOps) (*Dir, error) {
	if err := validateAbsolutePath(path); err != nil {
		return nil, err
	}
	pathFD, err := ops.open("/", oPath|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open directory root: %w", err)
	}
	keepPathFD := true
	defer func() {
		if keepPathFD {
			_ = ops.close(pathFD)
		}
	}()

	components := strings.Split(strings.TrimPrefix(path, string(filepath.Separator)), string(filepath.Separator))
	if path == string(filepath.Separator) {
		components = nil
	}
	for _, component := range components {
		nextFD, openErr := ops.openat(pathFD, component, oPath|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
		if errors.Is(openErr, syscall.ENOENT) && createParents {
			mkdirErr := ops.mkdirat(pathFD, component, 0o755)
			if mkdirErr != nil && !errors.Is(mkdirErr, syscall.EEXIST) {
				return nil, fmt.Errorf("create parent directory %q: %w", component, mkdirErr)
			}
			nextFD, openErr = ops.openat(pathFD, component, oPath|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
		}
		if openErr != nil {
			return nil, fmt.Errorf("open directory component %q: %w", component, openErr)
		}
		var stat syscall.Stat_t
		if err := ops.fstat(nextFD, &stat); err != nil {
			_ = ops.close(nextFD)
			return nil, fmt.Errorf("inspect directory component %q: %w", component, err)
		}
		if stat.Mode&syscall.S_IFMT != syscall.S_IFDIR {
			_ = ops.close(nextFD)
			return nil, fmt.Errorf("unsafe directory component type: %q", component)
		}
		if err := ops.close(pathFD); err != nil {
			keepPathFD = false
			_ = ops.close(nextFD)
			return nil, fmt.Errorf("close directory component: %w", err)
		}
		pathFD = nextFD
	}

	fd, err := ops.openat(pathFD, ".", syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open directory tree: %w", err)
	}
	dir := &Dir{fd: fd, closeFD: ops.close}
	var pathStat, openedStat syscall.Stat_t
	if err := ops.fstat(pathFD, &pathStat); err != nil {
		_ = dir.Close()
		return nil, fmt.Errorf("inspect directory tree path: %w", err)
	}
	if err := ops.fstat(fd, &openedStat); err != nil {
		_ = dir.Close()
		return nil, fmt.Errorf("inspect opened directory tree: %w", err)
	}
	if pathStat.Dev != openedStat.Dev || pathStat.Ino != openedStat.Ino || openedStat.Mode&syscall.S_IFMT != syscall.S_IFDIR {
		_ = dir.Close()
		return nil, fmt.Errorf("directory tree changed while opening")
	}
	closeErr := ops.close(pathFD)
	keepPathFD = false
	if closeErr != nil {
		_ = dir.Close()
		return nil, fmt.Errorf("close directory tree path: %w", closeErr)
	}
	return dir, nil
}

func validatePrivateDir(path string, fd int, ops linuxOps) error {
	var stat syscall.Stat_t
	if err := ops.fstat(fd, &stat); err != nil {
		return fmt.Errorf("inspect private directory: %w", err)
	}
	return validateDirectoryStat(path, &stat, 0o700)
}

func validateDirectoryStat(path string, stat *syscall.Stat_t, mode uint32) error {
	if stat.Mode&syscall.S_IFMT != syscall.S_IFDIR {
		return fmt.Errorf("unsafe private directory type: %s", path)
	}
	if int(stat.Uid) != os.Geteuid() {
		return fmt.Errorf("unsafe private directory owner: %s", path)
	}
	if stat.Mode&0o7777 != mode {
		return fmt.Errorf("unsafe private directory mode %o: %s", stat.Mode&0o7777, path)
	}
	return nil
}

func validateAbsolutePath(path string) error {
	if path == "" || strings.ContainsRune(path, '\x00') || !filepath.IsAbs(path) || filepath.Clean(path) != path || path != string(filepath.Separator) && strings.HasSuffix(path, string(filepath.Separator)) {
		return fmt.Errorf("unsafe path: %q", path)
	}
	return nil
}
