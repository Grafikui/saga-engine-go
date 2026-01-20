package saga

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"
)

var validTableName = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// PostgresStorage implements Storage using PostgreSQL.
type PostgresStorage struct {
	db        *sql.DB
	tableName string
}

// NewPostgresStorage creates a new PostgresStorage.
// tableName defaults to "transactions" if empty.
func NewPostgresStorage(db *sql.DB, tableName string) (*PostgresStorage, error) {
	if tableName == "" {
		tableName = "transactions"
	}
	if !validTableName.MatchString(tableName) {
		return nil, fmt.Errorf("invalid table name: %s", tableName)
	}
	return &PostgresStorage{db: db, tableName: tableName}, nil
}

// IsProductionSafe returns true - PostgresStorage is production safe.
func (s *PostgresStorage) IsProductionSafe() bool {
	return true
}

// Save persists the current step stack for a transaction.
func (s *PostgresStorage) Save(ctx context.Context, txID string, steps []StepContext, input any) error {
	stepsJSON, err := json.Marshal(steps)
	if err != nil {
		return fmt.Errorf("marshal steps: %w", err)
	}

	inputJSON, err := json.Marshal(input)
	if err != nil {
		return fmt.Errorf("marshal input: %w", err)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	// Check if row exists
	var exists bool
	checkQuery := fmt.Sprintf(`SELECT EXISTS(SELECT 1 FROM %s WHERE id = $1)`, s.tableName)
	if err := tx.QueryRowContext(ctx, checkQuery, txID).Scan(&exists); err != nil {
		return fmt.Errorf("check exists: %w", err)
	}

	if exists {
		// Update existing row
		updateQuery := fmt.Sprintf(`
			UPDATE %s
			SET step_stack = $1, updated_at = CURRENT_TIMESTAMP
			WHERE id = $2
		`, s.tableName)
		if _, err := tx.ExecContext(ctx, updateQuery, stepsJSON, txID); err != nil {
			return fmt.Errorf("update: %w", err)
		}
	} else {
		// Insert new row
		insertQuery := fmt.Sprintf(`
			INSERT INTO %s (id, status, step_stack, input, retry_count, created_at, updated_at)
			VALUES ($1, $2, $3, $4, 0, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
		`, s.tableName)
		if _, err := tx.ExecContext(ctx, insertQuery, txID, StatusPending, stepsJSON, inputJSON); err != nil {
			return fmt.Errorf("insert: %w", err)
		}
	}

	return tx.Commit()
}

// Load retrieves the step stack for a transaction.
func (s *PostgresStorage) Load(ctx context.Context, txID string) ([]StepContext, error) {
	query := fmt.Sprintf(`SELECT step_stack FROM %s WHERE id = $1`, s.tableName)
	var stepsJSON []byte
	err := s.db.QueryRowContext(ctx, query, txID).Scan(&stepsJSON)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}

	var steps []StepContext
	if err := json.Unmarshal(stepsJSON, &steps); err != nil {
		return nil, fmt.Errorf("unmarshal: %w", err)
	}
	return steps, nil
}

// Clear marks a transaction as completed.
func (s *PostgresStorage) Clear(ctx context.Context, txID string) error {
	query := fmt.Sprintf(`
		UPDATE %s
		SET status = $1, updated_at = CURRENT_TIMESTAMP
		WHERE id = $2
	`, s.tableName)
	_, err := s.db.ExecContext(ctx, query, StatusCompleted, txID)
	return err
}

// GetWorkflow retrieves the full workflow record.
func (s *PostgresStorage) GetWorkflow(ctx context.Context, txID string) (*WorkflowRecord, error) {
	query := fmt.Sprintf(`
		SELECT id, status, step_stack, input, retry_count, error, created_at, updated_at
		FROM %s WHERE id = $1
	`, s.tableName)

	var (
		record     WorkflowRecord
		stepsJSON  []byte
		inputJSON  []byte
		errorJSON  sql.NullString
	)

	err := s.db.QueryRowContext(ctx, query, txID).Scan(
		&record.ID,
		&record.Status,
		&stepsJSON,
		&inputJSON,
		&record.RetryCount,
		&errorJSON,
		&record.CreatedAt,
		&record.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}

	if err := json.Unmarshal(stepsJSON, &record.StepStack); err != nil {
		return nil, fmt.Errorf("unmarshal steps: %w", err)
	}
	record.Input = inputJSON

	if errorJSON.Valid {
		var we WorkflowError
		if err := json.Unmarshal([]byte(errorJSON.String), &we); err != nil {
			return nil, fmt.Errorf("unmarshal error: %w", err)
		}
		record.Error = &we
	}

	return &record, nil
}

// UpdateStatus updates the workflow status and optionally sets an error.
func (s *PostgresStorage) UpdateStatus(ctx context.Context, txID string, status WorkflowStatus, workflowErr *WorkflowError) error {
	var errorJSON any
	if workflowErr != nil {
		b, err := json.Marshal(workflowErr)
		if err != nil {
			return fmt.Errorf("marshal error: %w", err)
		}
		errorJSON = string(b)
	}

	query := fmt.Sprintf(`
		UPDATE %s
		SET status = $1, error = $2, updated_at = CURRENT_TIMESTAMP
		WHERE id = $3
	`, s.tableName)

	// If workflow doesn't exist, create it
	result, err := s.db.ExecContext(ctx, query, status, errorJSON, txID)
	if err != nil {
		return fmt.Errorf("update: %w", err)
	}

	rowsAffected, _ := result.RowsAffected()
	if rowsAffected == 0 {
		// Insert new row
		insertQuery := fmt.Sprintf(`
			INSERT INTO %s (id, status, step_stack, input, retry_count, error, created_at, updated_at)
			VALUES ($1, $2, '[]', '{}', 0, $3, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
		`, s.tableName)
		if _, err := s.db.ExecContext(ctx, insertQuery, txID, status, errorJSON); err != nil {
			return fmt.Errorf("insert: %w", err)
		}
	}

	return nil
}

// AtomicRetry atomically transitions a dead_letter workflow to pending.
func (s *PostgresStorage) AtomicRetry(ctx context.Context, txID string) (int, error) {
	query := fmt.Sprintf(`
		UPDATE %s
		SET status = $1, retry_count = retry_count + 1, error = NULL, updated_at = CURRENT_TIMESTAMP
		WHERE id = $2 AND status = $3
		RETURNING retry_count
	`, s.tableName)

	var retryCount int
	err := s.db.QueryRowContext(ctx, query, StatusPending, txID, StatusDeadLetter).Scan(&retryCount)
	if err == sql.ErrNoRows {
		return -1, nil
	}
	if err != nil {
		return -1, fmt.Errorf("query: %w", err)
	}
	return retryCount, nil
}

// Query retrieves workflows matching the filter.
func (s *PostgresStorage) Query(ctx context.Context, filter WorkflowFilter) (*WorkflowQueryResult, error) {
	// Build query
	query := fmt.Sprintf(`SELECT id, status, step_stack, input, retry_count, error, created_at, updated_at FROM %s WHERE 1=1`, s.tableName)
	countQuery := fmt.Sprintf(`SELECT COUNT(*) FROM %s WHERE 1=1`, s.tableName)
	args := []any{}
	argIndex := 1

	if len(filter.Status) > 0 {
		placeholders := ""
		for i, st := range filter.Status {
			if i > 0 {
				placeholders += ", "
			}
			placeholders += fmt.Sprintf("$%d", argIndex)
			args = append(args, st)
			argIndex++
		}
		statusFilter := fmt.Sprintf(" AND status IN (%s)", placeholders)
		query += statusFilter
		countQuery += statusFilter
	}

	if filter.CreatedAfter != nil {
		query += fmt.Sprintf(" AND created_at >= $%d", argIndex)
		countQuery += fmt.Sprintf(" AND created_at >= $%d", argIndex)
		args = append(args, *filter.CreatedAfter)
		argIndex++
	}

	if filter.CreatedBefore != nil {
		query += fmt.Sprintf(" AND created_at <= $%d", argIndex)
		countQuery += fmt.Sprintf(" AND created_at <= $%d", argIndex)
		args = append(args, *filter.CreatedBefore)
		argIndex++
	}

	if filter.UpdatedAfter != nil {
		query += fmt.Sprintf(" AND updated_at >= $%d", argIndex)
		countQuery += fmt.Sprintf(" AND updated_at >= $%d", argIndex)
		args = append(args, *filter.UpdatedAfter)
		argIndex++
	}

	// Get total count
	var total int
	if err := s.db.QueryRowContext(ctx, countQuery, args...).Scan(&total); err != nil {
		return nil, fmt.Errorf("count: %w", err)
	}

	// Add ordering and pagination
	query += " ORDER BY created_at DESC"

	limit := filter.Limit
	if limit <= 0 {
		limit = 100
	}
	query += fmt.Sprintf(" LIMIT %d", limit)

	if filter.Offset > 0 {
		query += fmt.Sprintf(" OFFSET %d", filter.Offset)
	}

	// Execute query
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	defer rows.Close()

	var workflows []WorkflowRecord
	for rows.Next() {
		var (
			record    WorkflowRecord
			stepsJSON []byte
			inputJSON []byte
			errorJSON sql.NullString
		)

		if err := rows.Scan(
			&record.ID,
			&record.Status,
			&stepsJSON,
			&inputJSON,
			&record.RetryCount,
			&errorJSON,
			&record.CreatedAt,
			&record.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}

		if err := json.Unmarshal(stepsJSON, &record.StepStack); err != nil {
			return nil, fmt.Errorf("unmarshal steps: %w", err)
		}
		record.Input = inputJSON

		if errorJSON.Valid {
			var we WorkflowError
			if err := json.Unmarshal([]byte(errorJSON.String), &we); err != nil {
				return nil, fmt.Errorf("unmarshal error: %w", err)
			}
			record.Error = &we
		}

		workflows = append(workflows, record)
	}

	return &WorkflowQueryResult{
		Workflows: workflows,
		Total:     total,
	}, nil
}

// CountByStatus counts workflows by status.
func (s *PostgresStorage) CountByStatus(ctx context.Context, statuses ...WorkflowStatus) (int, error) {
	if len(statuses) == 0 {
		return 0, nil
	}

	query := fmt.Sprintf(`SELECT COUNT(*) FROM %s WHERE status IN (`, s.tableName)
	args := make([]any, len(statuses))
	for i, st := range statuses {
		if i > 0 {
			query += ", "
		}
		query += fmt.Sprintf("$%d", i+1)
		args[i] = st
	}
	query += ")"

	var count int
	if err := s.db.QueryRowContext(ctx, query, args...).Scan(&count); err != nil {
		return 0, fmt.Errorf("count: %w", err)
	}
	return count, nil
}

// Ensure PostgresStorage implements Storage.
var _ Storage = (*PostgresStorage)(nil)
