package scenario_test

import (
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/SterneStehen/petz-m261-tooling/gen/go/m261points"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/clock"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/commands"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/faults"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/linkfault"
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
	// A coarse stepInterval (rather than the production 1s default) keeps
	// these tests, several of which span up to an hour of scenario at:
	// offsets, fast under -race — correctness here is about execution
	// order and timing relative to at:, never about exact tick counts
	// (that's physics.TestFastForward*'s job, in package physics).
	return newHarnessWithSOCAndStep(t, initialSOC, 5*time.Minute)
}

func newHarnessWithSOCAndStep(t *testing.T, initialSOC float64, stepInterval time.Duration) *harness {
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
	r := scenario.NewRunner(st, inj, proc, pr, clk, stepInterval, iec, mb)
	r.SetLinkCoordinator(linkfault.NewCoordinator())
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
clock: {start: "2026-08-12T00:00:00+03:00", speed: 1000000}
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
// Task 7 item 5's ordering rule directly, by observing the actual order
// two Store writes land in via store.Store.Subscribe — not indirectly,
// via a final-state check an out-of-order (or even fully reordered)
// execution could pass just as easily: two independent writes landing in
// either order both leave the same two points at the same final values,
// so a test that only inspects the end state (the previous version of
// this test) can't actually distinguish "declaration order" from "any
// order at all".
func TestRunnerSameTimestampDifferentPointsExecuteInDeclarationOrder(t *testing.T) {
	h := newHarness(t)
	changes, unsubscribe := h.store.Subscribe()
	defer unsubscribe()

	// at: 0s for both — deliberately no physics tick needed to reach
	// either deadline (advanceTo returns immediately when the clock is
	// already there): a real tick's own writeState publishes several
	// hundred Store Changes (every catalog point physics touches), which
	// would fill the Subscribe channel's small fixed buffer (64) before
	// the two writes this test actually cares about ever got a chance to
	// publish, and silently drop them (Store.publish is best-effort).
	mustLoad(t, h.runner, `
name: order
clock: {start: "2026-08-12T00:00:00+03:00", speed: 1000000}
steps:
  - at: 0s
    write: {device: EMS, point: set_operating_mode, value: 2}
  - at: 0s
    write: {device: EMS, point: set_active_power_kw, value: -33}
`)
	if err := h.runner.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitUntilStopped(t, h.runner)
	if err := h.runner.LastError(); err != nil {
		t.Fatalf("scenario failed: %v", err)
	}

	modeKey := m261points.PointKey{Device: "EMS", Slug: "set_operating_mode"}
	powerKey := m261points.PointKey{Device: "EMS", Slug: "set_active_power_kw"}
	var order []m261points.PointKey
loop:
	for {
		select {
		case c := <-changes:
			if c.Key == modeKey || c.Key == powerKey {
				order = append(order, c.Key)
			}
		default:
			break loop
		}
	}
	if len(order) != 2 || order[0] != modeKey || order[1] != powerKey {
		t.Fatalf("observed Store write order = %v, want [%v, %v] (declaration order) — a reordered or interleaved execution would fail this", order, modeKey, powerKey)
	}
}

// TestRunnerExpectFailureStopsScenario proves an execution-time check
// failure halts the scenario (not applied partially) rather than
// continuing past it.
func TestRunnerExpectFailureStopsScenario(t *testing.T) {
	h := newHarness(t)
	mustLoad(t, h.runner, `
name: bad-expect
clock: {start: "2026-08-12T00:00:00+03:00", speed: 1000000}
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

// TestRunnerRejectsWriteAtLoadTime proves Load itself rejects a write:
// step whose value commands.Processor.Validate would reject (Task 6's
// own validation, dry-run through the scenario path per Task 7 item 3's
// "no exception for the scenario path" rule) — at Load time, before
// Start is ever called, per Task 7 item 5's "reject the whole thing,
// don't apply it partially": an earlier version only discovered this at
// execution time, which meant any steps *before* the bad one had already
// landed in the Store by the time the scenario failed.
func TestRunnerRejectsWriteAtLoadTime(t *testing.T) {
	h := newHarness(t)
	s, err := scenario.Parse([]byte(`
name: bad-write
clock: {start: "2026-08-12T00:00:00+03:00", speed: 1000000}
steps:
  - at: 0s
    write: {device: EMS, point: set_active_power_kw, value: -10}
  - at: 1s
    write: {device: EMS, point: set_operating_mode, value: 99}
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := h.runner.Load(s); err == nil {
		t.Fatal("Load succeeded, want a rejection (99 is not a valid Set Operating Mode enum value)")
	}
	if h.runner.Loaded() != nil {
		t.Error("Loaded() is non-nil after a rejected Load — the scenario must not be installed")
	}
	// The first step's value was fine on its own — proves rejection came
	// from validating step 1, not step 0, and (since Load never even
	// calls Start) nothing was ever written to the Store.
	if v, _ := h.store.Get(m261points.PointKey{Device: "EMS", Slug: "set_active_power_kw"}); v != 0 {
		t.Errorf("set_active_power_kw = %v after a rejected Load, want unchanged 0 — Load must not execute any step", v)
	}
}

// TestRunnerRejectsFaultAtLoadTime mirrors the same for a fault: step
// targeting a non-alarm point.
func TestRunnerRejectsFaultAtLoadTime(t *testing.T) {
	h := newHarness(t)
	s, err := scenario.Parse([]byte(`
name: bad-fault
clock: {start: "2026-08-12T00:00:00+03:00", speed: 1000000}
steps:
  - at: 0s
    fault: {device: EMS, point: set_active_power_kw, value: 1}
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := h.runner.Load(s); err == nil {
		t.Fatal("Load succeeded, want a rejection (set_active_power_kw is not class:alarm)")
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
clock: {start: "2026-08-12T00:00:00+03:00", speed: 1000000}
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
clock: {start: "2026-08-12T00:00:00+03:00", speed: 1000000}
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
clock: {start: "2026-08-12T00:00:00+03:00", speed: 1000000}
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
//
// Reviewed gap this closes: an earlier version compared two large speeds
// (100000 vs 1000000) against each other, never against literal
// clock.speed: 1 (AGENT-TASK.md, Task 7 item 5's actual acceptance
// criterion, "speed: 1 and speed > 1 give the same final state") — a bug
// that only manifested relative to genuine real-time pacing (e.g. Speed
// being silently ignored and every run defaulting to the same internal
// behavior regardless of value) could pass a "two big speeds agree with
// each other" comparison without ever exercising real-time pacing at
// all. This scenario's total span (60ms of model time, a tiny
// stepInterval) keeps a literal speed: 1 run's real-time pacing well
// under a second instead of requiring an actual multi-second sleep.
func TestRunnerAcceleratedAndNormalSpeedGiveSameFinalState(t *testing.T) {
	run := func(speed int) map[m261points.PointKey]float64 {
		h := newHarnessWithSOCAndStep(t, 50, 20*time.Millisecond)
		yaml := `
name: speed-test
clock: {start: "2026-08-12T00:00:00+03:00", speed: ` + strconv.Itoa(speed) + `}
steps:
  - at: 0ms
    write: {device: EMS, point: set_operating_mode, value: 2}
  - at: 20ms
    write: {device: EMS, point: set_active_power_kw, value: -50}
  - at: 60ms
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

	slow := run(1) // literal clock.speed: 1 — real-time pacing, not just "another large speed"
	fast := run(1000000)

	if len(slow) != len(fast) {
		t.Fatalf("snapshot sizes differ: %d vs %d", len(slow), len(fast))
	}
	for k, v := range slow {
		if fast[k] != v {
			t.Errorf("%v: speed=1 -> %v, speed=60 -> %v, want equal", k, v, fast[k])
		}
	}
}

// TestLoadRejectedWhileRunningUnderConcurrency is the regression test
// for the reviewed Load-vs-Start race: an earlier version of Load
// checked running, released its lock to dry-validate the incoming
// scenario, and only re-acquired the lock to install it — a concurrent
// Start landing in that window could begin running the *old* scenario
// while Load was still deciding whether to accept a new one, and Load's
// eventual install would then silently replace the running scenario's
// state out from under it. lifecycleMu (held across Load's entire body,
// including validation) closes this by construction; this test stresses
// the interleaving many times under -race rather than trying to hit one
// exact window by timing.
func TestLoadRejectedWhileRunningUnderConcurrency(t *testing.T) {
	h := newHarness(t)
	mustLoad(t, h.runner, `
name: long
clock: {start: "2026-08-12T00:00:00+03:00", speed: 1000000}
steps:
  - at: 1h
    write: {device: EMS, point: set_operating_mode, value: 2}
`)
	other := mustParse(t, `
name: other
clock: {start: "2026-08-12T00:00:00+03:00", speed: 1000000}
steps:
  - at: 0s
    write: {device: EMS, point: set_operating_mode, value: 1}
`)

	for i := 0; i < 50; i++ {
		if err := h.runner.Start(); err != nil {
			t.Fatalf("iter %d: Start: %v", i, err)
		}
		// Load must either be rejected outright (scenario still running)
		// or — if it happens to land after Start's own run already
		// finished — succeed; either is a valid outcome of the race, but
		// it must never silently corrupt Cursor/loaded-scenario state
		// (checked below by asserting the runner is left in one
		// consistent, well-defined state either way).
		err := h.runner.Load(other)
		if err != nil && !errors.Is(err, scenario.ErrLoadWhileRunning) {
			t.Fatalf("iter %d: Load returned %v, want nil or ErrLoadWhileRunning", i, err)
		}
		h.runner.Stop()
		if h.runner.Running() {
			t.Fatalf("iter %d: Running() = true after Stop, want false", i)
		}
		if c := h.runner.Cursor(); c < 0 {
			t.Fatalf("iter %d: Cursor = %d, want >= 0", i, c)
		}
		h.runner.ResetPlayback()
		mustLoad(t, h.runner, `
name: long
clock: {start: "2026-08-12T00:00:00+03:00", speed: 1000000}
steps:
  - at: 1h
    write: {device: EMS, point: set_operating_mode, value: 2}
`)
	}
}

// TestStartFailsWhileDriveHeldExternally proves Start claims
// physics.Runner's drive lock *before* touching the clock (Clock.Set/
// Rebase), and fails cleanly with physics.ErrClockBusy — leaving
// haveStart/the clock untouched — if something else (modeled directly
// via TryAcquireDrive, standing in for a concurrent POST /clock/advance
// or another scenario run) already owns it. Reviewed ordering bug this
// guards: a previous version rebased the clock *before* claiming
// ownership, so a concurrent driver could observe or itself produce a dt
// computed against the wrong baseline.
func TestStartFailsWhileDriveHeldExternally(t *testing.T) {
	h := newHarness(t)
	mustLoad(t, h.runner, `
name: solo
clock: {start: "2026-08-12T00:00:00+03:00", speed: 1000000}
steps:
  - at: 0s
    write: {device: EMS, point: set_operating_mode, value: 2}
`)
	before := h.clk.Now()

	if !h.physics.TryAcquireDrive() {
		t.Fatal("TryAcquireDrive on a fresh physics.Runner returned false")
	}
	err := h.runner.Start()
	if !errors.Is(err, physics.ErrClockBusy) {
		t.Fatalf("Start while drive externally held: err = %v, want physics.ErrClockBusy", err)
	}
	if h.runner.Running() {
		t.Error("Running() = true after a failed Start, want false")
	}
	if !h.clk.Now().Equal(before) {
		t.Errorf("clock moved to %v after a failed Start, want unchanged (%v)", h.clk.Now(), before)
	}
	h.physics.ReleaseDrive()

	if err := h.runner.Start(); err != nil {
		t.Fatalf("Start after ReleaseDrive: %v, want success", err)
	}
	waitUntilStopped(t, h.runner)
	if err := h.runner.LastError(); err != nil {
		t.Fatalf("scenario failed: %v", err)
	}
}

// TestConcurrentLoadStartStopResetPlaybackNeverRace fuzzes all four
// lifecycle entry points against each other concurrently and repeatedly
// — the reviewed requirement that Load/Start/Stop/ResetPlayback fully
// serialize against each other (lifecycleMu) — asserting only that
// nothing panics, no method call ever blocks forever, and Cursor/
// Running/Loaded stay within their well-defined ranges throughout. Run
// under -race, this also catches any data race the lock design missed,
// not just deadlocks/panics.
func TestConcurrentLoadStartStopResetPlaybackNeverRace(t *testing.T) {
	h := newHarness(t)
	s := mustParse(t, `
name: fuzz
clock: {start: "2026-08-12T00:00:00+03:00", speed: 1000000}
steps:
  - at: 0s
    write: {device: EMS, point: set_operating_mode, value: 2}
  - at: 1s
    write: {device: EMS, point: set_operating_mode, value: 1}
`)
	mustLoad(t, h.runner, `
name: fuzz
clock: {start: "2026-08-12T00:00:00+03:00", speed: 1000000}
steps:
  - at: 0s
    write: {device: EMS, point: set_operating_mode, value: 2}
`)

	const workers = 6
	const iterations = 30
	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func(w int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				switch (w + i) % 4 {
				case 0:
					h.runner.Load(s) //nolint:errcheck // any of {nil, ErrLoadWhileRunning} is valid under concurrency
				case 1:
					h.runner.Start() //nolint:errcheck // any of {nil, ErrNoScenarioLoaded, ErrAlreadyRunning, physics.ErrClockBusy} is valid
				case 2:
					h.runner.Stop()
				case 3:
					h.runner.ResetPlayback()
				}
				if c := h.runner.Cursor(); c < 0 {
					t.Errorf("worker %d iter %d: Cursor = %d, want >= 0", w, i, c)
				}
			}
		}(w)
	}

	waitDone := make(chan struct{})
	go func() { wg.Wait(); close(waitDone) }()
	select {
	case <-waitDone:
	case <-time.After(30 * time.Second):
		t.Fatal("concurrent Load/Start/Stop/ResetPlayback fuzzing did not finish within 30s — likely deadlocked")
	}
	h.runner.Stop() // leave the runner quiesced for t.Cleanup
}

// TestFailingScenarioVsConcurrentLoadNeverRaces is Significant 3's
// regression test: run() used to set running=false and unlock r.mu
// *before* reading r.scenario.Name/r.cursor for its own failure log line
// — a concurrent Load(), which only checked r.running (not doneCh
// liveness) before installing a new scenario, could land in that exact
// window and mutate r.scenario/r.cursor while the failing run()
// goroutine's own deferred log line was still reading them, a genuine
// data race under -race. Deliberately uses a scenario that fails at its
// very first step (an always-false expect:), maximizing how often Start
// and the failure race a concurrent Load within this test's real time
// budget.
func TestFailingScenarioVsConcurrentLoadNeverRaces(t *testing.T) {
	h := newHarness(t)
	other := mustParse(t, `
name: other
clock: {start: "2026-08-12T00:00:00+03:00", speed: 1000000}
steps:
  - at: 0s
    write: {device: EMS, point: set_operating_mode, value: 1}
`)
	mustLoad(t, h.runner, `
name: always-fails
clock: {start: "2026-08-12T00:00:00+03:00", speed: 1000000}
steps:
  - at: 0s
    expect: {device: BMS, point: soc, value: 999}
`)

	const iterations = 50
	for i := 0; i < iterations; i++ {
		if err := h.runner.Start(); err != nil {
			t.Fatalf("iter %d: Start: %v", i, err)
		}
		// Race Load immediately, while the just-started run() goroutine is
		// likely still executing its first (failing) step's cleanup.
		err := h.runner.Load(other)
		if err != nil && !errors.Is(err, scenario.ErrLoadWhileRunning) {
			t.Fatalf("iter %d: Load returned %v, want nil or ErrLoadWhileRunning", i, err)
		}
		waitUntilStopped(t, h.runner)
		// Whichever scenario ended up loaded (this Load might have won or
		// lost the race), get back to a known state for the next
		// iteration.
		h.runner.ResetPlayback()
		if h.runner.Loaded() == nil || err != nil {
			mustLoad(t, h.runner, `
name: always-fails
clock: {start: "2026-08-12T00:00:00+03:00", speed: 1000000}
steps:
  - at: 0s
    expect: {device: BMS, point: soc, value: 999}
`)
		}
	}
}
