package saga

import (
	"errors"
	"fmt"
	"time"
)

// Sentinel errors for errors.Is() support
var (
	ErrExecutionTimeout    = errors.New("execution timeout")
	ErrIdempotencyRequired = errors.New("idempotency required")
	ErrCompensationFailed  = errors.New("compensation failed")
	ErrRetryCapExceeded    = errors.New("retry cap exceeded")
	ErrDeadLetter          = errors.New("dead letter")
	ErrTransactionLocked   = errors.New("transaction locked")
	ErrStepTimeout         = errors.New("step timeout")
)

// Error codes for saga errors
const (
	ErrCodeExecutionTimeout    = "EXECUTION_TIMEOUT"
	ErrCodeIdempotencyRequired = "IDEMPOTENCY_REQUIRED"
	ErrCodeCompensationFailed  = "COMPENSATION_FAILED"
	ErrCodeRetryCapExceeded    = "RETRY_CAP_EXCEEDED"
	ErrCodeDeadLetter          = "DEAD_LETTER"
	ErrCodeTransactionLocked   = "TRANSACTION_LOCKED"
	ErrCodeStepTimeout         = "STEP_TIMEOUT"
)

// SagaError is the base error type for all saga errors.
type SagaError struct {
	Code    string
	Message string
	Cause   error
}

func (e *SagaError) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("%s: %s (cause: %v)", e.Code, e.Message, e.Cause)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func (e *SagaError) Unwrap() error {
	return e.Cause
}

// ExecutionTimeoutError is thrown when wall-clock exceeds 15 minutes.
type ExecutionTimeoutError struct {
	SagaError
	TransactionID string
	DeadlineMs    int64
	ElapsedMs     int64
}

// NewExecutionTimeoutError creates a new ExecutionTimeoutError.
func NewExecutionTimeoutError(txID string, deadlineMs, elapsedMs int64) *ExecutionTimeoutError {
	return &ExecutionTimeoutError{
		SagaError: SagaError{
			Code:    ErrCodeExecutionTimeout,
			Message: fmt.Sprintf("transaction '%s' exceeded %d ms deadline (elapsed: %d ms)", txID, deadlineMs, elapsedMs),
		},
		TransactionID: txID,
		DeadlineMs:    deadlineMs,
		ElapsedMs:     elapsedMs,
	}
}

func (e *ExecutionTimeoutError) Is(target error) bool {
	return target == ErrExecutionTimeout
}

// IdempotencyRequiredError is thrown when idempotency key is missing.
type IdempotencyRequiredError struct {
	SagaError
	Location      string // "transaction" or "step"
	TransactionID string
	StepName      string
}

// NewIdempotencyRequiredError creates a new IdempotencyRequiredError.
func NewIdempotencyRequiredError(location, txID, stepName string) *IdempotencyRequiredError {
	var msg string
	if location == "transaction" {
		msg = fmt.Sprintf("transaction '%s' requires idempotencyKey in options", txID)
	} else {
		msg = fmt.Sprintf("step '%s' requires idempotencyKey", stepName)
	}
	return &IdempotencyRequiredError{
		SagaError: SagaError{
			Code:    ErrCodeIdempotencyRequired,
			Message: msg,
		},
		Location:      location,
		TransactionID: txID,
		StepName:      stepName,
	}
}

func (e *IdempotencyRequiredError) Is(target error) bool {
	return target == ErrIdempotencyRequired
}

// CompensationFailedError is thrown when compensation fails during rollback.
type CompensationFailedError struct {
	SagaError
	TransactionID     string
	FailedStep        string
	OriginalError     error
	CompensationError error
}

// NewCompensationFailedError creates a new CompensationFailedError.
func NewCompensationFailedError(txID, stepName string, originalErr, compErr error) *CompensationFailedError {
	return &CompensationFailedError{
		SagaError: SagaError{
			Code:    ErrCodeCompensationFailed,
			Message: fmt.Sprintf("compensation failed for step '%s' in transaction '%s'", stepName, txID),
			Cause:   compErr,
		},
		TransactionID:     txID,
		FailedStep:        stepName,
		OriginalError:     originalErr,
		CompensationError: compErr,
	}
}

func (e *CompensationFailedError) Is(target error) bool {
	return target == ErrCompensationFailed
}

// RetryCapExceededError is thrown when retry limit is exceeded.
type RetryCapExceededError struct {
	SagaError
	TransactionID string
	RetryCount    int
	MaxRetries    int
}

// NewRetryCapExceededError creates a new RetryCapExceededError.
func NewRetryCapExceededError(txID string, retryCount, maxRetries int) *RetryCapExceededError {
	return &RetryCapExceededError{
		SagaError: SagaError{
			Code:    ErrCodeRetryCapExceeded,
			Message: fmt.Sprintf("transaction '%s' exceeded retry cap (%d/%d)", txID, retryCount, maxRetries),
		},
		TransactionID: txID,
		RetryCount:    retryCount,
		MaxRetries:    maxRetries,
	}
}

func (e *RetryCapExceededError) Is(target error) bool {
	return target == ErrRetryCapExceeded
}

// DeadLetterError is thrown when workflow is in terminal dead_letter state.
type DeadLetterError struct {
	SagaError
	TransactionID string
}

// NewDeadLetterError creates a new DeadLetterError.
func NewDeadLetterError(txID string) *DeadLetterError {
	return &DeadLetterError{
		SagaError: SagaError{
			Code:    ErrCodeDeadLetter,
			Message: fmt.Sprintf("transaction '%s' is in terminal dead_letter state", txID),
		},
		TransactionID: txID,
	}
}

func (e *DeadLetterError) Is(target error) bool {
	return target == ErrDeadLetter
}

// TransactionLockedError is thrown when transaction is already locked.
type TransactionLockedError struct {
	SagaError
	TransactionID string
}

// NewTransactionLockedError creates a new TransactionLockedError.
func NewTransactionLockedError(txID string) *TransactionLockedError {
	return &TransactionLockedError{
		SagaError: SagaError{
			Code:    ErrCodeTransactionLocked,
			Message: fmt.Sprintf("transaction '%s' is locked by another process", txID),
		},
		TransactionID: txID,
	}
}

func (e *TransactionLockedError) Is(target error) bool {
	return target == ErrTransactionLocked
}

// StepTimeoutError is thrown when a step exceeds its timeout.
type StepTimeoutError struct {
	SagaError
	StepName  string
	TimeoutMs int64
}

// NewStepTimeoutError creates a new StepTimeoutError.
func NewStepTimeoutError(stepName string, timeoutMs int64) *StepTimeoutError {
	return &StepTimeoutError{
		SagaError: SagaError{
			Code:    ErrCodeStepTimeout,
			Message: fmt.Sprintf("step '%s' exceeded timeout of %d ms", stepName, timeoutMs),
		},
		StepName:  stepName,
		TimeoutMs: timeoutMs,
	}
}

func (e *StepTimeoutError) Is(target error) bool {
	return target == ErrStepTimeout
}

// WorkflowError represents a structured error stored in the database.
type WorkflowError struct {
	StepName          string    `json:"stepName"`
	Error             string    `json:"error"`
	CompensationError string    `json:"compensationError,omitempty"`
	Timestamp         time.Time `json:"timestamp"`
}

// TruncateError truncates an error message to MaxErrorLength.
func TruncateError(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	if len(msg) <= MaxErrorLength {
		return msg
	}
	marker := "... [TRUNCATED]"
	return msg[:MaxErrorLength-len(marker)] + marker
}

// CreateWorkflowError creates a WorkflowError from errors.
func CreateWorkflowError(stepName string, originalErr, compensationErr error) *WorkflowError {
	we := &WorkflowError{
		StepName:  stepName,
		Error:     TruncateError(originalErr),
		Timestamp: time.Now(),
	}
	if compensationErr != nil {
		we.CompensationError = TruncateError(compensationErr)
	}
	return we
}
