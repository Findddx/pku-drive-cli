//go:build linux && amd64

package download

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"unsafe"
)

const (
	oPathLinux        = 0x200000
	oTmpfileLinux     = 0x410000
	atEmptyPath       = 0x1000
	atSymlinkFollow   = 0x400
	atFDCWD           = -100
	renameExchange    = 2
	sysRenameat2AMD64 = 316
	randomNameBytes   = 16
	randomNameTries   = 128
)

var (
	errDestinationExists  = errors.New("destination already exists")
	errDestinationChanged = errors.New("destination changed during download")
	errTemporaryChanged   = errors.New("download temporary file changed")
	errUnsafeLocalEntry   = errors.New("unsafe local filesystem entry")
)

type localLinuxOps struct {
	open           func(string, int, uint32) (int, error)
	openat         func(int, string, int, uint32) (int, error)
	fstat          func(int, *syscall.Stat_t) error
	close          func(int) error
	fsync          func(int) error
	unlinkat       func(int, string) error
	linkFD         func(int, int, string) error
	renameExchange func(int, string, int, string) error
}

var hostLocalLinuxOps = localLinuxOps{
	open:           syscall.Open,
	openat:         syscall.Openat,
	fstat:          syscall.Fstat,
	close:          syscall.Close,
	fsync:          syscall.Fsync,
	unlinkat:       syscall.Unlinkat,
	linkFD:         linkFileDescriptor,
	renameExchange: renameat2Exchange,
}

type ownedLocalFD struct {
	fd      int
	closeFD func(int) error
}

func newOwnedLocalFD(fd int, closeFD func(int) error) *ownedLocalFD {
	return &ownedLocalFD{fd: fd, closeFD: closeFD}
}

func (f *ownedLocalFD) Close() error {
	if f == nil || f.fd < 0 {
		return nil
	}
	fd := f.fd
	f.fd = -1
	if f.closeFD == nil {
		f.closeFD = syscall.Close
	}
	return f.closeFD(fd)
}

type localIdentity struct {
	device uint64
	inode  uint64
}

func identityOf(stat *syscall.Stat_t) localIdentity {
	return localIdentity{device: uint64(stat.Dev), inode: stat.Ino}
}

func sameIdentity(stat *syscall.Stat_t, identity localIdentity) bool {
	return uint64(stat.Dev) == identity.device && stat.Ino == identity.inode
}

type localEntry struct {
	fd       *ownedLocalFD
	identity localIdentity
	stat     syscall.Stat_t
}

func (e *localEntry) Close() error {
	if e == nil {
		return nil
	}
	return e.fd.Close()
}

type downloadTarget struct {
	directory *ownedLocalFD
	name      string
	overwrite bool
	existing  *localEntry
	ops       localLinuxOps
	random    io.Reader
	closed    bool
}

func openDownloadTarget(path string, overwrite bool) (*downloadTarget, error) {
	return openDownloadTargetWithOps(path, overwrite, hostLocalLinuxOps, rand.Reader)
}

func openDownloadTargetWithOps(path string, overwrite bool, ops localLinuxOps, random io.Reader) (*downloadTarget, error) {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.ContainsRune(path, '\x00') {
		return nil, errUnsafeLocalEntry
	}
	name := filepath.Base(path)
	if name == "" || name == "." || name == ".." || name == string(filepath.Separator) || strings.ContainsRune(name, filepath.Separator) {
		return nil, errUnsafeLocalEntry
	}
	directory, err := openBoundDirectory(filepath.Dir(path), ops)
	if err != nil {
		return nil, err
	}
	target := &downloadTarget{directory: directory, name: name, overwrite: overwrite, ops: ops, random: random}
	existing, err := target.openEntry(name)
	if errors.Is(err, syscall.ENOENT) {
		return target, nil
	}
	if err != nil {
		_ = target.Close()
		return nil, err
	}
	if !overwrite {
		_ = existing.Close()
		_ = target.Close()
		return nil, errDestinationExists
	}
	if existing.stat.Mode&syscall.S_IFMT != syscall.S_IFREG {
		_ = existing.Close()
		_ = target.Close()
		return nil, errUnsafeLocalEntry
	}
	target.existing = existing
	return target, nil
}

func (t *downloadTarget) Close() error {
	if t == nil || t.closed {
		return nil
	}
	t.closed = true
	var first error
	if t.existing != nil {
		if err := t.existing.Close(); err != nil {
			first = err
		}
	}
	if err := t.directory.Close(); first == nil && err != nil {
		first = err
	}
	return first
}

func openBoundDirectory(path string, ops localLinuxOps) (*ownedLocalFD, error) {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.ContainsRune(path, '\x00') {
		return nil, errUnsafeLocalEntry
	}
	rootFD, err := ops.open(string(filepath.Separator), oPathLinux|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	current := newOwnedLocalFD(rootFD, ops.close)
	components := strings.Split(strings.TrimPrefix(path, string(filepath.Separator)), string(filepath.Separator))
	if path == string(filepath.Separator) {
		components = nil
	}
	for _, component := range components {
		if component == "" || component == "." || component == ".." {
			_ = current.Close()
			return nil, errUnsafeLocalEntry
		}
		nextFD, openErr := ops.openat(current.fd, component, oPathLinux|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
		if openErr != nil {
			_ = current.Close()
			return nil, openErr
		}
		next := newOwnedLocalFD(nextFD, ops.close)
		var nextStat syscall.Stat_t
		if err := ops.fstat(next.fd, &nextStat); err != nil || nextStat.Mode&syscall.S_IFMT != syscall.S_IFDIR {
			_ = next.Close()
			_ = current.Close()
			if err != nil {
				return nil, err
			}
			return nil, errUnsafeLocalEntry
		}
		if err := current.Close(); err != nil {
			_ = next.Close()
			return nil, err
		}
		current = next
	}

	directoryFD, err := ops.openat(current.fd, ".", syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		_ = current.Close()
		return nil, err
	}
	directory := newOwnedLocalFD(directoryFD, ops.close)
	var pathStat, directoryStat syscall.Stat_t
	if err := ops.fstat(current.fd, &pathStat); err != nil {
		_ = directory.Close()
		_ = current.Close()
		return nil, err
	}
	if err := ops.fstat(directory.fd, &directoryStat); err != nil || directoryStat.Mode&syscall.S_IFMT != syscall.S_IFDIR || identityOf(&pathStat) != identityOf(&directoryStat) {
		_ = directory.Close()
		_ = current.Close()
		if err != nil {
			return nil, err
		}
		return nil, errUnsafeLocalEntry
	}
	if err := current.Close(); err != nil {
		_ = directory.Close()
		return nil, err
	}
	return directory, nil
}

func (t *downloadTarget) openEntry(name string) (*localEntry, error) {
	fd, err := t.ops.openat(t.directory.fd, name, oPathLinux|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	owned := newOwnedLocalFD(fd, t.ops.close)
	var stat syscall.Stat_t
	if err := t.ops.fstat(fd, &stat); err != nil {
		_ = owned.Close()
		return nil, err
	}
	return &localEntry{fd: owned, identity: identityOf(&stat), stat: stat}, nil
}

type downloadTemporary struct {
	target      *downloadTarget
	file        *os.File
	identity    localIdentity
	namedOrigin string
	stage       string
	stageID     localIdentity
	linkedName  string
	installed   bool
	closed      bool
	initialLink uint64
}

func (t *downloadTarget) CreateTemp() (*downloadTemporary, error) {
	fd, err := t.ops.openat(t.directory.fd, ".", oTmpfileLinux|syscall.O_RDWR|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0o600)
	named := ""
	if unsupportedTmpfile(err) {
		for attempt := 0; attempt < randomNameTries; attempt++ {
			name, nameErr := t.randomName(".pku-drive-download-")
			if nameErr != nil {
				return nil, nameErr
			}
			fd, err = t.ops.openat(t.directory.fd, name, syscall.O_RDWR|syscall.O_CREAT|syscall.O_EXCL|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0o600)
			if errors.Is(err, syscall.EEXIST) {
				continue
			}
			if err == nil {
				named = name
			}
			break
		}
	}
	if err != nil {
		return nil, err
	}
	var stat syscall.Stat_t
	if err := t.ops.fstat(fd, &stat); err != nil {
		_ = t.ops.close(fd)
		return nil, err
	}
	links := uint64(0)
	if named != "" {
		links = 1
	}
	if !validTemporaryStat(&stat, links, 0) {
		_ = t.ops.close(fd)
		if named != "" {
			_, _ = t.unlinkIfIdentity(named, identityOf(&stat))
		}
		return nil, errUnsafeLocalEntry
	}
	file := os.NewFile(uintptr(fd), "pku-drive-download-temporary")
	if file == nil {
		_ = t.ops.close(fd)
		if named != "" {
			_, _ = t.unlinkIfIdentity(named, identityOf(&stat))
		}
		return nil, errors.New("could not create download file handle")
	}
	return &downloadTemporary{target: t, file: file, identity: identityOf(&stat), namedOrigin: named, initialLink: links}, nil
}

func unsupportedTmpfile(err error) bool {
	return errors.Is(err, syscall.EOPNOTSUPP) || errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.EISDIR) || errors.Is(err, syscall.ENOENT)
}

func validTemporaryStat(stat *syscall.Stat_t, links uint64, size int64) bool {
	return stat.Mode&syscall.S_IFMT == syscall.S_IFREG && stat.Mode&0o7777 == 0o600 && stat.Uid == uint32(os.Geteuid()) && stat.Nlink == links && stat.Size == size
}

func (t *downloadTemporary) File() *os.File { return t.file }

func (t *downloadTemporary) ValidateForWrite() error {
	return t.validate(0, t.initialLink, true)
}

func (t *downloadTemporary) validate(size int64, links uint64, validateOrigin bool) error {
	if t == nil || t.file == nil || t.closed {
		return errTemporaryChanged
	}
	var stat syscall.Stat_t
	if err := t.target.ops.fstat(int(t.file.Fd()), &stat); err != nil {
		return err
	}
	if !sameIdentity(&stat, t.identity) || !validTemporaryStat(&stat, links, size) {
		return errTemporaryChanged
	}
	if validateOrigin && t.namedOrigin != "" {
		entry, err := t.target.openEntry(t.namedOrigin)
		if err != nil {
			return errTemporaryChanged
		}
		matches := entry.identity == t.identity
		closeErr := entry.Close()
		if closeErr != nil {
			return closeErr
		}
		if !matches {
			return errTemporaryChanged
		}
	}
	return nil
}

func (t *downloadTemporary) Sync() error {
	if t == nil || t.file == nil || t.closed {
		return errTemporaryChanged
	}
	return t.target.ops.fsync(int(t.file.Fd()))
}

func (t *downloadTemporary) Close() error {
	if t == nil || t.closed {
		return nil
	}
	t.closed = true
	var cleanupErr error
	if t.stage != "" {
		_, cleanupErr = t.target.unlinkIfIdentity(t.stage, t.stageID)
	}
	if !t.installed && t.linkedName != "" {
		if _, err := t.target.unlinkIfIdentity(t.linkedName, t.identity); cleanupErr == nil && err != nil {
			cleanupErr = err
		}
	}
	if t.namedOrigin != "" {
		if _, err := t.target.unlinkIfIdentity(t.namedOrigin, t.identity); cleanupErr == nil && err != nil {
			cleanupErr = err
		}
	}
	closeErr := t.file.Close()
	t.file = nil
	if t.installed {
		// Publication already succeeded. A best-effort cleanup failure must not
		// turn a visible, verified destination into a reported pure failure.
		return nil
	}
	if cleanupErr != nil {
		return cleanupErr
	}
	return closeErr
}

func (t *downloadTarget) Install(temporary *downloadTemporary, size int64) error {
	if temporary == nil || temporary.target != t {
		return errTemporaryChanged
	}
	if err := temporary.validate(size, temporary.initialLink, true); err != nil {
		return err
	}
	if t.existing == nil {
		return t.installNoReplace(temporary)
	}
	return t.installOverwrite(temporary)
}

func (t *downloadTarget) installNoReplace(temporary *downloadTemporary) error {
	if err := t.ops.linkFD(int(temporary.file.Fd()), t.directory.fd, t.name); err != nil {
		if errors.Is(err, syscall.EEXIST) {
			return errDestinationExists
		}
		return err
	}
	temporary.linkedName = t.name
	entry, err := t.openEntry(t.name)
	if err != nil {
		return errDestinationChanged
	}
	matches := entry.identity == temporary.identity && entry.stat.Mode&syscall.S_IFMT == syscall.S_IFREG
	closeErr := entry.Close()
	if closeErr != nil {
		return closeErr
	}
	if !matches {
		return errDestinationChanged
	}
	temporary.installed = true
	temporary.linkedName = ""
	t.cleanupOriginAfterCommit(temporary)
	return t.ops.fsync(t.directory.fd)
}

func (t *downloadTarget) installOverwrite(temporary *downloadTemporary) error {
	stage, err := t.linkStaging(temporary)
	if err != nil {
		return err
	}
	temporary.stage = stage
	temporary.stageID = temporary.identity
	if err := t.validateEntryIdentity(stage, temporary.identity, syscall.S_IFREG); err != nil {
		return errTemporaryChanged
	}
	if err := t.validateExistingDestination(); err != nil {
		return err
	}
	if err := t.ops.renameExchange(t.directory.fd, stage, t.directory.fd, t.name); err != nil {
		if errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ENOTDIR) {
			return errDestinationChanged
		}
		return err
	}

	destination, destinationErr := t.openEntry(t.name)
	oldTarget, oldTargetErr := t.openEntry(stage)
	validDestination := destinationErr == nil && destination.identity == temporary.identity
	validOldTarget := oldTargetErr == nil && oldTarget.identity == t.existing.identity
	if destination != nil {
		_ = destination.Close()
	}
	if oldTarget != nil {
		_ = oldTarget.Close()
	}
	if !validDestination || !validOldTarget {
		// Never exchange names again after identity becomes uncertain. A second
		// pathname operation could otherwise move a concurrent writer's entry
		// into the destination. Descriptor-relative cleanup later removes only
		// entries that still identify our own temporary inode.
		return errDestinationChanged
	}

	temporary.installed = true
	temporary.stageID = t.existing.identity
	// The stage now names the exact preflight destination. Removal is best
	// effort after publication; never report the verified destination absent.
	if removed, _ := t.unlinkIfIdentity(stage, t.existing.identity); removed {
		temporary.stage = ""
	}
	t.cleanupOriginAfterCommit(temporary)
	return t.ops.fsync(t.directory.fd)
}

func (t *downloadTarget) linkStaging(temporary *downloadTemporary) (string, error) {
	for attempt := 0; attempt < randomNameTries; attempt++ {
		name, err := t.randomName(".pku-drive-stage-")
		if err != nil {
			return "", err
		}
		if err := t.ops.linkFD(int(temporary.file.Fd()), t.directory.fd, name); errors.Is(err, syscall.EEXIST) {
			continue
		} else if err != nil {
			return "", err
		}
		return name, nil
	}
	return "", errors.New("could not allocate download staging name")
}

func (t *downloadTarget) validateExistingDestination() error {
	if t.existing == nil {
		return errDestinationChanged
	}
	var held syscall.Stat_t
	if err := t.ops.fstat(t.existing.fd.fd, &held); err != nil || !sameIdentity(&held, t.existing.identity) || held.Mode&syscall.S_IFMT != syscall.S_IFREG {
		return errDestinationChanged
	}
	return t.validateEntryIdentity(t.name, t.existing.identity, syscall.S_IFREG)
}

func (t *downloadTarget) validateEntryIdentity(name string, identity localIdentity, fileType uint32) error {
	entry, err := t.openEntry(name)
	if err != nil {
		return err
	}
	matches := entry.identity == identity && entry.stat.Mode&syscall.S_IFMT == fileType
	closeErr := entry.Close()
	if closeErr != nil {
		return closeErr
	}
	if !matches {
		return errDestinationChanged
	}
	return nil
}

func (t *downloadTarget) cleanupOriginAfterCommit(temporary *downloadTemporary) {
	if temporary.namedOrigin == "" {
		return
	}
	if removed, _ := t.unlinkIfIdentity(temporary.namedOrigin, temporary.identity); removed {
		temporary.namedOrigin = ""
	}
}

func (t *downloadTarget) unlinkIfIdentity(name string, identity localIdentity) (bool, error) {
	entry, err := t.openEntry(name)
	if errors.Is(err, syscall.ENOENT) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if entry.identity != identity {
		_ = entry.Close()
		return false, nil
	}
	if err := t.ops.unlinkat(t.directory.fd, name); err != nil {
		_ = entry.Close()
		return false, err
	}
	closeErr := entry.Close()
	return true, closeErr
}

func (t *downloadTarget) randomName(prefix string) (string, error) {
	if t.random == nil {
		return "", errors.New("random source is not configured")
	}
	value := make([]byte, randomNameBytes)
	if _, err := io.ReadFull(t.random, value); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(value), nil
}

func linkFileDescriptor(fileFD, directoryFD int, name string) error {
	err := rawLinkat(fileFD, "", directoryFD, name, atEmptyPath)
	if err == nil {
		return nil
	}
	if !errors.Is(err, syscall.ENOENT) && !errors.Is(err, syscall.EPERM) && !errors.Is(err, syscall.EINVAL) {
		return err
	}
	// Unprivileged linkat(AT_EMPTY_PATH) is denied on some kernels. Procfs is
	// the documented O_TMPFILE fallback and still names this exact open fd.
	return rawLinkat(atFDCWD, "/proc/self/fd/"+strconv.Itoa(fileFD), directoryFD, name, atSymlinkFollow)
}

func rawLinkat(oldDirectoryFD int, oldName string, newDirectoryFD int, newName string, flags int) error {
	oldPointer, err := syscall.BytePtrFromString(oldName)
	if err != nil {
		return err
	}
	newPointer, err := syscall.BytePtrFromString(newName)
	if err != nil {
		return err
	}
	_, _, errno := syscall.Syscall6(syscall.SYS_LINKAT, uintptr(oldDirectoryFD), uintptr(unsafe.Pointer(oldPointer)), uintptr(newDirectoryFD), uintptr(unsafe.Pointer(newPointer)), uintptr(flags), 0)
	if errno != 0 {
		return errno
	}
	return nil
}

func renameat2Exchange(oldDirectoryFD int, oldName string, newDirectoryFD int, newName string) error {
	oldPointer, err := syscall.BytePtrFromString(oldName)
	if err != nil {
		return err
	}
	newPointer, err := syscall.BytePtrFromString(newName)
	if err != nil {
		return err
	}
	_, _, errno := syscall.Syscall6(sysRenameat2AMD64, uintptr(oldDirectoryFD), uintptr(unsafe.Pointer(oldPointer)), uintptr(newDirectoryFD), uintptr(unsafe.Pointer(newPointer)), uintptr(renameExchange), 0)
	if errno != 0 {
		return errno
	}
	return nil
}
