package decision

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/stakater/Reloader/internal/pkg/constants"
	"github.com/stakater/Reloader/pkg/common"
)

// maxPooledScratchCap bounds the capacity of scratch buffers retained in the
// pool. Larger buffers are dropped after the event so big inputs cannot pin
// memory for the lifetime of the engine.
const maxPooledScratchCap = 64 * 1024

// Decision is a committed workload reload decision. It is safe to read after
// Process returns. SHA is the hash of the trigger resource; raw Secret values
// are never present.
type Decision struct {
	Resource   ResourceRef
	AutoReload bool
	Result     constants.Result
	EnvVar     string
	Patch      *Patch
	SHA        string
}

// Stats is a point-in-time snapshot of engine activity and budget consumption.
type Stats struct {
	// Evaluated counts workloads admitted to the evaluate phase.
	Evaluated int64
	// Committed counts workloads for which a decision was recorded.
	Committed int64
	// Skipped counts evaluated workloads that turned out to be up to date.
	Skipped int64
	// Aborted counts events stopped by cancellation, shutdown or budgets.
	Aborted int64
	// ScratchOutstanding counts currently borrowed temporary buffers.
	ScratchOutstanding int64
	// Budget reports cumulative budget consumption.
	Budget BudgetUsage
}

// Engine runs phased workload reload decisions under caller supplied budgets
// and a cancellable context. The zero Engine is not usable; create one with
// NewEngine.
//
// An Engine is safe for concurrent use. Every workload (identified by
// kind/namespace/name) reaches the commit phase at most once: duplicate events
// coalesce onto the in-flight or already committed decision.
type Engine struct {
	mu        sync.Mutex
	gate      budgetGate
	opts      *common.ReloaderOptions
	planner   Planner
	now       func() time.Time
	awaitHook func()
	pool      *sync.Pool
	scratch   map[*bytes.Buffer]struct{}
	committed map[string]Decision
	order     []string
	inflight  map[string]*inflight
	closed    bool

	// engineCtx is canceled with ErrEngineClosed by Close. Derived per-event
	// contexts are canceled together with it so in-flight planning unblocks.
	engineCtx    context.Context
	engineCancel context.CancelCauseFunc

	// wg tracks leaders: goroutines that evaluate and may commit a workload.
	wg sync.WaitGroup

	evaluatedN atomic.Int64
	committedN atomic.Int64
	skippedN   atomic.Int64
	abortedN   atomic.Int64
	scratchN   atomic.Int64
}

// Option customizes an Engine at construction time.
type Option func(*engineConfig)

type engineConfig struct {
	planner Planner
	now     func() time.Time
	onAwait func()
}

// WithPlanner overrides the built-in planner. The custom planner must stay
// pure: it may only read the passed event and must never access the cluster.
func WithPlanner(planner Planner) Option {
	return func(c *engineConfig) {
		c.planner = planner
	}
}

// WithClock injects the time source used for the time budget. Production code
// uses the wall clock; tests use a controlled clock to exercise the exact
// deadline boundary deterministically.
func WithClock(now func() time.Time) Option {
	return func(c *engineConfig) {
		c.now = now
	}
}

// withAwaitHook installs a callback invoked when Process starts coalescing on
// an in-flight leader. It exists to make concurrent coalescing tests
// deterministic.
func withAwaitHook(hook func()) Option {
	return func(c *engineConfig) {
		c.onAwait = hook
	}
}

// NewEngine performs the init phase: it validates the budget, prepares scratch
// resources and returns an init-phase PhaseError on failure. opts may be nil,
// in which case the process wide ReloaderOptions are used.
func NewEngine(budget Budget, opts *common.ReloaderOptions, options ...Option) (*Engine, error) {
	cfg := engineConfig{planner: DefaultPlanner, now: time.Now}
	for _, option := range options {
		option(&cfg)
	}
	if err := budget.Validate(); err != nil {
		return nil, newPhaseError(PhaseInit, ReasonInitFailed, ResourceRef{}, err, 0, 0)
	}
	if cfg.planner == nil {
		return nil, newPhaseError(PhaseInit, ReasonInitFailed, ResourceRef{}, ErrPlanningFailed, 0, 0)
	}
	if cfg.now == nil {
		cfg.now = time.Now
	}
	if opts == nil {
		opts = common.GetCommandLineOptions()
	}

	engineCtx, cancel := context.WithCancelCause(context.Background())
	now := cfg.now()

	return &Engine{
		gate:         newBudgetGate(budget, now),
		opts:         opts,
		planner:      cfg.planner,
		now:          cfg.now,
		awaitHook:    cfg.onAwait,
		pool:         &sync.Pool{},
		scratch:      make(map[*bytes.Buffer]struct{}),
		committed:    make(map[string]Decision),
		inflight:     make(map[string]*inflight),
		engineCtx:    engineCtx,
		engineCancel: cancel,
	}, nil
}

type inflightOutcome uint8

const (
	inflightPending inflightOutcome = iota
	inflightCommitted
	inflightSkipped
	inflightAborted
)

// inflight coalesces concurrent events for the same workload onto one leader.
type inflight struct {
	done     chan struct{}
	outcome  inflightOutcome
	decision Decision
	err      *PhaseError
}

// Process evaluates one caller-provided event.
//
// Budgets and ctx are checked before entering the evaluate and commit phases.
// A positive result is committed at most once per workload; repeat calls for an
// already committed workload return the same decision without consuming budget.
// On cancellation, shutdown or budget exhaustion it returns a *PhaseError and
// releases all temporary resources acquired for the event.
//
// A nil decision with a nil error means the workload was evaluated and did not
// need reloading.
func (e *Engine) Process(parent context.Context, ev Event) (*Decision, error) {
	if parent == nil {
		parent = context.Background()
	}
	ref, bad := eventRef(ev)
	if bad != nil {
		e.abortedN.Add(1)
		return nil, bad
	}

	ctx, stop := context.WithCancelCause(parent)
	defer stop(nil)
	unlinkClose := context.AfterFunc(e.engineCtx, func() {
		stop(context.Cause(e.engineCtx))
	})
	defer unlinkClose()

	key := ref.key()

	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return nil, e.failure(PhaseEvaluate, ReasonClosed, ref, ErrEngineClosed, 0, 0)
	}
	if existing, ok := e.committed[key]; ok {
		e.mu.Unlock()
		decision := existing
		return &decision, nil
	}
	if peer, ok := e.inflight[key]; ok {
		e.mu.Unlock()
		return e.awaitInflight(ctx, peer, ref)
	}
	peer := &inflight{done: make(chan struct{})}
	e.inflight[key] = peer
	e.wg.Add(1)
	e.mu.Unlock()

	var (
		commit   bool
		decision Decision
		failure  *PhaseError
	)
	// Outcome is published before the WaitGroup is released so that Close
	// cannot outrun result publication.
	defer e.wg.Done()
	defer func() {
		e.mu.Lock()
		switch {
		case failure != nil:
			peer.outcome = inflightAborted
			peer.err = failure
		case commit:
			peer.outcome = inflightCommitted
			peer.decision = decision
			e.committed[key] = decision
			e.order = append(e.order, key)
			e.committedN.Add(1)
		default:
			peer.outcome = inflightSkipped
			e.skippedN.Add(1)
		}
		delete(e.inflight, key)
		close(peer.done)
		e.mu.Unlock()
	}()

	// Phase: evaluate — gate before entering.
	e.mu.Lock()
	failure = e.stopped(PhaseEvaluate, ref, ctx)
	e.mu.Unlock()
	if failure != nil {
		e.abortedN.Add(1)
		return nil, failure
	}

	// Scratch is borrowed only after passing the gate and is always released,
	// including every abort path below.
	buf := e.borrowScratch()
	defer e.releaseScratch(buf)

	size, err := estimateObjectSize(buf, ev.Workload)
	if err != nil {
		failure = newPhaseError(PhaseEvaluate, ReasonInvalidEvent, ref, ErrInvalidEvent, 0, 0)
		e.abortedN.Add(1)
		return nil, failure
	}

	// Atomic check-and-charge under one lock hold: either count and memory are
	// both reserved or neither, so an abort never leaves a partial reservation.
	e.mu.Lock()
	switch {
	case e.gate.countExceeded(1):
		failure = newPhaseError(PhaseEvaluate, ReasonCountBudgetExceeded, ref, ErrCountBudgetExceeded,
			int64(e.gate.usedCount), int64(e.gate.budget.Count))
	case e.gate.memoryExceeded(size):
		failure = newPhaseError(PhaseEvaluate, ReasonMemoryBudgetExceeded, ref, ErrMemoryBudgetExceeded,
			e.gate.usedMemory, e.gate.budget.Memory)
	default:
		failure = e.stopped(PhaseEvaluate, ref, ctx)
		if failure == nil {
			e.gate.charge(1, size)
			e.evaluatedN.Add(1)
		}
	}
	e.mu.Unlock()
	if failure != nil {
		e.abortedN.Add(1)
		return nil, failure
	}

	plan, needsReload, planErr := e.planner(ctx, ev, e.opts)
	if planErr != nil {
		failure = e.planError(ctx, ref, planErr)
		e.abortedN.Add(1)
		return nil, failure
	}
	if !needsReload || plan.Result != constants.Updated {
		// Evaluated but up to date or not reloadable: the skip outcome is
		// published by the deferred finalizer.
		return nil, nil
	}

	// Phase: commit — re-check everything before entering. A workload that
	// reaches here is recorded at most once.
	e.mu.Lock()
	failure = e.stopped(PhaseCommit, ref, ctx)
	e.mu.Unlock()
	if failure != nil {
		e.abortedN.Add(1)
		return nil, failure
	}

	decision = Decision{
		Resource:   ref,
		AutoReload: plan.AutoReload,
		Result:     plan.Result,
		EnvVar:     plan.EnvVar,
		Patch:      plan.Patch,
		SHA:        ev.Config.SHAValue,
	}
	commit = true
	result := decision
	return &result, nil
}

// awaitInflight coalesces onto a leader for the same workload. A canceled
// context aborts the wait; otherwise the leader's outcome is returned.
func (e *Engine) awaitInflight(ctx context.Context, peer *inflight, ref ResourceRef) (*Decision, error) {
	if e.awaitHook != nil {
		e.awaitHook()
	}
	select {
	case <-peer.done:
	case <-ctx.Done():
		return nil, e.cancelFailure(PhaseCommit, ref, ctx)
	}
	switch peer.outcome {
	case inflightCommitted:
		decision := peer.decision
		return &decision, nil
	case inflightSkipped:
		return nil, nil
	case inflightAborted:
		// Surface the leader's phased reason without double counting.
		return nil, peer.err
	default:
		return nil, nil
	}
}

// stopped returns a phased reason when the engine must not enter the next
// phase. The mutex must be held.
func (e *Engine) stopped(phase Phase, ref ResourceRef, ctx context.Context) *PhaseError {
	if err := ctx.Err(); err != nil {
		cause := context.Cause(ctx)
		if errors.Is(cause, ErrEngineClosed) || e.closed {
			return newPhaseError(phase, ReasonClosed, ref, ErrEngineClosed, 0, 0)
		}
		if cause == nil {
			cause = err
		}
		return newPhaseError(phase, ReasonCanceled, ref, cause, 0, 0)
	}
	if e.closed {
		return newPhaseError(phase, ReasonClosed, ref, ErrEngineClosed, 0, 0)
	}
	now := e.now()
	if e.gate.timeExceeded(now) {
		return newPhaseError(phase, ReasonTimeBudgetExceeded, ref, ErrTimeBudgetExceeded,
			int64(e.gate.elapsed(now)), int64(e.gate.budget.Time))
	}
	return nil
}

// planError maps a planner failure to a phased reason, preferring cancellation
// or shutdown observed while planning.
func (e *Engine) planError(ctx context.Context, ref ResourceRef, planErr error) *PhaseError {
	e.mu.Lock()
	defer e.mu.Unlock()
	if stop := e.stopped(PhaseEvaluate, ref, ctx); stop != nil {
		return stop
	}
	if errors.Is(planErr, ErrUnsupportedWorkload) {
		return newPhaseError(PhaseEvaluate, ReasonUnsupportedWorkload, ref, ErrUnsupportedWorkload, 0, 0)
	}
	// The external error is retained for Unwrap but not rendered in Error.
	return newPhaseError(PhaseEvaluate, ReasonPlanningFailed, ref, planErr, 0, 0)
}

func (e *Engine) cancelFailure(phase Phase, ref ResourceRef, ctx context.Context) *PhaseError {
	cause := context.Cause(ctx)
	if errors.Is(cause, ErrEngineClosed) {
		return e.failure(phase, ReasonClosed, ref, ErrEngineClosed, 0, 0)
	}
	if cause == nil {
		cause = ErrCanceled
	}
	return e.failure(phase, ReasonCanceled, ref, cause, 0, 0)
}

// failure constructs a phased error and counts the abort.
func (e *Engine) failure(phase Phase, reason Reason, ref ResourceRef, cause error, used, limit int64) *PhaseError {
	e.abortedN.Add(1)
	return newPhaseError(phase, reason, ref, cause, used, limit)
}

func (e *Engine) borrowScratch() *bytes.Buffer {
	e.mu.Lock()
	defer e.mu.Unlock()
	var buf *bytes.Buffer
	if e.pool != nil {
		if pooled := e.pool.Get(); pooled != nil {
			buf, _ = pooled.(*bytes.Buffer)
		}
	}
	if buf == nil {
		buf = new(bytes.Buffer)
	}
	e.scratch[buf] = struct{}{}
	e.scratchN.Add(1)
	return buf
}

func (e *Engine) releaseScratch(buf *bytes.Buffer) {
	if buf == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.scratch, buf)
	e.scratchN.Add(-1)
	if e.pool != nil && buf.Cap() <= maxPooledScratchCap {
		buf.Reset()
		e.pool.Put(buf)
	}
}

// Decisions returns a snapshot of committed decisions in commit order. It
// remains available after Close so callers can collect results during shutdown.
func (e *Engine) Decisions() []Decision {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]Decision, 0, len(e.order))
	for _, key := range e.order {
		out = append(out, e.committed[key])
	}
	return out
}

// Stats returns counters and current budget usage.
func (e *Engine) Stats() Stats {
	e.mu.Lock()
	usage := e.gate.usage(e.now())
	e.mu.Unlock()
	return Stats{
		Evaluated:          e.evaluatedN.Load(),
		Committed:          e.committedN.Load(),
		Skipped:            e.skippedN.Load(),
		Aborted:            e.abortedN.Load(),
		ScratchOutstanding: e.scratchN.Load(),
		Budget:             usage,
	}
}

// Close performs the shutdown phase: it prevents new work, cancels in-flight
// leaders so blocked planning unblocks, waits for them to reach a cancellation
// boundary and releases temporary resources. It is idempotent.
//
// Close must not be called from within a Planner goroutine executing on the
// same engine; canceling the context passed to Process is the supported way to
// stop such work.
func (e *Engine) Close() error {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return nil
	}
	e.closed = true
	e.engineCancel(ErrEngineClosed)
	pool := e.pool
	e.pool = nil
	e.mu.Unlock()

	// Leaders publish their outcome before releasing the WaitGroup.
	e.wg.Wait()

	e.mu.Lock()
	for buf := range e.scratch {
		delete(e.scratch, buf)
	}
	e.scratchN.Store(0)
	e.inflight = make(map[string]*inflight)
	e.mu.Unlock()

	// Dropping the last pool reference lets the garbage collector reclaim
	// retained scratch capacity; committed decisions stay available.
	_ = pool
	return nil
}
