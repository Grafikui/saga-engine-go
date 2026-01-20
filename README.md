# saga-engine-go

A crash-resilient, Postgres-backed saga executor for Go.

## Features

- **Crash Resilience**: Survives process crashes, restarts, and network failures
- **Postgres Durability**: All state persisted to PostgreSQL with advisory locking
- **Type-Safe Steps**: Generic `Step[T]` function for compile-time type safety
- **Automatic Compensation**: Failed workflows trigger reverse-order compensation
- **Dead Letter Queue**: Failed compensations move to `dead_letter` for manual review
- **Hard Limits**: 15-minute wall-clock execution, 10 retry cap, no infinite loops
- **Required Idempotency**: Idempotency keys mandatory at transaction AND step level

## Installation

```bash
go get github.com/grafikui/saga-engine-go
```

## Quick Start

```go
package main

import (
    "context"
    "database/sql"
    "log"

    saga "github.com/grafikui/saga-engine-go"
    _ "github.com/lib/pq"
)

func main() {
    db, _ := sql.Open("postgres", "postgres://localhost/mydb")
    storage, _ := saga.NewPostgresStorage(db, "transactions")
    lock := saga.NewPostgresLock(db)

    tx, err := saga.NewTransaction("order-123", storage, saga.TransactionOptions{
        IdempotencyKey: "order-123-v1",
        Lock:           lock,
        Input:          map[string]any{"orderId": "123", "amount": 99.99},
    })
    if err != nil {
        log.Fatal(err)
    }

    err = tx.Run(context.Background(), func(ctx context.Context, tx *saga.Transaction) error {
        // Step 1: Reserve inventory
        reservation, err := saga.Step(ctx, tx, "reserve-inventory", saga.StepDefinition[string]{
            IdempotencyKey: "reserve-inv-123",
            Execute: func(ctx context.Context) (string, error) {
                return reserveInventory(ctx, "SKU-001", 1)
            },
            Compensate: func(ctx context.Context, reservationID string) error {
                return releaseInventory(ctx, reservationID)
            },
        })
        if err != nil {
            return err
        }

        // Step 2: Charge payment
        _, err = saga.Step(ctx, tx, "charge-payment", saga.StepDefinition[string]{
            IdempotencyKey: "charge-pay-123",
            Execute: func(ctx context.Context) (string, error) {
                return chargePayment(ctx, 99.99)
            },
            Compensate: func(ctx context.Context, chargeID string) error {
                return refundPayment(ctx, chargeID)
            },
            Retry: &saga.RetryPolicy{
                Attempts:  3,
                BackoffMs: 1000,
            },
        })
        if err != nil {
            return err
        }

        // Step 3: Ship order
        _, err = saga.Step(ctx, tx, "ship-order", saga.StepDefinition[string]{
            IdempotencyKey: "ship-order-123",
            Execute: func(ctx context.Context) (string, error) {
                return shipOrder(ctx, reservation)
            },
            Compensate: func(ctx context.Context, trackingID string) error {
                return cancelShipment(ctx, trackingID)
            },
        })
        return err
    })

    if err != nil {
        log.Printf("Saga failed: %v", err)
    }
}
```

## Database Schema

```sql
CREATE TABLE transactions (
    id TEXT PRIMARY KEY,
    status TEXT NOT NULL DEFAULT 'pending',
    step_stack JSONB NOT NULL DEFAULT '[]',
    input JSONB,
    retry_count INTEGER NOT NULL DEFAULT 0,
    error JSONB,
    created_at TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX idx_transactions_status ON transactions(status);
CREATE INDEX idx_transactions_created_at ON transactions(created_at);
```

## CLI Tool

The `saga-admin` CLI helps manage workflows:

```bash
# Build the CLI
go build -o saga-admin ./cmd/saga-admin

# List all workflows
saga-admin -db "postgres://localhost/mydb" list

# Filter by status
saga-admin -db "postgres://localhost/mydb" list --status dead_letter

# Show workflow details
saga-admin -db "postgres://localhost/mydb" show tx-123

# Retry a dead_letter workflow
saga-admin -db "postgres://localhost/mydb" retry tx-123

# View statistics
saga-admin -db "postgres://localhost/mydb" stats
```

## Workflow States

| Status | Description |
|--------|-------------|
| `pending` | Workflow is running or ready for retry |
| `completed` | All steps succeeded |
| `failed` | Steps failed but compensation succeeded |
| `dead_letter` | Compensation failed - requires manual intervention |

## Error Types

All errors support `errors.Is()` and `errors.As()`:

```go
if errors.Is(err, saga.ErrExecutionTimeout) {
    // Handle 15-minute timeout
}

var compErr *saga.CompensationFailedError
if errors.As(err, &compErr) {
    log.Printf("Compensation failed at step: %s", compErr.FailedStep)
}
```

| Error | Sentinel | Description |
|-------|----------|-------------|
| `ExecutionTimeoutError` | `ErrExecutionTimeout` | Wall-clock exceeded 15 minutes |
| `IdempotencyRequiredError` | `ErrIdempotencyRequired` | Missing idempotency key |
| `CompensationFailedError` | `ErrCompensationFailed` | Compensation failed during rollback |
| `StepTimeoutError` | `ErrStepTimeout` | Step exceeded its timeout |
| `TransactionLockedError` | `ErrTransactionLocked` | Another process holds the lock |

## Hard Limits

These values are intentionally non-configurable:

| Limit | Value | Rationale |
|-------|-------|-----------|
| `MaxExecutionDuration` | 15 minutes | Prevents runaway workflows |
| `MaxRetryCount` | 10 | Prevents infinite retry loops |
| `MaxErrorLength` | 2048 chars | Prevents unbounded error storage |

## License

MIT
