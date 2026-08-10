package upload_test

import (
	"math"
	"testing"

	"github.com/Findddx/pku-drive-cli/internal/apperr"
	"github.com/Findddx/pku-drive-cli/internal/upload"
)

const MiB int64 = 1024 * 1024

func TestBuildPlanBoundaries(t *testing.T) {
	tests := []struct {
		name        string
		size        int64
		limits      upload.Limits
		concurrency int
		want        upload.Plan
	}{
		{
			name: "zero byte single put", size: 0,
			limits: upload.Limits{PartMinSize: MiB, PartMaxSize: 10 * MiB, PartMaxNum: 100}, concurrency: 4,
			want: upload.Plan{Multipart: false, Concurrency: 1},
		},
		{
			name: "exact threshold single put", size: 16 * MiB,
			limits: upload.Limits{PartMinSize: MiB, PartMaxSize: 10 * MiB, PartMaxNum: 100}, concurrency: 4,
			want: upload.Plan{Multipart: false, Concurrency: 1},
		},
		{
			name: "one byte above threshold multipart", size: 16*MiB + 1,
			limits: upload.Limits{PartMinSize: MiB, PartMaxSize: 10 * MiB, PartMaxNum: 100}, concurrency: 4,
			want: upload.Plan{Multipart: true, PartSize: 4 * MiB, PartCount: 5, Concurrency: 4},
		},
		{
			name: "server minimum raises threshold and part size", size: 16*MiB + 1,
			limits: upload.Limits{PartMinSize: 8 * MiB, PartMaxSize: 16 * MiB, PartMaxNum: 100}, concurrency: 4,
			want: upload.Plan{Multipart: true, PartSize: 8 * MiB, PartCount: 3, Concurrency: 3},
		},
		{
			name: "required part rounds up to MiB", size: 20*MiB + 1,
			limits: upload.Limits{PartMinSize: MiB, PartMaxSize: 8 * MiB, PartMaxNum: 4}, concurrency: 9,
			want: upload.Plan{Multipart: true, PartSize: 6 * MiB, PartCount: 4, Concurrency: 4},
		},
		{
			name: "maximum part size and count exact", size: 32 * MiB,
			limits: upload.Limits{PartMinSize: MiB, PartMaxSize: 8 * MiB, PartMaxNum: 4}, concurrency: 0,
			want: upload.Plan{Multipart: true, PartSize: 8 * MiB, PartCount: 4, Concurrency: 1},
		},
		{
			name: "concurrency capped by small part count", size: 17 * MiB,
			limits: upload.Limits{PartMinSize: MiB, PartMaxSize: 16 * MiB, PartMaxNum: 100}, concurrency: 4,
			want: upload.Plan{Multipart: true, PartSize: 4 * MiB, PartCount: 5, Concurrency: 4},
		},
		{
			name: "doubling minimum saturates without overflow", size: math.MaxInt64,
			limits: upload.Limits{PartMinSize: math.MaxInt64/2 + 1, PartMaxSize: math.MaxInt64, PartMaxNum: 1}, concurrency: 4,
			want: upload.Plan{Multipart: false, Concurrency: 1},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := upload.BuildPlan(tt.size, tt.limits, tt.concurrency)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("plan=%+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestBuildPlanRejectsImpossibleAndInvalidInputs(t *testing.T) {
	tests := []struct {
		name   string
		size   int64
		limits upload.Limits
	}{
		{name: "negative size", size: -1, limits: upload.Limits{PartMinSize: 1, PartMaxSize: 1, PartMaxNum: 1}},
		{name: "zero minimum", limits: upload.Limits{PartMinSize: 0, PartMaxSize: MiB, PartMaxNum: 1}},
		{name: "negative minimum", limits: upload.Limits{PartMinSize: -1, PartMaxSize: MiB, PartMaxNum: 1}},
		{name: "zero maximum", limits: upload.Limits{PartMinSize: 1, PartMaxSize: 0, PartMaxNum: 1}},
		{name: "minimum above maximum", limits: upload.Limits{PartMinSize: 2, PartMaxSize: 1, PartMaxNum: 1}},
		{name: "zero max count", limits: upload.Limits{PartMinSize: 1, PartMaxSize: MiB, PartMaxNum: 0}},
		{name: "negative max count", limits: upload.Limits{PartMinSize: 1, PartMaxSize: MiB, PartMaxNum: -1}},
		{
			name: "one byte beyond server capacity", size: 32*MiB + 1,
			limits: upload.Limits{PartMinSize: MiB, PartMaxSize: 8 * MiB, PartMaxNum: 4},
		},
		{
			name: "MiB round up overflows", size: math.MaxInt64,
			limits: upload.Limits{PartMinSize: 1, PartMaxSize: math.MaxInt64, PartMaxNum: 1},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := upload.BuildPlan(tt.size, tt.limits, 4)
			assertCategory(t, err, apperr.Usage)
		})
	}
}
