package decision

import (
	"time"
)

// Budget limits how much work a single decision Engine is allowed to perform.
//
// The caller supplies any combination of the three limits. A zero value means
// the corresponding resource is unlimited; negative values are rejected during
// the init phase.
type Budget struct {
	// Time bounds the wall-clock lifetime of the engine, starting at NewEngine.
	// A phase that is entered exactly at the deadline is still allowed; only an
	// elapsed deadline is treated as exhausted.
	Time time.Duration
	// Count bounds the number of workload objects admitted to the evaluate
	// phase over the lifetime of the engine. Reads of already committed
	// decisions and coalesced duplicate events do not consume the budget.
	Count int
	// Memory bounds the cumulative estimated size, in bytes, of the workload
	// objects admitted to the evaluate phase. The estimate covers only the
	// caller-provided object encoding, never Secret data held outside the
	// engine.
	Memory int64
}

// Validate reports an init-phase error when the budget cannot be used.
func (b Budget) Validate() error {
	if b.Time < 0 {
		return ErrInvalidBudget
	}
	if b.Count < 0 {
		return ErrInvalidBudget
	}
	if b.Memory < 0 {
		return ErrInvalidBudget
	}
	return nil
}

// budgetGate is the mutable accounting state of a Budget. All methods must be
// called while holding the engine mutex.
type budgetGate struct {
	budget     Budget
	started    time.Time
	deadline   time.Time
	usedCount  int
	usedMemory int64
}

func newBudgetGate(budget Budget, now time.Time) budgetGate {
	g := budgetGate{budget: budget, started: now}
	if budget.Time > 0 {
		g.deadline = now.Add(budget.Time)
	}
	return g
}

// timeExceeded reports whether the wall-clock budget is exhausted. The deadline
// boundary itself is inclusive: exactly exhausted time still admits the phase.
func (g *budgetGate) timeExceeded(now time.Time) bool {
	return !g.deadline.IsZero() && now.After(g.deadline)
}

func (g *budgetGate) elapsed(now time.Time) time.Duration {
	return now.Sub(g.started)
}

// countExceeded reports whether admitting add more workloads crosses the count
// limit. The boundary is inclusive: using exactly the full limit succeeds.
func (g *budgetGate) countExceeded(add int) bool {
	return g.budget.Count > 0 && g.usedCount+add > g.budget.Count
}

// memoryExceeded reports whether admitting add more estimated bytes crosses the
// memory limit. The boundary is inclusive: using exactly the full limit
// succeeds.
func (g *budgetGate) memoryExceeded(add int64) bool {
	return g.budget.Memory > 0 && g.usedMemory+add > g.budget.Memory
}

func (g *budgetGate) charge(count int, memory int64) {
	g.usedCount += count
	g.usedMemory += memory
}

// BudgetUsage is a point-in-time snapshot of budget consumption.
type BudgetUsage struct {
	Elapsed    time.Duration
	UsedCount  int
	UsedMemory int64
	Time       time.Duration
	Count      int
	Memory     int64
}

func (g *budgetGate) usage(now time.Time) BudgetUsage {
	return BudgetUsage{
		Elapsed:    g.elapsed(now),
		UsedCount:  g.usedCount,
		UsedMemory: g.usedMemory,
		Time:       g.budget.Time,
		Count:      g.budget.Count,
		Memory:     g.budget.Memory,
	}
}
