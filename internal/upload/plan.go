package upload

import (
	"errors"
	"math"

	"github.com/Findddx/pku-drive-cli/internal/apperr"
)

const (
	mib                    int64 = 1024 * 1024
	defaultSingleThreshold       = 16 * mib
	defaultPartSize              = 4 * mib
	maxConcurrency               = 4
)

// Limits are the multipart restrictions advertised by the server.
type Limits struct {
	PartMinSize int64
	PartMaxSize int64
	PartMaxNum  int
}

// Plan describes whether and how a local file is uploaded.
type Plan struct {
	Multipart   bool
	PartSize    int64
	PartCount   int
	Concurrency int
}

// BuildPlan derives an overflow-safe upload plan from a file size and the
// server's multipart constraints.
func BuildPlan(size int64, limits Limits, concurrency int) (Plan, error) {
	if size < 0 {
		return Plan{}, planError("negative file size")
	}
	if limits.PartMinSize <= 0 || limits.PartMaxSize <= 0 || limits.PartMinSize > limits.PartMaxSize || limits.PartMaxNum <= 0 {
		return Plan{}, planError("invalid multipart limits")
	}

	threshold := saturatingDouble(limits.PartMinSize)
	if threshold < defaultSingleThreshold {
		threshold = defaultSingleThreshold
	}
	if size <= threshold {
		return Plan{Concurrency: 1}, nil
	}

	partSize := defaultPartSize
	if partSize < limits.PartMinSize {
		partSize = limits.PartMinSize
	}
	if partSize > limits.PartMaxSize {
		partSize = limits.PartMaxSize
	}

	required := ceilDivPositive(size, int64(limits.PartMaxNum))
	roundedRequired, ok := roundUpMiB(required)
	if !ok {
		return Plan{}, planError("file exceeds multipart limits")
	}
	if roundedRequired > partSize {
		partSize = roundedRequired
	}
	if partSize > limits.PartMaxSize {
		return Plan{}, planError("file exceeds multipart limits")
	}

	partCount64 := ceilDivPositive(size, partSize)
	if partCount64 > int64(limits.PartMaxNum) {
		return Plan{}, planError("file exceeds multipart limits")
	}
	partCount := int(partCount64)
	workers := concurrency
	if workers < 1 {
		workers = 1
	}
	if workers > maxConcurrency {
		workers = maxConcurrency
	}
	if workers > partCount {
		workers = partCount
	}
	return Plan{Multipart: true, PartSize: partSize, PartCount: partCount, Concurrency: workers}, nil
}

func saturatingDouble(value int64) int64 {
	if value > math.MaxInt64/2 {
		return math.MaxInt64
	}
	return value * 2
}

func ceilDivPositive(dividend, divisor int64) int64 {
	return dividend/divisor + boolInt64(dividend%divisor != 0)
}

func roundUpMiB(value int64) (int64, bool) {
	remainder := value % mib
	if remainder == 0 {
		return value, true
	}
	increment := mib - remainder
	if value > math.MaxInt64-increment {
		return 0, false
	}
	return value + increment, true
}

func boolInt64(value bool) int64 {
	if value {
		return 1
	}
	return 0
}

func planError(message string) error {
	return apperr.Wrap(apperr.Usage, "upload plan", message, errors.New("invalid upload constraints"))
}
