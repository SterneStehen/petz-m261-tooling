package physics

import (
	"fmt"
	"time"

	"github.com/SterneStehen/petz-m261-tooling/gen/go/m261points"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/clock"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/store"
)

// Runner is the minimal glue between an Engine and the shared store,
// added per code review: without it, m261sim served static zero values —
// nothing ever called Engine.Step, so the physics model had no effect on
// what IEC-104/Modbus TCP actually reported.
//
// Each Tick reads the raw EMS "Set Active Power"/"Set Reactive Power"
// setpoints, steps the engine, and writes the derived state to every
// documented catalog point it corresponds to. It does no mode
// arbitration, no Power On/Off gating, and no BMS-limit-vs-EMS-setpoint
// reconciliation beyond what Engine.Step itself already clips to what's
// physically possible — Task 6's commands package owns that layer and is
// expected to sit in front of this (validating/arbitrating the setpoint
// before it reaches the store, or calling Engine.Step directly with its
// own resolved power instead of going through Tick's raw setpoint read).
type Runner struct {
	engine *Engine
	store  *store.Store
	clock  clock.Clock
	last   time.Time
}

// NewRunner builds a Runner. clk.Now() at construction time is the
// baseline for the first Tick's dt.
func NewRunner(engine *Engine, st *store.Store, clk clock.Clock) *Runner {
	return &Runner{engine: engine, store: st, clock: clk, last: clk.Now()}
}

// Tick advances the model by the time elapsed (per the injected clock,
// never time.Now() directly — AGENT-TASK §1.5) since the last Tick, or
// since NewRunner on the first call, and writes the result into the
// store. A non-positive elapsed duration (clock hasn't moved, or moved
// backwards) is a no-op rather than stepping with zero/negative dt.
func (r *Runner) Tick() {
	now := r.clock.Now()
	dt := now.Sub(r.last)
	r.last = now
	if dt <= 0 {
		return
	}
	r.step(dt)
}

func (r *Runner) step(dt time.Duration) {
	requestedPower, _ := r.store.Get(m261points.PointKey{Device: "EMS", Slug: "set_active_power_kw"})
	requestedReactive, _ := r.store.Get(m261points.PointKey{Device: "EMS", Slug: "set_reactive_power_kvar"})
	r.engine.Step(dt, requestedPower, requestedReactive)
	r.writeState()
}

// Run blocks, calling Tick every stepInterval until stop is closed. This
// is the one place allowed to run on the real wall clock (via
// time.Ticker) even though Tick itself only ever consults the injected
// clock — it's the outermost driving loop, not business logic, matching
// how simulator/internal/clock.Real is meant to be used. Tests call Tick
// directly against a fake clock instead of calling Run.
func (r *Runner) Run(stepInterval time.Duration, stop <-chan struct{}) {
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
}
