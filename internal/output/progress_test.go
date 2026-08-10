package output_test

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/Findddx/pku-drive-cli/internal/output"
)

func TestProgressInteractiveIsRateBoundedAndFinishesOnce(t *testing.T) {
	// Mutation caught: writing on every upload callback or producing duplicate/missing terminal newlines.
	var stderr bytes.Buffer
	now := time.Date(2026, 8, 9, 0, 0, 0, 0, time.UTC)
	progress := output.NewProgress(&stderr, false, true, func() time.Time { return now })
	progress.Started(100)
	initial := stderr.String()
	for i := 0; i < 20; i++ {
		progress.Advanced(1)
	}
	if stderr.String() != initial {
		t.Fatalf("updates were not rate bounded: %q", stderr.String())
	}
	now = now.Add(time.Second)
	progress.Advanced(10)
	if stderr.String() == initial {
		t.Fatal("no periodic update after rate interval")
	}
	progress.Finished()
	progress.Finished()
	if strings.Count(stderr.String(), "\n") != 1 || !strings.HasSuffix(stderr.String(), "\n") {
		t.Fatalf("final output=%q", stderr.String())
	}
}

func TestProgressQuietAndNoninteractiveSuppressPeriodicOutput(t *testing.T) {
	tests := []struct {
		name               string
		quiet, interactive bool
	}{{"quiet", true, true}, {"pipe", false, false}}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// Mutation caught: ignoring quiet mode or treating a pipe as an interactive terminal.
			var stderr bytes.Buffer
			now := time.Unix(0, 0)
			progress := output.NewProgress(&stderr, test.quiet, test.interactive, func() time.Time { return now })
			progress.Started(10)
			now = now.Add(10 * time.Second)
			progress.Advanced(5)
			progress.Finished()
			if stderr.Len() != 0 {
				t.Fatalf("stderr=%q", stderr.String())
			}
		})
	}
}

func TestDownloadProgressUsesDownloadVerb(t *testing.T) {
	var stderr bytes.Buffer
	progress := output.NewDownloadProgress(&stderr, false, true, time.Now)
	progress.Started(10)
	progress.Finished()
	if !strings.Contains(stderr.String(), "Downloaded 10/10 bytes") || strings.Contains(stderr.String(), "Uploaded") {
		t.Fatalf("stderr=%q", stderr.String())
	}
}

func TestAbortProgressDoesNotClaimCompletion(t *testing.T) {
	var stderr bytes.Buffer
	progress := output.NewDownloadProgress(&stderr, false, true, time.Now)
	progress.Started(10)
	progress.Advanced(4)
	output.AbortProgress(progress)
	if !strings.Contains(stderr.String(), "Downloaded 4/10 bytes") || strings.Contains(stderr.String(), "Downloaded 10/10 bytes") {
		t.Fatalf("stderr=%q", stderr.String())
	}
	if !strings.HasSuffix(stderr.String(), "\n") {
		t.Fatalf("missing terminal newline: %q", stderr.String())
	}
}

func TestAbortProgressBeforeStartProducesNoOutput(t *testing.T) {
	var stderr bytes.Buffer
	progress := output.NewDownloadProgress(&stderr, false, true, time.Now)
	output.AbortProgress(progress)
	if stderr.Len() != 0 {
		t.Fatalf("stderr=%q", stderr.String())
	}
}
