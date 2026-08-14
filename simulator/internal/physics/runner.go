package physics

import (
	"errors"
	"fmt"
	"math"
	"sync"
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

	gate *appgate.Gate // see SetGate

	// driveMu is held for the *entire duration* of one logical clock-
	// driving operation — a FastForward/PacedFastForward call, or an
	// active scenario run (Start to Stop/completion) — never just
	// per-tick. This is what makes two big operations mutually exclusive
	// instead of silently interleaving or racing.
	//
	// Reviewed regression this fixes: the previous version only made
	// each individual TickOnce atomic, not the *sequence* of ticks one
	// logical FastForward/scenario run makes. Two concurrent
	// FastForward(1s, ...) calls each read fc.Now() and computed their
	// own target independently — if both read the clock before either
	// had ticked, both computed the *same* target, and whichever ticked
	// there first made the other's own loop condition already false,
	// silently discarding its caller's requested advance (measured: 8
	// concurrent +1s advances produced ~1.07s of total movement, not
	// 8s). Likewise a Reset could land between two TickOnce calls inside
	// one FastForward, because each individual TickOnce only held
	// gate.Op for its own single tick, not for the whole loop.
	//
	// PacedRun deliberately does *not* hold driveMu across ticks — each
	// of its own ticks is independently attempted (TryLock, do one tick,
	// unlock) so a scenario or an advance can always preempt it between
	// ticks rather than PacedRun blocking them.
	driveMu sync.Mutex
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
	activePower, reactivePower := r.commands.ResolvePower(
		r.clock.Now(), state.MaxChargeableKW, state.MaxDischargeableKW, state.SoCPercent,
		state.ChargeProhibited, state.DischargeProhibited,
	)

	// Energy Storage Meter Power Direction (§4.4/Task 5 item 7): read every
	// step, not just once at startup, since it's a live setpoint a client
	// can change at any time.
	direction, _ := r.store.Get(m261points.PointKey{Device: "EMS", Slug: "energy_storage_meter_power_direction"})
	r.engine.SetMeterDirectionInverted(direction != 0)

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
// *not* build on this for their own multi-tick loops — see driveMu's
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
// its whole run from Start to Stop/completion — hold the same exclusive
// ownership FastForward/PacedFastForward give themselves internally for
// their own duration (driveMu's own doc comment has the full rationale).
// TryAcquireDrive never blocks: it returns false immediately if
// something else already owns driving the clock, so the caller can fail
// its own request clearly (e.g. controlapi mapping this to 409) instead
// of silently queuing behind an unrelated operation of unknown duration.
func (r *Runner) TryAcquireDrive() bool { return r.driveMu.TryLock() }
func (r *Runner) ReleaseDrive()         { r.driveMu.Unlock() }

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
// Each tick is independently attempted (TryAcquireDrive, do one tick,
// ReleaseDrive) rather than holding driveMu across ticks — a scenario or
// a manual advance can always preempt PacedRun between its ticks; a busy
// driveMu just means this particular tick is skipped, not that PacedRun
// blocks anything.
//
// A non-positive or non-finite stepInterval/speed is a no-op rather than
// a panic or a runaway ticker — main.go validates both upfront and fails
// fast before starting anything, but PacedRun doesn't assume every
// caller does that.
func (r *Runner) PacedRun(stepInterval time.Duration, speed float64, stop <-chan struct{}) {
	if stepInterval <= 0 || !validSpeed(speed) {
		return
	}
	realInterval := time.Duration(float64(stepInterval) / speed)
	if realInterval <= 0 {
		realInterval = time.Nanosecond // extreme speed: tick as fast as the ticker/scheduler allow, never zero/negative
	}
	ticker := time.NewTicker(realInterval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			if !r.TryAcquireDrive() {
				continue // a scenario or a manual POST /clock/advance owns the clock right now
			}
			r.TickOnce(stepInterval) //nolint:errcheck // this Runner's own clock is always a *clock.Fake when PacedRun is used; see main.go
			r.ReleaseDrive()
		}
	}
}

func validSpeed(speed float64) bool {
	return speed > 0 && !math.IsNaN(speed) && !math.IsInf(speed, 0)
}

// Rebase sets the dt baseline for the next Tick to now, without touching
// the running Engine at all — unlike Reset, which also replaces the
// Engine. Used when something external moved the shared clock by a large
// jump (Task 7's scenario engine setting the clock to a scenario's
// declared clock.start) and the very next Tick must compute a small, sane
// dt from that new baseline instead of one spanning however long it's
// been since the previous Tick. Only called from within
// controlapi.Server.doReset's own exclusive gate section (see appgate),
// or by the scenario engine while it already holds driveMu (via
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
// per tick: returns ErrClockBusy immediately (never blocks) if a
// scenario or another FastForward/PacedFastForward is already driving
// the clock, and blocks Reset (which needs gate.Exclusive) until this
// entire call — not just its next tick — has finished. See driveMu's own
// doc comment for the concurrent-advance/advance-vs-reset regressions
// this closes.
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
// scenario engine, which acquires driveMu once for its whole run (Start
// to Stop/completion, via TryAcquireDrive) and calls this once per step
// instead of PacedFastForward, which would otherwise try to reacquire
// the same driveMu this goroutine already holds: sync.Mutex isn't
// reentrant, so that would either deadlock the goroutine against itself
// or (via TryLock) incorrectly report ErrClockBusy against its own
// caller. See driveMu's own doc comment for why the *whole* run, not
// just each individual advanceTo call, needs to be one held operation.
func (r *Runner) PacedFastForwardLocked(total, stepInterval time.Duration, speed float64, stop <-chan struct{}) (completed bool, err error) {
	if !validSpeed(speed) {
		return false, fmt.Errorf("physics: PacedFastForwardLocked speed must be positive and finite, got %v", speed)
	}
	return r.fastForwardLocked(total, stepInterval, speed, stop)
}

// fastForward acquires driveMu for FastForward/PacedFastForward's own
// single-call duration, then delegates to fastForwardLocked.
func (r *Runner) fastForward(total, stepInterval time.Duration, speed float64, stop <-chan struct{}) (completed bool, err error) {
	if !r.TryAcquireDrive() {
		return false, ErrClockBusy
	}
	defer r.ReleaseDrive()
	return r.fastForwardLocked(total, stepInterval, speed, stop)
}

// fastForwardLocked is FastForward/PacedFastForward/PacedFastForwardLocked's
// shared implementation, assuming the caller already holds driveMu (either
// fastForward/PacedFastForward itself, for their own call's duration, or
// the scenario engine, across its whole run — see PacedFastForwardLocked's
// doc comment). speed == 0 means unpaced (FastForward's case); stop == nil
// means not interruptible (FastForward's case, and also true whenever a
// paced caller doesn't need interruption).
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
			pace := time.Duration(float64(next) / speed)
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
	r.store.Set(m261points.PointKey{Device: device, Slug: slug}, value)
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
}
