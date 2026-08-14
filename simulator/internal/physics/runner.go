package physics

import (
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/SterneStehen/petz-m261-tooling/gen/go/m261points"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/clock"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/commands"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/store"
)

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

// Run blocks, calling Tick every stepInterval until stop is closed. This
// is the one place allowed to run on the real wall clock (via
// time.Ticker) even though Tick itself only ever consults the injected
// clock — it's the outermost driving loop, not business logic, matching
// how simulator/internal/clock.Real is meant to be used. Tests call Tick
// directly against a fake clock instead of calling Run.
//
// A non-positive stepInterval is a no-op rather than the panic
// time.NewTicker would raise — main.go validates this upfront and fails
// fast with a clear message before starting anything, but Run doesn't
// assume every caller does that.
func (r *Runner) Run(stepInterval time.Duration, stop <-chan struct{}) {
	if stepInterval <= 0 {
		return
	}
	ticker := time.NewTicker(stepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			r.Tick()
		}
	}
}

// Rebase sets the dt baseline for the next Tick to now, without touching
// the running Engine at all — unlike Reset, which also replaces the
// Engine. Used when something external moved the shared clock by a large
// jump (Task 7's scenario engine setting the clock to a scenario's
// declared clock.start) and the very next Tick must compute a small, sane
// dt from that new baseline instead of one spanning however long it's
// been since the previous Tick.
func (r *Runner) Rebase(now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.last = now
}

// FastForward advances the shared clock from its current value up to
// current+total, calling Tick once per stepInterval-sized increment
// along the way (the final increment may be shorter) rather than one
// coarse jump — Task 7 item 8: a jump big enough to silently skip
// heartbeat increments would fail "no gaps in the model-time heartbeat
// sequence" by construction, no matter how the caller phrases the
// request. Used directly by the 72-hour acceptance test and by Task 7's
// POST /clock/advance; the scenario engine (package scenario) drives its
// own, interruptible version of the same increment-then-Tick loop instead
// of calling this, so Stop() can take effect between increments — but
// the increment size and the "Tick once per increment, never a bigger
// jump" rule are identical either way.
//
// Requires the Clock this Runner was built with to be a *clock.Fake —
// fast-forwarding a real wall clock has no meaning, and returns an error
// rather than silently doing nothing.
func (r *Runner) FastForward(total, stepInterval time.Duration) error {
	fc, ok := r.clock.(*clock.Fake)
	if !ok {
		return fmt.Errorf("physics: FastForward requires a *clock.Fake clock, got %T", r.clock)
	}
	if total < 0 {
		return fmt.Errorf("physics: FastForward total must be non-negative, got %s", total)
	}
	if stepInterval <= 0 {
		return fmt.Errorf("physics: FastForward stepInterval must be positive, got %s", stepInterval)
	}
	target := fc.Now().Add(total)
	for fc.Now().Before(target) {
		next := fc.Now().Add(stepInterval)
		if next.After(target) {
			next = target
		}
		fc.Set(next)
		r.Tick()
	}
	return nil
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
