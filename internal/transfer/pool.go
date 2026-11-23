package transfer

import (
	"context"
	"sync"
)

// runPool dispatches tasks in order to jobs workers; the first task error
// cancels the remaining work and is returned. A parent ctx cancellation
// surfaces as context.Err so the caller can classify it as Interrupted. The
// pattern mirrors uploader.uploadMissingParts: an unbuffered job channel, an
// outcomes channel sized jobs+1, and draining outcomes before consulting
// ctx.Err so a worker's original first error wins over the cancellation
// derived from it.
func runPool(ctx context.Context, jobs int, tasks []func(ctx context.Context) error) error {
	if len(tasks) == 0 {
		return nil
	}
	if jobs < 1 {
		jobs = 1
	}
	if jobs == 1 {
		// Serial fast path: no goroutines, deterministic first error.
		for _, task := range tasks {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := task(ctx); err != nil {
				return err
			}
		}
		return nil
	}

	workerCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobCh := make(chan func(ctx context.Context) error)
	type outcome struct{ err error }
	// At most one outcome per worker plus a simultaneous producer failure can
	// be reported before the coordinator starts draining this channel.
	outcomes := make(chan outcome, jobs+1)
	var workers sync.WaitGroup
	worker := func() {
		defer workers.Done()
		for task := range jobCh {
			if workerCtx.Err() != nil {
				return
			}
			err := task(workerCtx)
			if err != nil {
				if workerCtx.Err() != nil && ctx.Err() == nil {
					// Another worker already cancelled; its error is first.
					return
				}
				outcomes <- outcome{err: err}
				cancel()
				return
			}
		}
	}
	for i := 0; i < jobs; i++ {
		workers.Add(1)
		go worker()
	}

produce:
	for _, task := range tasks {
		if workerCtx.Err() != nil {
			break
		}
		select {
		case jobCh <- task:
		case <-workerCtx.Done():
			break produce
		}
	}
	close(jobCh)
	workers.Wait()
	cancel()
	close(outcomes)
	var firstErr error
	for result := range outcomes {
		if firstErr == nil && result.err != nil {
			firstErr = result.err
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return firstErr
}
