package scenario

import (
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/SterneStehen/petz-m261-tooling/gen/go/m261points"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/clock"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/commands"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/faults"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/linkfault"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/physics"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/store"
)

var (
	ErrNoScenarioLoaded = errors.New("scenario: no scenario loaded")
	ErrAlreadyRunning   = errors.New("scenario: already running")
	ErrLoadWhileRunning = errors.New("scenario: cannot load a new scenario while one is running")
)

// Runner executes a loaded Scenario deterministically (AGENT-TASK.md,
// Task 7 item 5): the shared clock (a *clock.Fake — the same one
// physics.Runner's real-time background pacer (PacedRun) and POST
// /clock/advance also drive, per main.go's single-clock wiring) is the
// only notion of time anywhere in this file — this package itself never
// calls time.Now/time.Sleep; real-time pacing for a loaded scenario's
// clock.speed is entirely physics.Runner.PacedFastForwardLocked's job
// (see advanceTo), the same "outermost driving loop" exception
// physics.Runner.PacedRun's own real ticker already relies on. Every
// clock advance funnels through that one primitive, one
// stepInterval-sized increment at a time on the way to each step's
// deadline, never a bigger jump — so heartbeat and every other per-tick
// point never skips a tick regardless of how far a step's at: is from
// the last one.
//
// A running Runner holds physics.Runner's drive lock (TryAcquireDrive,
// acquired once in Start, released when the run() goroutine exits) for
// its *whole* run, not just while a tick is actually in flight —
// otherwise PacedRun's independent ticks, or a concurrent POST
// /clock/advance, could interleave with this file's own advances between
// two steps, breaking the deterministic step-timing guarantee below.
//
// Load, Start, Stop, and ResetPlayback all serialize against each other
// via lifecycleMu, held for each call's *entire* body (including Load's
// dry-validation pass and Stop's blocking wait for the run() goroutine
// to actually exit) — not just the brief bit of state each one pokes.
// Reviewed gap this closes: Load used to check `running`, release the
// lock to dry-validate, then re-acquire it to install — a concurrent
// Start could land in that window and begin running the *previous*
// scenario while Load was still deciding whether to accept a new one.
// Similarly, ResetPlayback used to call Stop and then, separately,
// rewind the cursor — a Start could land between the two and begin a run
// against a not-yet-rewound cursor. Holding lifecycleMu across each
// call's whole body — never just the quick field pokes, which stay on
// the separate, lighter-weight mu below — closes both windows: none of
// these four methods can partially overlap another's.
//
// lifecycleMu is deliberately a *different* lock from mu (below), not an
// extension of it: the run() goroutine itself only ever touches mu, for
// its own brief per-iteration field reads/writes, and never touches
// lifecycleMu at all — so a Stop/ResetPlayback holding lifecycleMu
// across its blocking <-doneCh wait can never deadlock against the very
// goroutine it's waiting for (which needs mu, not lifecycleMu, to make
// progress and exit).
//
// Speed (the loaded Scenario's ClockSpec.Speed) genuinely paces
// advanceTo in real wall-clock time — rather than being decorative — an
// operator running a scenario live sees model time advance at Speed x
// real time, exactly like physics.Runner.PacedRun's own speed parameter
// for the no-scenario-loaded case. This does not threaten "accelerated
// vs normal execution produces the same final Store state": the
// *sequence* of stepInterval-sized dt values applied to the engine is
// identical regardless of Speed (only how far apart in real time each
// one lands), and the physics model's output depends only on that
// sequence, never on wall-clock timing.
type Runner struct {
	// lifecycleMu serializes Load/Start/Stop/ResetPlayback/LockForReset
	// against each other — see the package doc comment above.
	lifecycleMu sync.Mutex

	mu sync.Mutex

	scenario     *Scenario
	cursor       int
	running      bool
	lastErr      error
	startInstant time.Time
	haveStart    bool

	// doneCh is non-nil exactly while a run() goroutine is alive or has
	// been asked to stop but hasn't confirmed exit yet, and is closed by
	// run() itself right before it returns. Stop (and ResetPlayback,
	// which calls it) blocks on this channel so that "Stop returned"
	// really means "no step is executing and no Tick is in flight", not
	// just "we asked it to stop" — required for Task 7 item 7's reset
	// atomicity: a caller doing Reset must be able to touch Store/
	// Processor/physics.Runner state immediately after Stop returns
	// without a straggler goroutine still mutating any of it.
	//
	// Start checks this (not just running) before launching a new
	// goroutine: reviewed gap this closes — running flips to false the
	// instant Stop is called, before the run() goroutine has actually
	// exited, so a second Start racing in during that window used to see
	// running == false and launch a second concurrent run() goroutine
	// against the same Runner state. Checking doneCh (only replaced once
	// the previous one is confirmed closed) instead of running closes
	// that window. lifecycleMu (above) now also makes this scenario
	// structurally impossible on its own, but the check is kept as
	// defense in depth and because Stop/ResetPlayback still need doneCh
	// to know whether there's anything to wait for.
	doneCh chan struct{}

	// stopCh is closed by stopLocked to interrupt an in-flight
	// advanceTo/PacedFastForwardLocked call promptly, instead of it
	// running its pacing sleep out to completion first. Set fresh by
	// Start each time a new run() goroutine is launched; nilled out by
	// stopLocked once consumed, so a second Stop call (idempotent) never
	// double-closes it.
	stopCh chan struct{}

	store         *store.Store
	injector      *faults.Injector
	processor     *commands.Processor
	physicsRunner *physics.Runner
	clk           *clock.Fake
	stepInterval  time.Duration
	iecTarget     linkfault.Target
	modbusTarget  linkfault.Target

	linkCoord    *linkfault.Coordinator // see SetLinkCoordinator
	stepObserver func(StepEvent)
}

// SetLinkCoordinator wires the shared link-fault coordinator (package
// linkfault) — a link: step's Apply call becomes one operation fully
// serialized against every other coordinator-mediated link operation
// (POST /link, POST /link/clear, POST /reset's own clear) through it,
// closing the same protocol: both non-atomicity and heartbeat_pause-vs-
// reset staleness gaps linkfault.Coordinator's own doc comment describes.
// nil (never calling SetLinkCoordinator) disables coordination, same as
// every other gated type in this codebase.
func (r *Runner) SetLinkCoordinator(c *linkfault.Coordinator) { r.linkCoord = c }

// SetStepObserver installs the optional, non-authoritative progress hook used
// by presentation layers. The runner itself remains independent of those
// layers and keeps deterministic execution if no observer is installed.
func (r *Runner) SetStepObserver(observer func(StepEvent)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stepObserver = observer
}

func NewRunner(
	st *store.Store,
	injector *faults.Injector,
	processor *commands.Processor,
	physicsRunner *physics.Runner,
	clk *clock.Fake,
	stepInterval time.Duration,
	iecTarget, modbusTarget linkfault.Target,
) *Runner {
	return &Runner{
		store: st, injector: injector, processor: processor, physicsRunner: physicsRunner,
		clk: clk, stepInterval: stepInterval, iecTarget: iecTarget, modbusTarget: modbusTarget,
	}
}

// Load installs s as the current scenario, cursor at the beginning.
// Fails if a scenario is currently running — stop it first, matching
// POST /scenario/load's documented 409-on-conflict shape one layer up.
//
// Before installing anything, Load dry-validates every write: and
// fault: step against commands.Processor.Validate / faults.Injector's
// own checks — the same checks Write/Inject would apply at execution
// time — and rejects the whole scenario if any of them would fail
// (AGENT-TASK.md, Task 7 item 5: malformed actions fail before
// execution). Reviewed gap this closes: previously only structural
// validity (known device/point, right action shape) was checked at load
// time; a step whose *value* Processor/Injector would reject only failed
// once the scenario reached it, after any earlier steps' writes had
// already landed in the Store — a partial-execution outcome Task 7 item
// 5 explicitly rules out ("reject the whole thing, don't apply it
// partially").
//
// Load holds lifecycleMu for its *entire* body, including the
// dry-validation pass — see the package doc comment for the concurrent-
// Start race this closes (a previous version released the lock across
// validateAll and re-acquired it only to install, leaving a window where
// a concurrent Start could run against stale state).
//
// Checks goroutineStillAlive(r.doneCh), not just r.running — third-review
// -round fix: a scenario that fails or completes *on its own* (not via an
// external Stop()) sets running=false and only *afterward* logs its own
// failure context and closes doneCh (see run()'s own doc comment); a
// Load() landing in that narrow window used to see running==false and
// proceed to install a new scenario — including rewinding r.cursor and
// replacing r.scenario — while the old run() goroutine's own deferred
// cleanup was still reading those exact fields for its log message,
// a genuine data race (caught under -race). Treating a still-alive
// doneCh as a conflict, exactly like Start already does, closes it: the
// caller gets ErrLoadWhileRunning and can retry a moment later instead of
// racing the old goroutine's tail end.
func (r *Runner) Load(s *Scenario) error {
	r.lifecycleMu.Lock()
	defer r.lifecycleMu.Unlock()

	r.mu.Lock()
	conflict := r.running || goroutineStillAlive(r.doneCh)
	r.mu.Unlock()
	if conflict {
		return ErrLoadWhileRunning
	}

	if err := r.validateAll(s); err != nil {
		return err
	}

	r.mu.Lock()
	r.scenario = s
	r.cursor = 0
	r.lastErr = nil
	r.haveStart = false
	r.mu.Unlock()
	return nil
}

// validateAll dry-runs every write:/fault: step's value against the same
// validation Write/Inject apply, without touching the Store or any
// Processor/Injector internal state — expect:/link: steps aren't
// value-checked here (expect never mutates; link's protocol/mode/
// delay_ms enum-shape is already fully checked by Parse).
//
// Also validates, for every step in order, that the real-time chunk(s)
// physics.Runner.PacedFastForwardLocked will actually pace through to
// reach that step's at: — from the previous step's at: (or 0, for the
// first step) to this one — are representable at this Runner's *real*
// stepInterval and the scenario's declared clock.speed (fifth-review-round
// fix: Parse itself only validates speed is finite/positive syntax now;
// it can no longer catch a speed that's fine at a small real stepInterval
// but would overflow/underflow at one, nor reject one that's fine at the
// real stepInterval but would have overflowed only against an artificial
// stand-in — see physics.ValidatePacing and parse.go's own doc comment on
// why that check moved here). A rejection here, like every other
// validateAll rejection, fails the whole Load before installing s at all
// — the scenario's first step, even one at: 0s, never executes.
func (r *Runner) validateAll(s *Scenario) error {
	var prevAt time.Duration
	for i, step := range s.Steps {
		if err := physics.ValidatePacing(step.At-prevAt, r.stepInterval, s.Clock.Speed); err != nil {
			return fmt.Errorf("scenario: step %d: at: %s: pacing from the previous step: %w", i, step.At, err)
		}
		prevAt = step.At

		switch {
		case step.Write != nil:
			key := m261points.PointKey{Device: step.Write.Device, Slug: step.Write.Point}
			if err := r.processor.Validate(key, step.Write.Value); err != nil {
				return fmt.Errorf("scenario: step %d: write: %w", i, err)
			}
		case step.Fault != nil:
			key := m261points.PointKey{Device: step.Fault.Device, Slug: step.Fault.Point}
			if err := r.injector.Validate(key, step.Fault.Value); err != nil {
				return fmt.Errorf("scenario: step %d: fault: %w", i, err)
			}
		}
	}
	return nil
}

func (r *Runner) Loaded() *Scenario {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.scenario
}

func (r *Runner) Running() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.running
}

// LastError is the error the most recent run stopped on (a rejected
// write/fault, or a failed expect — AGENT-TASK.md, Task 7 item 4's
// "execution-time scenario failure"), or nil if the scenario hasn't run
// yet, completed successfully, or was stopped via Stop() rather than
// failing.
func (r *Runner) LastError() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastErr
}

func (r *Runner) Cursor() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cursor
}

// Start begins (or resumes, if previously Stop()ped mid-scenario)
// executing the loaded scenario in a background goroutine — returns
// immediately, doesn't block for the scenario's duration. The very first
// Start after Load sets the shared clock to the scenario's declared
// clock.start and rebases physics.Runner's dt baseline to match
// (Rebase, not Reset — the running Engine, and its SoC/temperature/
// energy, is untouched; only where the next Tick's dt is measured
// from). A resumed Start (cursor > 0) does neither: the clock keeps
// whatever value Stop() left it at, and step At offsets keep being
// measured against the original clock.start, not a new one.
//
// Fails with ErrAlreadyRunning if a previous run's goroutine hasn't
// confirmed its own exit yet (doneCh still open) — see doneCh's doc
// comment for why that's a different, stricter check than "running".
//
// Start claims physics.Runner's drive lock (TryAcquireDrive) for the
// *whole* run — Stop/completion releases it, in run()'s own cleanup —
// before touching the clock at all, and fails with physics.ErrClockBusy
// (mapped to 409 by controlapi, same as a concurrent POST
// /clock/advance) if a manual advance or another scenario run already
// owns it. Reviewed ordering bug this fixes: a previous version called
// Clock.Set/Rebase *before* claiming exclusive ownership of the clock,
// so a PacedRun tick — or a concurrent POST /clock/advance — could land
// between the rebase and the ownership claim and observe (or itself
// produce) a dt computed against the wrong baseline.
func (r *Runner) Start() error {
	r.lifecycleMu.Lock()
	defer r.lifecycleMu.Unlock()

	r.mu.Lock()
	if r.scenario == nil {
		r.mu.Unlock()
		return ErrNoScenarioLoaded
	}
	if r.running || goroutineStillAlive(r.doneCh) {
		r.mu.Unlock()
		return ErrAlreadyRunning
	}
	r.mu.Unlock()

	if !r.physicsRunner.TryAcquireDrive() {
		return physics.ErrClockBusy
	}

	r.mu.Lock()
	if !r.haveStart {
		r.startInstant = r.scenario.Clock.Start
		r.haveStart = true
		r.clk.Set(r.startInstant)
		r.physicsRunner.Rebase(r.startInstant)
	}
	done := make(chan struct{})
	stop := make(chan struct{})
	r.running = true
	r.lastErr = nil
	r.doneCh = done
	r.stopCh = stop
	r.mu.Unlock()

	go r.run(done, stop)
	return nil
}

// goroutineStillAlive reports whether done represents a run() goroutine
// that hasn't confirmed its own exit — nil (never started) or an already
// -closed channel both mean "no", matching Start's "safe to launch a new
// one" condition.
func goroutineStillAlive(done chan struct{}) bool {
	if done == nil {
		return false
	}
	select {
	case <-done:
		return false
	default:
		return true
	}
}

// Stop halts execution and blocks until the run() goroutine has actually
// exited — not merely until it's been asked to — so that no step
// (write/fault/expect/link) and no physics Tick can still be in flight
// once Stop returns. The cursor is preserved, so a later Start resumes
// rather than restarts. Idempotent: stopping an already-stopped Runner
// is a no-op that returns immediately.
//
// Holds lifecycleMu for its whole body — see the package doc comment for
// why that's safe (the run() goroutine Stop waits on never itself needs
// lifecycleMu to make progress) and for the race this closes against a
// concurrent Start/Load/ResetPlayback.
func (r *Runner) Stop() {
	r.lifecycleMu.Lock()
	defer r.lifecycleMu.Unlock()
	r.stopLocked()
}

// stopLocked is Stop's implementation, usable by a caller (Stop itself,
// or LockForReset) that already holds lifecycleMu.
func (r *Runner) stopLocked() {
	r.mu.Lock()
	done := r.doneCh
	stop := r.stopCh
	r.running = false
	r.stopCh = nil
	r.mu.Unlock()
	if stop != nil {
		close(stop) // wake up a blocked advanceTo/PacedFastForwardLocked immediately, instead of waiting out its current pace sleep
	}
	if !goroutineStillAlive(done) {
		return
	}
	<-done
}

// ResetPlayback implements the scenario-runner half of Task 7 item 7's
// reset: stops execution (like Stop, including the same wait-for-actual-
// exit guarantee) and returns the cursor to the beginning, but — unlike
// Load — keeps the currently loaded Scenario loaded and ready to Start
// again from step 0, and forgets the previously established clock.start
// baseline so the next Start re-establishes it fresh.
//
// Holds lifecycleMu across both the stop and the cursor rewind — see the
// package doc comment for the race this closes (a previous version did
// the two as separate, independently-locked steps, leaving a window
// where a concurrent Start could begin between them).
func (r *Runner) ResetPlayback() {
	r.lifecycleMu.Lock()
	defer r.lifecycleMu.Unlock()
	r.stopLocked()
	r.mu.Lock()
	r.cursor = 0
	r.lastErr = nil
	r.haveStart = false
	r.mu.Unlock()
}

// LockForReset is ResetPlayback's counterpart for controlapi's POST
// /reset: it does exactly what ResetPlayback does, but — instead of
// releasing lifecycleMu before returning — keeps it held until the
// caller invokes the returned unlock function, so no POST
// /scenario/load or /scenario/start can begin anywhere in between.
// Reviewed gap this closes: doReset used to call ResetPlayback (which
// releases the scenario lifecycle immediately) and only afterwards
// acquire its own separate exclusive gate for the rest of the reset
// sequence (physics engine, Store, link faults) — a POST
// /scenario/start racing into that gap could begin a new run against
// state doReset was still in the middle of resetting out from under it.
func (r *Runner) LockForReset() (unlock func()) {
	r.lifecycleMu.Lock()
	r.stopLocked()
	r.mu.Lock()
	r.cursor = 0
	r.lastErr = nil
	r.haveStart = false
	r.mu.Unlock()
	return r.lifecycleMu.Unlock
}

func (r *Runner) run(done, stop chan struct{}) {
	// Deferred in this order (LIFO: ReleaseDrive first, close(done)
	// second -- closing done runs first, then ReleaseDrive) so that
	// ReleaseDrive has already happened by the time Stop's <-done wait
	// returns. Reviewed regression this fixes: with the order reversed,
	// Stop could return (done observed closed) a moment before this
	// goroutine's own ReleaseDrive call actually ran, so a Start called
	// immediately after Stop returns could see physics.Runner's drive
	// lock still held by the very run this Stop just waited out and fail
	// with ErrClockBusy -- caught by TestRunnerStopThenStartResumes and
	// TestLoadRejectedWhileRunningUnderConcurrency under -race -count=5.
	defer close(done)
	defer r.physicsRunner.ReleaseDrive()
	for {
		r.mu.Lock()
		if !r.running {
			r.mu.Unlock()
			return
		}
		if r.cursor >= len(r.scenario.Steps) {
			r.running = false
			r.mu.Unlock()
			return
		}
		step := r.scenario.Steps[r.cursor]
		startInstant := r.startInstant
		speed := r.scenario.Clock.Speed
		r.mu.Unlock()

		if !r.advanceTo(startInstant.Add(step.At), speed, stop) {
			return // Stop() fired while ticking toward this step's deadline
		}

		if err := r.executeStep(step); err != nil {
			r.mu.Lock()
			r.lastErr = err
			r.running = false
			// name/cursor captured *inside* this same critical section,
			// not read again after unlocking below — third-review-round
			// fix: Load() only checked r.running (now also checks
			// goroutineStillAlive(r.doneCh), but a caller building against
			// an older assumption, or any other future reader of these
			// fields outside r.mu, could still race this goroutine's own
			// read of r.scenario.Name/r.cursor between the unlock below
			// and this log line — a real data race under -race once
			// something concurrently mutates r.scenario/r.cursor in that
			// window (which Load, pre-fix, could do). Reading them here,
			// under the lock, removes the window entirely rather than
			// relying on nothing else ever running concurrently.
			name, cursor := r.scenario.Name, r.cursor
			r.mu.Unlock()
			log.Printf("scenario: %q step %d (at %s) failed, scenario stopped: %v", name, cursor, step.At, err)
			return
		}

		r.mu.Lock()
		event := StepEvent{Scenario: r.scenario.Name, Index: r.cursor, At: step.At, Action: stepAction(step)}
		r.cursor++
		observer := r.stepObserver
		r.mu.Unlock()
		if observer != nil {
			observer(event)
		}
	}
}

func stepAction(step Step) string {
	switch {
	case step.Write != nil:
		return "write"
	case step.Fault != nil:
		return "fault"
	case step.Expect != nil:
		return "expect"
	case step.Link != nil:
		return "link"
	default:
		return "unknown"
	}
}

// advanceTo advances the shared clock from wherever it currently is up
// to deadline via physics.Runner.PacedFastForwardLocked — one
// stepInterval-sized physics tick at a time (never a bigger jump — see
// the package doc comment), each paced stepInterval/speed apart in real
// time, interruptible by stop between increments. "Locked" because Start
// already claimed this Runner's drive lock (physics.Runner.
// TryAcquireDrive) for the whole run, not just this one step —
// PacedFastForwardLocked assumes that ownership rather than trying to
// reacquire it. This is also why this package needs no time.Now/
// time.Sleep of its own (AGENT-TASK §1.5): all real-time pacing lives in
// physics.Runner, the one place already responsible for PacedRun's
// identical real ticker.
//
// Returns false if stop fired before deadline was reached, or if the
// underlying clock somehow isn't the *clock.Fake this Runner requires
// (surfaced as a failed step, recorded via lastErr, rather than a silent
// no-op or a panic — never reachable in practice, given main.go's
// wiring).
func (r *Runner) advanceTo(deadline time.Time, speed float64, stop chan struct{}) bool {
	if speed <= 0 {
		speed = 1
	}
	now := r.clk.Now()
	total := deadline.Sub(now)
	if total <= 0 {
		return true
	}

	completed, err := r.physicsRunner.PacedFastForwardLocked(total, r.stepInterval, speed, stop)
	if err != nil {
		r.mu.Lock()
		r.lastErr = err
		r.running = false
		r.mu.Unlock()
		return false
	}
	return completed
}

func (r *Runner) executeStep(step Step) error {
	switch {
	case step.Write != nil:
		key := m261points.PointKey{Device: step.Write.Device, Slug: step.Write.Point}
		return r.processor.Write(key, step.Write.Value)
	case step.Fault != nil:
		key := m261points.PointKey{Device: step.Fault.Device, Slug: step.Fault.Point}
		return r.injector.Inject(key, step.Fault.Value)
	case step.Expect != nil:
		return r.checkExpect(step.Expect)
	case step.Link != nil:
		return r.applyLink(step.Link)
	default:
		return fmt.Errorf("scenario: step has no action") // unreachable — Parse guarantees exactly one
	}
}

func (r *Runner) checkExpect(e *ExpectAction) error {
	key := m261points.PointKey{Device: e.Device, Slug: e.Point}
	got, ok := r.store.Get(key)
	if !ok {
		return fmt.Errorf("scenario: expect: %s/%s: point not found in store", e.Device, e.Point)
	}
	if e.Value != nil {
		if got != *e.Value {
			return fmt.Errorf("scenario: expect: %s/%s: got %v, want %v", e.Device, e.Point, got, *e.Value)
		}
		return nil
	}
	if e.Min != nil && got < *e.Min {
		return fmt.Errorf("scenario: expect: %s/%s: got %v, want >= %v", e.Device, e.Point, got, *e.Min)
	}
	if e.Max != nil && got > *e.Max {
		return fmt.Errorf("scenario: expect: %s/%s: got %v, want <= %v", e.Device, e.Point, got, *e.Max)
	}
	return nil
}

func (r *Runner) applyLink(l *LinkAction) error {
	delay := time.Duration(l.DelayMS) * time.Millisecond
	// linkfault.ApplyCoordinated: see controlapi.Server.handleLink's
	// identical comment for the races this closes (protocol: both vs a
	// concurrent reset/link operation, heartbeat_pause's capture vs a
	// concurrent reset).
	if err := linkfault.ApplyCoordinated(r.linkCoord, r.store, r.iecTarget, r.modbusTarget, linkfault.Protocol(l.Protocol), linkfault.Mode(l.Mode), delay); err != nil {
		return err
	}
	// Fifth-review-round fix: see controlapi.Server.handleLink's identical
	// comment — called after ApplyCoordinated has already released
	// r.linkCoord, never while still holding it. A scenario's own link:
	// step must give the same heartbeat-fencing guarantee POST /link
	// does: this step doesn't complete (the scenario doesn't advance to
	// its next step) until every heartbeat frame admitted before it has
	// actually been sent.
	r.iecTarget.FenceHeartbeat()
	r.modbusTarget.FenceHeartbeat()
	return nil
}
