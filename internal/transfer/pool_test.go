package transfer

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Findddx/pku-drive-cli/internal/apperr"
)

func TestRunPoolSerialFastPathRunsAllInOrder(t *testing.T) {
	var order []int
	var mu sync.Mutex
	tasks := make([]func(context.Context) error, 5)
	for i := range tasks {
		i := i
		tasks[i] = func(context.Context) error {
			mu.Lock()
			order = append(order, i)
			mu.Unlock()
			return nil
		}
	}
	if err := runPool(context.Background(), 1, tasks); err != nil {
		t.Fatal(err)
	}
	want := []int{0, 1, 2, 3, 4}
	if len(order) != len(want) {
		t.Fatalf("order=%v", order)
	}
	for i, v := range want {
		if order[i] != v {
			t.Fatalf("order=%v want=%v", order, want)
		}
	}
}

func TestRunPoolParallelCompletesAllTasks(t *testing.T) {
	var done atomic.Int32
	tasks := make([]func(context.Context) error, 20)
	for i := range tasks {
		tasks[i] = func(context.Context) error {
			done.Add(1)
			return nil
		}
	}
	if err := runPool(context.Background(), 4, tasks); err != nil {
		t.Fatal(err)
	}
	if got := done.Load(); got != 20 {
		t.Fatalf("completed=%d want=20", got)
	}
}

func TestRunPoolFirstErrorCancelsRemaining(t *testing.T) {
	var executed atomic.Int32
	tasks := make([]func(context.Context) error, 100)
	tasks[0] = func(context.Context) error {
		executed.Add(1)
		return errors.New("first failure")
	}
	for i := 1; i < len(tasks); i++ {
		tasks[i] = func(context.Context) error {
			executed.Add(1)
			return nil
		}
	}
	// With 100 quick tasks some may complete before cancel propagates, but
	// the returned error must be the first failure and far fewer than 100
	// must execute.
	err := runPool(context.Background(), 2, tasks)
	if err == nil || err.Error() != "first failure" {
		t.Fatalf("err=%v", err)
	}
	if got := executed.Load(); got >= 100 {
		t.Fatalf("cancellation did not stop the queue; executed=%d", got)
	}
}

func TestRunPoolPreservesAppErrCategoryFromTask(t *testing.T) {
	sentinel := apperr.Wrap(apperr.Remote, "task", "remote failed", errors.New("conflict"))
	err := runPool(context.Background(), 3, []func(context.Context) error{
		func(context.Context) error { return sentinel },
		func(context.Context) error { return nil },
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err=%v", err)
	}
	var appErr *apperr.Error
	if !errors.As(err, &appErr) || appErr.Category != apperr.Remote {
		t.Fatalf("err=%v want Remote", err)
	}
}

func TestRunPoolParentCancelSurfacesAsContextError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	tasks := make([]func(context.Context) error, 4)
	for i := range tasks {
		tasks[i] = func(ctx context.Context) error {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(2 * time.Second):
				return nil
			}
		}
	}
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	err := runPool(ctx, 2, tasks)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v want context.Canceled (caller classifies Interrupted)", err)
	}
}

func TestRunPoolEmptyTaskList(t *testing.T) {
	if err := runPool(context.Background(), 4, nil); err != nil {
		t.Fatalf("err=%v", err)
	}
}
