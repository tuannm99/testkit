// Package trigger defines what both triggers share: the Process function.
package trigger

import (
	"context"

	"github.com/tuannm99/testkit/reference-worker/internal/domain"
)

// ProcessFunc is usecase.Processor.Process.
type ProcessFunc func(ctx context.Context, job domain.Job) error
