//go:build integration

package saga

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"testing"
	"time"

	_ "github.com/lib/pq"
)

// getTestDB returns a database connection for integration tests.
// Set DATABASE_URL environment variable to run these tests.
func getTestDB(t *testing.T) *sql.DB {
	t.Helper()

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("Failed to connect to database: %v", err)
	}

	// Verify connection
	if err := db.Ping(); err != nil {
		db.Close()
		t.Fatalf("Failed to ping database: %v", err)
	}

	return db
}

// setupTestTable creates a test table and returns a cleanup function.
func setupTestTable(t *testing.T, db *sql.DB, tableName string) func() {
	t.Helper()

	createSQL := `
		CREATE TABLE IF NOT EXISTS ` + tableName + ` (
			id TEXT PRIMARY KEY,
			status TEXT NOT NULL DEFAULT 'pending',
			step_stack JSONB NOT NULL DEFAULT '[]',
			input JSONB,
			retry_count INTEGER NOT NULL DEFAULT 0,
			error JSONB,
			created_at TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP
		)`

	if _, err := db.Exec(createSQL); err != nil {
		t.Fatalf("Failed to create test table: %v", err)
	}

	return func() {
		db.Exec("DROP TABLE IF EXISTS " + tableName)
		db.Close()
	}
}

func TestPostgresStorage_SaveAndLoad(t *testing.T) {
	db := getTestDB(t)
	tableName := "test_saga_save_load"
	cleanup := setupTestTable(t, db, tableName)
	defer cleanup()

	storage, err := NewPostgresStorage(db, tableName)
	if err != nil {
		t.Fatalf("NewPostgresStorage: %v", err)
	}

	ctx := context.Background()

	// Save a workflow
	steps := []StepContext{
		{Name: "step1", IdempotencyKey: "idem-1", Result: "result1", Status: StepStatusCompleted},
		{Name: "step2", IdempotencyKey: "idem-2", Result: map[string]any{"key": "value"}, Status: StepStatusCompleted},
	}
	input := map[string]any{"orderId": "123"}

	err = storage.Save(ctx, "tx-1", steps, input)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Load the workflow
	loaded, err := storage.Load(ctx, "tx-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if len(loaded) != 2 {
		t.Fatalf("Load returned %d steps, want 2", len(loaded))
	}
	if loaded[0].Name != "step1" {
		t.Errorf("loaded[0].Name = %q, want %q", loaded[0].Name, "step1")
	}

	// Load non-existent
	notFound, err := storage.Load(ctx, "non-existent")
	if err != nil {
		t.Fatalf("Load non-existent: %v", err)
	}
	if notFound != nil {
		t.Errorf("expected nil for non-existent workflow, got %v", notFound)
	}
}

func TestPostgresStorage_UpdateAndSave(t *testing.T) {
	db := getTestDB(t)
	tableName := "test_saga_update"
	cleanup := setupTestTable(t, db, tableName)
	defer cleanup()

	storage, err := NewPostgresStorage(db, tableName)
	if err != nil {
		t.Fatalf("NewPostgresStorage: %v", err)
	}

	ctx := context.Background()

	// Save initial state
	err = storage.Save(ctx, "tx-update", []StepContext{
		{Name: "step1", IdempotencyKey: "idem-1", Status: StepStatusCompleted},
	}, nil)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Update with more steps
	err = storage.Save(ctx, "tx-update", []StepContext{
		{Name: "step1", IdempotencyKey: "idem-1", Status: StepStatusCompleted},
		{Name: "step2", IdempotencyKey: "idem-2", Status: StepStatusCompleted},
	}, nil)
	if err != nil {
		t.Fatalf("Save update: %v", err)
	}

	loaded, err := storage.Load(ctx, "tx-update")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(loaded) != 2 {
		t.Errorf("expected 2 steps after update, got %d", len(loaded))
	}
}

func TestPostgresStorage_Clear(t *testing.T) {
	db := getTestDB(t)
	tableName := "test_saga_clear"
	cleanup := setupTestTable(t, db, tableName)
	defer cleanup()

	storage, err := NewPostgresStorage(db, tableName)
	if err != nil {
		t.Fatalf("NewPostgresStorage: %v", err)
	}

	ctx := context.Background()

	// Save a workflow
	err = storage.Save(ctx, "tx-clear", []StepContext{
		{Name: "step1", IdempotencyKey: "idem-1", Status: StepStatusCompleted},
	}, nil)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Clear it
	err = storage.Clear(ctx, "tx-clear")
	if err != nil {
		t.Fatalf("Clear: %v", err)
	}

	// Verify status is completed
	wf, err := storage.GetWorkflow(ctx, "tx-clear")
	if err != nil {
		t.Fatalf("GetWorkflow: %v", err)
	}
	if wf.Status != StatusCompleted {
		t.Errorf("status = %q, want %q", wf.Status, StatusCompleted)
	}
}

func TestPostgresStorage_GetWorkflow(t *testing.T) {
	db := getTestDB(t)
	tableName := "test_saga_get_workflow"
	cleanup := setupTestTable(t, db, tableName)
	defer cleanup()

	storage, err := NewPostgresStorage(db, tableName)
	if err != nil {
		t.Fatalf("NewPostgresStorage: %v", err)
	}

	ctx := context.Background()

	// Save a workflow
	steps := []StepContext{
		{Name: "step1", IdempotencyKey: "idem-1", Result: "result1", Status: StepStatusCompleted},
	}
	input := map[string]any{"amount": 99.99}

	err = storage.Save(ctx, "tx-get", steps, input)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Get workflow
	wf, err := storage.GetWorkflow(ctx, "tx-get")
	if err != nil {
		t.Fatalf("GetWorkflow: %v", err)
	}

	if wf == nil {
		t.Fatal("GetWorkflow returned nil")
	}
	if wf.ID != "tx-get" {
		t.Errorf("ID = %q, want %q", wf.ID, "tx-get")
	}
	if wf.Status != StatusPending {
		t.Errorf("Status = %q, want %q", wf.Status, StatusPending)
	}
	if len(wf.StepStack) != 1 {
		t.Errorf("StepStack length = %d, want 1", len(wf.StepStack))
	}
	if wf.CreatedAt.IsZero() {
		t.Error("CreatedAt should not be zero")
	}

	// Get non-existent
	notFound, err := storage.GetWorkflow(ctx, "non-existent")
	if err != nil {
		t.Fatalf("GetWorkflow non-existent: %v", err)
	}
	if notFound != nil {
		t.Error("expected nil for non-existent workflow")
	}
}

func TestPostgresStorage_UpdateStatus(t *testing.T) {
	db := getTestDB(t)
	tableName := "test_saga_update_status"
	cleanup := setupTestTable(t, db, tableName)
	defer cleanup()

	storage, err := NewPostgresStorage(db, tableName)
	if err != nil {
		t.Fatalf("NewPostgresStorage: %v", err)
	}

	ctx := context.Background()

	// Save a workflow
	err = storage.Save(ctx, "tx-status", []StepContext{}, nil)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Update to dead_letter with error
	workflowErr := &WorkflowError{
		StepName:  "step1",
		Error:     "step failed",
		Timestamp: time.Now(),
	}
	err = storage.UpdateStatus(ctx, "tx-status", StatusDeadLetter, workflowErr)
	if err != nil {
		t.Fatalf("UpdateStatus: %v", err)
	}

	wf, err := storage.GetWorkflow(ctx, "tx-status")
	if err != nil {
		t.Fatalf("GetWorkflow: %v", err)
	}
	if wf.Status != StatusDeadLetter {
		t.Errorf("Status = %q, want %q", wf.Status, StatusDeadLetter)
	}
	if wf.Error == nil {
		t.Error("Error should not be nil")
	}
	if wf.Error.StepName != "step1" {
		t.Errorf("Error.StepName = %q, want %q", wf.Error.StepName, "step1")
	}

	// Update status for non-existent workflow (should create it)
	err = storage.UpdateStatus(ctx, "tx-new", StatusFailed, nil)
	if err != nil {
		t.Fatalf("UpdateStatus for new workflow: %v", err)
	}

	wfNew, _ := storage.GetWorkflow(ctx, "tx-new")
	if wfNew == nil || wfNew.Status != StatusFailed {
		t.Error("expected new workflow to be created with failed status")
	}
}

func TestPostgresStorage_AtomicRetry(t *testing.T) {
	db := getTestDB(t)
	tableName := "test_saga_atomic_retry"
	cleanup := setupTestTable(t, db, tableName)
	defer cleanup()

	storage, err := NewPostgresStorage(db, tableName)
	if err != nil {
		t.Fatalf("NewPostgresStorage: %v", err)
	}

	ctx := context.Background()

	// Create a dead_letter workflow
	err = storage.UpdateStatus(ctx, "tx-retry", StatusDeadLetter, &WorkflowError{
		StepName: "step1",
		Error:    "failed",
	})
	if err != nil {
		t.Fatalf("UpdateStatus: %v", err)
	}

	// First retry should succeed
	count, err := storage.AtomicRetry(ctx, "tx-retry")
	if err != nil {
		t.Fatalf("AtomicRetry: %v", err)
	}
	if count != 1 {
		t.Errorf("retry count = %d, want 1", count)
	}

	// Verify status changed
	wf, _ := storage.GetWorkflow(ctx, "tx-retry")
	if wf.Status != StatusPending {
		t.Errorf("Status = %q, want %q", wf.Status, StatusPending)
	}
	if wf.Error != nil {
		t.Error("Error should be nil after retry")
	}

	// Second retry should fail (not in dead_letter)
	count, err = storage.AtomicRetry(ctx, "tx-retry")
	if err != nil {
		t.Fatalf("AtomicRetry second: %v", err)
	}
	if count != -1 {
		t.Errorf("expected -1 for non-dead_letter, got %d", count)
	}

	// Retry non-existent workflow
	count, err = storage.AtomicRetry(ctx, "non-existent")
	if err != nil {
		t.Fatalf("AtomicRetry non-existent: %v", err)
	}
	if count != -1 {
		t.Errorf("expected -1 for non-existent, got %d", count)
	}
}

func TestPostgresStorage_Query(t *testing.T) {
	db := getTestDB(t)
	tableName := "test_saga_query"
	cleanup := setupTestTable(t, db, tableName)
	defer cleanup()

	storage, err := NewPostgresStorage(db, tableName)
	if err != nil {
		t.Fatalf("NewPostgresStorage: %v", err)
	}

	ctx := context.Background()

	// Create workflows with different statuses
	_ = storage.UpdateStatus(ctx, "wf-1", StatusCompleted, nil)
	_ = storage.UpdateStatus(ctx, "wf-2", StatusFailed, nil)
	_ = storage.UpdateStatus(ctx, "wf-3", StatusDeadLetter, nil)
	_ = storage.UpdateStatus(ctx, "wf-4", StatusPending, nil)

	// Query all
	result, err := storage.Query(ctx, WorkflowFilter{Limit: 100})
	if err != nil {
		t.Fatalf("Query all: %v", err)
	}
	if result.Total != 4 {
		t.Errorf("total = %d, want 4", result.Total)
	}

	// Query by status
	result, err = storage.Query(ctx, WorkflowFilter{
		Status: []WorkflowStatus{StatusDeadLetter, StatusFailed},
		Limit:  100,
	})
	if err != nil {
		t.Fatalf("Query by status: %v", err)
	}
	if result.Total != 2 {
		t.Errorf("filtered total = %d, want 2", result.Total)
	}

	// Query with pagination
	result, err = storage.Query(ctx, WorkflowFilter{Limit: 2})
	if err != nil {
		t.Fatalf("Query with limit: %v", err)
	}
	if len(result.Workflows) != 2 {
		t.Errorf("paginated results = %d, want 2", len(result.Workflows))
	}
	if result.Total != 4 {
		t.Errorf("total should still be 4, got %d", result.Total)
	}

	// Query with offset
	result, err = storage.Query(ctx, WorkflowFilter{Limit: 100, Offset: 2})
	if err != nil {
		t.Fatalf("Query with offset: %v", err)
	}
	if len(result.Workflows) != 2 {
		t.Errorf("offset results = %d, want 2", len(result.Workflows))
	}
}

func TestPostgresStorage_QueryDateFilters(t *testing.T) {
	db := getTestDB(t)
	tableName := "test_saga_query_dates"
	cleanup := setupTestTable(t, db, tableName)
	defer cleanup()

	storage, err := NewPostgresStorage(db, tableName)
	if err != nil {
		t.Fatalf("NewPostgresStorage: %v", err)
	}

	ctx := context.Background()

	// Create a workflow
	_ = storage.UpdateStatus(ctx, "wf-date", StatusCompleted, nil)

	now := time.Now()
	past := now.Add(-1 * time.Hour)
	future := now.Add(1 * time.Hour)

	// Query created after (should find)
	result, err := storage.Query(ctx, WorkflowFilter{CreatedAfter: &past, Limit: 100})
	if err != nil {
		t.Fatalf("Query created after: %v", err)
	}
	if result.Total != 1 {
		t.Errorf("expected 1 workflow created after past, got %d", result.Total)
	}

	// Query created after future (should not find)
	result, err = storage.Query(ctx, WorkflowFilter{CreatedAfter: &future, Limit: 100})
	if err != nil {
		t.Fatalf("Query created after future: %v", err)
	}
	if result.Total != 0 {
		t.Errorf("expected 0 workflows created after future, got %d", result.Total)
	}

	// Query created before future (should find)
	result, err = storage.Query(ctx, WorkflowFilter{CreatedBefore: &future, Limit: 100})
	if err != nil {
		t.Fatalf("Query created before: %v", err)
	}
	if result.Total != 1 {
		t.Errorf("expected 1 workflow created before future, got %d", result.Total)
	}
}

func TestPostgresStorage_CountByStatus(t *testing.T) {
	db := getTestDB(t)
	tableName := "test_saga_count"
	cleanup := setupTestTable(t, db, tableName)
	defer cleanup()

	storage, err := NewPostgresStorage(db, tableName)
	if err != nil {
		t.Fatalf("NewPostgresStorage: %v", err)
	}

	ctx := context.Background()

	// Create workflows
	_ = storage.UpdateStatus(ctx, "wf-1", StatusCompleted, nil)
	_ = storage.UpdateStatus(ctx, "wf-2", StatusCompleted, nil)
	_ = storage.UpdateStatus(ctx, "wf-3", StatusDeadLetter, nil)

	// Count completed
	count, err := storage.CountByStatus(ctx, StatusCompleted)
	if err != nil {
		t.Fatalf("CountByStatus: %v", err)
	}
	if count != 2 {
		t.Errorf("completed count = %d, want 2", count)
	}

	// Count dead_letter
	count, err = storage.CountByStatus(ctx, StatusDeadLetter)
	if err != nil {
		t.Fatalf("CountByStatus: %v", err)
	}
	if count != 1 {
		t.Errorf("dead_letter count = %d, want 1", count)
	}

	// Count multiple statuses
	count, err = storage.CountByStatus(ctx, StatusCompleted, StatusDeadLetter)
	if err != nil {
		t.Fatalf("CountByStatus multiple: %v", err)
	}
	if count != 3 {
		t.Errorf("combined count = %d, want 3", count)
	}

	// Count with no statuses
	count, err = storage.CountByStatus(ctx)
	if err != nil {
		t.Fatalf("CountByStatus empty: %v", err)
	}
	if count != 0 {
		t.Errorf("empty count = %d, want 0", count)
	}
}

func TestPostgresStorage_InvalidTableName(t *testing.T) {
	db := getTestDB(t)
	defer db.Close()

	// Invalid table names should be rejected
	invalidNames := []string{
		"table; DROP TABLE users;--",
		"123_invalid",
		"table-name",
		"table.name",
		"",
	}

	for _, name := range invalidNames {
		_, err := NewPostgresStorage(db, name)
		if err == nil && name != "" {
			t.Errorf("expected error for invalid table name %q", name)
		}
	}

	// Valid table names should work
	validNames := []string{
		"transactions",
		"saga_workflows",
		"_private_table",
		"MyTable",
	}

	for _, name := range validNames {
		_, err := NewPostgresStorage(db, name)
		if err != nil {
			t.Errorf("unexpected error for valid table name %q: %v", name, err)
		}
	}
}

func TestPostgresStorage_IsProductionSafe(t *testing.T) {
	db := getTestDB(t)
	defer db.Close()

	storage, err := NewPostgresStorage(db, "transactions")
	if err != nil {
		t.Fatalf("NewPostgresStorage: %v", err)
	}

	if !storage.IsProductionSafe() {
		t.Error("PostgresStorage should be production safe")
	}
}

func TestPostgresLock_AcquireAndRelease(t *testing.T) {
	db1 := getTestDB(t)
	defer db1.Close()

	// Need a second connection to test lock contention
	// PostgreSQL advisory locks are re-entrant within the same session
	db2 := getTestDB(t)
	defer db2.Close()

	lock1 := NewPostgresLock(db1)
	lock2 := NewPostgresLock(db2)
	ctx := context.Background()

	// Acquire lock on first connection
	token, err := lock1.Acquire(ctx, "test-lock-1", time.Minute)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if token == "" {
		t.Error("expected non-empty token")
	}

	// Try to acquire same lock from second connection (should fail)
	_, err = lock2.Acquire(ctx, "test-lock-1", time.Minute)
	if err == nil {
		t.Error("expected error when acquiring already-held lock")
	}
	var lockedErr *TransactionLockedError
	if !errors.As(err, &lockedErr) {
		t.Errorf("expected TransactionLockedError, got %T", err)
	}

	// Release lock from first connection
	err = lock1.Release(ctx, "test-lock-1", token)
	if err != nil {
		t.Fatalf("Release: %v", err)
	}

	// Now second connection should be able to acquire
	token2, err := lock2.Acquire(ctx, "test-lock-1", time.Minute)
	if err != nil {
		t.Fatalf("Acquire after release: %v", err)
	}
	_ = lock2.Release(ctx, "test-lock-1", token2)
}

func TestPostgresLock_DifferentTransactions(t *testing.T) {
	db := getTestDB(t)
	defer db.Close()

	lock := NewPostgresLock(db)
	ctx := context.Background()

	// Acquire locks for different transactions
	token1, err := lock.Acquire(ctx, "tx-1", time.Minute)
	if err != nil {
		t.Fatalf("Acquire tx-1: %v", err)
	}

	token2, err := lock.Acquire(ctx, "tx-2", time.Minute)
	if err != nil {
		t.Fatalf("Acquire tx-2: %v", err)
	}

	// Both should succeed
	if token1 == "" || token2 == "" {
		t.Error("expected non-empty tokens for both locks")
	}

	// Cleanup
	_ = lock.Release(ctx, "tx-1", token1)
	_ = lock.Release(ctx, "tx-2", token2)
}

// TestPostgresIntegration_FullWorkflow tests a complete saga workflow with PostgreSQL.
func TestPostgresIntegration_FullWorkflow(t *testing.T) {
	db := getTestDB(t)
	tableName := "test_saga_full_workflow"
	cleanup := setupTestTable(t, db, tableName)
	defer cleanup()

	storage, err := NewPostgresStorage(db, tableName)
	if err != nil {
		t.Fatalf("NewPostgresStorage: %v", err)
	}

	lock := NewPostgresLock(db)
	ctx := context.Background()

	// Create and run a transaction
	tx, err := NewTransaction("full-workflow-1", storage, TransactionOptions{
		IdempotencyKey: "full-idem-1",
		Lock:           lock,
		Input:          map[string]any{"orderId": "order-123"},
	})
	if err != nil {
		t.Fatalf("NewTransaction: %v", err)
	}

	var results []string

	err = tx.Run(ctx, func(ctx context.Context, tx *Transaction) error {
		r1, err := Step(ctx, tx, "step1", StepDefinition[string]{
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
		results = append(results, r1)

		r2, err := Step(ctx, tx, "step2", StepDefinition[string]{
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
		results = append(results, r2)

		return nil
	})

	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(results) != 2 || results[0] != "result1" || results[1] != "result2" {
		t.Errorf("results = %v, want [result1 result2]", results)
	}

	// Verify final state
	wf, err := storage.GetWorkflow(ctx, "full-workflow-1")
	if err != nil {
		t.Fatalf("GetWorkflow: %v", err)
	}
	if wf.Status != StatusCompleted {
		t.Errorf("status = %q, want %q", wf.Status, StatusCompleted)
	}
}

// TestPostgresIntegration_CompensationFlow tests compensation with PostgreSQL.
func TestPostgresIntegration_CompensationFlow(t *testing.T) {
	db := getTestDB(t)
	tableName := "test_saga_compensation"
	cleanup := setupTestTable(t, db, tableName)
	defer cleanup()

	storage, err := NewPostgresStorage(db, tableName)
	if err != nil {
		t.Fatalf("NewPostgresStorage: %v", err)
	}

	lock := NewPostgresLock(db)
	ctx := context.Background()

	var compensated []string

	tx, _ := NewTransaction("comp-workflow", storage, TransactionOptions{
		IdempotencyKey: "comp-idem",
		Lock:           lock,
	})

	stepErr := errors.New("step2 failed")

	err = tx.Run(ctx, func(ctx context.Context, tx *Transaction) error {
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

		return stepErr
	})

	if err == nil {
		t.Fatal("expected error from failed step")
	}

	if len(compensated) != 1 || compensated[0] != "step1" {
		t.Errorf("compensated = %v, want [step1]", compensated)
	}

	wf, _ := storage.GetWorkflow(ctx, "comp-workflow")
	if wf.Status != StatusFailed {
		t.Errorf("status = %q, want %q", wf.Status, StatusFailed)
	}
}
