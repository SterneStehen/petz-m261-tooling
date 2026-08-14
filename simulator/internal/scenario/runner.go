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
// Task 7 item 5): the shared clock (which must be a *clock.Fake — this
// package exists specifically for the fake-clock-controlled mode, see
// controlapi's doc comment) is the only notion of time anywhere in this
// file; ticks physics.Runner one stepInterval at a time on the way to
// each step's deadline, never a bigger jump (see advanceTo); Speed from
// the loaded Scenario's ClockSpec is not consulted anywhere in this
// file — every run, at any declared speed, drives the identical sequence
// of stepInterval-sized Ticks with no artificial real-time delay between
// them, which is what makes "accelerated vs normal execution produces
// the same final Store state" true unconditionally, by construction,
// rather than something that has to be carefully arranged. See
// scenarios/README.md (or the package doc comment) if a future change
// ever wants Speed to introduce real-time pacing for a live demo — that
// would be additive, not a change to this determinism guarantee.
type Runner struct {
	mu sync.Mutex

	scenario     *Scenario
	cursor       int
	running      bool
	lastErr      error
	startInstant time.Time
	haveStart    bool

	// doneCh is non-nil exactly while a run() goroutine is alive, and is
	// closed by run() itself right before it returns. Stop (and
	// ResetPlayback, which calls it) blocks on this channel so that "Stop
	// returned" really means "no step is executing and no Tick is in
	// flight", not just "we asked it to stop" — required for Task 7,
	// item 7's reset atomicity: a caller doing Reset must be able to touch
	// Store/Processor/physics.Runner state immediately after Stop returns
	// without a straggler goroutine still mutating any of it.
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
func (r *Runner) Load(s *Scenario) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.running {
		return ErrLoadWhileRunning
	}
	r.scenario = s
	r.cursor = 0
	r.lastErr = nil
	r.haveStart = false
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
func (r *Runner) Start() error {
	r.mu.Lock()
	if r.scenario == nil {
		r.mu.Unlock()
		return ErrNoScenarioLoaded
	}
	if r.running {
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

	go r.run(done)
	return nil
}

// Stop halts execution and blocks until the run() goroutine has actually
// exited — not merely until it's been asked to — so that no step
// (write/fault/expect/link) and no physics Tick can still be in flight
// once Stop returns. The cursor is preserved, so a later Start resumes
// rather than restarts. Idempotent: stopping an already-stopped Runner
// is a no-op that returns immediately.
func (r *Runner) Stop() {
	r.mu.Lock()
	running := r.running
	done := r.doneCh
	r.running = false
	r.mu.Unlock()
	if !running || done == nil {
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

func (r *Runner) run(done chan struct{}) {
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
		r.mu.Unlock()

		if !r.advanceTo(startInstant.Add(step.At)) {
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
// checking for Stop() between every increment. Returns false if Stop()
// interrupted it before reaching deadline.
func (r *Runner) advanceTo(deadline time.Time) bool {
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
		next := now.Add(r.stepInterval)
		if next.After(deadline) {
			next = deadline
		}
		r.clk.Set(next)
		r.physicsRunner.Tick()
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
