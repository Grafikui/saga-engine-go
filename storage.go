package saga

import "context"

// Storage is the interface for workflow persistence.
type Storage interface {
	// IsProductionSafe returns true if this storage is safe for production use.
	IsProductionSafe() bool

	// Save persists the current step stack for a transaction.
	Save(ctx context.Context, txID string, steps []StepContext, input any) error

	// Load retrieves the step stack for a transaction.
	Load(ctx context.Context, txID string) ([]StepContext, error)

	// Clear marks a transaction as completed.
	Clear(ctx context.Context, txID string) error

	// GetWorkflow retrieves the full workflow record.
	GetWorkflow(ctx context.Context, txID string) (*WorkflowRecord, error)

	// UpdateStatus updates the workflow status and optionally sets an error.
	UpdateStatus(ctx context.Context, txID string, status WorkflowStatus, workflowErr *WorkflowError) error

	// AtomicRetry atomically transitions a dead_letter workflow to pending.
	// Returns the new retry count, or -1 if the workflow is not in dead_letter state.
	AtomicRetry(ctx context.Context, txID string) (int, error)

	// Query retrieves workflows matching the filter.
	Query(ctx context.Context, filter WorkflowFilter) (*WorkflowQueryResult, error)

	// CountByStatus counts workflows by status.
	CountByStatus(ctx context.Context, statuses ...WorkflowStatus) (int, error)
}
