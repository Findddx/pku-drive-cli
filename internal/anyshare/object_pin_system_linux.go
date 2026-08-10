//go:build linux

package anyshare

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

const (
	objectPinMaximumBytes = 4096
	oPathLinux            = 0x200000
)

type systemObjectPinLocation struct {
	root      string
	directory string
	uid       uint32
	gid       uint32
}

var pkuSystemObjectPinLocation = systemObjectPinLocation{
	root:      "/",
	directory: "etc/pku-drive-cli",
	uid:       0,
	gid:       0,
}

func (l systemObjectPinLocation) fullDirectory() string {
	return filepath.Join(l.root, filepath.FromSlash(l.directory))
}

// readSystemObjectPinConfig reads a public, root-managed trust anchor without
// following symlinks. Every opened directory remains descriptor-bound while
// the next component is inspected.
func readSystemObjectPinConfig(location systemObjectPinLocation) (objectPinConfig, error) {
	var config objectPinConfig
	if !validSystemObjectPinLocation(location) {
		return config, errInvalidObjectPinConfig
	}
	fd, err := syscall.Open(location.root, oPathLinux|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return config, fmt.Errorf("open system trust root: %w", err)
	}
	// Keep the small, fixed directory chain open until return. Each descriptor
	// is closed exactly once; retrying close after an error can close an
	// unrelated descriptor if Linux already released and reused its number.
	directoryFDs := []int{fd}
	defer func() {
		for index := len(directoryFDs) - 1; index >= 0; index-- {
			_ = syscall.Close(directoryFDs[index])
		}
	}()
	if err := validateSystemPinDirectory(fd, location, false); err != nil {
		return config, err
	}

	components := strings.Split(filepath.ToSlash(location.directory), "/")
	for index, component := range components {
		nextFD, openErr := syscall.Openat(fd, component, oPathLinux|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
		if openErr != nil {
			return config, fmt.Errorf("open system trust directory: %w", openErr)
		}
		directoryFDs = append(directoryFDs, nextFD)
		fd = nextFD
		if err := validateSystemPinDirectory(fd, location, index == len(components)-1); err != nil {
			return config, err
		}
	}

	fileFD, err := syscall.Openat(fd, objectPinFileName, syscall.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return config, fmt.Errorf("open system trust file: %w", err)
	}
	file := os.NewFile(uintptr(fileFD), objectPinFileName)
	if file == nil {
		_ = syscall.Close(fileFD)
		return config, errors.New("open system trust file: file handle failed")
	}
	var stat syscall.Stat_t
	if err := syscall.Fstat(fileFD, &stat); err != nil {
		_ = file.Close()
		return config, fmt.Errorf("inspect system trust file: %w", err)
	}
	if stat.Mode&syscall.S_IFMT != syscall.S_IFREG || stat.Uid != location.uid || stat.Gid != location.gid || stat.Mode&0o7777 != 0o644 || stat.Size < 0 || stat.Size > objectPinMaximumBytes {
		_ = file.Close()
		return config, errInvalidObjectPinConfig
	}
	data, readErr := io.ReadAll(io.LimitReader(file, objectPinMaximumBytes+1))
	closeErr := file.Close()
	if readErr != nil {
		return config, fmt.Errorf("read system trust file: %w", readErr)
	}
	if closeErr != nil {
		return config, fmt.Errorf("close system trust file: %w", closeErr)
	}
	if len(data) > objectPinMaximumBytes || json.Unmarshal(data, &config) != nil {
		return objectPinConfig{}, errInvalidObjectPinConfig
	}
	return config, nil
}

func validSystemObjectPinLocation(location systemObjectPinLocation) bool {
	if location.root == "" || !filepath.IsAbs(location.root) || filepath.Clean(location.root) != location.root || location.directory == "" || filepath.IsAbs(location.directory) || strings.ContainsRune(location.directory, '\x00') {
		return false
	}
	for _, component := range strings.Split(filepath.ToSlash(location.directory), "/") {
		if component == "" || component == "." || component == ".." {
			return false
		}
	}
	return true
}

func validateSystemPinDirectory(fd int, location systemObjectPinLocation, final bool) error {
	var stat syscall.Stat_t
	if err := syscall.Fstat(fd, &stat); err != nil {
		return fmt.Errorf("inspect system trust directory: %w", err)
	}
	if stat.Mode&syscall.S_IFMT != syscall.S_IFDIR || stat.Uid != location.uid || stat.Gid != location.gid {
		return errInvalidObjectPinConfig
	}
	mode := stat.Mode & 0o7777
	if final {
		if mode != 0o755 {
			return errInvalidObjectPinConfig
		}
		return nil
	}
	if mode&0o022 != 0 {
		return errInvalidObjectPinConfig
	}
	return nil
}
