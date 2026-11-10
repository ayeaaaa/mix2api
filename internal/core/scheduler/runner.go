package scheduler

import (
	"context"
	"strings"
	"time"
)

// DefaultInterval is the provider job poll interval. Provider jobs are
// internally idempotent and only act when their local-time slot is due.
const DefaultInterval = 30 * time.Second

// Runner executes provider-contributed jobs on a fixed tick. It never starts a
// job before the listener is ready because Run is called by the command after
// Serve begins.
type Runner struct {
	Jobs     []Job
	Interval time.Duration
	Logger   func(string)
}

// NewRunner builds a runner. A non-positive interval uses DefaultInterval.
func NewRunner(jobs []Job, interval time.Duration, logger func(string)) *Runner {
	if interval <= 0 {
		interval = DefaultInterval
	}
	return &Runner{Jobs: append([]Job(nil), jobs...), Interval: interval, Logger: logger}
}

// Run blocks until ctx is cancelled, calling every job once per tick. Jobs run
// sequentially: two provider jobs may touch the same account slot lock, and a
// burst of concurrent upstream mutations is not desirable. A job error is
// logged (name only) and never stops the loop.
func (r *Runner) Run(ctx context.Context) {
	if r == nil || len(r.Jobs) == 0 {
		return
	}
	ticker := time.NewTicker(r.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.runOnce(ctx)
		}
	}
}

// RunOnce executes one cycle synchronously. Tests and the startup catch-up use
// it; Run also calls it on each tick.
func (r *Runner) RunOnce(ctx context.Context) {
	if r == nil {
		return
	}
	r.runOnce(ctx)
}

func (r *Runner) runOnce(ctx context.Context) {
	for _, job := range r.Jobs {
		if job == nil {
			continue
		}
		if err := ctx.Err(); err != nil {
			return
		}
		if err := job.Run(ctx); err != nil && r.Logger != nil {
			r.Logger("scheduler: job " + jobName(job) + " failed: " + err.Error())
		}
	}
}

func jobName(job Job) string {
	if job == nil {
		return ""
	}
	name := strings.TrimSpace(job.Name())
	if name == "" {
		return "<unnamed>"
	}
	return name
}
