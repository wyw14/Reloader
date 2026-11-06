package decision

import (
	"errors"
	"fmt"
)

// Reason explains why processing stopped at a given phase.
type Reason string

const (
	// ReasonInitFailed means the engine could not be initialized.
	ReasonInitFailed Reason = "initialization_failed"
	// ReasonCanceled means the caller context was canceled.
	ReasonCanceled Reason = "canceled"
	// ReasonTimeBudgetExceeded means the time budget was elapsed.
	ReasonTimeBudgetExceeded Reason = "time_budget_exceeded"
	// ReasonCountBudgetExceeded means the workload count budget was full.
	ReasonCountBudgetExceeded Reason = "count_budget_exceeded"
	// ReasonMemoryBudgetExceeded means the memory budget was full.
	ReasonMemoryBudgetExceeded Reason = "memory_budget_exceeded"
	// ReasonClosed means the engine had been (or was being) closed.
	ReasonClosed Reason = "engine_closed"
	// ReasonInvalidEvent means the caller supplied event could not be processed.
	ReasonInvalidEvent Reason = "invalid_event"
	// ReasonUnsupportedWorkload means the workload object has no supported view.
	ReasonUnsupportedWorkload Reason = "unsupported_workload"
	// ReasonPlanningFailed means the configured planner returned an error.
	ReasonPlanningFailed Reason = "planning_failed"
)

// Sentinel errors. They intentionally carry no resource payload so that they
// can never contain Secret material.
var (
	// ErrInvalidBudget is returned from NewEngine for a negative budget.
	ErrInvalidBudget = errors.New("invalid resource budget")
	// ErrCanceled wraps a caller context cancellation.
	ErrCanceled = errors.New("workload reload decision processing canceled")
	// ErrTimeBudgetExceeded is returned once the time budget is elapsed.
	ErrTimeBudgetExceeded = errors.New("time budget exhausted")
	// ErrCountBudgetExceeded is returned once the count budget is full.
	ErrCountBudgetExceeded = errors.New("workload count budget exhausted")
	// ErrMemoryBudgetExceeded is returned once the memory budget is full.
	ErrMemoryBudgetExceeded = errors.New("memory budget exhausted")
	// ErrEngineClosed is returned after Close, or when Close cancels in-flight work.
	ErrEngineClosed = errors.New("workload reload decision engine is closed")
	// ErrInvalidEvent wraps a malformed caller event.
	ErrInvalidEvent = errors.New("invalid workload reload event")
	// ErrUnsupportedWorkload is returned for workload kinds the default planner does not understand.
	ErrUnsupportedWorkload = errors.New("unsupported workload type")
	// ErrPlanningFailed wraps an arbitrary planner error without exposing its text.
	ErrPlanningFailed = errors.New("workload reload planning failed")
)

// PhaseError is the phased reason returned when processing stops. The Error
// rendering is restricted to the phase, reason, a non-sensitive resource
// reference and numeric budget usage. Raw error text from outside this package
// is deliberately never rendered so Secret values supplied by a planner cannot
// leak through error strings; the cause remains available via Unwrap/errors.As.
type PhaseError struct {
	// Phase is the phase that could not be entered or completed.
	Phase Phase
	// Reason categorizes the stop cause.
	Reason Reason
	// Resource identifies the workload, never the trigger resource data.
	Resource ResourceRef
	// Used and Limit describe budget usage for budget reasons; zero otherwise.
	Used  int64
	Limit int64
	// cause is the wrapped error, available via Unwrap but not necessarily
	// rendered in Error.
	cause error
}

func newPhaseError(phase Phase, reason Reason, ref ResourceRef, cause error, used, limit int64) *PhaseError {
	return &PhaseError{
		Phase:    phase,
		Reason:   reason,
		Resource: ref,
		Used:     used,
		Limit:    limit,
		cause:    cause,
	}
}

// Error renders a message that is safe even when the trigger was a Secret.
func (e *PhaseError) Error() string {
	if e == nil {
		return "<nil>"
	}
	msg := fmt.Sprintf("workload reload decision engine: phase %s stopped: %s", e.Phase, e.Reason)
	if e.Limit > 0 {
		msg += fmt.Sprintf(" (used %d of %d)", e.Used, e.Limit)
	}
	if e.Resource.Name != "" {
		msg += " while processing " + e.Resource.String()
	}
	// Only render text of the package-owned, static sentinels. External causes
	// (for example an arbitrary planner error) are exposed through Unwrap but
	// never printed, because their text is outside the Secret-safety boundary.
	if text := safeCauseText(e.cause); text != "" {
		msg += ": " + text
	}
	return msg
}

// Unwrap exposes the underlying cause for errors.Is and errors.As.
func (e *PhaseError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

// safeCauseText returns the cause message only for errors owned by this
// package, whose wording is known to be Secret-free.
func safeCauseText(cause error) string {
	if cause == nil {
		return ""
	}
	for _, safe := range []error{
		ErrInvalidBudget,
		ErrCanceled,
		ErrTimeBudgetExceeded,
		ErrCountBudgetExceeded,
		ErrMemoryBudgetExceeded,
		ErrEngineClosed,
		ErrInvalidEvent,
		ErrUnsupportedWorkload,
		ErrPlanningFailed,
	} {
		if errors.Is(cause, safe) {
			return safe.Error()
		}
	}
	return ""
}
