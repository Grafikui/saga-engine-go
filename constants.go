package saga

import "time"

// Hard limits - non-configurable by design
const (
	// MaxExecutionDuration is the maximum wall-clock time for a workflow (15 minutes).
	MaxExecutionDuration = 15 * time.Minute

	// MaxExecutionMs is MaxExecutionDuration in milliseconds.
	MaxExecutionMs = int64(MaxExecutionDuration / time.Millisecond)

	// MaxErrorLength is the maximum length of error messages stored in DB (2KB).
	MaxErrorLength = 2048

	// MaxRetryCount is the maximum number of CLI retries for dead_letter workflows.
	MaxRetryCount = 10
)

// WorkflowStatus represents the state of a workflow.
type WorkflowStatus string

const (
	StatusPending    WorkflowStatus = "pending"
	StatusCompleted  WorkflowStatus = "completed"
	StatusFailed     WorkflowStatus = "failed"
	StatusDeadLetter WorkflowStatus = "dead_letter"
)

// StepStatus represents the state of a step.
type StepStatus string

const (
	StepStatusCompleted StepStatus = "completed"
)
