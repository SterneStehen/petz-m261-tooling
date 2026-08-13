package physics

import (
	"testing"
	"time"

	"github.com/SterneStehen/petz-m261-tooling/gen/go/m261points"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/clock"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/commands"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/store"
)

func get(t *testing.T, st *store.Store, device, slug string) float64 {
	t.Helper()
	v, ok := st.Get(m261points.PointKey{Device: device, Slug: slug})
	if !ok {
		t.Fatalf("point (%s, %s) does not exist in the catalog", device, slug)
	}
	return v
}

// newTestProcessor builds a commands.Processor on st/clk and immediately
// puts it in Remote mode (Set Operating Mode = 2). These tests pre-date
// Task 6's mode arbitration and are exercising the engine/store plumbing
// (does a requested power reach the store, does energy accumulate, and so
// on) — Remote mode is what makes "write Set Active Power" behave exactly
// like it did before commands.Processor existed. Mode arbitration itself
// (Manual/Auto Strategy/limits/watchdog/priority) has its own dedicated
// coverage in simulator/internal/commands.
func newTestProcessor(t *testing.T, st *store.Store, clk clock.Clock) *commands.Processor {
	t.Helper()
	p, err := commands.NewProcessor(st, clk, commands.DefaultConfig())
	if err != nil {
		t.Fatalf("commands.NewProcessor: %v", err)
	}
	if err := p.Write(m261points.PointKey{Device: "EMS", Slug: "set_operating_mode"}, 2); err != nil {
		t.Fatalf("Write(set_operating_mode, 2): %v", err)
	}
	return p
}

func TestTickDoesNothingWithoutElapsedTime(t *testing.T) {
	st := store.New()
	clk := clock.NewFake(time.Now())
	r := NewRunner(New(DefaultParams(), 50), st, clk, newTestProcessor(t, st, clk))
	r.Tick() // clock hasn't moved since NewRunner — must be a no-op
	if hb := get(t, st, "EMS", "ems_periodic_heartbeat_indicator"); hb != 0 {
		t.Errorf("heartbeat = %v after a zero-dt Tick, want 0 (no-op)", hb)
	}
}

func TestTickWritesSoCPowerAndHeartbeatToStore(t *testing.T) {
	st := store.New()
	clk := clock.NewFake(time.Now())
	cmds := newTestProcessor(t, st, clk)
	if err := cmds.Write(m261points.PointKey{Device: "EMS", Slug: "set_active_power_kw"}, -50); err != nil { // request charge
		t.Fatal(err)
	}
	r := NewRunner(New(DefaultParams(), 50), st, clk, cmds)

	clk.Advance(time.Second)
	r.Tick()

	if hb := get(t, st, "EMS", "ems_periodic_heartbeat_indicator"); hb != 1 {
		t.Errorf("heartbeat = %v after one Tick, want 1", hb)
	}
	if online := get(t, st, "EMS", "online_status"); online != 1 {
		t.Errorf("online_status = %v after one Tick, want 1", online)
	}
	soc := get(t, st, "BMS", "soc")
	if soc <= 50 {
		t.Errorf("BMS soc = %v after charging for 1s, want > 50 (starting SoC)", soc)
	}
	if v := get(t, st, "BMS", "battery_total_voltage_v"); v < 676 || v > 936 {
		t.Errorf("battery_total_voltage_v = %v, want within [676, 936]", v)
	}
	if actual := get(t, st, "EMS", "last_charge_discharge_power_kw"); actual >= 0 {
		t.Errorf("last_charge_discharge_power_kw = %v, want negative (charging, per the -50kW request)", actual)
	}
}

func TestTickWritesAllCellVoltagesAndTemperatures(t *testing.T) {
	st := store.New()
	clk := clock.NewFake(time.Now())
	r := NewRunner(New(DefaultParams(), 50), st, clk, newTestProcessor(t, st, clk))
	clk.Advance(time.Second)
	r.Tick()

	for i := 1; i <= 240; i++ {
		v := get(t, st, "BMS_CELLS", cellVoltageSlug(i))
		if v < 2600 || v > 3700 {
			t.Fatalf("cell voltage %d = %v mV, outside a plausible LFP range", i, v)
		}
	}
	for i := 1; i <= 140; i++ {
		c := get(t, st, "BMS_CELLS", cellTemperatureSlug(i))
		if c < -50 || c > 100 {
			t.Fatalf("cell temperature %d = %v °C, outside a plausible range", i, c)
		}
	}
}

func TestTickWritesPCSElectricals(t *testing.T) {
	st := store.New()
	clk := clock.NewFake(time.Now())
	cmds := newTestProcessor(t, st, clk)
	if err := cmds.Write(m261points.PointKey{Device: "EMS", Slug: "set_active_power_kw"}, 50); err != nil { // discharge
		t.Fatal(err)
	}
	r := NewRunner(New(DefaultParams(), 50), st, clk, cmds)
	clk.Advance(time.Second)
	r.Tick()

	if f := get(t, st, "PCS", "grid_frequency_hz"); f != 50 {
		t.Errorf("grid_frequency_hz = %v, want 50", f)
	}
	if i := get(t, st, "PCS", "phase_a_current_a"); i <= 0 {
		t.Errorf("phase_a_current_a = %v after a discharge request, want > 0", i)
	}
}

// TestTickWritesPCSMeterDevice is the code-review-requested fix: the
// dedicated "Energy Storage Meter" device (PCS_METER, Unit ID 163, §4.1)
// was left entirely static while EMS/PCS reported real values. Unit-level
// complement to cmd/m261sim.TestPCSMeterDeviceIsPopulatedThroughModbus.
func TestTickWritesPCSMeterDevice(t *testing.T) {
	st := store.New()
	clk := clock.NewFake(time.Now())
	cmds := newTestProcessor(t, st, clk)
	if err := cmds.Write(m261points.PointKey{Device: "EMS", Slug: "set_active_power_kw"}, 20); err != nil { // discharge
		t.Fatal(err)
	}
	r := NewRunner(New(DefaultParams(), 50), st, clk, cmds)
	clk.Advance(time.Second)
	r.Tick()

	if v := get(t, st, "PCS_METER", "phase_a_voltage_v"); v <= 0 {
		t.Errorf("PCS_METER phase_a_voltage_v = %v, want > 0", v)
	}
	if p := get(t, st, "PCS_METER", "total_active_power_kw"); p <= 0 {
		t.Errorf("PCS_METER total_active_power_kw = %v after a discharge, want > 0", p)
	}
	if e := get(t, st, "PCS_METER", "forward_active_total_energy_kwh"); e <= 0 {
		t.Errorf("PCS_METER forward_active_total_energy_kwh = %v, want > 0", e)
	}
	if o := get(t, st, "PCS_METER", "online_status"); o != 1 {
		t.Errorf("PCS_METER online_status = %v, want 1", o)
	}
}

func TestTickAccumulatesEnergyMeter(t *testing.T) {
	st := store.New()
	clk := clock.NewFake(time.Now())
	cmds := newTestProcessor(t, st, clk)
	if err := cmds.Write(m261points.PointKey{Device: "EMS", Slug: "set_active_power_kw"}, 10); err != nil { // discharge
		t.Fatal(err)
	}
	r := NewRunner(New(DefaultParams(), 50), st, clk, cmds)
	for i := 0; i < 5; i++ {
		clk.Advance(time.Second)
		r.Tick()
	}
	if e := get(t, st, "EMS", "total_forward_energy_kwh"); e <= 0 {
		t.Errorf("total_forward_energy_kwh = %v after discharging for 5s, want > 0", e)
	}
}

// TestTickReadsMeterDirectionFromStoreEveryTick is the code-review-
// requested fix: Tick previously never read "Energy Storage Meter Power
// Direction" at all, so the setpoint had no effect. Unit-level complement
// to cmd/m261sim.TestMeterDirectionInvertedIsAppliedEachTick, which proves
// the same thing through a real Modbus client.
func TestTickReadsMeterDirectionFromStoreEveryTick(t *testing.T) {
	st := store.New()
	clk := clock.NewFake(time.Now())
	cmds := newTestProcessor(t, st, clk)
	if err := cmds.Write(m261points.PointKey{Device: "EMS", Slug: "set_active_power_kw"}, 10); err != nil { // discharge
		t.Fatal(err)
	}
	st.Set(m261points.PointKey{Device: "EMS", Slug: "energy_storage_meter_power_direction"}, 1)
	r := NewRunner(New(DefaultParams(), 50), st, clk, cmds)
	clk.Advance(time.Second)
	r.Tick()

	if e := get(t, st, "EMS", "total_reverse_energy_kwh"); e <= 0 {
		t.Errorf("total_reverse_energy_kwh = %v after a discharge with direction inverted, want > 0", e)
	}
	if e := get(t, st, "EMS", "total_forward_energy_kwh"); e != 0 {
		t.Errorf("total_forward_energy_kwh = %v after a discharge with direction inverted, want 0", e)
	}

	// Flip it back mid-run and confirm the NEXT tick honors the change —
	// direction is read fresh every Tick, not cached from construction.
	st.Set(m261points.PointKey{Device: "EMS", Slug: "energy_storage_meter_power_direction"}, 0)
	reverseBefore := get(t, st, "EMS", "total_reverse_energy_kwh")
	clk.Advance(time.Second)
	r.Tick()
	if e := get(t, st, "EMS", "total_forward_energy_kwh"); e <= 0 {
		t.Errorf("total_forward_energy_kwh = %v after direction reverted to normal, want > 0", e)
	}
	if e := get(t, st, "EMS", "total_reverse_energy_kwh"); e != reverseBefore {
		t.Errorf("total_reverse_energy_kwh changed to %v after direction reverted, want it to stay at %v", e, reverseBefore)
	}
}

func TestRunCallsTickOnRealCadenceUntilStopped(t *testing.T) {
	st := store.New()
	clk := clock.Real{}
	r := NewRunner(New(DefaultParams(), 50), st, clk, newTestProcessor(t, st, clk))
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		r.Run(10*time.Millisecond, stop)
		close(done)
	}()

	time.Sleep(55 * time.Millisecond)
	close(stop)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not return after stop was closed")
	}

	if hb := get(t, st, "EMS", "ems_periodic_heartbeat_indicator"); hb < 2 {
		t.Errorf("heartbeat = %v after ~55ms at a 10ms cadence, want at least 2 ticks to have fired", hb)
	}
}

// TestRunDoesNotPanicOnNonPositiveInterval is the code-review-requested
// fix for time.NewTicker's panic on a zero/negative duration — main.go
// validates -physics-step before calling Run, but Run itself must not be
// fragile to a caller that doesn't.
func TestRunDoesNotPanicOnNonPositiveInterval(t *testing.T) {
	for _, interval := range []time.Duration{0, -time.Second} {
		st := store.New()
		clk := clock.Real{}
		r := NewRunner(New(DefaultParams(), 50), st, clk, newTestProcessor(t, st, clk))
		done := make(chan struct{})
		go func() {
			defer close(done)
			r.Run(interval, nil) // must return immediately, not panic or hang
		}()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Errorf("Run(%v, nil) did not return", interval)
		}
	}
}

// TestNewRunnerPublishesInitialStateImmediately is the code-review-
// requested fix: before this, the store held zero values until the first
// Tick, so a client connecting in that window saw an impossible state
// (zero SoC, zero voltage, offline). See also
// cmd/m261sim.TestInitialStateIsPublishedBeforeFirstTick for the same
// assertion through a real protocol client.
func TestNewRunnerPublishesInitialStateImmediately(t *testing.T) {
	st := store.New()
	clk := clock.NewFake(time.Now())
	NewRunner(New(DefaultParams(), 73), st, clk, newTestProcessor(t, st, clk)) // no Tick call

	if soc := get(t, st, "BMS", "soc"); soc != 73 {
		t.Errorf("BMS soc immediately after NewRunner = %v, want the configured initial 73", soc)
	}
	if v := get(t, st, "BMS", "battery_total_voltage_v"); v < 676 || v > 936 {
		t.Errorf("battery_total_voltage_v immediately after NewRunner = %v, want within [676, 936], not 0", v)
	}
	if online := get(t, st, "EMS", "online_status"); online != 1 {
		t.Errorf("online_status immediately after NewRunner = %v, want 1", online)
	}
}
