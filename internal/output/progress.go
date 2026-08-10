package output

import (
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/Findddx/pku-drive-cli/internal/upload"
)

const progressInterval = time.Second

type progress struct {
	mu       sync.Mutex
	stderr   io.Writer
	enabled  bool
	now      func() time.Time
	total    int64
	advanced int64
	last     time.Time
	wrote    bool
	finished bool
	verb     string
}

func NewProgress(stderr io.Writer, quiet, interactive bool, now func() time.Time) upload.Progress {
	return newProgress(stderr, quiet, interactive, now, "Uploaded")
}

func NewDownloadProgress(stderr io.Writer, quiet, interactive bool, now func() time.Time) upload.Progress {
	return newProgress(stderr, quiet, interactive, now, "Downloaded")
}

func newProgress(stderr io.Writer, quiet, interactive bool, now func() time.Time, verb string) upload.Progress {
	if now == nil {
		now = time.Now
	}
	return &progress{stderr: stderr, enabled: !quiet && interactive, now: now, verb: verb}
}

func (p *progress) Started(total int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.enabled || p.finished {
		return
	}
	p.total, p.advanced, p.last = total, 0, p.now()
	p.write()
}

func (p *progress) Advanced(delta int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.enabled || p.finished {
		return
	}
	p.advanced += delta
	now := p.now()
	if now.Sub(p.last) < progressInterval {
		return
	}
	p.last = now
	p.write()
}

func (p *progress) Finished() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.enabled || p.finished {
		return
	}
	p.finished = true
	p.advanced = p.total
	p.write()
	if p.wrote {
		_, _ = io.WriteString(p.stderr, "\n")
	}
}

// AbortProgress terminates an interactive progress line without claiming that
// the operation completed. It is safe to call for quiet and non-interactive
// progress values and for implementations from another package.
func AbortProgress(value upload.Progress) {
	p, ok := value.(*progress)
	if !ok {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.enabled || p.finished {
		return
	}
	p.finished = true
	if !p.wrote {
		return
	}
	p.write()
	_, _ = io.WriteString(p.stderr, "\n")
}

func (p *progress) write() {
	_, _ = fmt.Fprintf(p.stderr, "\r%s %d/%d bytes", p.verb, p.advanced, p.total)
	p.wrote = true
}
