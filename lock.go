package saga

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"fmt"
	"time"
)

// Lock is the interface for distributed locking.
type Lock interface {
	// Acquire attempts to acquire a lock for the transaction.
	// Returns a token if successful, or an error if the lock is held.
	Acquire(ctx context.Context, txID string, ttl time.Duration) (string, error)

	// Release releases the lock for the transaction.
	Release(ctx context.Context, txID string, token string) error
}

// NoOpLock is a lock that does nothing (for single-process use).
type NoOpLock struct{}

// Acquire always succeeds for NoOpLock.
func (l *NoOpLock) Acquire(ctx context.Context, txID string, ttl time.Duration) (string, error) {
	return "noop", nil
}

// Release does nothing for NoOpLock.
func (l *NoOpLock) Release(ctx context.Context, txID string, token string) error {
	return nil
}

// Ensure NoOpLock implements Lock.
var _ Lock = (*NoOpLock)(nil)

// PostgresLock implements Lock using PostgreSQL advisory locks.
type PostgresLock struct {
	db *sql.DB
}

// NewPostgresLock creates a new PostgresLock.
func NewPostgresLock(db *sql.DB) *PostgresLock {
	return &PostgresLock{db: db}
}

// hashToLockKey converts a transaction ID to a 64-bit lock key using SHA-256.
func hashToLockKey(txID string) int64 {
	hash := sha256.Sum256([]byte(txID))
	// Read first 8 bytes as signed int64
	return int64(binary.BigEndian.Uint64(hash[:8]))
}

// Acquire attempts to acquire a PostgreSQL advisory lock.
func (l *PostgresLock) Acquire(ctx context.Context, txID string, ttl time.Duration) (string, error) {
	lockKey := hashToLockKey(txID)

	var acquired bool
	err := l.db.QueryRowContext(ctx, "SELECT pg_try_advisory_lock($1)", lockKey).Scan(&acquired)
	if err != nil {
		return "", fmt.Errorf("advisory lock: %w", err)
	}

	if !acquired {
		return "", NewTransactionLockedError(txID)
	}

	// Return the lock key as the token
	return fmt.Sprintf("%d", lockKey), nil
}

// Release releases a PostgreSQL advisory lock.
func (l *PostgresLock) Release(ctx context.Context, txID string, token string) error {
	lockKey := hashToLockKey(txID)

	var released bool
	err := l.db.QueryRowContext(ctx, "SELECT pg_advisory_unlock($1)", lockKey).Scan(&released)
	if err != nil {
		return fmt.Errorf("advisory unlock: %w", err)
	}

	// Note: released will be false if we didn't hold the lock, which is fine
	return nil
}

// Ensure PostgresLock implements Lock.
var _ Lock = (*PostgresLock)(nil)
