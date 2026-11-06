package decision

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/stakater/Reloader/pkg/common"
)

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

type fakeClock struct {
	t time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Unix(1_700_000_000, 0)}
}

func (c *fakeClock) now() time.Time { return c.t }

func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

// blockingPlanner signals entered on every leader invocation, blocks until the
// context is canceled or release is closed, then delegates to inner.
type blockingPlanner struct {
	entered chan struct{}
	release chan struct{}
	calls   atomic.Int64
	inner   Planner
}

func newBlockingPlanner(inner Planner) *blockingPlanner {
	return &blockingPlanner{
		entered: make(chan struct{}, 1024),
		release: make(chan struct{}),
		inner:   inner,
	}
}

func (b *blockingPlanner) plan(ctx context.Context, ev Event, opts *common.ReloaderOptions) (Plan, bool, error) {
	b.calls.Add(1)
	select {
	case b.entered <- struct{}{}:
	default:
	}
	select {
	case <-ctx.Done():
		return Plan{}, false, ctx.Err()
	case <-b.release:
	}
	return b.inner(ctx, ev, opts)
}

func objectEncodedSize(t *testing.T, obj runtime.Object) int64 {
	t.Helper()
	var buf bytes.Buffer
	require.NoError(t, json.NewEncoder(&buf).Encode(obj))
	return int64(buf.Len())
}

func eventFor(t *testing.T, obj runtime.Object, config common.Config, kind string) Event {
	t.Helper()
	return Event{Config: config, Kind: kind, Workload: obj}
}

func secretEventFor(obj runtime.Object, sha string) Event {
	return Event{Config: secretConfig(sha), Kind: "Deployment", Workload: obj}
}

func requirePhaseError(t *testing.T, err error, phase Phase, reason Reason, target error) *PhaseError {
	t.Helper()
	require.Error(t, err)
	var phaseErr *PhaseError
	require.True(t, errors.As(err, &phaseErr), "expected *PhaseError, got %T: %v", err, err)
	assert.Equal(t, phase, phaseErr.Phase, "error: %v", err)
	assert.Equal(t, reason, phaseErr.Reason, "error: %v", err)
	if target != nil {
		assert.ErrorIs(t, err, target)
	}
	return phaseErr
}

func requireNoScratchLeak(t *testing.T, engine *Engine) {
	t.Helper()
	assert.Zero(t, engine.Stats().ScratchOutstanding, "temporary scratch buffers must be released")
}

// ---------------------------------------------------------------------------
// Init phase failures
// ---------------------------------------------------------------------------

func TestNewEngineInitFailures(t *testing.T) {
	cases := []struct {
		name   string
		budget Budget
	}{
		{"negative count", Budget{Count: -1}},
		{"negative memory", Budget{Memory: -10}},
		{"negative time", Budget{Time: -time.Second}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			engine, err := NewEngine(tc.budget, testOptions())
			requirePhaseError(t, err, PhaseInit, ReasonInitFailed, ErrInvalidBudget)
			assert.Nil(t, engine)
		})
	}
}

func TestNewEngineNilPlannerInitFailure(t *testing.T) {
	engine, err := NewEngine(Budget{}, testOptions(), WithPlanner(nil))
	requirePhaseError(t, err, PhaseInit, ReasonInitFailed, ErrPlanningFailed)
	assert.Nil(t, engine)
}

func TestNewEngineSuccess(t *testing.T) {
	engine, err := NewEngine(Budget{}, testOptions())
	require.NoError(t, err)
	require.NotNil(t, engine)
	require.NoError(t, engine.Close())
}

// ---------------------------------------------------------------------------
// Count budget: boundary exactly exhausted
// ---------------------------------------------------------------------------

func TestCountBudgetExactBoundary(t *testing.T) {
	engine, err := NewEngine(Budget{Count: 2}, testOptions())
	require.NoError(t, err)
	defer func() { require.NoError(t, engine.Close()) }()

	// First admission commits; the second admission is skipped (does not match)
	// but still consumes the count budget.
	depA := namedSecretDeployment("workload-a", "db-secret", nil)
	depB := namedSecretDeployment("workload-b", "other-secret", nil)
	depC := namedSecretDeployment("workload-c", "db-secret", nil)

	decisionA, err := engine.Process(context.Background(), secretEventFor(depA, "sha-1"))
	require.NoError(t, err)
	require.NotNil(t, decisionA)

	decisionB, err := engine.Process(context.Background(), secretEventFor(depB, "sha-1"))
	require.NoError(t, err)
	assert.Nil(t, decisionB)

	// Exactly exhausted: the third admission must be rejected.
	_, err = engine.Process(context.Background(), secretEventFor(depC, "sha-1"))
	phaseErr := requirePhaseError(t, err, PhaseEvaluate, ReasonCountBudgetExceeded, ErrCountBudgetExceeded)
	assert.Equal(t, int64(2), phaseErr.Used)
	assert.Equal(t, int64(2), phaseErr.Limit)

	stats := engine.Stats()
	assert.Equal(t, int64(2), stats.Evaluated)
	assert.Equal(t, int64(1), stats.Committed)
	assert.Equal(t, int64(1), stats.Skipped)
	assert.Equal(t, int64(1), stats.Aborted)
	assert.Len(t, engine.Decisions(), 1)
	requireNoScratchLeak(t, engine)
}

func TestCommittedReadsDoNotConsumeCountBudget(t *testing.T) {
	engine, err := NewEngine(Budget{Count: 1}, testOptions())
	require.NoError(t, err)
	defer func() { require.NoError(t, engine.Close()) }()

	dep := namedSecretDeployment("workload-a", "db-secret", nil)
	ev := secretEventFor(dep, "sha-1")

	first, err := engine.Process(context.Background(), ev)
	require.NoError(t, err)
	require.NotNil(t, first)

	// Repeated processing of an already committed workload is served from the
	// commit log without consuming budget or planning again.
	second, err := engine.Process(context.Background(), ev)
	require.NoError(t, err)
	require.NotNil(t, second)
	assert.Equal(t, first.SHA, second.SHA)
	assert.Equal(t, first.Resource, second.Resource)

	other := namedSecretDeployment("workload-b", "db-secret", nil)
	_, err = engine.Process(context.Background(), secretEventFor(other, "sha-1"))
	requirePhaseError(t, err, PhaseEvaluate, ReasonCountBudgetExceeded, ErrCountBudgetExceeded)
}

// ---------------------------------------------------------------------------
// Memory budget: boundary exactly exhausted
// ---------------------------------------------------------------------------

func TestMemoryBudgetExactBoundary(t *testing.T) {
	depA := namedSecretDeployment("workload-a", "db-secret", nil)
	depB := namedSecretDeployment("workload-b", "db-secret", nil)
	depC := namedSecretDeployment("workload-c", "db-secret", nil)
	sizeA := objectEncodedSize(t, depA)
	sizeB := objectEncodedSize(t, depB)

	// An object that fits exactly is admitted.
	engineExact, err := NewEngine(Budget{Memory: sizeA}, testOptions())
	require.NoError(t, err)
	decision, err := engineExact.Process(context.Background(), secretEventFor(depA, "sha-1"))
	require.NoError(t, err)
	require.NotNil(t, decision)
	_, err = engineExact.Process(context.Background(), secretEventFor(depB, "sha-1"))
	requirePhaseError(t, err, PhaseEvaluate, ReasonMemoryBudgetExceeded, ErrMemoryBudgetExceeded)
	require.NoError(t, engineExact.Close())

	// Exactly sizeA+sizeB admits both A and B; C crosses the boundary by sizeC.
	engine, err := NewEngine(Budget{Memory: sizeA + sizeB}, testOptions())
	require.NoError(t, err)
	defer func() { require.NoError(t, engine.Close()) }()

	_, err = engine.Process(context.Background(), secretEventFor(depA, "sha-1"))
	require.NoError(t, err)
	_, err = engine.Process(context.Background(), secretEventFor(depB, "sha-1"))
	require.NoError(t, err)

	_, err = engine.Process(context.Background(), secretEventFor(depC, "sha-1"))
	phaseErr := requirePhaseError(t, err, PhaseEvaluate, ReasonMemoryBudgetExceeded, ErrMemoryBudgetExceeded)
	assert.Equal(t, sizeA+sizeB, phaseErr.Used)
	assert.Equal(t, sizeA+sizeB, phaseErr.Limit)
	requireNoScratchLeak(t, engine)
}

// ---------------------------------------------------------------------------
// Time budget: exact boundary, commit-phase expiry and caller deadline
// ---------------------------------------------------------------------------

func TestTimeBudgetExactBoundaryInclusive(t *testing.T) {
	clock := newFakeClock()
	engine, err := NewEngine(Budget{Time: 10 * time.Millisecond}, testOptions(), WithClock(clock.now))
	require.NoError(t, err)
	defer func() { require.NoError(t, engine.Close()) }()

	first, err := engine.Process(context.Background(), secretEventFor(namedSecretDeployment("w1", "db-secret", nil), "sha-1"))
	require.NoError(t, err)
	require.NotNil(t, first)

	// Exactly at the deadline: boundary is inclusive.
	clock.advance(10 * time.Millisecond)
	second, err := engine.Process(context.Background(), secretEventFor(namedSecretDeployment("w2", "db-secret", nil), "sha-1"))
	require.NoError(t, err)
	require.NotNil(t, second)

	// One nanosecond past the deadline: entering evaluate is refused.
	clock.advance(time.Nanosecond)
	_, err = engine.Process(context.Background(), secretEventFor(namedSecretDeployment("w3", "db-secret", nil), "sha-1"))
	phaseErr := requirePhaseError(t, err, PhaseEvaluate, ReasonTimeBudgetExceeded, ErrTimeBudgetExceeded)
	assert.Equal(t, int64((10*time.Millisecond + time.Nanosecond)), phaseErr.Used)
	assert.Equal(t, int64(10*time.Millisecond), phaseErr.Limit)
}

func TestTimeBudgetExpiringInCommitPhase(t *testing.T) {
	clock := newFakeClock()
	planner := Planner(func(ctx context.Context, ev Event, opts *common.ReloaderOptions) (Plan, bool, error) {
		// Planning consumes the remaining time budget.
		clock.advance(11 * time.Millisecond)
		return DefaultPlanner(ctx, ev, opts)
	})
	engine, err := NewEngine(Budget{Time: 10 * time.Millisecond}, testOptions(), WithClock(clock.now), WithPlanner(planner))
	require.NoError(t, err)
	defer func() { require.NoError(t, engine.Close()) }()

	_, err = engine.Process(context.Background(), secretEventFor(namedSecretDeployment("slow", "db-secret", nil), "sha-1"))
	requirePhaseError(t, err, PhaseCommit, ReasonTimeBudgetExceeded, ErrTimeBudgetExceeded)
	assert.Empty(t, engine.Decisions(), "expired commit must not be recorded")

	// The engine keeps refusing new work at the evaluate gate afterwards.
	_, err = engine.Process(context.Background(), secretEventFor(namedSecretDeployment("next", "db-secret", nil), "sha-1"))
	requirePhaseError(t, err, PhaseEvaluate, ReasonTimeBudgetExceeded, ErrTimeBudgetExceeded)
}

func TestExpiredCallerContextReportedAsCanceled(t *testing.T) {
	engine, err := NewEngine(Budget{Time: time.Hour}, testOptions())
	require.NoError(t, err)
	defer func() { require.NoError(t, engine.Close()) }()

	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Minute))
	defer cancel()

	_, err = engine.Process(ctx, secretEventFor(namedSecretDeployment("w1", "db-secret", nil), "sha-1"))
	phaseErr := requirePhaseError(t, err, PhaseEvaluate, ReasonCanceled, context.DeadlineExceeded)
	assert.NotEqual(t, ReasonTimeBudgetExceeded, phaseErr.Reason)
}

// ---------------------------------------------------------------------------
// Commit phase happens at most once per workload
// ---------------------------------------------------------------------------

func TestCommitAtMostOnceSequential(t *testing.T) {
	var calls atomic.Int64
	planner := Planner(func(ctx context.Context, ev Event, opts *common.ReloaderOptions) (Plan, bool, error) {
		calls.Add(1)
		return DefaultPlanner(ctx, ev, opts)
	})
	engine, err := NewEngine(Budget{}, testOptions(), WithPlanner(planner))
	require.NoError(t, err)
	defer func() { require.NoError(t, engine.Close()) }()

	ev := secretEventFor(namedSecretDeployment("once", "db-secret", nil), "sha-1")
	first, err := engine.Process(context.Background(), ev)
	require.NoError(t, err)
	require.NotNil(t, first)

	// A newer SHA arriving for the same workload still returns the committed decision.
	ev2 := secretEventFor(namedSecretDeployment("once", "db-secret", nil), "sha-2")
	second, err := engine.Process(context.Background(), ev2)
	require.NoError(t, err)
	require.NotNil(t, second)
	assert.Equal(t, "sha-1", second.SHA)

	assert.Equal(t, int64(1), calls.Load())
	assert.Len(t, engine.Decisions(), 1)
}

func TestCommitAtMostOnceConcurrent(t *testing.T) {
	blocker := newBlockingPlanner(DefaultPlanner)
	engine, err := NewEngine(Budget{}, testOptions(), WithPlanner(blocker.plan))
	require.NoError(t, err)
	defer func() { require.NoError(t, engine.Close()) }()

	ev := secretEventFor(namedSecretDeployment("once", "db-secret", nil), "sha-1")

	const waiters = 30
	var wg sync.WaitGroup
	results := make([]*Decision, waiters)
	errs := make([]error, waiters)
	start := make(chan struct{})
	for i := 0; i < waiters; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i], errs[i] = engine.Process(context.Background(), ev)
		}(i)
	}
	close(start)

	// Wait until the leader entered planning, then release it.
	<-blocker.entered
	close(blocker.release)
	wg.Wait()

	for i := 0; i < waiters; i++ {
		require.NoError(t, errs[i], "waiter %d", i)
		require.NotNil(t, results[i], "waiter %d", i)
		assert.Equal(t, "Deployment/test-ns/once", results[i].Resource.String())
	}
	assert.Equal(t, int64(1), blocker.calls.Load(), "planning must run at most once per workload")
	assert.Len(t, engine.Decisions(), 1)
}

// ---------------------------------------------------------------------------
// Concurrent cancellation
// ---------------------------------------------------------------------------

func TestCancelDuringPlanning(t *testing.T) {
	blocker := newBlockingPlanner(DefaultPlanner)
	engine, err := NewEngine(Budget{}, testOptions(), WithPlanner(blocker.plan))
	require.NoError(t, err)
	defer func() { require.NoError(t, engine.Close()) }()

	ctx, cancel := context.WithCancel(context.Background())
	resultErr := make(chan error, 1)
	go func() {
		_, err := engine.Process(ctx, secretEventFor(namedSecretDeployment("canceled", "db-secret", nil), "sha-1"))
		resultErr <- err
	}()

	<-blocker.entered
	cancel()

	requirePhaseError(t, <-resultErr, PhaseEvaluate, ReasonCanceled, context.Canceled)
	assert.Empty(t, engine.Decisions())
	requireNoScratchLeak(t, engine)

	// Release blocked planners so later events proceed; the canceled key was
	// not poisoned and can be re-attempted by a fresh leader.
	close(blocker.release)

	// The engine is still usable with a fresh context and a new workload after cancellation.
	decision, err := engine.Process(context.Background(), secretEventFor(namedSecretDeployment("next", "db-secret", nil), "sha-1"))
	require.NoError(t, err)
	require.NotNil(t, decision)
}

func TestCancelWhileWaitingForInflightCommit(t *testing.T) {
	blocker := newBlockingPlanner(DefaultPlanner)
	waiterReady := make(chan struct{})
	engine, err := NewEngine(
		Budget{},
		testOptions(),
		WithPlanner(blocker.plan),
		withAwaitHook(func() { close(waiterReady) }),
	)
	require.NoError(t, err)
	defer func() { require.NoError(t, engine.Close()) }()

	ev := secretEventFor(namedSecretDeployment("coalesced", "db-secret", nil), "sha-1")

	leaderErr := make(chan error, 1)
	go func() {
		_, err := engine.Process(context.Background(), ev)
		leaderErr <- err
	}()
	<-blocker.entered

	waiterCtx, cancelWaiter := context.WithCancel(context.Background())
	waiterErr := make(chan error, 1)
	go func() {
		_, err := engine.Process(waiterCtx, ev)
		waiterErr <- err
	}()
	<-waiterReady
	cancelWaiter()
	requirePhaseError(t, <-waiterErr, PhaseCommit, ReasonCanceled, context.Canceled)

	// Releasing the leader still commits exactly once; the canceled waiter did not poison the key.
	close(blocker.release)
	require.NoError(t, <-leaderErr)
	assert.Len(t, engine.Decisions(), 1)
	assert.Equal(t, int64(1), blocker.calls.Load())
}

func TestConcurrentCancellationBurst(t *testing.T) {
	planner := Planner(func(ctx context.Context, ev Event, opts *common.ReloaderOptions) (Plan, bool, error) {
		// Observe cancellation before doing any work; the default planner does
		// real per-workload work in between, allowing the cancel to interleave.
		if err := ctx.Err(); err != nil {
			return Plan{}, false, err
		}
		return DefaultPlanner(ctx, ev, opts)
	})
	engine, err := NewEngine(Budget{}, testOptions(), WithPlanner(planner))
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	const workers = 8
	const perWorker = 100

	var wg sync.WaitGroup
	var mu sync.Mutex
	var okKeys []string
	var phaseErrs []*PhaseError
	start := make(chan struct{})
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			<-start
			for i := 0; i < perWorker; i++ {
				name := fmt.Sprintf("workload-%d-%d", w, i)
				dep := namedSecretDeployment(name, "db-secret", nil)
				decision, err := engine.Process(ctx, secretEventFor(dep, "sha-1"))
				mu.Lock()
				if err != nil {
					var pe *PhaseError
					if errors.As(err, &pe) {
						phaseErrs = append(phaseErrs, pe)
					} else {
						t.Errorf("unexpected non-phased error: %v", err)
					}
				} else if decision != nil {
					okKeys = append(okKeys, decision.Resource.key())
				}
				mu.Unlock()
			}
		}(w)
	}
	close(start)
	time.Sleep(2 * time.Millisecond)
	cancel()
	wg.Wait()

	require.NoError(t, engine.Close())

	mu.Lock()
	defer mu.Unlock()
	for _, pe := range phaseErrs {
		assert.Contains(t, []Reason{ReasonCanceled, ReasonClosed, ReasonTimeBudgetExceeded}, pe.Reason)
	}
	committed := engine.Decisions()
	assert.Len(t, committed, int(engine.Stats().Committed))
	committedKeys := make(map[string]struct{}, len(committed))
	for _, d := range committed {
		committedKeys[d.Resource.key()] = struct{}{}
	}
	for _, key := range okKeys {
		_, ok := committedKeys[key]
		assert.True(t, ok, "returned decision %s missing from commit log", key)
	}
	assert.Zero(t, engine.Stats().ScratchOutstanding)
}

// ---------------------------------------------------------------------------
// Shutdown / close phase
// ---------------------------------------------------------------------------

func TestCloseIsIdempotentAndRetainsDecisions(t *testing.T) {
	engine, err := NewEngine(Budget{}, testOptions())
	require.NoError(t, err)

	d1, err := engine.Process(context.Background(), secretEventFor(namedSecretDeployment("w1", "db-secret", nil), "sha-1"))
	require.NoError(t, err)
	require.NotNil(t, d1)

	require.NoError(t, engine.Close())
	require.NoError(t, engine.Close(), "close must be idempotent")

	assert.Len(t, engine.Decisions(), 1, "committed decisions remain available during shutdown")

	_, err = engine.Process(context.Background(), secretEventFor(namedSecretDeployment("w2", "db-secret", nil), "sha-1"))
	requirePhaseError(t, err, PhaseEvaluate, ReasonClosed, ErrEngineClosed)

	engine.mu.Lock()
	assert.Nil(t, engine.pool, "scratch pool must be released on close")
	engine.mu.Unlock()
	assert.Zero(t, engine.Stats().ScratchOutstanding)
}

func TestCloseCancelsInFlightPlanning(t *testing.T) {
	blocker := newBlockingPlanner(DefaultPlanner)
	engine, err := NewEngine(Budget{}, testOptions(), WithPlanner(blocker.plan))
	require.NoError(t, err)

	resultErr := make(chan error, 1)
	go func() {
		_, err := engine.Process(context.Background(), secretEventFor(namedSecretDeployment("inflight", "db-secret", nil), "sha-1"))
		resultErr <- err
	}()
	<-blocker.entered

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = engine.Close()
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return while planning was blocked")
	}

	requirePhaseError(t, <-resultErr, PhaseEvaluate, ReasonClosed, ErrEngineClosed)

	_, err = engine.Process(context.Background(), secretEventFor(namedSecretDeployment("after", "db-secret", nil), "sha-1"))
	requirePhaseError(t, err, PhaseEvaluate, ReasonClosed, ErrEngineClosed)
	assert.Zero(t, engine.Stats().ScratchOutstanding)
}

// ---------------------------------------------------------------------------
// Invalid events
// ---------------------------------------------------------------------------

func TestInvalidEvents(t *testing.T) {
	engine, err := NewEngine(Budget{}, testOptions())
	require.NoError(t, err)
	defer func() { require.NoError(t, engine.Close()) }()

	_, err = engine.Process(context.Background(), Event{})
	requirePhaseError(t, err, PhaseEvaluate, ReasonInvalidEvent, ErrInvalidEvent)

	_, err = engine.Process(context.Background(), Event{
		Config:   secretConfig("sha-1"),
		Kind:     "Deployment",
		Workload: nil,
	})
	requirePhaseError(t, err, PhaseEvaluate, ReasonInvalidEvent, ErrInvalidEvent)

	_, err = engine.Process(context.Background(), Event{
		Config:   secretConfig("sha-1"),
		Kind:     "Pod",
		Workload: &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "test-ns"}},
	})
	requirePhaseError(t, err, PhaseEvaluate, ReasonUnsupportedWorkload, ErrUnsupportedWorkload)
}

// ---------------------------------------------------------------------------
// Secret values must never leak
// ---------------------------------------------------------------------------

func TestSecretValuesNeverLeak(t *testing.T) {
	const secretValue = "SUPER-SECRET-VALUE-9f3e2d1c-do-not-print"

	secret := &v1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "db-secret", Namespace: "test-ns"},
		Data:       map[string][]byte{"password": []byte(secretValue)},
	}
	config := common.GetSecretConfig(secret)
	require.NotContains(t, config.SHAValue, secretValue)
	require.NotEmpty(t, config.SHAValue)

	var allMessages []string

	// 1. Count budget exhaustion while the trigger is a Secret change.
	engine, err := NewEngine(Budget{Count: 1}, testOptions())
	require.NoError(t, err)
	dep1 := namedSecretDeployment("w1", "db-secret", nil)
	dep2 := namedSecretDeployment("w2", "db-secret", nil)
	decision, err := engine.Process(context.Background(), eventFor(t, dep1, config, "Deployment"))
	require.NoError(t, err)
	require.NotNil(t, decision)
	_, err = engine.Process(context.Background(), eventFor(t, dep2, config, "Deployment"))
	allMessages = append(allMessages, err.Error())
	committed := engine.Decisions()
	require.NoError(t, engine.Close())

	// 2. Cancellation while planning a Secret-triggered decision.
	blocker := newBlockingPlanner(DefaultPlanner)
	engine2, err := NewEngine(Budget{}, testOptions(), WithPlanner(blocker.plan))
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, pErr := engine2.Process(ctx, eventFor(t, namedSecretDeployment("w3", "db-secret", nil), config, "Deployment"))
		errCh <- pErr
	}()
	<-blocker.entered
	cancel()
	allMessages = append(allMessages, (<-errCh).Error())
	require.NoError(t, engine2.Close())

	// 3. Shutdown rejection.
	engine3, err := NewEngine(Budget{}, testOptions())
	require.NoError(t, err)
	require.NoError(t, engine3.Close())
	_, err = engine3.Process(context.Background(), eventFor(t, namedSecretDeployment("w4", "db-secret", nil), config, "Deployment"))
	allMessages = append(allMessages, err.Error())

	// 4. Invalid event and unsupported workload errors.
	engine4, err := NewEngine(Budget{}, testOptions())
	require.NoError(t, err)
	_, err = engine4.Process(context.Background(), Event{})
	allMessages = append(allMessages, err.Error())
	_, err = engine4.Process(context.Background(), eventFor(t,
		&v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "standalone", Namespace: "test-ns"}},
		config, "Pod"))
	allMessages = append(allMessages, err.Error())
	require.NoError(t, engine4.Close())

	// 5. A planner error whose text contains the secret value must not be rendered.
	leakyPlanner := Planner(func(context.Context, Event, *common.ReloaderOptions) (Plan, bool, error) {
		return Plan{}, false, fmt.Errorf("internal failure processing %s", secretValue)
	})
	engine5, err := NewEngine(Budget{}, testOptions(), WithPlanner(leakyPlanner))
	require.NoError(t, err)
	_, err = engine5.Process(context.Background(), eventFor(t, namedSecretDeployment("w5", "db-secret", nil), config, "Deployment"))
	allMessages = append(allMessages, err.Error())
	var phaseErr *PhaseError
	require.True(t, errors.As(err, &phaseErr))
	assert.Equal(t, ReasonPlanningFailed, phaseErr.Reason)
	require.NoError(t, engine5.Close())

	for _, msg := range allMessages {
		assert.NotContains(t, msg, secretValue, "phased error leaked Secret material: %s", msg)
	}

	// Committed patches only embed the SHA hash.
	encodedDecisions, err := json.Marshal(committed)
	require.NoError(t, err)
	assert.NotContains(t, string(encodedDecisions), secretValue)
	assert.Contains(t, string(encodedDecisions), config.SHAValue)
}

// ---------------------------------------------------------------------------
// Architectural guard: the decision package processes passed objects only and
// must never reach out to the cluster.
// ---------------------------------------------------------------------------

func TestPackageHasNoClusterAccess(t *testing.T) {
	entries, err := os.ReadDir(".")
	require.NoError(t, err)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		content, readErr := os.ReadFile(entry.Name())
		require.NoError(t, readErr)
		source := string(content)
		assert.NotContains(t, source, "stakater/Reloader/pkg/kube", "decision package must not depend on kube clients")
		assert.NotContains(t, source, "k8s.io/client-go/kubernetes", "decision package must not import kubernetes clients")
		assert.NotContains(t, source, "net/http", "decision package must not perform network calls")
	}
}

func TestUnlimitedBudgetsAllowAllWorkloads(t *testing.T) {
	engine, err := NewEngine(Budget{}, testOptions())
	require.NoError(t, err)
	defer func() { require.NoError(t, engine.Close()) }()

	for i := 0; i < 25; i++ {
		dep := namedSecretDeployment(fmt.Sprintf("workload-%d", i), "db-secret", nil)
		decision, err := engine.Process(context.Background(), secretEventFor(dep, "sha-1"))
		require.NoError(t, err)
		require.NotNil(t, decision)
	}
	assert.Equal(t, int64(25), engine.Stats().Committed)
}
