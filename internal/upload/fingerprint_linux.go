//go:build linux

// Package upload provides safe local primitives used by upload workflows.
package upload

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"hash"
	"hash/crc32"
	"io"
	"os"
	"syscall"

	"github.com/Findddx/pku-drive-cli/internal/apperr"
)

const (
	sliceSize             = 200 * 1024
	fingerprintBufferSize = 4 * 1024 * 1024
)

// Fingerprint identifies an opened local file and records the checksums needed
// by the upload protocol.
type Fingerprint struct {
	Device   uint64 `json:"device"`
	Inode    uint64 `json:"inode"`
	Size     int64  `json:"size"`
	MtimeNS  int64  `json:"mtime_ns"`
	MD5      string `json:"md5"`
	SliceMD5 string `json:"slice_md5"`
	CRC32    string `json:"crc32"`
}

// OpenFingerprint opens path without following its final symlink, fingerprints
// that exact regular-file descriptor in one pass, and returns it rewound.
func OpenFingerprint(path string) (*os.File, Fingerprint, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, Fingerprint{}, localError("open upload file", err)
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = syscall.Close(fd)
		return nil, Fingerprint{}, localError("open upload file", fmt.Errorf("could not create file handle"))
	}
	fail := func(op string, err error) (*os.File, Fingerprint, error) {
		_ = file.Close()
		return nil, Fingerprint{}, localError(op, err)
	}

	var stat syscall.Stat_t
	if err := syscall.Fstat(fd, &stat); err != nil {
		return fail("inspect upload file", err)
	}
	if stat.Mode&syscall.S_IFMT != syscall.S_IFREG {
		return fail("inspect upload file", fmt.Errorf("not a regular file"))
	}

	fullMD5 := md5.New()
	sliceMD5 := md5.New()
	crc := crc32.NewIEEE()
	prefix := &prefixHashWriter{hash: sliceMD5, remaining: sliceSize}
	// Hide os.File's WriterTo method so CopyBuffer uses the large caller-owned
	// buffer. This materially reduces read syscall latency on the shared campus
	// storage that holds multi-gigabyte upload sources.
	reader := struct{ io.Reader }{Reader: file}
	if _, err := io.CopyBuffer(io.MultiWriter(fullMD5, crc, prefix), reader, make([]byte, fingerprintBufferSize)); err != nil {
		return fail("fingerprint upload file", err)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return fail("rewind upload file", err)
	}

	return file, Fingerprint{
		Device:   uint64(stat.Dev),
		Inode:    stat.Ino,
		Size:     stat.Size,
		MtimeNS:  stat.Mtim.Nano(),
		MD5:      upperHex(fullMD5.Sum(nil)),
		SliceMD5: upperHex(sliceMD5.Sum(nil)),
		CRC32:    upperHex(crc.Sum(nil)),
	}, nil
}

// Unchanged reports whether file still has the identity and metadata captured
// by the fingerprint. It inspects the supplied descriptor, not a pathname.
func (f Fingerprint) Unchanged(file *os.File) (bool, error) {
	if file == nil {
		return false, localError("inspect upload file", fmt.Errorf("missing file descriptor"))
	}
	var stat syscall.Stat_t
	if err := syscall.Fstat(int(file.Fd()), &stat); err != nil {
		return false, localError("inspect upload file", err)
	}
	return uint64(stat.Dev) == f.Device && stat.Ino == f.Inode && stat.Size == f.Size && stat.Mtim.Nano() == f.MtimeNS, nil
}

type prefixHashWriter struct {
	hash      hash.Hash
	remaining int
}

func (w *prefixHashWriter) Write(p []byte) (int, error) {
	if len(p) <= w.remaining {
		_, _ = w.hash.Write(p)
		w.remaining -= len(p)
	} else if w.remaining > 0 {
		_, _ = w.hash.Write(p[:w.remaining])
		w.remaining = 0
	}
	return len(p), nil
}

func upperHex(value []byte) string {
	encoded := make([]byte, hex.EncodedLen(len(value)))
	hex.Encode(encoded, value)
	for i, b := range encoded {
		if b >= 'a' && b <= 'f' {
			encoded[i] -= 'a' - 'A'
		}
	}
	return string(encoded)
}

func localError(op string, err error) error {
	return apperr.Wrap(apperr.Local, op, "local file operation failed", err)
}
