// Package securefs provides small, Linux-oriented secure persistence helpers.
package securefs

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"
)

// WriteJSONAtomic serializes value to a same-directory private temporary file
// and atomically replaces path.
func WriteJSONAtomic(path string, value any) error {
	parent, name, err := splitAbsoluteEntryPath(path)
	if err != nil {
		return err
	}
	dir, err := OpenPrivateDir(parent)
	if err != nil {
		return err
	}
	defer dir.Close()
	return WriteJSONAtomicAt(dir, name, value)
}

// WriteJSONAtomicAt atomically writes the entry named by name relative to dir.
func WriteJSONAtomicAt(dir *Dir, name string, value any) error {
	if err := validateEntryName(name); err != nil {
		return err
	}
	if _, err := checkRegular0600At(dir, name); err != nil {
		return err
	}
	tmp, tmpName, err := createTempAt(dir, name)
	if err != nil {
		return err
	}
	keepTemp := true
	defer func() {
		if keepTemp {
			_ = syscall.Unlinkat(dir.fd, tmpName)
		}
	}()
	if err := json.NewEncoder(tmp).Encode(value); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("encode JSON: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync temporary file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temporary file: %w", err)
	}
	if _, err := checkRegular0600At(dir, name); err != nil {
		return err
	}
	if err := syscall.Renameat(dir.fd, tmpName, dir.fd, name); err != nil {
		return fmt.Errorf("replace JSON file: %w", err)
	}
	keepTemp = false
	if err := syscall.Fsync(dir.fd); err != nil {
		return fmt.Errorf("sync parent directory: %w", err)
	}
	return nil
}

// ReadJSON0600 decodes a private regular JSON file, rejecting unsafe files.
func ReadJSON0600(path string, value any) error {
	parent, name, err := splitAbsoluteEntryPath(path)
	if err != nil {
		return err
	}
	dir, err := OpenPrivateDir(parent)
	if err != nil {
		return err
	}
	defer dir.Close()
	return ReadJSON0600At(dir, name, value)
}

// ReadJSON0600At decodes a private regular entry named by name relative to dir.
func ReadJSON0600At(dir *Dir, name string, value any) error {
	if err := validateEntryName(name); err != nil {
		return err
	}
	f, err := openRegular0600At(dir, name, syscall.O_RDONLY)
	if err != nil {
		return err
	}
	defer f.Close()
	decoder := json.NewDecoder(f)
	if err := decoder.Decode(value); err != nil {
		return fmt.Errorf("decode JSON: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("decode JSON: multiple values")
		}
		return fmt.Errorf("decode JSON: %w", err)
	}
	return nil
}

// Remove0600 removes a private regular file without following a symlink.
func Remove0600(path string) error {
	parent, name, err := splitAbsoluteEntryPath(path)
	if err != nil {
		return err
	}
	dir, err := OpenPrivateDir(parent)
	if err != nil {
		return err
	}
	defer dir.Close()
	return Remove0600At(dir, name)
}

// Remove0600At removes a private regular entry relative to dir.
func Remove0600At(dir *Dir, name string) error {
	return remove0600AtWithOps(dir, name, hostDeletionOps())
}

type deletionOps struct {
	renameNoReplace func(int, string, int, string) error
	openat          func(int, string, int, uint32) (int, error)
	fstat           func(int, *syscall.Stat_t) error
	close           func(int) error
	unlinkat        func(int, string) error
	fsync           func(int) error
}

func hostDeletionOps() deletionOps {
	return deletionOps{
		renameNoReplace: renameat2NoReplace,
		openat:          syscall.Openat,
		fstat:           syscall.Fstat,
		close:           syscall.Close,
		unlinkat:        syscall.Unlinkat,
		fsync:           syscall.Fsync,
	}
}

func remove0600AtWithOps(dir *Dir, name string, ops deletionOps) error {
	if err := validateEntryName(name); err != nil {
		return err
	}
	quarantine, exists, err := isolateForDeletion(dir, name, ops)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	fd, err := ops.openat(dir.fd, quarantine, syscall.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return restoreAfterFailedDeletion(dir, name, quarantine, fmt.Errorf("open isolated private file: %w", err), ops)
	}
	var stat syscall.Stat_t
	if err := ops.fstat(fd, &stat); err != nil {
		_ = ops.close(fd)
		return restoreAfterFailedDeletion(dir, name, quarantine, fmt.Errorf("inspect isolated private file: %w", err), ops)
	}
	if err := validateRegular0600(quarantine, &stat); err != nil {
		_ = ops.close(fd)
		return restoreAfterFailedDeletion(dir, name, quarantine, err, ops)
	}
	if err := ops.close(fd); err != nil {
		return restoreAfterFailedDeletion(dir, name, quarantine, fmt.Errorf("close isolated private file: %w", err), ops)
	}
	if err := ops.unlinkat(dir.fd, quarantine); err != nil {
		return restoreAfterFailedDeletion(dir, name, quarantine, fmt.Errorf("remove isolated private file: %w", err), ops)
	}
	if err := ops.fsync(dir.fd); err != nil {
		return fmt.Errorf("sync parent directory: %w", err)
	}
	return nil
}

func isolateForDeletion(dir *Dir, name string, ops deletionOps) (string, bool, error) {
	var random [16]byte
	for range 10 {
		if _, err := rand.Read(random[:]); err != nil {
			return "", false, fmt.Errorf("random quarantine name: %w", err)
		}
		quarantine := ".pku-drive-delete-" + hex.EncodeToString(random[:])
		err := ops.renameNoReplace(dir.fd, name, dir.fd, quarantine)
		if errors.Is(err, syscall.ENOENT) {
			return "", false, nil
		}
		if errors.Is(err, syscall.EEXIST) {
			continue
		}
		if err != nil {
			return "", false, fmt.Errorf("isolate private file for deletion: %w", err)
		}
		return quarantine, true, nil
	}
	return "", false, fmt.Errorf("isolate private file for deletion: too many quarantine name collisions")
}

func restoreAfterFailedDeletion(dir *Dir, name, quarantine string, cause error, ops deletionOps) error {
	if err := ops.renameNoReplace(dir.fd, quarantine, dir.fd, name); err != nil {
		return fmt.Errorf("%w; isolated candidate %q retained because restore failed: %v", cause, quarantine, err)
	}
	if err := ops.fsync(dir.fd); err != nil {
		return fmt.Errorf("%w; restored original name but directory sync failed: %v", cause, err)
	}
	return cause
}

func renameat2NoReplace(oldDirFD int, oldName string, newDirFD int, newName string) error {
	oldPath, err := syscall.BytePtrFromString(oldName)
	if err != nil {
		return err
	}
	newPath, err := syscall.BytePtrFromString(newName)
	if err != nil {
		return err
	}
	const renameNoReplace = 1
	// SYS_RENAMEAT2 is 316 on the approved Linux amd64 target.
	const sysRenameat2LinuxAMD64 = 316
	_, _, errno := syscall.Syscall6(
		sysRenameat2LinuxAMD64,
		uintptr(oldDirFD),
		uintptr(unsafe.Pointer(oldPath)),
		uintptr(newDirFD),
		uintptr(unsafe.Pointer(newPath)),
		renameNoReplace,
		0,
	)
	if errno != 0 {
		return errno
	}
	return nil
}

func checkRegular0600At(dir *Dir, name string) (bool, error) {
	f, err := openRegular0600At(dir, name, syscall.O_RDONLY)
	if err != nil {
		if errors.Is(err, syscall.ENOENT) {
			return false, nil
		}
		return false, err
	}
	if err := f.Close(); err != nil {
		return true, fmt.Errorf("close private file: %w", err)
	}
	return true, nil
}

func openRegular0600At(dir *Dir, name string, flags int) (*os.File, error) {
	fd, err := syscall.Openat(dir.fd, name, flags|syscall.O_NONBLOCK|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open private file: %w", err)
	}
	f := os.NewFile(uintptr(fd), name)
	if f == nil {
		_ = syscall.Close(fd)
		return nil, fmt.Errorf("open private file: could not create file handle")
	}
	var stat syscall.Stat_t
	if err := syscall.Fstat(fd, &stat); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("inspect private file: %w", err)
	}
	if err := validateRegular0600(name, &stat); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

func createTempAt(dir *Dir, name string) (*os.File, string, error) {
	var random [16]byte
	for range 10 {
		if _, err := rand.Read(random[:]); err != nil {
			return nil, "", fmt.Errorf("random temporary name: %w", err)
		}
		tmpName := "." + name + ".tmp-" + hex.EncodeToString(random[:])
		fd, err := syscall.Openat(dir.fd, tmpName, syscall.O_WRONLY|syscall.O_CREAT|syscall.O_EXCL|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0o600)
		if errors.Is(err, syscall.EEXIST) {
			continue
		}
		if err != nil {
			return nil, "", fmt.Errorf("create temporary file: %w", err)
		}
		if err := syscall.Fchmod(fd, 0o600); err != nil {
			_ = syscall.Close(fd)
			_ = syscall.Unlinkat(dir.fd, tmpName)
			return nil, "", fmt.Errorf("chmod temporary file: %w", err)
		}
		var stat syscall.Stat_t
		if err := syscall.Fstat(fd, &stat); err != nil {
			_ = syscall.Close(fd)
			_ = syscall.Unlinkat(dir.fd, tmpName)
			return nil, "", fmt.Errorf("inspect temporary file: %w", err)
		}
		if err := validateRegular0600(tmpName, &stat); err != nil {
			_ = syscall.Close(fd)
			_ = syscall.Unlinkat(dir.fd, tmpName)
			return nil, "", err
		}
		f := os.NewFile(uintptr(fd), tmpName)
		if f == nil {
			_ = syscall.Close(fd)
			_ = syscall.Unlinkat(dir.fd, tmpName)
			return nil, "", fmt.Errorf("create temporary file: could not create file handle")
		}
		return f, tmpName, nil
	}
	return nil, "", fmt.Errorf("create temporary file: too many name collisions")
}

func validateRegular0600(path string, stat *syscall.Stat_t) error {
	if stat.Mode&syscall.S_IFMT != syscall.S_IFREG {
		return fmt.Errorf("unsafe non-regular file: %s", path)
	}
	if int(stat.Uid) != os.Geteuid() {
		return fmt.Errorf("unsafe file owner: %s", path)
	}
	if stat.Mode&0o7777 != 0o600 {
		return fmt.Errorf("unsafe file mode %o: %s", stat.Mode&0o7777, path)
	}
	return nil
}

func validateEntryName(name string) error {
	if name == "" || strings.ContainsRune(name, '\x00') || strings.ContainsRune(name, filepath.Separator) || filepath.Base(name) != name || name == "." || name == ".." {
		return fmt.Errorf("unsafe file name: %q", name)
	}
	return nil
}

func splitAbsoluteEntryPath(path string) (string, string, error) {
	if err := validateAbsolutePath(path); err != nil {
		return "", "", err
	}
	parent, name := filepath.Dir(path), filepath.Base(path)
	if err := validateEntryName(name); err != nil {
		return "", "", err
	}
	return parent, name, nil
}
