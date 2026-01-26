package saga

import "time"

// TransactionEvents provides hooks for observability and monitoring.
// All callbacks are optional - only set the ones you need.
// Event handlers are called synchronously but wrapped in panic recovery,
// so a panicking handler won't break the transaction flow.
//
// Example:
//
//	events := &saga.TransactionEvents{
//	    OnStepComplete: func(name string, result any, duration time.Duration) {
//	        log.Printf("Step %s completed in %v", name, duration)
//	    },
//	    OnDeadLetter: func(id string, err *saga.WorkflowError) {
//	        alerting.SendAlert("Workflow %s moved to dead letter: %s", id, err.Error)
//	    },
//	}
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
