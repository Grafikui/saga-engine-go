<div align="center">

# saga-engine-go

**Crash-resilient, PostgreSQL-backed saga executor for Go**

A battle-tested implementation of the saga pattern for distributed transactions.<br/>
Survives crashes. Compensates failures. Never loses state.

[![Go Reference](https://pkg.go.dev/badge/github.com/grafikui/saga-engine-go.svg)](https://pkg.go.dev/github.com/grafikui/saga-engine-go)
[![Go Report Card](https://goreportcard.com/badge/github.com/grafikui/saga-engine-go)](https://goreportcard.com/report/github.com/grafikui/saga-engine-go)
[![CI](https://github.com/grafikui/saga-engine-go/actions/workflows/ci.yml/badge.svg)](https://github.com/grafikui/saga-engine-go/actions/workflows/ci.yml)
[![codecov](https://codecov.io/gh/grafikui/saga-engine-go/branch/main/graph/badge.svg)](https://codecov.io/gh/grafikui/saga-engine-go)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

</div>

---

## Why This Exists

When a payment succeeds but shipping fails, you need to refund. When your process crashes mid-transaction, you need to resume. When compensations fail, you need visibility.

This library handles all of that with a single PostgreSQL table and zero external dependencies.

---

## Table of Contents

- [Features](#features)
- [Installation](#installation)
- [Quick Start](#quick-start)
- [Database Schema](#database-schema)
- [CLI Tool](#cli-tool)
- [Workflow States](#workflow-states)
- [Error Handling](#error-handling)
- [Configuration](#configuration)
- [Edge Cases](#edge-cases)
- [Testing](#testing)
- [License](#license)

---

## Features

<table>
<tr>
<td width="50%">

### Crash Resilience
State persisted to PostgreSQL after every step. Process dies? Pick up exactly where you left off.

### Type-Safe Generics
`Step[T]` function provides compile-time type safety. No `interface{}` casting at runtime.

### Automatic Compensation
Failed workflows trigger reverse-order compensation. Step 3 fails? Steps 2 and 1 roll back automatically.

</td>
<td width="50%">

### Dead Letter Queue
When compensation fails, workflows move to `dead_letter` for manual review. Nothing silently disappears.

### Advisory Locking
PostgreSQL advisory locks prevent concurrent execution of the same workflow. No distributed lock service needed.

### Hard Limits
15-minute execution cap. 10 retry maximum. No runaway workflows, no infinite loops.

</td>
</tr>
</table>

---

## Installation

```bash
go get github.com/grafikui/saga-engine-go
```

**Requirements:** Go 1.22+, PostgreSQL 12+

---

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
        IdempotencyKey: "order-123-v1",  // Required at transaction level
        Lock:           lock,
        Input:          map[string]any{"orderId": "123", "amount": 99.99},
    })
    if err != nil {
        log.Fatal(err)
    }

    err = tx.Run(context.Background(), func(ctx context.Context, tx *saga.Transaction) error {
        // Step 1: Reserve inventory
        reservation, err := saga.Step(ctx, tx, "reserve-inventory", saga.StepDefinition[string]{
            IdempotencyKey: "reserve-123",  // Required at step level
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

        // Step 2: Charge payment (with retry)
        _, err = saga.Step(ctx, tx, "charge-payment", saga.StepDefinition[string]{
            IdempotencyKey: "charge-123",
            Execute: func(ctx context.Context) (string, error) {
                return chargePayment(ctx, 99.99)
            },
            Compensate: func(ctx context.Context, chargeID string) error {
                return refundPayment(ctx, chargeID)
            },
            Retry: &saga.RetryPolicy{Attempts: 3, BackoffMs: 1000},
        })
        if err != nil {
            return err
        }

        // Step 3: Ship order
        _, err = saga.Step(ctx, tx, "ship-order", saga.StepDefinition[string]{
            IdempotencyKey: "ship-123",
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

---

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

---

## CLI Tool

The `saga-admin` CLI provides operational visibility and recovery tools.

```bash
# Build
go build -o saga-admin ./cmd/saga-admin

# List workflows
saga-admin -db "postgres://localhost/mydb" list
saga-admin -db "postgres://localhost/mydb" list --status dead_letter

# Inspect a workflow
saga-admin -db "postgres://localhost/mydb" show tx-123

# Retry a failed workflow
saga-admin -db "postgres://localhost/mydb" retry tx-123

# View statistics
saga-admin -db "postgres://localhost/mydb" stats
```

---

## Workflow States

| Status | Description |
|:-------|:------------|
| `pending` | Running or awaiting retry |
| `completed` | All steps succeeded |
| `failed` | Steps failed, compensation succeeded |
| `dead_letter` | Compensation failed, requires manual intervention |

```
pending ──> completed
    │
    └──> failed (compensation succeeded)
    │
    └──> dead_letter (compensation failed)
```

---

## Error Handling

All errors support `errors.Is()` and `errors.As()` for idiomatic Go error handling:

```go
if errors.Is(err, saga.ErrExecutionTimeout) {
    // Workflow exceeded 15-minute limit
}

var compErr *saga.CompensationFailedError
if errors.As(err, &compErr) {
    log.Printf("Compensation failed at step: %s", compErr.FailedStep)
}
```

| Error Type | Sentinel | When |
|:-----------|:---------|:-----|
| `ExecutionTimeoutError` | `ErrExecutionTimeout` | Wall-clock exceeded 15 minutes |
| `IdempotencyRequiredError` | `ErrIdempotencyRequired` | Missing idempotency key |
| `CompensationFailedError` | `ErrCompensationFailed` | Rollback failed |
| `StepTimeoutError` | `ErrStepTimeout` | Step exceeded timeout |
| `TransactionLockedError` | `ErrTransactionLocked` | Another process holds lock |

---

## Configuration

### Hard Limits

These values are intentionally **non-configurable** to prevent misuse:

| Constant | Value | Purpose |
|:---------|:------|:--------|
| `MaxExecutionDuration` | 15 minutes | Prevents runaway workflows |
| `MaxRetryCount` | 10 | Prevents infinite retry loops |
| `MaxErrorLength` | 2048 chars | Prevents unbounded storage |

### What We Don't Do

| Scope Limitation | Rationale |
|:-----------------|:----------|
| Distributed transactions | Single-process, single-database design. No 2PC. |
| Long-running workflows | 15-minute limit. Use [Temporal](https://temporal.io) for hours/days. |
| External consistency | If Stripe charges before crash, it stays charged. Use their idempotency keys. |
| Auto-resume dead letters | Terminal state by design. Manual intervention required. |

---

## Edge Cases

<details>
<summary><b>Resume Semantics</b></summary>

When resuming from saved state:

1. Completed steps are skipped (fires `OnStepSkipped` event)
2. Results reconstructed via JSON round-trip
3. Types must be JSON-serializable (exported fields only)

```go
// Works: exported fields
type OrderResult struct {
    ID     string  `json:"id"`
    Amount float64 `json:"amount"`
}

// Won't work: unexported fields
type badResult struct {
    id string  // not serialized
}
```
</details>

<details>
<summary><b>Step Timeouts</b></summary>

When timeout fires:
1. Context is cancelled
2. Goroutine continues until it checks `ctx.Done()`
3. Well-behaved functions should respect cancellation

```go
// Good: respects context
Execute: func(ctx context.Context) (string, error) {
    select {
    case <-ctx.Done():
        return "", ctx.Err()
    case result := <-doWork():
        return result, nil
    }
}
```
</details>

<details>
<summary><b>External System Idempotency</b></summary>

This library handles *your* idempotency. For external APIs, use *their* idempotency keys:

```go
Execute: func(ctx context.Context) (string, error) {
    charge, err := stripe.Charges.New(&stripe.ChargeParams{
        Amount:         stripe.Int64(1000),
        IdempotencyKey: stripe.String("order-123-charge"),
    })
    return charge.ID, err
}
```

Without this, a crash after Stripe charges (but before persisting) causes double-charge on retry.
</details>

<details>
<summary><b>Dead Letter Recovery</b></summary>

```bash
# 1. Find dead letters
saga-admin -db "$DATABASE_URL" dead-letter

# 2. Investigate
saga-admin -db "$DATABASE_URL" show tx-failed-123

# 3. Fix root cause, then retry
saga-admin -db "$DATABASE_URL" retry tx-failed-123
```

Maximum 10 retries per workflow.
</details>

---

## Testing

```bash
# Unit tests
go test ./...

# Integration tests (requires PostgreSQL)
DATABASE_URL="postgres://localhost/testdb" go test -tags=integration ./...

# Race detector
go test -race ./...

# Coverage
go test -cover ./...
```

---

## License

[MIT](LICENSE)

---

<div align="center">

**[Documentation](https://pkg.go.dev/github.com/grafikui/saga-engine-go)** ·
**[Report Bug](https://github.com/grafikui/saga-engine-go/issues)** ·
**[Request Feature](https://github.com/grafikui/saga-engine-go/issues)**

</div>
