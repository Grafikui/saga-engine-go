package saga

import "time"

// TransactionEvents provides hooks for observability.
type TransactionEvents struct {
	// Transaction lifecycle
	OnTransactionStart    func(id string, input any)
	OnTransactionComplete func(id string)
	OnTransactionFailed   func(id string, err error)

	// Step lifecycle
	OnStepStart    func(name string)
	OnStepComplete func(name string, result any, duration time.Duration)
	OnStepFailed   func(name string, err error, attempt int)
	OnStepRetry    func(name string, attempt int, err error)
	OnStepSkipped  func(name string, cachedResult any)
	OnStepTimeout  func(name string, timeout time.Duration)

	// Compensation lifecycle
	OnCompensationStart    func(name string)
	OnCompensationComplete func(name string)
	OnCompensationFailed   func(name string, err error)

	// Terminal state
	OnDeadLetter func(id string, err *WorkflowError)
}

// emitEvent safely calls an event handler, catching any panics.
func emitEvent(events *TransactionEvents, handler func()) {
	if events == nil || handler == nil {
		return
	}
	defer func() {
		// Catch panics from event handlers - never break transaction flow
		_ = recover()
	}()
	handler()
}
