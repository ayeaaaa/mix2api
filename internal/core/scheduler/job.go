// Package scheduler defines background job contracts shared by the core and
// provider modules. The scheduling engine itself lives in the core and knows
// nothing about individual providers.
package scheduler

import "context"

// Job is a named background task contributed by a provider.
type Job interface {
	// Name returns the job's stable name.
	Name() string

	// Run executes one cycle of the job. The context is cancelled when the
	// process is shutting down.
	Run(ctx context.Context) error
}
