package saga

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Transaction is the core saga executor.
type Transaction struct {
	id             string
	idempotencyKey string
	storage        Storage
	lock           Lock
	input          any
	events         *TransactionEvents

	// Runtime state
	deadline  time.Time
	stepStack []registeredStep
	history   map[string]any
	lockToken string
}

// NewTransaction creates a new Transaction.
func NewTransaction(id string, storage Storage, opts TransactionOptions) (*Transaction, error) {
	if opts.IdempotencyKey == "" {
		return nil, NewIdempotencyRequiredError("transaction", id, "")
	}

	lock := opts.Lock
	if lock == nil {
		lock = &NoOpLock{}
	}

	return &Transaction{
		id:             id,
		idempotencyKey: opts.IdempotencyKey,
		storage:        storage,
		lock:           lock,
		input:          opts.Input,
		events:         opts.Events,
		history:        make(map[string]any),
	}, nil
}

// Run executes the saga workflow.
func (tx *Transaction) Run(ctx context.Context, workflow func(ctx context.Context, t *Transaction) error) error {
	// 1. Emit transaction start
	emitEvent(tx.events, func() {
		if tx.events.OnTransactionStart != nil {
			tx.events.OnTransactionStart(tx.id, tx.input)
		}
	})

	// 2. Acquire lock
	token, err := tx.lock.Acquire(ctx, tx.id, MaxExecutionDuration)
	if err != nil {
		var lockedErr *TransactionLockedError
		if errors.As(err, &lockedErr) {
			return err
		}
		return fmt.Errorf("acquire lock: %w", err)
	}
	tx.lockToken = token
	defer tx.releaseLock(ctx)

	// 3. Set deadline
	tx.deadline = time.Now().Add(MaxExecutionDuration)

	// 4. Check for existing state (resume support)
	savedState, err := tx.storage.Load(ctx, tx.id)
	if err != nil {
		return fmt.Errorf("load state: %w", err)
	}

	if savedState != nil {
		// Check if we're past deadline (including downtime)
		workflow, err := tx.storage.GetWorkflow(ctx, tx.id)
		if err != nil {
			return fmt.Errorf("get workflow: %w", err)
		}
		if workflow != nil && !workflow.CreatedAt.IsZero() {
			elapsed := time.Since(workflow.CreatedAt)
			if elapsed > MaxExecutionDuration {
				// Past deadline - move to dead_letter
				timeoutErr := NewExecutionTimeoutError(tx.id, MaxExecutionMs, elapsed.Milliseconds())
				if err := tx.transitionToDeadLetter(ctx, timeoutErr, "timeout-check"); err != nil {
					return err
				}
				return timeoutErr
			}
		}
		// Resume: populate history from saved state
		for _, step := range savedState {
			tx.history[step.Name] = step.Result
		}
	}

	// 5. Execute workflow
	runErr := workflow(ctx, tx)

	// 6. Handle result
	if runErr != nil {
		// Compensate
		compErr := tx.compensate(ctx, runErr)
		if compErr != nil {
			// Compensation failed - already transitioned to dead_letter
			return compErr
		}

		// All compensations succeeded - mark as failed
		if err := tx.storage.UpdateStatus(ctx, tx.id, StatusFailed, nil); err != nil {
			return fmt.Errorf("update status to failed: %w", err)
		}

		emitEvent(tx.events, func() {
			if tx.events.OnTransactionFailed != nil {
				tx.events.OnTransactionFailed(tx.id, runErr)
			}
		})

		return runErr
	}

	// 7. Success - clear and mark complete
	if err := tx.storage.Clear(ctx, tx.id); err != nil {
		return fmt.Errorf("clear: %w", err)
	}

	emitEvent(tx.events, func() {
		if tx.events.OnTransactionComplete != nil {
			tx.events.OnTransactionComplete(tx.id)
		}
	})

	return nil
}

// Step executes a saga step.
func Step[T any](ctx context.Context, tx *Transaction, name string, def StepDefinition[T]) (T, error) {
	var zero T

	// Validate idempotency key
	if def.IdempotencyKey == "" {
		return zero, NewIdempotencyRequiredError("step", tx.id, name)
	}

	// Check deadline
	if err := tx.checkDeadline(); err != nil {
		return zero, err
	}

	// Check if already completed (resume)
	if cached, ok := tx.history[name]; ok {
		emitEvent(tx.events, func() {
			if tx.events.OnStepSkipped != nil {
				tx.events.OnStepSkipped(name, cached)
			}
		})

		// Type assert the cached result
		if result, ok := cached.(T); ok {
			return result, nil
		}
		// Try JSON round-trip for complex types
		b, _ := json.Marshal(cached)
		var result T
		if err := json.Unmarshal(b, &result); err == nil {
			return result, nil
		}
		return zero, fmt.Errorf("cached result type mismatch for step '%s'", name)
	}

	// Emit step start
	emitEvent(tx.events, func() {
		if tx.events.OnStepStart != nil {
			tx.events.OnStepStart(name)
		}
	})

	stepStart := time.Now()

	// Execute with retry
	result, err := executeWithRetry(ctx, tx, name, def)
	if err != nil {
		return zero, err
	}

	// Emit step complete
	emitEvent(tx.events, func() {
		if tx.events.OnStepComplete != nil {
			tx.events.OnStepComplete(name, result, time.Since(stepStart))
		}
	})

	// Store result in history
	tx.history[name] = result

	// Register compensation
	tx.stepStack = append(tx.stepStack, registeredStep{
		name:               name,
		idempotencyKey:     def.IdempotencyKey,
		compensate:         func(ctx context.Context) error { return def.Compensate(ctx, result) },
		compensationPolicy: def.CompensationPolicy,
	})

	// Persist state
	stepContexts := make([]StepContext, len(tx.stepStack))
	for i, s := range tx.stepStack {
		stepContexts[i] = StepContext{
			Name:           s.name,
			IdempotencyKey: s.idempotencyKey,
			Result:         tx.history[s.name],
			Status:         StepStatusCompleted,
		}
	}

	if err := tx.storage.Save(ctx, tx.id, stepContexts, tx.input); err != nil {
		return zero, fmt.Errorf("save state: %w", err)
	}

	return result, nil
}

// executeWithRetry executes a step with retry policy.
func executeWithRetry[T any](ctx context.Context, tx *Transaction, name string, def StepDefinition[T]) (T, error) {
	var zero T

	attempts := 1
	if def.Retry != nil && def.Retry.Attempts > 0 {
		attempts = def.Retry.Attempts
	}

	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		// Check deadline before each attempt
		if err := tx.checkDeadline(); err != nil {
			return zero, err
		}

		// Execute with timeout
		result, err := executeWithTimeout(ctx, tx, name, def)
		if err == nil {
			return result, nil
		}

		lastErr = err

		// Emit failure event
		emitEvent(tx.events, func() {
			if tx.events.OnStepFailed != nil {
				tx.events.OnStepFailed(name, err, attempt)
			}
		})

		// If not the last attempt, retry
		if attempt < attempts {
			emitEvent(tx.events, func() {
				if tx.events.OnStepRetry != nil {
					tx.events.OnStepRetry(name, attempt+1, err)
				}
			})

			// Backoff
			if def.Retry != nil && def.Retry.BackoffMs > 0 {
				select {
				case <-ctx.Done():
					return zero, ctx.Err()
				case <-time.After(time.Duration(def.Retry.BackoffMs) * time.Millisecond):
				}
			}
		}
	}

	return zero, lastErr
}

// executeWithTimeout executes a step with timeout.
func executeWithTimeout[T any](ctx context.Context, tx *Transaction, name string, def StepDefinition[T]) (T, error) {
	var zero T

	timeout := def.Timeout
	if timeout <= 0 {
		// No timeout - execute directly
		return def.Execute(ctx)
	}

	// Create timeout context
	timeoutCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Channel for result
	type executeResult struct {
		result T
		err    error
	}
	resultCh := make(chan executeResult, 1)

	go func() {
		result, err := def.Execute(timeoutCtx)
		resultCh <- executeResult{result, err}
	}()

	select {
	case <-timeoutCtx.Done():
		if timeoutCtx.Err() == context.DeadlineExceeded {
			emitEvent(tx.events, func() {
				if tx.events.OnStepTimeout != nil {
					tx.events.OnStepTimeout(name, timeout)
				}
			})
			return zero, NewStepTimeoutError(name, timeout.Milliseconds())
		}
		return zero, timeoutCtx.Err()
	case res := <-resultCh:
		return res.result, res.err
	}
}

// checkDeadline checks if the execution deadline has been exceeded.
func (tx *Transaction) checkDeadline() error {
	elapsed := time.Since(tx.deadline.Add(-MaxExecutionDuration))
	if elapsed > MaxExecutionDuration {
		return NewExecutionTimeoutError(tx.id, MaxExecutionMs, elapsed.Milliseconds())
	}
	return nil
}

// compensate runs compensation for all completed steps in reverse order.
func (tx *Transaction) compensate(ctx context.Context, originalErr error) error {
	// Compensate in reverse order
	for i := len(tx.stepStack) - 1; i >= 0; i-- {
		step := tx.stepStack[i]

		emitEvent(tx.events, func() {
			if tx.events.OnCompensationStart != nil {
				tx.events.OnCompensationStart(step.name)
			}
		})

		compErr := tx.executeCompensation(ctx, step)
		if compErr != nil {
			emitEvent(tx.events, func() {
				if tx.events.OnCompensationFailed != nil {
					tx.events.OnCompensationFailed(step.name, compErr)
				}
			})

			// Transition to dead_letter
			if err := tx.transitionToDeadLetter(ctx, originalErr, step.name, compErr); err != nil {
				return err
			}

			return NewCompensationFailedError(tx.id, step.name, originalErr, compErr)
		}

		emitEvent(tx.events, func() {
			if tx.events.OnCompensationComplete != nil {
				tx.events.OnCompensationComplete(step.name)
			}
		})
	}

	return nil
}

// executeCompensation executes a single compensation with optional retry.
func (tx *Transaction) executeCompensation(ctx context.Context, step registeredStep) error {
	attempts := 1
	var backoffMs int64 = 0
	var timeout time.Duration = 0

	if step.compensationPolicy != nil {
		if step.compensationPolicy.Retry != nil {
			attempts = step.compensationPolicy.Retry.Attempts
			backoffMs = step.compensationPolicy.Retry.BackoffMs
		}
		timeout = step.compensationPolicy.Timeout
	}

	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		var err error
		if timeout > 0 {
			timeoutCtx, cancel := context.WithTimeout(ctx, timeout)
			err = step.compensate(timeoutCtx)
			cancel()
		} else {
			err = step.compensate(ctx)
		}

		if err == nil {
			return nil
		}

		lastErr = err

		if attempt < attempts && backoffMs > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(backoffMs) * time.Millisecond):
			}
		}
	}

	return lastErr
}

// transitionToDeadLetter moves the workflow to dead_letter state.
func (tx *Transaction) transitionToDeadLetter(ctx context.Context, originalErr error, stepName string, compErr ...error) error {
	var compensationErr error
	if len(compErr) > 0 {
		compensationErr = compErr[0]
	}

	workflowErr := CreateWorkflowError(stepName, originalErr, compensationErr)

	if err := tx.storage.UpdateStatus(ctx, tx.id, StatusDeadLetter, workflowErr); err != nil {
		return fmt.Errorf("update status to dead_letter: %w", err)
	}

	emitEvent(tx.events, func() {
		if tx.events.OnDeadLetter != nil {
			tx.events.OnDeadLetter(tx.id, workflowErr)
		}
	})

	return nil
}

// releaseLock releases the transaction lock.
func (tx *Transaction) releaseLock(ctx context.Context) {
	if tx.lockToken != "" {
		tx.lock.Release(ctx, tx.id, tx.lockToken)
		tx.lockToken = ""
	}
}

// ID returns the transaction ID.
func (tx *Transaction) ID() string {
	return tx.id
}

// IdempotencyKey returns the transaction's idempotency key.
func (tx *Transaction) IdempotencyKey() string {
	return tx.idempotencyKey
}
