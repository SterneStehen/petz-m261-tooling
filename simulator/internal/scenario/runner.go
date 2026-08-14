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
// only notion of time anywhere in this file. Every clock advance funnels
// through physics.Runner.TickOnce, one stepInterval-sized increment at a
// time on the way to each step's deadline, never a bigger jump (see
// advanceTo) — so heartbeat and every other per-tick point never skips a
// tick regardless of how far a step's at: is from the last one.
//
// A running Runner suspends physics.Runner's own real-time pacing for
// its whole duration (SuspendPacing, called once in Start, resumed when
// the run() goroutine exits) — otherwise PacedRun's independent ticks
// would race this file's own TickOnce calls for who advances the shared
// clock next, breaking the deterministic step-timing guarantee below.
//
// Speed (the loaded Scenario's ClockSpec.Speed) genuinely paces
// advanceTo in real wall-clock time — a real time.Sleep of
// stepInterval/Speed before each increment, interruptible by Stop()
// between increments — rather than being decorative: an operator running
// a scenario live sees model time advance at Speed x real time, exactly
// like physics.Runner.PacedRun's own speed parameter for the
// no-scenario-loaded case. This does not threaten "accelerated vs normal
// execution produces the same final Store state": the *sequence* of
// stepInterval-sized dt values applied to the engine is identical
// regardless of Speed (only how far apart in real time each one lands),
// and the physics model's output depends only on that sequence, never on
// wall-clock timing.
type Runner struct {
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
	// that window.
	doneCh chan struct{}

	store         *store.Store
	injector      *faults.Injector
	processor     *commands.Processor
	physicsRunner *physics.Runner
	clk           *clock.Fake
	stepInterval  time.Duration
	iecTarget     linkfault.Target
	modbusTarget  linkfault.Target
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
func (r *Runner) Load(s *Scenario) error {
	r.mu.Lock()
	if r.running {
		r.mu.Unlock()
		return ErrLoadWhileRunning
	}
	r.mu.Unlock()

	if err := r.validateAll(s); err != nil {
		return err
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.scenario = s
	r.cursor = 0
	r.lastErr = nil
	r.haveStart = false
	return nil
}

// validateAll dry-runs every write:/fault: step's value against the same
// validation Write/Inject apply, without touching the Store or any
// Processor/Injector internal state — expect:/link: steps aren't
// value-checked here (expect never mutates; link's protocol/mode/
// delay_ms enum-shape is already fully checked by Parse).
func (r *Runner) validateAll(s *Scenario) error {
	for i, step := range s.Steps {
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
func (r *Runner) Start() error {
	r.mu.Lock()
	if r.scenario == nil {
		r.mu.Unlock()
		return ErrNoScenarioLoaded
	}
	if r.running || goroutineStillAlive(r.doneCh) {
		r.mu.Unlock()
		return ErrAlreadyRunning
	}
	if !r.haveStart {
		r.startInstant = r.scenario.Clock.Start
		r.haveStart = true
		r.clk.Set(r.startInstant)
		r.physicsRunner.Rebase(r.startInstant)
	}
	done := make(chan struct{})
	r.running = true
	r.lastErr = nil
	r.doneCh = done
	r.mu.Unlock()

	resumePacing := r.physicsRunner.SuspendPacing()
	go r.run(done, resumePacing)
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
func (r *Runner) Stop() {
	r.mu.Lock()
	done := r.doneCh
	r.running = false
	r.mu.Unlock()
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
func (r *Runner) ResetPlayback() {
	r.Stop()
	r.mu.Lock()
	r.cursor = 0
	r.lastErr = nil
	r.haveStart = false
	r.mu.Unlock()
}

func (r *Runner) run(done chan struct{}, resumePacing func()) {
	defer resumePacing()
	defer close(done)
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

		if !r.advanceTo(startInstant.Add(step.At), speed) {
			return // Stop() fired while ticking toward this step's deadline
		}

		if err := r.executeStep(step); err != nil {
			r.mu.Lock()
			r.lastErr = err
			r.running = false
			r.mu.Unlock()
			log.Printf("scenario: %q step %d (at %s) failed, scenario stopped: %v", r.scenario.Name, r.cursor, step.At, err)
			return
		}

		r.mu.Lock()
		r.cursor++
		r.mu.Unlock()
	}
}

// advanceTo ticks physics forward in stepInterval-sized increments
// (never a bigger jump — see the package doc comment and
// physics.Runner.FastForward's) until the shared clock reaches deadline,
// checking for Stop() between every increment. Each increment is paced
// in real wall-clock time at stepInterval/speed (see the package doc
// comment on why this doesn't affect the resulting physics state, only
// how long real time this takes) — sleeping happens between increments,
// never while holding r.mu, so Stop() lands promptly rather than after
// however long the current sleep has left. Returns false if Stop()
// interrupted it before reaching deadline.
func (r *Runner) advanceTo(deadline time.Time, speed float64) bool {
	if speed <= 0 {
		speed = 1
	}
	for {
		r.mu.Lock()
		running := r.running
		r.mu.Unlock()
		if !running {
			return false
		}
		now := r.clk.Now()
		if !now.Before(deadline) {
			return true
		}
		next := r.stepInterval
		if remaining := deadline.Sub(now); remaining < next {
			next = remaining
		}

		pace := time.Duration(float64(next) / speed)
		if pace > 0 && !r.sleepInterruptible(pace) {
			return false
		}

		if err := r.physicsRunner.TickOnce(next); err != nil {
			// Only reachable if this Runner's clock somehow isn't the
			// *clock.Fake it was constructed with — never in practice,
			// but surfacing it as a failed step is more honest than a
			// silent no-op or a panic.
			r.mu.Lock()
			r.lastErr = err
			r.running = false
			r.mu.Unlock()
			return false
		}
	}
}

// sleepInterruptible sleeps up to d in small quanta, checking Running()
// between each one, so a concurrent Stop() takes effect within about a
// millisecond instead of only after the full pace duration elapses —
// relevant mainly at low speed, where a single increment's real-time
// pace can be seconds long. Returns false if Stop() fired before d
// elapsed.
func (r *Runner) sleepInterruptible(d time.Duration) bool {
	const quantum = time.Millisecond
	end := time.Now().Add(d)
	for {
		if !r.Running() {
			return false
		}
		remaining := time.Until(end)
		if remaining <= 0 {
			return true
		}
		sleep := quantum
		if remaining < sleep {
			sleep = remaining
		}
		time.Sleep(sleep)
	}
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
	var hbValue float64
	if l.Mode == string(linkfault.ModeHeartbeatPause) {
		hbValue, _ = r.store.Get(linkfault.HeartbeatKey)
	}
	delay := time.Duration(l.DelayMS) * time.Millisecond
	return linkfault.Apply(r.iecTarget, r.modbusTarget, linkfault.Protocol(l.Protocol), linkfault.Mode(l.Mode), delay, hbValue)
}
