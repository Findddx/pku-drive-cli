package picker

import (
	"context"
	"errors"
	"io"
	"reflect"
	"sync"
	"testing"
)

type fakeSource struct {
	mu      sync.Mutex
	entries map[string][]Entry
	errors  map[string]error
	calls   []string
}

func (s *fakeSource) List(ctx context.Context, relativeDir string) ([]Entry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, relativeDir)
	if err := s.errors[relativeDir]; err != nil {
		return nil, err
	}
	return append([]Entry(nil), s.entries[relativeDir]...), nil
}

func (s *fakeSource) listed() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.calls...)
}

type fakeSession struct {
	mu       sync.Mutex
	keys     []key
	read     func(context.Context) (key, error)
	readErr  error
	drawErr  error
	closeErr error
	screens  []string
	closed   bool
}

func (s *fakeSession) Size() (int, int) { return 80, 12 }

func (s *fakeSession) Draw(screen string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.screens = append(s.screens, screen)
	return s.drawErr
}

func (s *fakeSession) ReadKey(ctx context.Context) (key, error) {
	if s.read != nil {
		return s.read(ctx)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.keys) > 0 {
		pressed := s.keys[0]
		s.keys = s.keys[1:]
		return pressed, nil
	}
	if s.readErr != nil {
		return keyUnknown, s.readErr
	}
	return keyUnknown, io.EOF
}

func (s *fakeSession) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return s.closeErr
}

func (s *fakeSession) wasClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

func TestRunSessionListsLazilyAndPreservesCrossDirectorySelection(t *testing.T) {
	directory := Entry{ID: "dir", Path: "docs", Name: "docs", Type: EntryDirectory}
	rootFile := Entry{ID: "a", Path: "a.txt", Name: "a.txt", Type: EntryFile, Size: 10}
	nestedFile := Entry{ID: "z", Path: "docs/z.txt", Name: "z.txt", Type: EntryFile, Size: 20}
	source := &fakeSource{entries: map[string][]Entry{
		"docs": {nestedFile},
	}}
	terminal := &fakeSession{keys: []key{
		keyRight, // enter docs; this is the first time its children are listed
		keySpace, // select docs/z.txt
		keyLeft,  // restore root without listing it again
		keyDown,
		keySpace, // select a.txt
		keyEnter,
	}}

	selected, err := runSession(context.Background(), source, terminal, "Pick", []Entry{rootFile, directory})
	if err != nil {
		t.Fatalf("runSession: %v", err)
	}
	want := []Entry{rootFile, nestedFile}
	if !reflect.DeepEqual(selected, want) {
		t.Fatalf("selected = %#v, want %#v", selected, want)
	}
	if calls := source.listed(); !reflect.DeepEqual(calls, []string{"docs"}) {
		t.Fatalf("List calls = %v, want only entered directory", calls)
	}
	if !terminal.wasClosed() {
		t.Fatal("terminal was not closed")
	}
}

func TestRunSessionEmptyConfirmation(t *testing.T) {
	terminal := &fakeSession{keys: []key{keyEnter}}
	selected, err := runSession(context.Background(), &fakeSource{}, terminal, "", nil)
	if err != nil {
		t.Fatalf("runSession: %v", err)
	}
	if selected == nil || len(selected) != 0 {
		t.Fatalf("selected = %#v, want non-nil empty slice", selected)
	}
	if !terminal.wasClosed() {
		t.Fatal("terminal was not closed")
	}
}

func TestRunSessionCancelAndInterrupt(t *testing.T) {
	tests := []struct {
		name string
		key  key
		want error
	}{
		{name: "quit", key: keyQuit, want: ErrCanceled},
		{name: "escape", key: keyEscape, want: ErrCanceled},
		{name: "ctrl-c", key: keyInterrupt, want: context.Canceled},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			terminal := &fakeSession{keys: []key{test.key}}
			_, err := runSession(context.Background(), &fakeSource{}, terminal, "", nil)
			if !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want errors.Is(_, %v)", err, test.want)
			}
			if !terminal.wasClosed() {
				t.Fatal("terminal was not closed")
			}
		})
	}
}

func TestRunSessionRestoresOnEveryErrorPath(t *testing.T) {
	sentinel := errors.New("sentinel")
	tests := []struct {
		name     string
		source   *fakeSource
		terminal *fakeSession
	}{
		{
			name:     "EOF",
			source:   &fakeSource{},
			terminal: &fakeSession{readErr: io.EOF},
		},
		{
			name:     "read error",
			source:   &fakeSource{},
			terminal: &fakeSession{readErr: sentinel},
		},
		{
			name:     "draw error",
			source:   &fakeSource{},
			terminal: &fakeSession{drawErr: sentinel},
		},
		{
			name: "list error",
			source: &fakeSource{errors: map[string]error{
				"dir": sentinel,
			}},
			terminal: &fakeSession{keys: []key{keyRight}},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := []Entry(nil)
			if test.name == "list error" {
				root = []Entry{{ID: "dir", Path: "dir", Name: "dir", Type: EntryDirectory}}
			}
			_, _ = runSession(context.Background(), test.source, test.terminal, "", root)
			if !test.terminal.wasClosed() {
				t.Fatal("terminal was not closed")
			}
		})
	}
}

func TestRunSessionContextCancellationClosesTerminal(t *testing.T) {
	started := make(chan struct{})
	terminal := &fakeSession{read: func(ctx context.Context) (key, error) {
		close(started)
		<-ctx.Done()
		return keyUnknown, ctx.Err()
	}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := runSession(ctx, &fakeSource{}, terminal, "", nil)
		done <- err
	}()
	<-started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if !terminal.wasClosed() {
		t.Fatal("terminal was not closed")
	}
}

func TestRunSessionJoinsCloseError(t *testing.T) {
	closeErr := errors.New("close failed")
	terminal := &fakeSession{keys: []key{keyQuit}, closeErr: closeErr}
	_, err := runSession(context.Background(), &fakeSource{}, terminal, "", nil)
	if !errors.Is(err, ErrCanceled) || !errors.Is(err, closeErr) {
		t.Fatalf("joined error = %v, want cancellation and close errors", err)
	}
}
