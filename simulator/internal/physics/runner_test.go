package physics

import (
	"testing"
	"time"

	"github.com/SterneStehen/petz-m261-tooling/gen/go/m261points"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/clock"
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

func TestTickDoesNothingWithoutElapsedTime(t *testing.T) {
	st := store.New()
	clk := clock.NewFake(time.Now())
	r := NewRunner(New(DefaultParams(), 50), st, clk)
	r.Tick() // clock hasn't moved since NewRunner — must be a no-op
	if hb := get(t, st, "EMS", "ems_periodic_heartbeat_indicator"); hb != 0 {
		t.Errorf("heartbeat = %v after a zero-dt Tick, want 0 (no-op)", hb)
	}
}

func TestTickWritesSoCPowerAndHeartbeatToStore(t *testing.T) {
	st := store.New()
	st.Set(m261points.PointKey{Device: "EMS", Slug: "set_active_power_kw"}, -50) // request charge
	clk := clock.NewFake(time.Now())
	r := NewRunner(New(DefaultParams(), 50), st, clk)

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
	r := NewRunner(New(DefaultParams(), 50), st, clk)
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
	st.Set(m261points.PointKey{Device: "EMS", Slug: "set_active_power_kw"}, 50) // discharge
	clk := clock.NewFake(time.Now())
	r := NewRunner(New(DefaultParams(), 50), st, clk)
	clk.Advance(time.Second)
	r.Tick()

	if f := get(t, st, "PCS", "grid_frequency_hz"); f != 50 {
		t.Errorf("grid_frequency_hz = %v, want 50", f)
	}
	if i := get(t, st, "PCS", "phase_a_current_a"); i <= 0 {
		t.Errorf("phase_a_current_a = %v after a discharge request, want > 0", i)
	}
}

func TestTickAccumulatesEnergyMeter(t *testing.T) {
	st := store.New()
	st.Set(m261points.PointKey{Device: "EMS", Slug: "set_active_power_kw"}, 10) // discharge
	clk := clock.NewFake(time.Now())
	r := NewRunner(New(DefaultParams(), 50), st, clk)
	for i := 0; i < 5; i++ {
		clk.Advance(time.Second)
		r.Tick()
	}
	if e := get(t, st, "EMS", "total_forward_energy_kwh"); e <= 0 {
		t.Errorf("total_forward_energy_kwh = %v after discharging for 5s, want > 0", e)
	}
}

func TestRunCallsTickOnRealCadenceUntilStopped(t *testing.T) {
	st := store.New()
	r := NewRunner(New(DefaultParams(), 50), st, clock.Real{})
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
