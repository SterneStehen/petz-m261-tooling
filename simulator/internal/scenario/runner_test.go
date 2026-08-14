package scenario_test

import (
	"strconv"
	"testing"
	"time"

	"github.com/SterneStehen/petz-m261-tooling/gen/go/m261points"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/clock"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/commands"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/faults"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/physics"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/scenario"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/store"
)

// fakeLinkTarget is a minimal linkfault.Target — this package's own tests
// only need to prove a link: step reaches the right target, not exercise
// real protocol servers (modbustcp/iec104 have their own link-fault
// tests against the real thing).
type fakeLinkTarget struct {
	drop, hang, cleared bool
	delay               time.Duration
}

func (f *fakeLinkTarget) SetDrop()                  { f.drop = true }
func (f *fakeLinkTarget) SetHang()                  { f.hang = true }
func (f *fakeLinkTarget) SetDelay(d time.Duration)  { f.delay = d }
func (f *fakeLinkTarget) SetHeartbeatPause(float64) {}
func (f *fakeLinkTarget) ClearLinkFaults()          { *f = fakeLinkTarget{cleared: true} }

type harness struct {
	store     *store.Store
	injector  *faults.Injector
	processor *commands.Processor
	physics   *physics.Runner
	clk       *clock.Fake
	iec       *fakeLinkTarget
	mb        *fakeLinkTarget
	runner    *scenario.Runner
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	return newHarnessWithSOC(t, 50)
}

func newHarnessWithSOC(t *testing.T, initialSOC float64) *harness {
	t.Helper()
	st := store.New()
	clk := clock.NewFake(time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC))
	proc, err := commands.NewProcessor(st, clk, commands.DefaultConfig())
	if err != nil {
		t.Fatalf("commands.NewProcessor: %v", err)
	}
	pr := physics.NewRunner(physics.New(physics.DefaultParams(), initialSOC), st, clk, proc)
	inj := faults.NewInjector(st)
	iec, mb := &fakeLinkTarget{}, &fakeLinkTarget{}
	// A coarse stepInterval (rather than the production 1s default) keeps
	// these tests, several of which span up to an hour of scenario at:
	// offsets, fast under -race — correctness here is about execution
	// order and timing relative to at:, never about exact tick counts
	// (that's physics.TestFastForward*'s job, in package physics).
	r := scenario.NewRunner(st, inj, proc, pr, clk, 5*time.Minute, iec, mb)
	return &harness{store: st, injector: inj, processor: proc, physics: pr, clk: clk, iec: iec, mb: mb, runner: r}
}

func mustParse(t *testing.T, yaml string) *scenario.Scenario {
	t.Helper()
	s, err := scenario.Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return s
}

func mustLoad(t *testing.T, r *scenario.Runner, yaml string) {
	t.Helper()
	if err := r.Load(mustParse(t, yaml)); err != nil {
		t.Fatalf("Load: %v", err)
	}
}

// waitUntilStopped polls Running() — the execution goroutine is
// asynchronous by design (Start returns immediately), so tests that want
// to assert on the end state need to wait for it to actually finish
// rather than race it.
func waitUntilStopped(t *testing.T, r *scenario.Runner) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for r.Running() {
		if time.Now().After(deadline) {
			t.Fatal("scenario did not stop within 5s (real time) — likely stuck")
		}
		time.Sleep(time.Millisecond)
	}
}

// TestRunnerExecutesFullExampleScenario runs the corrected AGENT-TASK.md
// example almost verbatim — one deliberate change from the doc's literal
// text: the final expect no longer asserts
// discharge_prohibition_protection=1. That would only become true if
// injecting cell_temperature_too_high fed back into physics.Engine's own
// SoC-bound prohibition logic, and it doesn't — there's no
// manufacturer-documented linkage from a specific alarm to forced
// discharge prohibition for this simulator to model (AGENT-TASK §1 rule
// 1: don't invent one). The doc's example is illustrative of the YAML
// shape, not a literal guarantee for every possible physics starting
// condition; this test instead confirms the fault injected one step
// earlier is still reflected, which is guaranteed.
func TestRunnerExecutesFullExampleScenario(t *testing.T) {
	h := newHarness(t)
	mustLoad(t, h.runner, `
name: example
clock: {start: "2026-08-12T00:00:00+03:00", speed: 60}
steps:
  - at: 0s
    write: {device: EMS, point: set_operating_mode, value: 2}
  - at: 5s
    write: {device: EMS, point: set_active_power_kw, value: -100}
  - at: 30m
    expect: {device: BMS, point: soc, min: 30}
  - at: 35m
    link: {protocol: iec104, mode: drop}
  - at: 40m
    fault: {device: BMS, point: cell_temperature_too_high, value: 1}
  - at: 45m
    expect: {device: BMS, point: cell_temperature_too_high, value: 1}
`)
	if err := h.runner.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitUntilStopped(t, h.runner)

	if err := h.runner.LastError(); err != nil {
		t.Fatalf("scenario failed: %v", err)
	}
	if got := h.runner.Cursor(); got != 6 {
		t.Errorf("Cursor after completion = %d, want 6 (all steps executed)", got)
	}
	if v, _ := h.store.Get(m261points.PointKey{Device: "EMS", Slug: "set_operating_mode"}); v != 2 {
		t.Errorf("set_operating_mode = %v, want 2", v)
	}
	if v, _ := h.store.Get(m261points.PointKey{Device: "BMS", Slug: "cell_temperature_too_high"}); v != 1 {
		t.Errorf("cell_temperature_too_high = %v, want 1 (fault injected)", v)
	}
	if !h.iec.drop {
		t.Error("iec target's drop was not set — link: step didn't reach it")
	}
}

// TestRunnerSameTimestampDifferentPointsExecuteInDeclarationOrder proves
// Task 7 item 5's ordering rule directly, not just via a final-state
// snapshot: two writes at the identical at: must apply in the order they
// were declared.
func TestRunnerSameTimestampDifferentPointsExecuteInDeclarationOrder(t *testing.T) {
	h := newHarness(t)
	mustLoad(t, h.runner, `
name: order
clock: {start: "2026-08-12T00:00:00+03:00", speed: 1}
steps:
  - at: 5s
    write: {device: EMS, point: set_operating_mode, value: 2}
  - at: 5s
    write: {device: EMS, point: set_active_power_kw, value: -33}
  - at: 5s
    expect: {device: EMS, point: set_active_power_kw, value: -33}
`)
	if err := h.runner.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitUntilStopped(t, h.runner)
	if err := h.runner.LastError(); err != nil {
		t.Fatalf("scenario failed (order not respected — the expect at step 2 ran before the write at step 1 landed): %v", err)
	}
}

// TestRunnerExpectFailureStopsScenario proves an execution-time check
// failure halts the scenario (not applied partially) rather than
// continuing past it.
func TestRunnerExpectFailureStopsScenario(t *testing.T) {
	h := newHarness(t)
	mustLoad(t, h.runner, `
name: bad-expect
clock: {start: "2026-08-12T00:00:00+03:00", speed: 1}
steps:
  - at: 0s
    expect: {device: BMS, point: soc, value: 999}
  - at: 1s
    write: {device: EMS, point: set_operating_mode, value: 2}
`)
	if err := h.runner.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitUntilStopped(t, h.runner)

	if h.runner.LastError() == nil {
		t.Fatal("LastError() = nil, want the failed expect's error")
	}
	if got := h.runner.Cursor(); got != 0 {
		t.Errorf("Cursor after a failed expect = %d, want 0 (stopped before advancing past it)", got)
	}
	if v, _ := h.store.Get(m261points.PointKey{Device: "EMS", Slug: "set_operating_mode"}); v != 0 {
		t.Errorf("set_operating_mode = %v, want 0 (step 1 must never have run)", v)
	}
}

// TestRunnerRejectedWriteStopsScenario mirrors the expect-failure case
// for a write commands.Processor itself rejects (Task 6's own
// validation, exercised through the scenario path per Task 7 item 3's
// "no exception for the scenario path" rule).
func TestRunnerRejectedWriteStopsScenario(t *testing.T) {
	h := newHarness(t)
	mustLoad(t, h.runner, `
name: bad-write
clock: {start: "2026-08-12T00:00:00+03:00", speed: 1}
steps:
  - at: 0s
    write: {device: EMS, point: set_operating_mode, value: 99}
`)
	if err := h.runner.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitUntilStopped(t, h.runner)
	if h.runner.LastError() == nil {
		t.Fatal("LastError() = nil, want the rejected write's error (99 is not a valid Set Operating Mode enum value)")
	}
}

// TestRunnerRejectedFaultStopsScenario mirrors the same for a fault: step
// targeting a non-alarm point.
func TestRunnerRejectedFaultStopsScenario(t *testing.T) {
	h := newHarness(t)
	// Parse doesn't reject this — class:alarm-ness isn't checked until
	// execution (faults.Injector), same split as commands.Processor for
	// write:.
	mustLoad(t, h.runner, `
name: bad-fault
clock: {start: "2026-08-12T00:00:00+03:00", speed: 1}
steps:
  - at: 0s
    fault: {device: EMS, point: set_active_power_kw, value: 1}
`)
	if err := h.runner.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitUntilStopped(t, h.runner)
	if h.runner.LastError() == nil {
		t.Fatal("LastError() = nil, want the rejected fault's error (set_active_power_kw is not class:alarm)")
	}
}

// TestRunnerStopIsSynchronous is Task 7 item 7's atomicity requirement
// at the scenario-engine layer: Stop must not return until the run
// goroutine has actually exited, so a caller (Reset) can safely touch
// shared state immediately after.
func TestRunnerStopIsSynchronous(t *testing.T) {
	h := newHarness(t)
	mustLoad(t, h.runner, `
name: long
clock: {start: "2026-08-12T00:00:00+03:00", speed: 1}
steps:
  - at: 1h
    write: {device: EMS, point: set_operating_mode, value: 2}
`)
	if err := h.runner.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	time.Sleep(10 * time.Millisecond) // let it start ticking toward the 1h deadline
	h.runner.Stop()
	if h.runner.Running() {
		t.Error("Running() = true immediately after Stop() returned, want false")
	}
}

// TestRunnerStopThenStartResumes proves cursor/clock state survive a
// Stop, and Start resumes from where it left off rather than restarting.
// Deliberately doesn't assert Stop landed on any *specific* cursor value
// mid-flight — real-time-sleep-based interruption timing is inherently
// racy (how many stepInterval ticks toward a distant deadline complete
// before Stop() is called depends on machine speed, not on this
// package's own logic) — only that whatever cursor Stop() leaves things
// at, a resumed run reaches the same, correct final state.
func TestRunnerStopThenStartResumes(t *testing.T) {
	h := newHarness(t)
	mustLoad(t, h.runner, `
name: resume
clock: {start: "2026-08-12T00:00:00+03:00", speed: 1}
steps:
  - at: 0s
    write: {device: EMS, point: set_operating_mode, value: 2}
  - at: 10h
    write: {device: EMS, point: set_active_power_kw, value: -10}
`)
	if err := h.runner.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	time.Sleep(5 * time.Millisecond) // let it start ticking toward step 1's very distant deadline
	h.runner.Stop()
	cursorAfterStop := h.runner.Cursor()
	if cursorAfterStop < 0 || cursorAfterStop > 2 {
		t.Fatalf("Cursor after Stop = %d, want 0, 1, or 2", cursorAfterStop)
	}

	if err := h.runner.Start(); err != nil {
		t.Fatalf("resume Start: %v", err)
	}
	waitUntilStopped(t, h.runner)
	if err := h.runner.LastError(); err != nil {
		t.Fatalf("scenario failed on resume: %v", err)
	}
	if got := h.runner.Cursor(); got != 2 {
		t.Errorf("Cursor after resume completes = %d, want 2", got)
	}
}

// TestRunnerResetPlaybackReturnsToStepZero proves ResetPlayback (the
// scenario-runner half of Task 7 item 7's reset) stops execution and
// rewinds the cursor, while keeping the scenario loaded.
func TestRunnerResetPlaybackReturnsToStepZero(t *testing.T) {
	h := newHarness(t)
	mustLoad(t, h.runner, `
name: resettable
clock: {start: "2026-08-12T00:00:00+03:00", speed: 1}
steps:
  - at: 0s
    write: {device: EMS, point: set_operating_mode, value: 2}
`)
	if err := h.runner.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitUntilStopped(t, h.runner)
	if got := h.runner.Cursor(); got != 1 {
		t.Fatalf("setup: Cursor = %d, want 1", got)
	}

	h.runner.ResetPlayback()
	if h.runner.Running() {
		t.Error("Running() = true after ResetPlayback, want false")
	}
	if got := h.runner.Cursor(); got != 0 {
		t.Errorf("Cursor after ResetPlayback = %d, want 0", got)
	}
	if h.runner.Loaded() == nil {
		t.Error("Loaded() = nil after ResetPlayback, want the scenario still loaded")
	}
}

// TestRunnerAcceleratedAndNormalSpeedGiveSameFinalState is the
// accelerated-vs-normal equivalence acceptance criterion: the same
// scenario, differing only in clock.speed, must land on the identical
// final Store state — see scenario.Runner's own doc comment for why this
// holds unconditionally (Speed is never consulted by execution itself).
func TestRunnerAcceleratedAndNormalSpeedGiveSameFinalState(t *testing.T) {
	run := func(speed int) map[m261points.PointKey]float64 {
		h := newHarness(t)
		yaml := `
name: speed-test
clock: {start: "2026-08-12T00:00:00+03:00", speed: ` + strconv.Itoa(speed) + `}
steps:
  - at: 0s
    write: {device: EMS, point: set_operating_mode, value: 2}
  - at: 5s
    write: {device: EMS, point: set_active_power_kw, value: -50}
  - at: 30s
    fault: {device: BMS, point: cell_temperature_too_high, value: 1}
`
		if err := h.runner.Load(mustParse(t, yaml)); err != nil {
			t.Fatalf("Load: %v", err)
		}
		if err := h.runner.Start(); err != nil {
			t.Fatalf("Start: %v", err)
		}
		waitUntilStopped(t, h.runner)
		if err := h.runner.LastError(); err != nil {
			t.Fatalf("scenario failed at speed=%d: %v", speed, err)
		}
		return h.store.Snapshot()
	}

	slow := run(1)
	fast := run(60)

	if len(slow) != len(fast) {
		t.Fatalf("snapshot sizes differ: %d vs %d", len(slow), len(fast))
	}
	for k, v := range slow {
		if fast[k] != v {
			t.Errorf("%v: speed=1 -> %v, speed=60 -> %v, want equal", k, v, fast[k])
		}
	}
}
