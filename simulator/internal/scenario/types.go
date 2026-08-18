// Package scenario implements Task 7's scenario engine: parsing a YAML
// scenario file (AGENT-TASK.md, Task 7 item 4) into a validated,
// in-memory Scenario, and deterministically executing it (item 5) against
// a shared Store/commands.Processor/faults.Injector/physics.Runner and
// the two protocol servers' link-fault controls, driven entirely by an
// injected *clock.Fake — no time.Now(), no real sleep in the execution
// path itself (AGENT-TASK §1.5).
package scenario

import "time"

// Scenario is one fully parsed and validated scenario file.
type Scenario struct {
	Name  string
	Clock ClockSpec
	Steps []Step
}

// ClockSpec is a scenario's clock: block — Start is the absolute instant
// step At offsets are measured from; Speed is documentation only in this
// implementation (see runner.go's doc comment on why: every Tick this
// package drives runs back-to-back with no artificial real-time delay
// regardless of Speed, which is what makes "accelerated vs normal gives
// the same final state" true by construction rather than by careful
// pacing).
type ClockSpec struct {
	Start time.Time
	Speed float64
}

// Step is exactly one of Write/Fault/Expect/Link — Parse guarantees
// exactly one of the four is non-nil (AGENT-TASK.md, Task 7 item 4:
// "four step types, exactly one per step").
type Step struct {
	At     time.Duration
	Write  *WriteAction
	Fault  *FaultAction
	Expect *ExpectAction
	Link   *LinkAction
}

// StepEvent is emitted after a scenario step has completed successfully.
// Index is zero-based, matching Cursor before the runner advances it.
// Observers are informational only: they must not mutate runner state or
// block scenario execution.
type StepEvent struct {
	Scenario string
	Index    int
	At       time.Duration
	Action   string
	Result   string
	Error    string
}

// WriteAction and FaultAction share a shape (a target point plus one
// engineering-unit value) but are kept as distinct types rather than one
// shared struct: they go through entirely different validation/execution
// paths (commands.Processor vs faults.Injector — AGENT-TASK.md, Task 7
// item 3) and conflating them would blur that boundary in the API too.
type WriteAction struct {
	Device string
	Point  string
	Value  float64
}

type FaultAction struct {
	Device string
	Point  string
	Value  float64
}

// ExpectAction is a check, not a mutation: exactly one of Value or
// (Min and/or Max) is set — Parse enforces this, matching AGENT-TASK.md,
// Task 7 item 4's "exact match or a range", not both at once.
type ExpectAction struct {
	Device string
	Point  string
	Value  *float64
	Min    *float64
	Max    *float64
}

// LinkAction mirrors linkfault.Apply's own parameters — Protocol/Mode are
// kept as plain strings here (validated against linkfault's enums by
// Parse) rather than importing linkfault's types directly, so this
// package's public Step shape doesn't leak an unrelated package's types
// for what is, from a scenario author's point of view, just two strings.
type LinkAction struct {
	Protocol string // "iec104" | "modbus" | "both"
	Mode     string // "drop" | "hang" | "delay" | "heartbeat_pause" | "clear"
	DelayMS  int    // only meaningful for Mode == "delay"
}
