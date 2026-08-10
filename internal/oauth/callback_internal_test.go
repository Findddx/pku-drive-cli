package oauth

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync"
	"testing"
)

type controlledDeadlineContext struct {
	context.Context
	done chan struct{}
}

func (c *controlledDeadlineContext) Done() <-chan struct{} { return c.done }

func (c *controlledDeadlineContext) Err() error {
	select {
	case <-c.done:
		return context.DeadlineExceeded
	default:
		return nil
	}
}

type closeRaceListener struct {
	net.Listener
	mu                  sync.Mutex
	acceptCalls         int
	closeCalls          int
	secondAcceptStarted chan struct{}
	allowAcceptReturn   chan struct{}
}

func (l *closeRaceListener) Accept() (net.Conn, error) {
	l.mu.Lock()
	l.acceptCalls++
	call := l.acceptCalls
	if call == 2 {
		close(l.secondAcceptStarted)
	}
	l.mu.Unlock()
	conn, err := l.Listener.Accept()
	if call >= 2 && err != nil {
		<-l.allowAcceptReturn
	}
	return conn, err
}

func (l *closeRaceListener) Close() error {
	err := l.Listener.Close()
	l.mu.Lock()
	l.closeCalls++
	if l.closeCalls == 2 {
		close(l.allowAcceptReturn)
	}
	l.mu.Unlock()
	return err
}

func TestCallbackTimeoutDoesNotCacheClosedListenerErrorAfterServeAcceptsRequest(t *testing.T) {
	base, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listener := &closeRaceListener{
		Listener: base, secondAcceptStarted: make(chan struct{}), allowAcceptReturn: make(chan struct{}),
	}
	t.Cleanup(func() {
		_ = listener.Close()
		_ = listener.Close()
	})
	callback := &Callback{
		listener: listener, redirectURI: "http://" + listener.Addr().String() + "/callback",
		result: make(chan callbackResult, 1), done: make(chan struct{}), stateReady: make(chan struct{}),
	}
	callback.server = &http.Server{Handler: http.HandlerFunc(callback.handle)}
	go func() { _ = callback.server.Serve(listener) }()

	response, err := http.Get("http://" + listener.Addr().String() + "/wrong")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("wrong-path status=%d, want 404", response.StatusCode)
	}
	<-listener.secondAcceptStarted
	ctx := &controlledDeadlineContext{Context: context.Background(), done: make(chan struct{})}
	close(ctx.done)
	_, waitErr := callback.Wait(ctx, "right-state")
	if !errors.Is(waitErr, context.DeadlineExceeded) {
		t.Fatalf("Wait() error=%v, want context deadline exceeded", waitErr)
	}
	if err := callback.Close(); err != nil {
		t.Fatalf("idempotent Close() error=%v, want nil", err)
	}
}
