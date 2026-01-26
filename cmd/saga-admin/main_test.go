//go:build integration

package main

import (
	"bytes"
	"context"
	"database/sql"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	saga "github.com/grafikui/saga-engine-go"
	_ "github.com/lib/pq"
)

// testDB holds the test database connection.
var testDB *sql.DB

// testStorage holds the test storage instance.
var testStorage *saga.PostgresStorage

// testTableName is the table name used for tests.
const testTableName = "test_saga_admin"

// setupTestEnv sets up the test environment.
func setupTestEnv(t *testing.T) func() {
	t.Helper()

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set, skipping CLI integration test")
	}

	var err error
	testDB, err = sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("Failed to connect to database: %v", err)
	}

	if err := testDB.Ping(); err != nil {
		testDB.Close()
		t.Fatalf("Failed to ping database: %v", err)
	}

	// Create test table
	createSQL := `
		CREATE TABLE IF NOT EXISTS ` + testTableName + ` (
			id TEXT PRIMARY KEY,
			status TEXT NOT NULL DEFAULT 'pending',
			step_stack JSONB NOT NULL DEFAULT '[]',
			input JSONB,
			retry_count INTEGER NOT NULL DEFAULT 0,
			error JSONB,
			created_at TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP
		)`

	if _, err := testDB.Exec(createSQL); err != nil {
		testDB.Close()
		t.Fatalf("Failed to create test table: %v", err)
	}

	testStorage, err = saga.NewPostgresStorage(testDB, testTableName)
	if err != nil {
		testDB.Exec("DROP TABLE IF EXISTS " + testTableName)
		testDB.Close()
		t.Fatalf("Failed to create storage: %v", err)
	}

	// Set global vars for CLI
	databaseURL = dsn
	tableName = testTableName

	return func() {
		testDB.Exec("DROP TABLE IF EXISTS " + testTableName)
		testDB.Close()
	}
}

// captureOutput captures stdout during function execution.
func captureOutput(t *testing.T, f func()) string {
	t.Helper()

	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	f()

	w.Close()
	os.Stdout = old

	var buf bytes.Buffer
	io.Copy(&buf, r)
	return buf.String()
}

// captureStderr captures stderr during function execution.
func captureStderr(t *testing.T, f func()) string {
	t.Helper()

	old := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w

	f()

	w.Close()
	os.Stderr = old

	var buf bytes.Buffer
	io.Copy(&buf, r)
	return buf.String()
}

func TestTruncate(t *testing.T) {
	tests := []struct {
		input    string
		maxLen   int
		expected string
	}{
		{"hello", 10, "hello"},
		{"hello", 5, "hello"},
		{"hello world", 8, "hello..."},
		{"abc", 3, "abc"},
		{"abcd", 3, "..."},
		{"", 5, ""},
	}

	for _, tt := range tests {
		result := truncate(tt.input, tt.maxLen)
		if result != tt.expected {
			t.Errorf("truncate(%q, %d) = %q, want %q", tt.input, tt.maxLen, result, tt.expected)
		}
	}
}

func TestPrettyPrint(t *testing.T) {
	// Test valid JSON
	output := captureOutput(t, func() {
		prettyPrint([]byte(`{"key":"value"}`))
	})
	if !strings.Contains(output, "key") || !strings.Contains(output, "value") {
		t.Errorf("prettyPrint should format JSON, got: %s", output)
	}

	// Test invalid JSON (should print raw)
	output = captureOutput(t, func() {
		prettyPrint([]byte("not json"))
	})
	if !strings.Contains(output, "not json") {
		t.Errorf("prettyPrint should print raw for invalid JSON, got: %s", output)
	}
}

func TestPrintUsage(t *testing.T) {
	output := captureOutput(t, func() {
		printUsage()
	})

	expectedStrings := []string{
		"saga-admin",
		"Usage:",
		"Commands:",
		"list",
		"show",
		"retry",
		"stats",
		"dead-letter",
	}

	for _, s := range expectedStrings {
		if !strings.Contains(output, s) {
			t.Errorf("printUsage should contain %q, got: %s", s, output)
		}
	}
}

func TestRunList_Empty(t *testing.T) {
	cleanup := setupTestEnv(t)
	defer cleanup()

	output := captureOutput(t, func() {
		runList([]string{})
	})

	if !strings.Contains(output, "No workflows found") {
		t.Errorf("runList with empty table should say no workflows, got: %s", output)
	}
}

func TestRunList_WithWorkflows(t *testing.T) {
	cleanup := setupTestEnv(t)
	defer cleanup()

	ctx := context.Background()

	// Create some workflows
	_ = testStorage.UpdateStatus(ctx, "wf-list-1", saga.StatusCompleted, nil)
	_ = testStorage.UpdateStatus(ctx, "wf-list-2", saga.StatusPending, nil)
	_ = testStorage.UpdateStatus(ctx, "wf-list-3", saga.StatusDeadLetter, &saga.WorkflowError{
		StepName: "step1",
		Error:    "failed",
	})

	output := captureOutput(t, func() {
		runList([]string{})
	})

	if !strings.Contains(output, "wf-list-1") {
		t.Errorf("runList should show wf-list-1, got: %s", output)
	}
	if !strings.Contains(output, "3 workflows") || !strings.Contains(output, "Showing") {
		t.Errorf("runList should show count, got: %s", output)
	}
}

func TestRunList_FilterByStatus(t *testing.T) {
	cleanup := setupTestEnv(t)
	defer cleanup()

	ctx := context.Background()

	_ = testStorage.UpdateStatus(ctx, "wf-filter-1", saga.StatusCompleted, nil)
	_ = testStorage.UpdateStatus(ctx, "wf-filter-2", saga.StatusDeadLetter, nil)

	output := captureOutput(t, func() {
		runList([]string{"--status", "dead_letter"})
	})

	if !strings.Contains(output, "wf-filter-2") {
		t.Errorf("runList filtered should show dead_letter workflow, got: %s", output)
	}
	if strings.Contains(output, "wf-filter-1") {
		t.Errorf("runList filtered should NOT show completed workflow, got: %s", output)
	}
}

func TestRunList_Pagination(t *testing.T) {
	cleanup := setupTestEnv(t)
	defer cleanup()

	ctx := context.Background()

	// Create 5 workflows
	for i := 1; i <= 5; i++ {
		_ = testStorage.UpdateStatus(ctx, "wf-page-"+string(rune('0'+i)), saga.StatusCompleted, nil)
	}

	output := captureOutput(t, func() {
		runList([]string{"--limit", "2"})
	})

	if !strings.Contains(output, "Showing 2 of 5") {
		t.Errorf("runList with limit should show pagination info, got: %s", output)
	}
}

func TestRunShow_NotFound(t *testing.T) {
	cleanup := setupTestEnv(t)
	defer cleanup()

	// Capture exit and stderr
	oldOsExit := osExit
	var exitCode int
	osExit = func(code int) {
		exitCode = code
		panic("os.Exit called")
	}
	defer func() { osExit = oldOsExit }()

	stderr := captureStderr(t, func() {
		defer func() { recover() }()
		runShow([]string{"non-existent-workflow"})
	})

	if exitCode != 1 {
		t.Errorf("runShow for non-existent should exit with code 1, got: %d", exitCode)
	}
	if !strings.Contains(stderr, "not found") {
		t.Errorf("runShow for non-existent should say not found, got: %s", stderr)
	}
}

func TestRunShow_NoID(t *testing.T) {
	cleanup := setupTestEnv(t)
	defer cleanup()

	oldOsExit := osExit
	var exitCode int
	osExit = func(code int) {
		exitCode = code
		panic("os.Exit called")
	}
	defer func() { osExit = oldOsExit }()

	stderr := captureStderr(t, func() {
		defer func() { recover() }()
		runShow([]string{})
	})

	if exitCode != 1 {
		t.Errorf("runShow without ID should exit with code 1, got: %d", exitCode)
	}
	if !strings.Contains(stderr, "ID required") {
		t.Errorf("runShow without ID should ask for ID, got: %s", stderr)
	}
}

func TestRunShow_WithWorkflow(t *testing.T) {
	cleanup := setupTestEnv(t)
	defer cleanup()

	ctx := context.Background()

	// Create a workflow with steps
	_ = testStorage.Save(ctx, "wf-show-detail", []saga.StepContext{
		{Name: "step1", IdempotencyKey: "idem-1", Result: "result1", Status: saga.StepStatusCompleted},
	}, map[string]any{"orderId": "123"})

	output := captureOutput(t, func() {
		runShow([]string{"wf-show-detail"})
	})

	expectedStrings := []string{
		"wf-show-detail",
		"Status:",
		"pending",
		"step1",
	}

	for _, s := range expectedStrings {
		if !strings.Contains(output, s) {
			t.Errorf("runShow should contain %q, got: %s", s, output)
		}
	}
}

func TestRunShow_WithError(t *testing.T) {
	cleanup := setupTestEnv(t)
	defer cleanup()

	ctx := context.Background()

	_ = testStorage.UpdateStatus(ctx, "wf-show-error", saga.StatusDeadLetter, &saga.WorkflowError{
		StepName:          "payment",
		Error:             "insufficient funds",
		CompensationError: "refund failed",
		Timestamp:         time.Now(),
	})

	output := captureOutput(t, func() {
		runShow([]string{"wf-show-error"})
	})

	expectedStrings := []string{
		"dead_letter",
		"Error:",
		"payment",
		"insufficient funds",
		"refund failed",
	}

	for _, s := range expectedStrings {
		if !strings.Contains(output, s) {
			t.Errorf("runShow with error should contain %q, got: %s", s, output)
		}
	}
}

func TestRunStats(t *testing.T) {
	cleanup := setupTestEnv(t)
	defer cleanup()

	ctx := context.Background()

	// Create workflows with different statuses
	_ = testStorage.UpdateStatus(ctx, "wf-stats-1", saga.StatusCompleted, nil)
	_ = testStorage.UpdateStatus(ctx, "wf-stats-2", saga.StatusCompleted, nil)
	_ = testStorage.UpdateStatus(ctx, "wf-stats-3", saga.StatusFailed, nil)
	_ = testStorage.UpdateStatus(ctx, "wf-stats-4", saga.StatusDeadLetter, nil)

	output := captureOutput(t, func() {
		runStats([]string{})
	})

	expectedStrings := []string{
		"Statistics",
		"completed:",
		"failed:",
		"dead_letter:",
		"Total:",
	}

	for _, s := range expectedStrings {
		if !strings.Contains(output, s) {
			t.Errorf("runStats should contain %q, got: %s", s, output)
		}
	}

	// Check counts
	if !strings.Contains(output, "2") { // completed count
		t.Errorf("runStats should show completed count of 2, got: %s", output)
	}
}

func TestRunDeadLetter_Empty(t *testing.T) {
	cleanup := setupTestEnv(t)
	defer cleanup()

	output := captureOutput(t, func() {
		runDeadLetter([]string{})
	})

	if !strings.Contains(output, "No dead_letter workflows found") {
		t.Errorf("runDeadLetter with empty should say no workflows, got: %s", output)
	}
}

func TestRunDeadLetter_WithWorkflows(t *testing.T) {
	cleanup := setupTestEnv(t)
	defer cleanup()

	ctx := context.Background()

	_ = testStorage.UpdateStatus(ctx, "wf-dl-1", saga.StatusDeadLetter, &saga.WorkflowError{
		StepName: "charge",
		Error:    "payment declined",
	})
	_ = testStorage.UpdateStatus(ctx, "wf-dl-2", saga.StatusDeadLetter, &saga.WorkflowError{
		StepName: "ship",
		Error:    "out of stock",
	})

	output := captureOutput(t, func() {
		runDeadLetter([]string{})
	})

	expectedStrings := []string{
		"Dead Letter",
		"wf-dl-1",
		"wf-dl-2",
		"charge",
		"payment declined",
		"retry",
	}

	for _, s := range expectedStrings {
		if !strings.Contains(output, s) {
			t.Errorf("runDeadLetter should contain %q, got: %s", s, output)
		}
	}
}

func TestRunRetry_NoID(t *testing.T) {
	cleanup := setupTestEnv(t)
	defer cleanup()

	oldOsExit := osExit
	var exitCode int
	osExit = func(code int) {
		exitCode = code
		panic("os.Exit called")
	}
	defer func() { osExit = oldOsExit }()

	stderr := captureStderr(t, func() {
		defer func() { recover() }()
		runRetry([]string{})
	})

	if exitCode != 1 {
		t.Errorf("runRetry without ID should exit with code 1")
	}
	if !strings.Contains(stderr, "ID required") {
		t.Errorf("runRetry without ID should ask for ID, got: %s", stderr)
	}
}

func TestRunRetry_NotFound(t *testing.T) {
	cleanup := setupTestEnv(t)
	defer cleanup()

	oldOsExit := osExit
	var exitCode int
	osExit = func(code int) {
		exitCode = code
		panic("os.Exit called")
	}
	defer func() { osExit = oldOsExit }()

	stderr := captureStderr(t, func() {
		defer func() { recover() }()
		runRetry([]string{"non-existent"})
	})

	if exitCode != 1 {
		t.Errorf("runRetry for non-existent should exit with code 1")
	}
	if !strings.Contains(stderr, "not found") {
		t.Errorf("runRetry for non-existent should say not found, got: %s", stderr)
	}
}

func TestRunRetry_NotDeadLetter(t *testing.T) {
	cleanup := setupTestEnv(t)
	defer cleanup()

	ctx := context.Background()
	_ = testStorage.UpdateStatus(ctx, "wf-retry-completed", saga.StatusCompleted, nil)

	oldOsExit := osExit
	var exitCode int
	osExit = func(code int) {
		exitCode = code
		panic("os.Exit called")
	}
	defer func() { osExit = oldOsExit }()

	stderr := captureStderr(t, func() {
		defer func() { recover() }()
		runRetry([]string{"wf-retry-completed"})
	})

	if exitCode != 1 {
		t.Errorf("runRetry for non-dead_letter should exit with code 1")
	}
	if !strings.Contains(stderr, "not in dead_letter") {
		t.Errorf("runRetry for non-dead_letter should explain status, got: %s", stderr)
	}
}

func TestRunRetry_Success(t *testing.T) {
	cleanup := setupTestEnv(t)
	defer cleanup()

	ctx := context.Background()
	_ = testStorage.UpdateStatus(ctx, "wf-retry-success", saga.StatusDeadLetter, &saga.WorkflowError{
		StepName: "step1",
		Error:    "failed",
	})

	output := captureOutput(t, func() {
		runRetry([]string{"wf-retry-success"})
	})

	if !strings.Contains(output, "pending") {
		t.Errorf("runRetry should mention transition to pending, got: %s", output)
	}
	if !strings.Contains(output, "retry 1") {
		t.Errorf("runRetry should show retry count, got: %s", output)
	}

	// Verify in database
	wf, _ := testStorage.GetWorkflow(ctx, "wf-retry-success")
	if wf.Status != saga.StatusPending {
		t.Errorf("workflow should be pending after retry, got: %s", wf.Status)
	}
	if wf.RetryCount != 1 {
		t.Errorf("workflow retry count should be 1, got: %d", wf.RetryCount)
	}
}

func TestRunRetry_ExceedsRetryCap(t *testing.T) {
	cleanup := setupTestEnv(t)
	defer cleanup()

	ctx := context.Background()

	// Create workflow and set retry count to max
	_ = testStorage.UpdateStatus(ctx, "wf-retry-capped", saga.StatusDeadLetter, &saga.WorkflowError{
		StepName: "step1",
		Error:    "failed",
	})

	// Manually update retry_count to max
	_, _ = testDB.Exec("UPDATE "+testTableName+" SET retry_count = $1 WHERE id = $2",
		saga.MaxRetryCount, "wf-retry-capped")

	oldOsExit := osExit
	var exitCode int
	osExit = func(code int) {
		exitCode = code
		panic("os.Exit called")
	}
	defer func() { osExit = oldOsExit }()

	stderr := captureStderr(t, func() {
		defer func() { recover() }()
		runRetry([]string{"wf-retry-capped"})
	})

	if exitCode != 1 {
		t.Errorf("runRetry for capped workflow should exit with code 1")
	}
	if !strings.Contains(stderr, "exceeded retry cap") {
		t.Errorf("runRetry for capped should mention retry cap, got: %s", stderr)
	}
}

func TestGetStorage_NoURL(t *testing.T) {
	// Save and clear database URL
	oldURL := databaseURL
	databaseURL = ""
	defer func() { databaseURL = oldURL }()

	oldOsExit := osExit
	var exitCode int
	osExit = func(code int) {
		exitCode = code
		panic("os.Exit called")
	}
	defer func() { osExit = oldOsExit }()

	stderr := captureStderr(t, func() {
		defer func() { recover() }()
		getStorage()
	})

	if exitCode != 1 {
		t.Errorf("getStorage without URL should exit with code 1")
	}
	if !strings.Contains(stderr, "DATABASE_URL") {
		t.Errorf("getStorage without URL should mention DATABASE_URL, got: %s", stderr)
	}
}

