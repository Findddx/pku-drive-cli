// Package picker implements the interactive file selector used for shared links.
package picker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
)

// EntryType identifies the kind of an entry returned by a Source.
type EntryType uint8

const (
	EntryFile EntryType = iota
	EntryDirectory
)

// Entry is one file or directory in a shared link.
//
// ID is opaque to the picker and is returned unchanged. Path is relative to the
// share root and is used when asking Source for a directory's children. Size may
// be -1 when it is unknown.
type Entry struct {
	ID   string
	Path string
	Name string
	Type EntryType
	Size int64
}

// Source lists the immediate children of relativeDir. Implementations should
// not recursively expand descendants.
type Source interface {
	List(ctx context.Context, relativeDir string) ([]Entry, error)
}

// Options configures the interactive picker. Input and Output must both refer
// to terminals. Title is optional and is sanitized before display.
type Options struct {
	Input  *os.File
	Output *os.File
	Title  string
}

var (
	// ErrNotTerminal is returned when either side of the picker is not a TTY.
	ErrNotTerminal = errors.New("picker requires terminal input and output")
	// ErrCanceled is returned when the user presses q or Escape.
	ErrCanceled = errors.New("file selection canceled")
)

// Run opens an interactive, lazy directory browser. The returned files are
// sorted by Path (and then ID) so callers receive deterministic results.
func Run(ctx context.Context, source Source, options Options) ([]Entry, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if source == nil {
		return nil, errors.New("picker source is nil")
	}
	if err := validateTerminalFiles(options.Input, options.Output); err != nil {
		return nil, err
	}

	runCtx, stopSignals := signal.NotifyContext(ctx, pickerSignals()...)
	defer stopSignals()

	entries, err := source.List(runCtx, "")
	if err != nil {
		return nil, fmt.Errorf("list shared-link root: %w", err)
	}
	if err := runCtx.Err(); err != nil {
		return nil, err
	}

	terminal, err := newTerminalSession(options.Input, options.Output)
	if err != nil {
		return nil, err
	}
	return runSession(runCtx, source, terminal, options.Title, entries)
}

type session interface {
	Size() (width, height int)
	Draw(screen string) error
	ReadKey(context.Context) (key, error)
	Close() error
}

func runSession(ctx context.Context, source Source, terminal session, title string, root []Entry) (selected []Entry, err error) {
	defer func() {
		if closeErr := terminal.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("restore terminal: %w", closeErr))
		}
	}()

	model := newModel(root)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		width, height := terminal.Size()
		rows := visibleRows(height)
		model.ensureVisible(rows)
		if err := terminal.Draw(render(model, title, width, height)); err != nil {
			return nil, fmt.Errorf("draw picker: %w", err)
		}

		pressed, err := terminal.ReadKey(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) && ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if errors.Is(err, io.EOF) {
				return nil, io.EOF
			}
			return nil, fmt.Errorf("read picker input: %w", err)
		}

		effect := model.handle(pressed, rows)
		switch effect.kind {
		case effectNone:
			continue
		case effectEnterDirectory:
			children, listErr := source.List(ctx, effect.entry.Path)
			if listErr != nil {
				return nil, fmt.Errorf("list shared-link directory: %w", listErr)
			}
			model.enter(effect.entry.Path, children)
		case effectLeaveDirectory:
			model.leave()
		case effectConfirm:
			selected = model.selection()
			sort.SliceStable(selected, func(i, j int) bool {
				if selected[i].Path != selected[j].Path {
					return selected[i].Path < selected[j].Path
				}
				return selected[i].ID < selected[j].ID
			})
			return selected, nil
		case effectCancel:
			return nil, ErrCanceled
		case effectInterrupt:
			return nil, context.Canceled
		}
	}
}
