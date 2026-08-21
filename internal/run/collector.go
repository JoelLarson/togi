package run

import (
	"context"
	"runtime"
	"sync"
)

const defaultMaximumWorkers = 4

// Collect executes requests with a fixed worker pool and preserves request order.
func Collect(ctx context.Context, executor Executor, requests []Request, limit int) []GateReport {
	results := make([]GateReport, len(requests))
	if len(requests) == 0 {
		return results
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if limit <= 0 {
		limit = min(runtime.NumCPU(), defaultMaximumWorkers)
	}
	limit = max(1, min(limit, len(requests)))

	type job struct {
		index   int
		request Request
	}
	jobs := make(chan job)
	var workers sync.WaitGroup
	workers.Add(limit)
	for range limit {
		go func() {
			defer workers.Done()
			for item := range jobs {
				results[item.index] = executor.Execute(ctx, item.request)
			}
		}()
	}
	for index, request := range requests {
		jobs <- job{index: index, request: request}
	}
	close(jobs)
	workers.Wait()
	return results
}
