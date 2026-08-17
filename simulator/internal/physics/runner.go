package physics

import (
	"errors"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/SterneStehen/petz-m261-tooling/gen/go/m261points"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/appgate"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/clock"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/commands"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/store"
)

// ErrClockBusy is returned by FastForward/PacedFastForward, and by
// TryAcquireDrive's caller convention, when something else (another
// FastForward/PacedFastForward call, or an active scenario run) already
// owns driving the clock — controlapi maps this to 409, not 500 or 400:
// it's a real, expected conflict between two legitimate requests, not a
// malformed one.
var ErrClockBusy = errors.New("physics: clock is already being driven by another operation")

// Runner is the glue between an Engine and the shared store, added per
// code review: without it, m261sim served static zero values — nothing
// ever called Engine.Step, so the physics model had no effect on what
// IEC-104/Modbus TCP actually reported.
//
// Each Tick asks commands (Task 6) for the active/reactive power to
// dispatch this step — mode arbitration (Manual/Auto Strategy/Remote),
// Strategy Period schedule execution, EMS-level limits, watchdog, and
// Power On/Off/Trip gating all happen there, against the engine's own
// current BMS headroom (State().MaxChargeableKW/MaxDischargeableKW) —
// then steps the engine with that resolved power and writes the result to
// every documented catalog point it corresponds to. Engine.Step itself
// remains responsible only for clipping to what's physically possible
// right now (SoC headroom, thermal derating), per its own doc comment.
type Runner struct {
	mu sync.Mutex // guards engine and last — see Tick and Reset

	engine   *Engine
	store    *store.Store
	clock    clock.Clock
	commands *commands.Processor
	last     time.Time
	writes   []store.KeyValue // scratch buffer, protected by mu

	gate *appgate.Gate // see SetGate

	// ownerMu is claimed by TryAcquireDrive for the *entire duration* of
	// one external clock-driving operation — a FastForward/
	// PacedFastForward call, or an active scenario run (Start to Stop/
	// completion) — never just per-tick. Mutual exclusion between two
	// such *external* operations is the only source of ErrClockBusy.
	//
	// PacedRun (the background live pacer) never touches ownerMu at all
	// — third-review-round regression this fixes: the previous design
	// had PacedRun's own per-tick attempts contend for the *same* lock
	// external drivers used, and at any nontrivial speed those ticks are
	// frequent enough that a genuinely valid external request (POST
	// /clock/advance, POST /scenario/start) could repeatedly collide
	// with an in-flight pacer tick and be rejected with ErrClockBusy even
	// though nothing else was actually driving the clock (reproduced
	// black-box: 20/20 advances failed at speed=1000000; 1 in 500 failed
	// even at speed=1). See pauseRequested and tickMu below for how the
	// pacer now yields to an external driver instead of contending with
	// it — fail-fast ErrClockBusy is only correct between two external
	// drivers of unknown, possibly long duration; the background pacer's
	// own tick is always brief (a bare Advance+Engine.Step, never a
	// sleep) and should never be the reason a real request fails.
	//
	// Underlying regression ownerMu (still) fixes, unchanged from the
	// first review round: two concurrent FastForward(1s, ...) calls each
	// read fc.Now() and computed their own target independently — if
	// both read the clock before either had ticked, both computed the
	// *same* target, and whichever ticked there first made the other's
	// own loop condition already false, silently discarding its caller's
	// requested advance. Likewise a Reset could land between two
	// TickOnce calls inside one FastForward, because each individual
	// TickOnce only held gate.Op for its own single tick, not for the
	// whole loop.
	ownerMu sync.Mutex

	// pauseRequested (read/written only via sync/atomic — never under
	// mu/tickMu) is incremented by TryAcquireDrive before it waits on
	// tickMu, and decremented by ReleaseDrive. PacedRun's own tick loop
	// checks it before every tick attempt and skips (does not even try
	// tickMu) once it's positive — this is purely a cooperative signal,
	// not itself a source of exclusion (tickMu still provides that): its
	// only job is letting the pacer back off *before* contending, instead
	// of the external driver having to repeatedly retry a TryLock against
	// a pacer that keeps re-acquiring tickMu on every tick interval.
	pauseRequested int32

	// tickMu is the actual per-tick execution lock, and the only lock
	// PacedRun's own ticks ever touch. Each of PacedRun's ticks
	// independently TryLocks it (skip this tick if busy — an external
	// driver already ticking always wins; this never produces
	// ErrClockBusy, since PacedRun has no caller to report it to and
	// simply tries again next interval). TryAcquireDrive, once it has
	// ownerMu and has signaled pauseRequested, blocks on tickMu with a
	// real Lock (not TryLock) — this wait is bounded by at most one
	// already-in-flight pacer tick (never indefinite, and never a sleep),
	// since pauseRequested guarantees the pacer won't start another one.
	tickMu sync.Mutex
}

// SetGate wires the process-wide reset-atomicity gate (see package
// appgate) — every Tick/TickOnce becomes a shared (Op) operation against
// it once set, so a controlapi.Server.doReset's exclusive section can
// never interleave with one. nil (the zero value, if SetGate is never
// called) disables gating entirely — every package test that doesn't
// care about cross-component reset atomicity keeps working unchanged.
func (r *Runner) SetGate(g *appgate.Gate) { r.gate = g }

func (r *Runner) opDone() func() {
	if r.gate == nil {
		return func() {}
	}
	return r.gate.Op()
}

// NewRunner builds a Runner and immediately publishes the engine's
// current (constructed) state to the store — a client connecting before
// the first Tick fires (the default step is 1s, but is configurable, so
// this window is not always negligible) must see the configured initial
// SoC/voltage/online status, not zero values nothing has written yet.
// clk.Now() at construction time is the baseline for the first Tick's dt.
// cmds must be non-nil — commands.NewProcessor(st, clk, ...) with the same
// store and clock this Runner uses.
func NewRunner(engine *Engine, st *store.Store, clk clock.Clock, cmds *commands.Processor) *Runner {
	r := &Runner{engine: engine, store: st, clock: clk, commands: cmds, last: clk.Now()}
	r.writeState()
	return r
}

// Tick advances the model by the time elapsed (per the injected clock,
// never time.Now() directly — AGENT-TASK §1.5) since the last Tick, or
// since NewRunner on the first call, and writes the result into the
// store. A non-positive elapsed duration (clock hasn't moved, or moved
// backwards) is a no-op rather than stepping with zero/negative dt.
//
// Locked against Reset (Task 7 item 7): a concurrent Reset must never
// interleave with a Tick in progress — either the whole Tick completes
// against the pre-reset engine, or Reset completes first and this Tick
// runs against the freshly reset one, never a mix of both.
func (r *Runner) Tick() {
	done := r.opDone()
	defer done()
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.clock.Now()
	dt := now.Sub(r.last)
	r.last = now
	if dt <= 0 {
		return
	}
	r.step(dt)
}

// step assumes r.mu is already held (Tick's caller, or Reset's own
// writeState below via NewRunner's construction-time call pattern).
func (r *Runner) step(dt time.Duration) {
	state := r.engine.State() // this tick's starting BMS headroom/SoC, before Step
	activePower, reactivePower, meterDirectionInverted := r.commands.ResolveDispatch(
		r.clock.Now(), state.MaxChargeableKW, state.MaxDischargeableKW, state.SoCPercent,
		state.ChargeProhibited, state.DischargeProhibited,
	)
	// Fifth-review-round fix: Energy Storage Meter Power Direction (§4.4/
	// Task 5 item 7) used to be read here, separately, via its own
	// unlocked store.Store.Get call *after* ResolvePower already
	// returned — a single legal FC16 request changing both Set Active
	// Power and this point together could land in the gap between the
	// two reads, so this tick could apply the *old* power together with
	// the *new* direction (or vice versa), a combination the client never
	// actually requested. ResolveDispatch now reads both together, under
	// one lock, as part of the same tick snapshot — see its own doc
	// comment.
	r.engine.SetMeterDirectionInverted(meterDirectionInverted)

	r.engine.Step(dt, activePower, reactivePower)
	r.writeState()
}

// Reset replaces the running Engine with a fresh one (Task 7 item 7:
// physics returns to its initial SoC/temperature/energy/heartbeat and
// the *same* RNG seed, not a new random one — engine must already be
// physics.New(sameParams, sameInitialSOCPercent) the caller originally
// used, so reset reproduces the same simulated future a fresh process
// start would) and rebases the dt baseline to now, so the next Tick
// computes a sane (small, non-negative) dt instead of one spanning
// however long the simulator had been running before the reset.
// Mutually exclusive with Tick via the same lock — see Tick's doc
// comment.
func (r *Runner) Reset(engine *Engine, now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.engine = engine
	r.last = now
	r.writeState()
}

// TickOnce advances the shared *clock.Fake by exactly d and steps the
// engine once, as a single atomic operation (one gate.Op, one r.mu
// critical section covering both the clock advance and the step) — a
// standalone single-tick primitive for direct/test use and for
// PacedRun's own per-tick attempts. FastForward/PacedFastForward do
// *not* build on this for their own multi-tick loops — see ownerMu's
// doc comment for why one gate.Op per tick isn't enough to make a whole
// multi-tick operation atomic, only each individual tick.
//
// Requires the Clock this Runner was built with to be a *clock.Fake —
// advancing a real wall clock by a caller-chosen amount has no meaning,
// and returns an error rather than silently doing nothing.
func (r *Runner) TickOnce(d time.Duration) error {
	fc, ok := r.clock.(*clock.Fake)
	if !ok {
		return fmt.Errorf("physics: TickOnce requires a *clock.Fake clock, got %T", r.clock)
	}
	if d < 0 {
		return fmt.Errorf("physics: TickOnce d must be non-negative, got %s", d)
	}
	done := r.opDone()
	defer done()
	r.mu.Lock()
	defer r.mu.Unlock()
	fc.Advance(d)
	now := fc.Now()
	dt := now.Sub(r.last)
	r.last = now
	if dt > 0 {
		r.step(dt)
	}
	return nil
}

// TryAcquireDrive/ReleaseDrive let a caller that needs to own the clock
// across more than one tick — currently only the scenario engine, for
// its whole run from Start to Stop/completion, and FastForward/
// PacedFastForward for their own call's duration — claim the same
// exclusive external-driver ownership (ownerMu's own doc comment has the
// full rationale). TryAcquireDrive never blocks *against another
// external driver*: it returns false immediately if one already owns
// driving the clock, so the caller can fail its own request clearly
// (e.g. controlapi mapping this to 409) instead of silently queuing
// behind an unrelated operation of unknown duration. It *does* briefly
// block against the background live pacer (PacedRun) — see tickMu's doc
// comment — but that wait is bounded by at most one already-in-flight
// tick, never indefinite, so it's not something callers need to guard
// against separately.
func (r *Runner) TryAcquireDrive() bool {
	if !r.ownerMu.TryLock() {
		return false
	}
	atomic.AddInt32(&r.pauseRequested, 1)
	r.tickMu.Lock()
	return true
}

func (r *Runner) ReleaseDrive() {
	r.tickMu.Unlock()
	atomic.AddInt32(&r.pauseRequested, -1)
	r.ownerMu.Unlock()
}

// PacedRun blocks, advancing the shared clock (which must be a
// *clock.Fake) in stepInterval-sized ticks, spaced stepInterval/speed
// apart in real time, until stop is closed — the "live"/default-running
// mode: an operator watching the simulator with no scenario loaded and
// no manual POST /clock/advance in flight sees model time advance at
// speed x real time (speed=1 is real-time pace). Deliberately ticks in
// fixed stepInterval increments regardless of speed, the same tick
// granularity FastForward/PacedFastForward use — not one coarse
// stepInterval*speed jump per real-time tick (a reviewed gap in an
// earlier version): a single Step() call with a speed-scaled-up dt is
// not the same physics as many stepInterval-sized ones, and would
// silently skip per-tick point updates (heartbeat included) that a
// client polling in real time between ticks would otherwise see.
//
// Each tick is independently attempted directly against tickMu (TryLock,
// do one tick, unlock) — deliberately *never* through TryAcquireDrive/
// ownerMu, so PacedRun itself can never be the reason an external
// driver's own TryAcquireDrive fails (see ownerMu/pauseRequested/tickMu's
// own doc comments for the full rationale and the regression this fixes,
// found in the second review round: PacedRun contending for the very
// same lock external callers used made a valid external request fail
// with ErrClockBusy whenever it happened to land on top of one of
// PacedRun's own frequent ticks). A busy tickMu just means this
// particular tick is skipped, not that PacedRun blocks anything.
//
// A non-positive or non-finite stepInterval/speed is a no-op rather than
// a panic or a runaway ticker — main.go validates both upfront and fails
// fast before starting anything, but PacedRun doesn't assume every
// caller does that. An extreme-but-technically-valid speed (e.g. a tiny
// positive value close to zero, or — fourth review round — an
// enormous one) that would make stepInterval/speed over/underflow a
// time.Duration is also a no-op — PacedRun itself has no caller to
// report the failure through (main.go's own -speed validation is
// expected to reject this before ever calling PacedRun; see
// CheckedPace), and running at a runaway cadence instead (a bare,
// unchecked float64->Duration conversion is implementation-defined once
// out of int64 range, not a panic; a duration that underflows to zero
// falls back to an uncontrolled as-fast-as-possible ticker either way)
// would silently misrepresent the requested speed rather than visibly
// doing nothing.
func (r *Runner) PacedRun(stepInterval time.Duration, speed float64, stop <-chan struct{}) {
	if stepInterval <= 0 || !validSpeed(speed) {
		return
	}
	realInterval, err := CheckedPace(stepInterval, speed)
	if err != nil {
		return // speed is too extreme (either direction) for a meaningful real-time pace at this stepInterval -- see CheckedPace
	}
	ticker := time.NewTicker(realInterval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			if atomic.LoadInt32(&r.pauseRequested) > 0 {
				continue // an external driver is taking over -- yield instead of contending for tickMu
			}
			if !r.tickMu.TryLock() {
				continue // an external driver already owns ticking (a narrow race against the check above, not the common case)
			}
			r.TickOnce(stepInterval) //nolint:errcheck // this Runner's own clock is always a *clock.Fake when PacedRun is used; see main.go
			r.tickMu.Unlock()
		}
	}
}

func validSpeed(speed float64) bool {
	return speed > 0 && !math.IsNaN(speed) && !math.IsInf(speed, 0)
}

// CheckedPace computes d/speed as a time.Duration, or an error instead
// of an implementation-defined result if the quotient (in nanoseconds)
// isn't finite or doesn't fit in a time.Duration's int64 nanosecond
// range. A bare time.Duration(float64(d)/speed) conversion is undefined
// by the Go spec once the float is out of range — not a panic, just an
// unspecified int64 value — which is exactly how a single individually
// "valid" (positive, finite) but extreme speed like 1e-300 used to
// silently turn a 1s stepInterval/speed into a ~1ns real interval
// instead of the "almost stopped" pace the caller actually asked for
// (reproduced: heartbeat reached 24,478 within seconds of real time).
//
// Also rejects the opposite extreme: a speed so *large* (still finite,
// still individually "valid" by validSpeed's own check) that d/speed
// underflows to a duration of zero once truncated to whole nanoseconds.
// Fourth-review-round finding: the previous version silently accepted
// this and let PacedRun fall back to a fixed 1ns real interval — a
// runaway ticker firing as fast as the OS scheduler allows, exactly the
// "destructive" outcome the tiny-speed check above already guards
// against, just from the other direction. d == 0 is exempt (a
// zero-length remaining chunk legitimately paces to nothing, not an
// extreme speed).
func CheckedPace(d time.Duration, speed float64) (time.Duration, error) {
	ns := float64(d) / speed
	if math.IsNaN(ns) || math.IsInf(ns, 0) || ns > float64(math.MaxInt64) || ns < float64(math.MinInt64) {
		return 0, fmt.Errorf("physics: %s / %v does not fit in a representable duration", d, speed)
	}
	pace := time.Duration(ns)
	if d > 0 && pace <= 0 {
		return 0, fmt.Errorf("physics: %s / %v underflows to a non-positive duration -- speed is too large relative to this interval for a meaningful real-time pace", d, speed)
	}
	return pace, nil
}

// ValidatePacing reports whether every real-time pace fastForwardLocked
// would ever need to compute while advancing total in stepInterval-sized
// chunks (the exact chunking FastForward/PacedFastForward/
// PacedFastForwardLocked themselves use — see fastForwardLocked's own
// next/remaining loop) is representable, without advancing, sleeping, or
// touching any clock at all. Fifth-review-round addition: scenario.Runner.
// Load calls this against the *real* stepInterval it will actually pace
// with and every real timing chunk between two scenario timestamps,
// before installing the scenario — so a speed that would later fail
// CheckedPace mid-run (previously only discoverable once
// PacedFastForwardLocked itself reached the offending chunk, partway
// through an already-running scenario) is rejected up front instead.
//
// total <= 0 is a no-op (nil) — matches fastForwardLocked's own "already
// at or past this deadline, nothing to tick" early return; a scenario
// step whose at: repeats the previous one, or falls exactly on the
// current clock position, contributes no chunk to check. stepInterval
// must be positive, matching fastForwardLocked's own requirement.
//
// Every "next" value fastForwardLocked's loop can ever produce for a
// given (total, stepInterval) pair is one of exactly two distinct
// durations: stepInterval itself, repeated as many whole times as fit in
// total, and — only if total isn't an exact multiple of stepInterval — one
// final, strictly smaller remainder (total mod stepInterval) for the last
// partial chunk. Checking those two values once each, rather than
// replaying fastForwardLocked's whole loop (which could mean millions of
// iterations for a long scenario at a fine stepInterval), covers the
// identical set of pace computations the real run will ever make.
func ValidatePacing(total, stepInterval time.Duration, speed float64) error {
	if total <= 0 {
		return nil
	}
	if stepInterval <= 0 {
		return fmt.Errorf("physics: ValidatePacing stepInterval must be positive, got %s", stepInterval)
	}
	if total >= stepInterval {
		if _, err := CheckedPace(stepInterval, speed); err != nil {
			return fmt.Errorf("physics: stepInterval chunk %s: %w", stepInterval, err)
		}
	}
	if remainder := total % stepInterval; remainder > 0 {
		if _, err := CheckedPace(remainder, speed); err != nil {
			return fmt.Errorf("physics: final remainder chunk %s: %w", remainder, err)
		}
	}
	return nil
}

// Rebase sets the dt baseline for the next Tick to now, without touching
// the running Engine at all — unlike Reset, which also replaces the
// Engine. Used when something external moved the shared clock by a large
// jump (Task 7's scenario engine setting the clock to a scenario's
// declared clock.start) and the very next Tick must compute a small, sane
// dt from that new baseline instead of one spanning however long it's
// been since the previous Tick. Only called from within
// controlapi.Server.doReset's own exclusive gate section (see appgate),
// or by the scenario engine while it already holds ownerMu/tickMu (via
// TryAcquireDrive) — never independently gated/drive-locked itself,
// unlike TickOnce/FastForward.
func (r *Runner) Rebase(now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.last = now
}

// FastForward advances the shared clock from its current value up to
// current+total, ticking once per stepInterval-sized increment along the
// way (the final increment may be shorter) rather than one coarse jump —
// Task 7 item 8: a jump big enough to silently skip heartbeat increments
// would fail "no gaps in the model-time heartbeat sequence" by
// construction, no matter how the caller phrases the request.
//
// The whole call — target computation through the last tick — runs under
// one TryAcquireDrive/ReleaseDrive pair and one gate.Op/r.mu hold, not
// per tick: returns ErrClockBusy immediately (never blocks, other than
// the bounded wait TryAcquireDrive may do against an in-flight pacer
// tick) if a scenario or another FastForward/PacedFastForward is already
// driving the clock, and blocks Reset (which needs gate.Exclusive) until
// this entire call — not just its next tick — has finished. See
// ownerMu's own doc comment for the concurrent-advance/advance-vs-reset/
// pacer-contention regressions this closes.
func (r *Runner) FastForward(total, stepInterval time.Duration) error {
	_, err := r.fastForward(total, stepInterval, 0, nil)
	return err
}

// PacedFastForward is FastForward's real-time-paced counterpart —
// scenario.Runner's own primitive for a loaded Scenario's clock.speed
// (AGENT-TASK.md, Task 7 item 5), moved here rather than left in package
// scenario so that business logic never itself calls time.Now/time.Sleep
// (AGENT-TASK §1.5): this is the same "outermost driving loop" exception
// PacedRun's own real ticker already relies on, not a carve-out invented
// for this method. Identical to FastForward — one drive/gate/engine lock
// held for the whole call, ErrClockBusy if already owned — except each
// tick is additionally paced stepInterval/speed apart in real time
// (interruptible via stop, returning completed=false if stop fires
// before total is reached). Never affects the resulting physics state:
// the sequence of dt values applied to the engine is identical regardless
// of speed, only how far apart in real time each one lands — so a
// scenario run at speed=1 and the same scenario at any faster speed
// still produce the same final Store state.
func (r *Runner) PacedFastForward(total, stepInterval time.Duration, speed float64, stop <-chan struct{}) (completed bool, err error) {
	if !validSpeed(speed) {
		return false, fmt.Errorf("physics: PacedFastForward speed must be positive and finite, got %v", speed)
	}
	if !r.TryAcquireDrive() {
		return false, ErrClockBusy
	}
	defer r.ReleaseDrive()
	return r.fastForwardLocked(total, stepInterval, speed, stop)
}

// PacedFastForwardLocked is PacedFastForward's variant for a caller that
// already holds this Runner's drive lock for a larger enclosing
// operation spanning more than this one call — currently only the
// scenario engine, which acquires ownerMu/tickMu once for its whole run
// (Start to Stop/completion, via TryAcquireDrive) and calls this once
// per step instead of PacedFastForward, which would otherwise try to
// reacquire the same locks this goroutine already holds: sync.Mutex
// isn't reentrant, so that would either deadlock the goroutine against
// itself or (via TryLock) incorrectly report ErrClockBusy against its
// own caller. See ownerMu's own doc comment for why the *whole* run, not
// just each individual advanceTo call, needs to be one held operation.
func (r *Runner) PacedFastForwardLocked(total, stepInterval time.Duration, speed float64, stop <-chan struct{}) (completed bool, err error) {
	if !validSpeed(speed) {
		return false, fmt.Errorf("physics: PacedFastForwardLocked speed must be positive and finite, got %v", speed)
	}
	return r.fastForwardLocked(total, stepInterval, speed, stop)
}

// fastForward acquires ownerMu/tickMu (via TryAcquireDrive) for
// FastForward/PacedFastForward's own single-call duration, then
// delegates to fastForwardLocked.
func (r *Runner) fastForward(total, stepInterval time.Duration, speed float64, stop <-chan struct{}) (completed bool, err error) {
	if !r.TryAcquireDrive() {
		return false, ErrClockBusy
	}
	defer r.ReleaseDrive()
	return r.fastForwardLocked(total, stepInterval, speed, stop)
}

// fastForwardLocked is FastForward/PacedFastForward/PacedFastForwardLocked's
// shared implementation, assuming the caller already holds tickMu (via
// TryAcquireDrive — either fastForward/PacedFastForward itself, for their
// own call's duration, or the scenario engine, across its whole run —
// see PacedFastForwardLocked's doc comment). speed == 0 means unpaced
// (FastForward's case); stop == nil means not interruptible (FastForward's
// case, and also true whenever a paced caller doesn't need interruption).
func (r *Runner) fastForwardLocked(total, stepInterval time.Duration, speed float64, stop <-chan struct{}) (completed bool, err error) {
	if total < 0 {
		return false, fmt.Errorf("physics: FastForward total must be non-negative, got %s", total)
	}
	if stepInterval <= 0 {
		return false, fmt.Errorf("physics: FastForward stepInterval must be positive, got %s", stepInterval)
	}
	fc, ok := r.clock.(*clock.Fake)
	if !ok {
		return false, fmt.Errorf("physics: FastForward requires a *clock.Fake clock, got %T", r.clock)
	}

	done := r.opDone() // held for the *whole* operation — Reset can't interleave with any single tick of it
	defer done()
	r.mu.Lock()
	defer r.mu.Unlock()

	target := fc.Now().Add(total)
	for fc.Now().Before(target) {
		if stop != nil {
			select {
			case <-stop:
				return false, nil
			default:
			}
		}
		next := stepInterval
		if remaining := target.Sub(fc.Now()); remaining < next {
			next = remaining
		}
		if speed > 0 {
			pace, err := CheckedPace(next, speed)
			if err != nil {
				return false, err
			}
			if pace > 0 {
				if stop != nil {
					timer := time.NewTimer(pace)
					select {
					case <-timer.C:
					case <-stop:
						timer.Stop()
						return false, nil
					}
				} else {
					time.Sleep(pace)
				}
			}
		}
		fc.Advance(next)
		now := fc.Now()
		dt := now.Sub(r.last)
		r.last = now
		if dt > 0 {
			r.step(dt)
		}
	}
	return true, nil
}

func (r *Runner) set(device, slug string, value float64) {
	r.writes = append(r.writes, store.KeyValue{Key: m261points.PointKey{Device: device, Slug: slug}, Value: value})
}

func boolToFloat(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

func cellVoltageSlug(n int) string     { return fmt.Sprintf("cell_voltage_%03d_mv", n) }
func cellTemperatureSlug(n int) string { return fmt.Sprintf("cell_temperature_%03d_c", n) }

// writeState maps physics.State onto the documented catalog points it
// corresponds to. Every slug here was verified against the real
// catalog/point_catalog.json before being used, not guessed from the
// point names in AGENT-TASK's prose.
func (r *Runner) writeState() {
	s := r.engine.State()
	r.writes = r.writes[:0]

	// EMS
	r.set("EMS", "desired_active_power_kw", s.RequestedPowerKW)
	r.set("EMS", "last_charge_discharge_power_kw", s.ActualPowerKW)
	r.set("EMS", "maximum_chargeable_power_kw", s.MaxChargeableKW)
	r.set("EMS", "maximum_dischargeable_power_kw", s.MaxDischargeableKW)
	r.set("EMS", "charge_prohibition_protection", boolToFloat(s.ChargeProhibited))
	r.set("EMS", "discharge_prohibition_protection", boolToFloat(s.DischargeProhibited))
	r.set("EMS", "ems_periodic_heartbeat_indicator", float64(s.HeartbeatCounter))
	r.set("EMS", "online_status", boolToFloat(s.Online))
	r.set("EMS", "pcs_total_active_power_kw", s.ActualPowerKW)
	r.set("EMS", "pcs_total_reactive_power_kvar", s.ReactivePowerKvar)
	r.set("EMS", "total_forward_energy_kwh", s.TotalForwardEnergyKWh)
	r.set("EMS", "total_reverse_energy_kwh", s.TotalReverseEnergyKWh)

	// BMS
	r.set("BMS", "soc", s.SoCPercent)
	r.set("BMS", "display_soc", s.SoCPercent)
	r.set("BMS", "battery_total_voltage_v", s.PackVoltageV)
	r.set("BMS", "average_temperature_c", s.BatteryTempC)
	if s.PackVoltageV != 0 {
		r.set("BMS", "total_current_a", s.ActualPowerKW*1000/s.PackVoltageV)
	}

	// BMS_CELLS: 240 voltages + 140 temperatures, §3.3.
	for i, v := range s.CellVoltagesMV {
		r.set("BMS_CELLS", cellVoltageSlug(i+1), v)
	}
	for i, c := range s.CellTemperaturesC {
		r.set("BMS_CELLS", cellTemperatureSlug(i+1), c)
	}

	// PCS
	r.set("PCS", "phase_a_voltage_v", s.PhaseVoltagesV[0])
	r.set("PCS", "phase_b_voltage_v", s.PhaseVoltagesV[1])
	r.set("PCS", "phase_c_voltage_v", s.PhaseVoltagesV[2])
	r.set("PCS", "phase_a_current_a", s.PhaseCurrentsA[0])
	r.set("PCS", "phase_b_current_a", s.PhaseCurrentsA[1])
	r.set("PCS", "phase_c_current_a", s.PhaseCurrentsA[2])
	r.set("PCS", "grid_frequency_hz", s.FrequencyHz)
	r.set("PCS", "total_power_factor", s.PowerFactor)
	r.set("PCS", "total_active_power_kw", s.ActualPowerKW)
	r.set("PCS", "total_reactive_power_kvar", s.ReactivePowerKvar)

	// PCS_METER ("Energy Storage Meter", device_addr/Unit ID 163) — a
	// separate physical meter from PCS's own internal monitoring, but
	// wired to read the same AC connection point, so it gets the same
	// electricals rather than being left static (code review: an entire
	// device reporting all-zero while EMS/PCS show real values is
	// contradictory, not merely incomplete). Per-phase figures assume the
	// same balanced-3-phase split used for PCS above.
	apparentKVA := math.Hypot(s.ActualPowerKW, s.ReactivePowerKvar)
	r.set("PCS_METER", "phase_a_voltage_v", s.PhaseVoltagesV[0])
	r.set("PCS_METER", "phase_b_voltage_v", s.PhaseVoltagesV[1])
	r.set("PCS_METER", "phase_c_voltage_v", s.PhaseVoltagesV[2])
	r.set("PCS_METER", "phase_a_current_a", s.PhaseCurrentsA[0])
	r.set("PCS_METER", "phase_b_current_a", s.PhaseCurrentsA[1])
	r.set("PCS_METER", "phase_c_current_a", s.PhaseCurrentsA[2])
	r.set("PCS_METER", "total_active_power_kw", s.ActualPowerKW)
	r.set("PCS_METER", "phase_a_active_power_kw", s.ActualPowerKW/3)
	r.set("PCS_METER", "phase_b_active_power_kw", s.ActualPowerKW/3)
	r.set("PCS_METER", "phase_c_active_power_kw", s.ActualPowerKW/3)
	r.set("PCS_METER", "total_reactive_power_kvar", s.ReactivePowerKvar)
	r.set("PCS_METER", "phase_a_reactive_power_kvar", s.ReactivePowerKvar/3)
	r.set("PCS_METER", "phase_b_reactive_power_kvar", s.ReactivePowerKvar/3)
	r.set("PCS_METER", "phase_c_reactive_power_kvar", s.ReactivePowerKvar/3)
	r.set("PCS_METER", "total_apparent_power_kva", apparentKVA)
	r.set("PCS_METER", "phase_a_apparent_power_kva", apparentKVA/3)
	r.set("PCS_METER", "phase_b_apparent_power_kva", apparentKVA/3)
	r.set("PCS_METER", "phase_c_apparent_power_kva", apparentKVA/3)
	r.set("PCS_METER", "total_power_factor", s.PowerFactor)
	r.set("PCS_METER", "phase_a_power_factor", s.PowerFactor)
	r.set("PCS_METER", "phase_b_power_factor", s.PowerFactor)
	r.set("PCS_METER", "phase_c_power_factor", s.PowerFactor)
	r.set("PCS_METER", "forward_active_total_energy_kwh", s.TotalForwardEnergyKWh)
	r.set("PCS_METER", "reverse_active_total_energy_kwh", s.TotalReverseEnergyKWh)
	r.set("PCS_METER", "phase_a_forward_active_energy_kwh", s.TotalForwardEnergyKWh/3)
	r.set("PCS_METER", "phase_b_forward_active_energy_kwh", s.TotalForwardEnergyKWh/3)
	r.set("PCS_METER", "phase_c_forward_active_energy_kwh", s.TotalForwardEnergyKWh/3)
	r.set("PCS_METER", "phase_a_reverse_active_energy_kwh", s.TotalReverseEnergyKWh/3)
	r.set("PCS_METER", "phase_b_reverse_active_energy_kwh", s.TotalReverseEnergyKWh/3)
	r.set("PCS_METER", "phase_c_reverse_active_energy_kwh", s.TotalReverseEnergyKWh/3)
	r.set("PCS_METER", "online_status", boolToFloat(s.Online))
	// A physics tick is one Store revision and one visible state change.
	// SetBatch keeps snapshots and UI subscribers from seeing a half-tick.
	if !r.store.SetBatch(r.writes) {
		panic("physics: writeState references a point absent from the generated catalog")
	}
}
