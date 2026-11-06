// Package decision produces workload reload decisions from objects and events
// that are handed to it by the caller. It never lists, watches or fetches
// anything from a Kubernetes cluster and it never receives raw Secret values:
// trigger events only carry the precomputed SHA of the changed resource.
//
// Decision processing is split into phases. Before every phase the engine
// checks the caller supplied time, workload count and memory budgets as well as
// cancellation of the caller context. A decision is committed at most once per
// workload. Cancellation, shutdown and budget exhaustion return structured
// *PhaseError values that identify the phase and reason, and all temporary
// resources (scratch buffers, inflight state) are released.
package decision

// Phase identifies a stage of workload reload decision processing.
type Phase uint8

const (
	// PhaseNone is the zero value, reported when a phase is not applicable.
	PhaseNone Phase = iota
	// PhaseInit covers engine construction and initialization.
	PhaseInit
	// PhaseEvaluate admits an event, charges the budgets and decides whether a
	// caller-provided workload needs to be reloaded.
	PhaseEvaluate
	// PhaseCommit turns a positive evaluation into a recorded reload decision.
	// A workload reaches this phase at most once.
	PhaseCommit
	// PhaseClose shuts the engine down and releases temporary resources.
	PhaseClose
)

// String returns a stable, lowercase identifier used in error messages and metrics.
func (p Phase) String() string {
	switch p {
	case PhaseInit:
		return "init"
	case PhaseEvaluate:
		return "evaluate"
	case PhaseCommit:
		return "commit"
	case PhaseClose:
		return "close"
	default:
		return "none"
	}
}
