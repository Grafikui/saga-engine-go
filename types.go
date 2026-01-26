// Package saga provides a crash-resilient, PostgreSQL-backed saga executor for Go.
//
// The saga pattern coordinates distributed transactions by executing a series of
// steps, each with a compensating action. If any step fails, previously completed
// steps are compensated in reverse order.
//
// Key features:
//   - Crash resilience: State is persisted to PostgreSQL after each step
//   - Type-safe steps: Generic Step[T] function provides compile-time type safety
//   - Automatic compensation: Failed workflows trigger reverse-order compensation
//   - Dead letter queue: Failed compensations move to dead_letter for manual review
//   - Hard limits: 15-minute execution max, 10 retry cap to prevent runaway workflows
//   - Required idempotency: Keys mandatory at transaction AND step level
//
// Example:
//
//	tx, _ := saga.NewTransaction("order-123", storage, saga.TransactionOptions{
//	    IdempotencyKey: "order-123-v1",
//	    Lock:           lock,
//	})
//	err := tx.Run(ctx, func(ctx context.Context, tx *saga.Transaction) error {
//	    _, err := saga.Step(ctx, tx, "reserve", saga.StepDefinition[string]{
//	        IdempotencyKey: "reserve-123",
//	        Execute:        func(ctx context.Context) (string, error) { ... },
//	        Compensate:     func(ctx context.Context, id string) error { ... },
//	    })
//	    return err
//	})
package saga

import (
	"context"
	"encoding/json"
	"time"
)

// StepContext represents a completed step stored in the database.
type StepContext struct {
	Name           string     `json:"name"`
	IdempotencyKey string     `json:"idempotencyKey"`
	Result         any        `json:"result"`
	Status         StepStatus `json:"status"`
}

// WorkflowRecord represents a workflow stored in the database.
type WorkflowRecord struct {
	ID         string          `json:"id"`
	Status     WorkflowStatus  `json:"status"`
	StepStack  []StepContext   `json:"stepStack"`
	Input      json.RawMessage `json:"input"`
	RetryCount int             `json:"retryCount"`
	Error      *WorkflowError  `json:"error,omitempty"`
	CreatedAt  time.Time       `json:"createdAt"`
	UpdatedAt  time.Time       `json:"updatedAt"`
}

// WorkflowFilter is used to query workflows.
type WorkflowFilter struct {
	Status        []WorkflowStatus
	CreatedAfter  *time.Time
	CreatedBefore *time.Time
	UpdatedAfter  *time.Time
	Offset        int
	Limit         int
}

// WorkflowQueryResult is the result of a workflow query.
type WorkflowQueryResult struct {
	Workflows []WorkflowRecord
	Total     int
}

// RetryPolicy configures retry behavior for a step.
type RetryPolicy struct {
	Attempts  int
	BackoffMs int64
}

// CompensationPolicy configures compensation behavior.
type CompensationPolicy struct {
	Retry   *RetryPolicy
	Timeout time.Duration
}

// StepDefinition defines a saga step.
type StepDefinition[T any] struct {
	IdempotencyKey     string
	Execute            func(ctx context.Context) (T, error)
	Compensate         func(ctx context.Context, result T) error
	Retry              *RetryPolicy
	Timeout            time.Duration
	CompensationPolicy *CompensationPolicy
}

// TransactionOptions configures a transaction.
type TransactionOptions struct {
	IdempotencyKey string
	Input          any
	Lock           Lock
	Events         *TransactionEvents
}

// registeredStep holds step info for compensation.
type registeredStep struct {
	name               string
	idempotencyKey     string
	compensate         func(ctx context.Context) error
	compensationPolicy *CompensationPolicy
}
