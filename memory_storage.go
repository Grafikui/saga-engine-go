package saga

import (
	"context"
	"encoding/json"
	"sync"
	"time"
)

// MemoryStorage implements Storage using in-memory maps.
// WARNING: Not production safe - use only for testing.
type MemoryStorage struct {
	mu        sync.RWMutex
	workflows map[string]*WorkflowRecord
}

// NewMemoryStorage creates a new MemoryStorage.
func NewMemoryStorage() *MemoryStorage {
	return &MemoryStorage{
		workflows: make(map[string]*WorkflowRecord),
	}
}

// IsProductionSafe returns false - MemoryStorage is not production safe.
func (s *MemoryStorage) IsProductionSafe() bool {
	return false
}

// Save persists the current step stack for a transaction.
func (s *MemoryStorage) Save(ctx context.Context, txID string, steps []StepContext, input any) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	if existing, ok := s.workflows[txID]; ok {
		existing.StepStack = steps
		existing.UpdatedAt = now
	} else {
		// Convert input to json.RawMessage
		var inputJSON json.RawMessage
		if input != nil {
			b, err := json.Marshal(input)
			if err != nil {
				return err
			}
			inputJSON = b
		}
		s.workflows[txID] = &WorkflowRecord{
			ID:        txID,
			Status:    StatusPending,
			StepStack: steps,
			Input:     inputJSON,
			CreatedAt: now,
			UpdatedAt: now,
		}
	}
	return nil
}

// Load retrieves the step stack for a transaction.
func (s *MemoryStorage) Load(ctx context.Context, txID string) ([]StepContext, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if wf, ok := s.workflows[txID]; ok {
		return wf.StepStack, nil
	}
	return nil, nil
}

// Clear marks a transaction as completed.
func (s *MemoryStorage) Clear(ctx context.Context, txID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if wf, ok := s.workflows[txID]; ok {
		wf.Status = StatusCompleted
		wf.UpdatedAt = time.Now()
	}
	return nil
}

// GetWorkflow retrieves the full workflow record.
func (s *MemoryStorage) GetWorkflow(ctx context.Context, txID string) (*WorkflowRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if wf, ok := s.workflows[txID]; ok {
		// Return a copy to avoid race conditions
		copy := *wf
		return &copy, nil
	}
	return nil, nil
}

// UpdateStatus updates the workflow status and optionally sets an error.
func (s *MemoryStorage) UpdateStatus(ctx context.Context, txID string, status WorkflowStatus, workflowErr *WorkflowError) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	if wf, ok := s.workflows[txID]; ok {
		wf.Status = status
		wf.Error = workflowErr
		wf.UpdatedAt = now
	} else {
		s.workflows[txID] = &WorkflowRecord{
			ID:        txID,
			Status:    status,
			StepStack: []StepContext{},
			Error:     workflowErr,
			CreatedAt: now,
			UpdatedAt: now,
		}
	}
	return nil
}

// AtomicRetry atomically transitions a dead_letter workflow to pending.
func (s *MemoryStorage) AtomicRetry(ctx context.Context, txID string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if wf, ok := s.workflows[txID]; ok && wf.Status == StatusDeadLetter {
		wf.Status = StatusPending
		wf.RetryCount++
		wf.Error = nil
		wf.UpdatedAt = time.Now()
		return wf.RetryCount, nil
	}
	return -1, nil
}

// Query retrieves workflows matching the filter.
func (s *MemoryStorage) Query(ctx context.Context, filter WorkflowFilter) (*WorkflowQueryResult, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var workflows []WorkflowRecord
	for _, wf := range s.workflows {
		// Apply status filter
		if len(filter.Status) > 0 {
			found := false
			for _, st := range filter.Status {
				if wf.Status == st {
					found = true
					break
				}
			}
			if !found {
				continue
			}
		}

		// Apply date filters
		if filter.CreatedAfter != nil && wf.CreatedAt.Before(*filter.CreatedAfter) {
			continue
		}
		if filter.CreatedBefore != nil && wf.CreatedAt.After(*filter.CreatedBefore) {
			continue
		}
		if filter.UpdatedAfter != nil && wf.UpdatedAt.Before(*filter.UpdatedAfter) {
			continue
		}

		workflows = append(workflows, *wf)
	}

	total := len(workflows)

	// Apply pagination
	if filter.Offset > 0 && filter.Offset < len(workflows) {
		workflows = workflows[filter.Offset:]
	} else if filter.Offset >= len(workflows) {
		workflows = nil
	}

	limit := filter.Limit
	if limit <= 0 {
		limit = 100
	}
	if len(workflows) > limit {
		workflows = workflows[:limit]
	}

	return &WorkflowQueryResult{
		Workflows: workflows,
		Total:     total,
	}, nil
}

// CountByStatus counts workflows by status.
func (s *MemoryStorage) CountByStatus(ctx context.Context, statuses ...WorkflowStatus) (int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	count := 0
	for _, wf := range s.workflows {
		for _, st := range statuses {
			if wf.Status == st {
				count++
				break
			}
		}
	}
	return count, nil
}

// Ensure MemoryStorage implements Storage.
var _ Storage = (*MemoryStorage)(nil)
