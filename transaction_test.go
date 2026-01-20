package saga

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestBasicSagaExecution(t *testing.T) {
	storage := NewMemoryStorage()
	ctx := context.Background()

	tx, err := NewTransaction("test-saga-1", storage, TransactionOptions{
		IdempotencyKey: "idem-1",
	})
	if err != nil {
		t.Fatalf("NewTransaction: %v", err)
	}

	var step1Result, step2Result string

	err = tx.Run(ctx, func(ctx context.Context, tx *Transaction) error {
		result1, err := Step(ctx, tx, "step1", StepDefinition[string]{
			IdempotencyKey: "step1-idem",
			Execute: func(ctx context.Context) (string, error) {
				return "result1", nil
			},
			Compensate: func(ctx context.Context, result string) error {
				return nil
			},
		})
		if err != nil {
			return err
		}
		step1Result = result1

		result2, err := Step(ctx, tx, "step2", StepDefinition[string]{
			IdempotencyKey: "step2-idem",
			Execute: func(ctx context.Context) (string, error) {
				return "result2", nil
			},
			Compensate: func(ctx context.Context, result string) error {
				return nil
			},
		})
		if err != nil {
			return err
		}
		step2Result = result2

		return nil
	})

	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if step1Result != "result1" {
		t.Errorf("step1Result = %q, want %q", step1Result, "result1")
	}
	if step2Result != "result2" {
		t.Errorf("step2Result = %q, want %q", step2Result, "result2")
	}

	// Verify workflow is completed
	wf, _ := storage.GetWorkflow(ctx, "test-saga-1")
	if wf.Status != StatusCompleted {
		t.Errorf("status = %q, want %q", wf.Status, StatusCompleted)
	}
}

func TestIdempotencyKeyRequired(t *testing.T) {
	storage := NewMemoryStorage()

	// Transaction without idempotency key
	_, err := NewTransaction("test", storage, TransactionOptions{})
	if err == nil {
		t.Fatal("expected error for missing idempotency key")
	}

	var idempErr *IdempotencyRequiredError
	if !errors.As(err, &idempErr) {
		t.Errorf("expected IdempotencyRequiredError, got %T", err)
	}
}

func TestStepIdempotencyKeyRequired(t *testing.T) {
	storage := NewMemoryStorage()
	ctx := context.Background()

	tx, _ := NewTransaction("test", storage, TransactionOptions{
		IdempotencyKey: "tx-idem",
	})

	err := tx.Run(ctx, func(ctx context.Context, tx *Transaction) error {
		_, err := Step(ctx, tx, "step1", StepDefinition[string]{
			// Missing IdempotencyKey
			Execute: func(ctx context.Context) (string, error) {
				return "result", nil
			},
			Compensate: func(ctx context.Context, result string) error {
				return nil
			},
		})
		return err
	})

	if err == nil {
		t.Fatal("expected error for missing step idempotency key")
	}

	var idempErr *IdempotencyRequiredError
	if !errors.As(err, &idempErr) {
		t.Errorf("expected IdempotencyRequiredError, got %T", err)
	}
}

func TestCompensationOnFailure(t *testing.T) {
	storage := NewMemoryStorage()
	ctx := context.Background()

	var compensated []string

	tx, _ := NewTransaction("test-comp", storage, TransactionOptions{
		IdempotencyKey: "comp-idem",
	})

	stepErr := errors.New("step2 failed")

	err := tx.Run(ctx, func(ctx context.Context, tx *Transaction) error {
		_, err := Step(ctx, tx, "step1", StepDefinition[string]{
			IdempotencyKey: "step1-idem",
			Execute: func(ctx context.Context) (string, error) {
				return "result1", nil
			},
			Compensate: func(ctx context.Context, result string) error {
				compensated = append(compensated, "step1")
				return nil
			},
		})
		if err != nil {
			return err
		}

		_, err = Step(ctx, tx, "step2", StepDefinition[string]{
			IdempotencyKey: "step2-idem",
			Execute: func(ctx context.Context) (string, error) {
				return "", stepErr
			},
			Compensate: func(ctx context.Context, result string) error {
				compensated = append(compensated, "step2")
				return nil
			},
		})
		return err
	})

	if err == nil {
		t.Fatal("expected error from failed step")
	}

	// Only step1 should be compensated (step2 never completed)
	if len(compensated) != 1 || compensated[0] != "step1" {
		t.Errorf("compensated = %v, want [step1]", compensated)
	}

	// Status should be failed (compensation succeeded)
	wf, _ := storage.GetWorkflow(ctx, "test-comp")
	if wf.Status != StatusFailed {
		t.Errorf("status = %q, want %q", wf.Status, StatusFailed)
	}
}

func TestCompensationReverseOrder(t *testing.T) {
	storage := NewMemoryStorage()
	ctx := context.Background()

	var compensationOrder []string

	tx, _ := NewTransaction("test-order", storage, TransactionOptions{
		IdempotencyKey: "order-idem",
	})

	err := tx.Run(ctx, func(ctx context.Context, tx *Transaction) error {
		for i := 1; i <= 3; i++ {
			name := string(rune('A' + i - 1))
			_, err := Step(ctx, tx, name, StepDefinition[int]{
				IdempotencyKey: name + "-idem",
				Execute: func(ctx context.Context) (int, error) {
					return i, nil
				},
				Compensate: func(ctx context.Context, result int) error {
					compensationOrder = append(compensationOrder, name)
					return nil
				},
			})
			if err != nil {
				return err
			}
		}
		return errors.New("trigger compensation")
	})

	if err == nil {
		t.Fatal("expected error")
	}

	// Should be C, B, A (reverse order)
	if len(compensationOrder) != 3 {
		t.Fatalf("compensationOrder length = %d, want 3", len(compensationOrder))
	}
	if compensationOrder[0] != "C" || compensationOrder[1] != "B" || compensationOrder[2] != "A" {
		t.Errorf("compensationOrder = %v, want [C B A]", compensationOrder)
	}
}

func TestDeadLetterOnCompensationFailure(t *testing.T) {
	storage := NewMemoryStorage()
	ctx := context.Background()

	tx, _ := NewTransaction("test-dead", storage, TransactionOptions{
		IdempotencyKey: "dead-idem",
	})

	compErr := errors.New("compensation failed")
	stepErr := errors.New("step failed")

	err := tx.Run(ctx, func(ctx context.Context, tx *Transaction) error {
		_, err := Step(ctx, tx, "step1", StepDefinition[string]{
			IdempotencyKey: "step1-idem",
			Execute: func(ctx context.Context) (string, error) {
				return "result1", nil
			},
			Compensate: func(ctx context.Context, result string) error {
				return compErr // Compensation fails
			},
		})
		if err != nil {
			return err
		}

		return stepErr // Trigger compensation
	})

	if err == nil {
		t.Fatal("expected error")
	}

	var compFailedErr *CompensationFailedError
	if !errors.As(err, &compFailedErr) {
		t.Errorf("expected CompensationFailedError, got %T", err)
	}

	// Should be in dead_letter state
	wf, _ := storage.GetWorkflow(ctx, "test-dead")
	if wf.Status != StatusDeadLetter {
		t.Errorf("status = %q, want %q", wf.Status, StatusDeadLetter)
	}
}

func TestRetryPolicy(t *testing.T) {
	storage := NewMemoryStorage()
	ctx := context.Background()

	var attempts int32

	tx, _ := NewTransaction("test-retry", storage, TransactionOptions{
		IdempotencyKey: "retry-idem",
	})

	err := tx.Run(ctx, func(ctx context.Context, tx *Transaction) error {
		_, err := Step(ctx, tx, "flaky", StepDefinition[string]{
			IdempotencyKey: "flaky-idem",
			Execute: func(ctx context.Context) (string, error) {
				count := atomic.AddInt32(&attempts, 1)
				if count < 3 {
					return "", errors.New("transient error")
				}
				return "success", nil
			},
			Compensate: func(ctx context.Context, result string) error {
				return nil
			},
			Retry: &RetryPolicy{
				Attempts:  5,
				BackoffMs: 10,
			},
		})
		return err
	})

	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if attempts != 3 {
		t.Errorf("attempts = %d, want 3", attempts)
	}
}

func TestStepTimeout(t *testing.T) {
	storage := NewMemoryStorage()
	ctx := context.Background()

	tx, _ := NewTransaction("test-timeout", storage, TransactionOptions{
		IdempotencyKey: "timeout-idem",
	})

	err := tx.Run(ctx, func(ctx context.Context, tx *Transaction) error {
		_, err := Step(ctx, tx, "slow", StepDefinition[string]{
			IdempotencyKey: "slow-idem",
			Timeout:        50 * time.Millisecond,
			Execute: func(ctx context.Context) (string, error) {
				select {
				case <-time.After(500 * time.Millisecond):
					return "done", nil
				case <-ctx.Done():
					return "", ctx.Err()
				}
			},
			Compensate: func(ctx context.Context, result string) error {
				return nil
			},
		})
		return err
	})

	if err == nil {
		t.Fatal("expected timeout error")
	}

	var timeoutErr *StepTimeoutError
	if !errors.As(err, &timeoutErr) {
		t.Errorf("expected StepTimeoutError, got %T: %v", err, err)
	}
}

func TestEventCallbacks(t *testing.T) {
	storage := NewMemoryStorage()
	ctx := context.Background()

	var events []string

	tx, _ := NewTransaction("test-events", storage, TransactionOptions{
		IdempotencyKey: "events-idem",
		Events: &TransactionEvents{
			OnTransactionStart: func(id string, input any) {
				events = append(events, "tx-start")
			},
			OnStepStart: func(name string) {
				events = append(events, "step-start:"+name)
			},
			OnStepComplete: func(name string, result any, duration time.Duration) {
				events = append(events, "step-complete:"+name)
			},
			OnTransactionComplete: func(id string) {
				events = append(events, "tx-complete")
			},
		},
	})

	err := tx.Run(ctx, func(ctx context.Context, tx *Transaction) error {
		_, err := Step(ctx, tx, "step1", StepDefinition[string]{
			IdempotencyKey: "step1-idem",
			Execute: func(ctx context.Context) (string, error) {
				return "result", nil
			},
			Compensate: func(ctx context.Context, result string) error {
				return nil
			},
		})
		return err
	})

	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	expected := []string{"tx-start", "step-start:step1", "step-complete:step1", "tx-complete"}
	if len(events) != len(expected) {
		t.Fatalf("events = %v, want %v", events, expected)
	}
	for i, e := range expected {
		if events[i] != e {
			t.Errorf("events[%d] = %q, want %q", i, events[i], e)
		}
	}
}

func TestGenericStepTypes(t *testing.T) {
	storage := NewMemoryStorage()
	ctx := context.Background()

	type OrderResult struct {
		OrderID string
		Total   float64
	}

	tx, _ := NewTransaction("test-generic", storage, TransactionOptions{
		IdempotencyKey: "generic-idem",
	})

	var result OrderResult

	err := tx.Run(ctx, func(ctx context.Context, tx *Transaction) error {
		r, err := Step(ctx, tx, "create-order", StepDefinition[OrderResult]{
			IdempotencyKey: "order-idem",
			Execute: func(ctx context.Context) (OrderResult, error) {
				return OrderResult{
					OrderID: "order-123",
					Total:   99.99,
				}, nil
			},
			Compensate: func(ctx context.Context, result OrderResult) error {
				// Cancel order
				return nil
			},
		})
		if err != nil {
			return err
		}
		result = r
		return nil
	})

	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if result.OrderID != "order-123" || result.Total != 99.99 {
		t.Errorf("result = %+v, want {OrderID:order-123 Total:99.99}", result)
	}
}

func TestAtomicRetry(t *testing.T) {
	storage := NewMemoryStorage()
	ctx := context.Background()

	// Set up a dead_letter workflow
	storage.UpdateStatus(ctx, "test-retry", StatusDeadLetter, &WorkflowError{
		StepName: "step1",
		Error:    "test error",
	})

	// Retry should work
	count, err := storage.AtomicRetry(ctx, "test-retry")
	if err != nil {
		t.Fatalf("AtomicRetry: %v", err)
	}
	if count != 1 {
		t.Errorf("count = %d, want 1", count)
	}

	wf, _ := storage.GetWorkflow(ctx, "test-retry")
	if wf.Status != StatusPending {
		t.Errorf("status = %q, want %q", wf.Status, StatusPending)
	}

	// Second retry should fail (not in dead_letter anymore)
	count, _ = storage.AtomicRetry(ctx, "test-retry")
	if count != -1 {
		t.Errorf("expected -1 for non-dead_letter workflow, got %d", count)
	}
}

func TestNoOpLock(t *testing.T) {
	lock := &NoOpLock{}
	ctx := context.Background()

	token, err := lock.Acquire(ctx, "test", time.Minute)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if token == "" {
		t.Error("expected non-empty token")
	}

	err = lock.Release(ctx, "test", token)
	if err != nil {
		t.Errorf("Release: %v", err)
	}
}

func TestErrorTypes(t *testing.T) {
	// ExecutionTimeoutError
	execErr := NewExecutionTimeoutError("tx-1", 900000, 900001)
	if execErr.Error() == "" {
		t.Error("ExecutionTimeoutError.Error() returned empty string")
	}
	if !errors.Is(execErr, ErrExecutionTimeout) {
		t.Error("ExecutionTimeoutError should match ErrExecutionTimeout")
	}

	// StepTimeoutError
	stepErr := NewStepTimeoutError("step1", 5000)
	if stepErr.Error() == "" {
		t.Error("StepTimeoutError.Error() returned empty string")
	}
	if !errors.Is(stepErr, ErrStepTimeout) {
		t.Error("StepTimeoutError should match ErrStepTimeout")
	}

	// IdempotencyRequiredError
	idempErr := NewIdempotencyRequiredError("step", "tx-1", "step1")
	if idempErr.Error() == "" {
		t.Error("IdempotencyRequiredError.Error() returned empty string")
	}
	if !errors.Is(idempErr, ErrIdempotencyRequired) {
		t.Error("IdempotencyRequiredError should match ErrIdempotencyRequired")
	}

	// CompensationFailedError
	compErr := NewCompensationFailedError("tx-1", "step1", errors.New("original"), errors.New("comp"))
	if compErr.Error() == "" {
		t.Error("CompensationFailedError.Error() returned empty string")
	}
	if !errors.Is(compErr, ErrCompensationFailed) {
		t.Error("CompensationFailedError should match ErrCompensationFailed")
	}

	// TransactionLockedError
	lockErr := NewTransactionLockedError("tx-1")
	if lockErr.Error() == "" {
		t.Error("TransactionLockedError.Error() returned empty string")
	}
	if !errors.Is(lockErr, ErrTransactionLocked) {
		t.Error("TransactionLockedError should match ErrTransactionLocked")
	}
}

func TestWorkflowFilter(t *testing.T) {
	storage := NewMemoryStorage()
	ctx := context.Background()

	// Create some workflows
	storage.UpdateStatus(ctx, "wf-1", StatusCompleted, nil)
	storage.UpdateStatus(ctx, "wf-2", StatusFailed, nil)
	storage.UpdateStatus(ctx, "wf-3", StatusDeadLetter, nil)
	storage.UpdateStatus(ctx, "wf-4", StatusPending, nil)

	// Query all
	result, err := storage.Query(ctx, WorkflowFilter{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if result.Total != 4 {
		t.Errorf("total = %d, want 4", result.Total)
	}

	// Query by status
	result, _ = storage.Query(ctx, WorkflowFilter{
		Status: []WorkflowStatus{StatusDeadLetter, StatusFailed},
	})
	if result.Total != 2 {
		t.Errorf("filtered total = %d, want 2", result.Total)
	}

	// Count by status
	count, _ := storage.CountByStatus(ctx, StatusDeadLetter)
	if count != 1 {
		t.Errorf("dead_letter count = %d, want 1", count)
	}
}
