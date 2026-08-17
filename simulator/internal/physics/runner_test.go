package physics

import (
	"errors"
	"math"
	"sync"
	"sync/atomic"
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

func TestPacedRunCallsTickOnRealCadenceUntilStopped(t *testing.T) {
	st := store.New()
	clk := clock.NewFake(time.Now())
	r := NewRunner(New(DefaultParams(), 50), st, clk, newTestProcessor(t, st, clk))
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		r.PacedRun(10*time.Millisecond, 1, stop)
		close(done)
	}()

	time.Sleep(55 * time.Millisecond)
	close(stop)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("PacedRun did not return after stop was closed")
	}

	if hb := get(t, st, "EMS", "ems_periodic_heartbeat_indicator"); hb < 2 {
		t.Errorf("heartbeat = %v after ~55ms at a 10ms cadence, want at least 2 ticks to have fired", hb)
	}
}

// TestPacedRunDoesNotPanicOnNonPositiveInterval is the code-review-
// requested fix for time.NewTicker's panic on a zero/negative duration —
// main.go validates -physics-step/-speed before calling PacedRun, but
// PacedRun itself must not be fragile to a caller that doesn't.
func TestPacedRunDoesNotPanicOnNonPositiveInterval(t *testing.T) {
	cases := []struct {
		interval time.Duration
		speed    float64
	}{
		{0, 1},
		{-time.Second, 1},
		{time.Second, 0},
		{time.Second, -1},
	}
	for _, c := range cases {
		st := store.New()
		clk := clock.NewFake(time.Now())
		r := NewRunner(New(DefaultParams(), 50), st, clk, newTestProcessor(t, st, clk))
		done := make(chan struct{})
		go func() {
			defer close(done)
			r.PacedRun(c.interval, c.speed, nil) // must return immediately, not panic or hang
		}()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Errorf("PacedRun(%v, %v, nil) did not return", c.interval, c.speed)
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

// TestResetReseedsEngineDeterministically is Task 7 item 7's core
// requirement: reset must reproduce the same simulated future a fresh
// process start would, not just some valid one — same RNG seed, not a
// new random one. Cell voltage bias (drawn once from the RNG at
// construction, per-cell, never touched again) is a direct, deterministic
// fingerprint of the seed: a Reset engine and a freshly constructed one
// built from the identical params/initial SoC must produce byte-identical
// bias, proving Reset didn't silently reseed.
func TestResetReseedsEngineDeterministically(t *testing.T) {
	st := store.New()
	clk := clock.NewFake(time.Now())
	proc := newTestProcessor(t, st, clk)
	r := NewRunner(New(DefaultParams(), 50), st, clk, proc)

	clk.Advance(10 * time.Second)
	r.Tick() // drift the engine away from its freshly-constructed state

	clk.Advance(time.Hour) // simulate a long-running process before reset
	r.Reset(New(DefaultParams(), 50), clk.Now())

	got := get(t, st, "BMS", "battery_total_voltage_v")

	// A wholly independent, freshly constructed Runner built the same way
	// NewRunner was originally built above must land on the exact same
	// voltage — both derive it from the same RNG seed via the same
	// construction path, so if Reset had reseeded randomly, this would
	// not match (or would match only by 1-in-huge-odds coincidence).
	st2 := store.New()
	clk2 := clock.NewFake(time.Now())
	NewRunner(New(DefaultParams(), 50), st2, clk2, newTestProcessor(t, st2, clk2))
	want := get(t, st2, "BMS", "battery_total_voltage_v")

	if got != want {
		t.Errorf("battery_total_voltage_v after Reset = %v, want %v (same RNG seed as a fresh start)", got, want)
	}
}

// TestResetRebasesDtBaseline proves Reset doesn't leave the next Tick
// computing dt across however long the simulator had been running before
// the reset — a large stale dt would apply an entire "missing" period's
// worth of energy/thermal change in one Step, right after reset claims to
// have returned to a known-good initial state.
func TestResetRebasesDtBaseline(t *testing.T) {
	st := store.New()
	clk := clock.NewFake(time.Now())
	proc := newTestProcessor(t, st, clk)
	r := NewRunner(New(DefaultParams(), 50), st, clk, proc)

	clk.Advance(24 * time.Hour) // a long gap with no Tick at all
	r.Reset(New(DefaultParams(), 50), clk.Now())

	hbBeforeTick := get(t, st, "EMS", "ems_periodic_heartbeat_indicator")
	clk.Advance(time.Second)
	r.Tick()
	hbAfterTick := get(t, st, "EMS", "ems_periodic_heartbeat_indicator")

	if hbAfterTick != hbBeforeTick+1 {
		t.Errorf("heartbeat after one 1s Tick post-Reset = %v (was %v), want exactly +1 — dt baseline wasn't rebased to Reset's own now", hbAfterTick, hbBeforeTick)
	}
}

// TestRebaseDoesNotReplaceEngine distinguishes Rebase from Reset: only
// the dt baseline moves, SoC/temperature/energy (the running Engine's own
// state) is untouched.
func TestRebaseDoesNotReplaceEngine(t *testing.T) {
	st := store.New()
	clk := clock.NewFake(time.Now())
	proc := newTestProcessor(t, st, clk)
	r := NewRunner(New(DefaultParams(), 50), st, clk, proc)

	if err := proc.Write(m261points.PointKey{Device: "EMS", Slug: "set_active_power_kw"}, -50); err != nil {
		t.Fatalf("Write(set_active_power_kw, -50): %v", err)
	}
	clk.Advance(time.Minute)
	r.Tick()
	socBeforeRebase := get(t, st, "BMS", "soc")

	clk.Advance(time.Hour)
	r.Rebase(clk.Now())
	socAfterRebase := get(t, st, "BMS", "soc")

	if socAfterRebase != socBeforeRebase {
		t.Errorf("SoC changed from %v to %v across Rebase alone (no Tick in between) — Rebase must not touch Engine state", socBeforeRebase, socAfterRebase)
	}

	// And the next Tick's dt is measured from Rebase's now, not from the
	// last real Tick an hour "ago" — a 1s Tick should look like a 1s Tick,
	// not a ~1-hour one.
	hbBefore := get(t, st, "EMS", "ems_periodic_heartbeat_indicator")
	clk.Advance(time.Second)
	r.Tick()
	hbAfter := get(t, st, "EMS", "ems_periodic_heartbeat_indicator")
	if hbAfter != hbBefore+1 {
		t.Errorf("heartbeat after one 1s Tick post-Rebase = %v (was %v), want exactly +1", hbAfter, hbBefore)
	}
}

// TestFastForwardTicksOncePerStepInterval is Task 7 item 8's "no gaps"
// requirement at its most direct: HeartbeatCounter increments exactly
// once per Engine.Step call, so advancing total=10s at stepInterval=1s
// must produce exactly 10 Ticks, not one coarse jump.
func TestFastForwardTicksOncePerStepInterval(t *testing.T) {
	st := store.New()
	clk := clock.NewFake(time.Now())
	proc := newTestProcessor(t, st, clk)
	r := NewRunner(New(DefaultParams(), 50), st, clk, proc)

	if err := r.FastForward(10*time.Second, time.Second); err != nil {
		t.Fatalf("FastForward: %v", err)
	}

	if hb := get(t, st, "EMS", "ems_periodic_heartbeat_indicator"); hb != 10 {
		t.Errorf("heartbeat after FastForward(10s, 1s) = %v, want exactly 10 (one per stepInterval, no gaps)", hb)
	}
}

// TestFastForwardHandlesNonMultipleTotal proves the final, shorter
// increment (when total isn't an exact multiple of stepInterval) still
// results in exactly one extra Tick, not a skipped or double-counted one.
func TestFastForwardHandlesNonMultipleTotal(t *testing.T) {
	st := store.New()
	clk := clock.NewFake(time.Now())
	proc := newTestProcessor(t, st, clk)
	r := NewRunner(New(DefaultParams(), 50), st, clk, proc)

	if err := r.FastForward(2500*time.Millisecond, time.Second); err != nil {
		t.Fatalf("FastForward: %v", err)
	}
	if hb := get(t, st, "EMS", "ems_periodic_heartbeat_indicator"); hb != 3 {
		t.Errorf("heartbeat after FastForward(2.5s, 1s) = %v, want 3 (1s, 1s, then a final 0.5s increment)", hb)
	}
}

// TestFastForwardRequiresFakeClock proves fast-forwarding against a real
// clock — which cannot be told to report a specific future time — fails
// clearly instead of silently doing nothing or panicking.
func TestFastForwardRequiresFakeClock(t *testing.T) {
	st := store.New()
	clk := clock.Real{}
	proc := newTestProcessor(t, st, clk)
	r := NewRunner(New(DefaultParams(), 50), st, clk, proc)

	if err := r.FastForward(time.Second, time.Second); err == nil {
		t.Error("FastForward with a clock.Real succeeded, want an error")
	}
}

// TestConcurrentFastForwardIsLinearizable is the regression test for the
// reviewed linearizability bug: two concurrent FastForward(1s, ...)
// calls each used to read fc.Now() and independently compute their own
// target before either had ticked, so both landed on the same target —
// whichever ticked there first silently discarded the other caller's
// entire requested advance (measured against the pre-driveMu version: 8
// concurrent +1s advances produced ~1.07s of total movement, not 8s, and
// every caller still received a nil error).
//
// The fix (driveMu, TryAcquireDrive) makes concurrent drivers of the
// clock mutually exclusive via fail-fast contention instead of silent
// interleaving: every one of n concurrent FastForward(1s, ...) calls
// either advances the clock by the full, exact 1s it asked for, or gets
// ErrClockBusy immediately — never a nil error with a silently short/
// lost advance, and never two callers' advances merged into fewer total
// seconds than the number of successes claims. This is a deliberate
// fail-fast design (not an attempt to make every concurrent call
// eventually succeed serialized behind a queue — the same ErrClockBusy
// -> 409 idiom is already how this codebase resolves every other
// clock-ownership conflict, Task 7 item 2's control-API precedent), so
// the test doesn't assert exactly one success: Go's scheduler can just
// as validly run several of these n goroutines one after another with no
// true overlap at all, each with its own full, uncontended success — the
// property that actually detects the reviewed bug (two callers each
// computing target from the same stale fc.Now() read, so the second to
// tick there silently discards the first's already-"succeeded" advance)
// is total movement == successes x 1s, checked below.
func TestConcurrentFastForwardIsLinearizable(t *testing.T) {
	const n = 8
	for iter := 0; iter < 20; iter++ {
		st := store.New()
		start := time.Now()
		clk := clock.NewFake(start)
		proc := newTestProcessor(t, st, clk)
		r := NewRunner(New(DefaultParams(), 50), st, clk, proc)

		var wg sync.WaitGroup
		var succeeded, busy int32
		ready := make(chan struct{})
		wg.Add(n)
		for i := 0; i < n; i++ {
			go func() {
				defer wg.Done()
				<-ready // released together, to maximize genuine overlap
				if err := r.FastForward(time.Second, time.Second); err != nil {
					if !errors.Is(err, ErrClockBusy) {
						t.Errorf("FastForward error = %v, want nil or ErrClockBusy", err)
						return
					}
					atomic.AddInt32(&busy, 1)
					return
				}
				atomic.AddInt32(&succeeded, 1)
			}()
		}
		close(ready)
		wg.Wait()

		if succeeded < 1 {
			t.Fatalf("iter %d: 0 of %d concurrent FastForward calls succeeded, want at least 1", iter, n)
		}
		if succeeded+busy != n {
			t.Fatalf("iter %d: succeeded=%d busy=%d, want they sum to %d", iter, succeeded, busy, n)
		}
		want := time.Duration(succeeded) * time.Second
		if got := clk.Now().Sub(start); got != want {
			t.Fatalf("iter %d: clock advanced by %v after %d successful calls, want exactly %v (no lost/merged advances)", iter, got, succeeded, want)
		}
	}
}

// TestFastForwardBusyWhileDriveHeld proves FastForward fails immediately
// with ErrClockBusy — never blocks, never silently ticks anyway — while
// something else (modeled here directly via TryAcquireDrive, standing in
// for a running scenario) already owns driving the clock, and that a
// FastForward issued after ReleaseDrive succeeds normally once the
// conflict is gone.
func TestFastForwardBusyWhileDriveHeld(t *testing.T) {
	st := store.New()
	clk := clock.NewFake(time.Now())
	proc := newTestProcessor(t, st, clk)
	r := NewRunner(New(DefaultParams(), 50), st, clk, proc)

	if !r.TryAcquireDrive() {
		t.Fatal("TryAcquireDrive on a fresh Runner returned false")
	}
	if err := r.FastForward(time.Second, time.Second); !errors.Is(err, ErrClockBusy) {
		t.Fatalf("FastForward while drive held: err = %v, want ErrClockBusy", err)
	}
	r.ReleaseDrive()

	if err := r.FastForward(time.Second, time.Second); err != nil {
		t.Fatalf("FastForward after ReleaseDrive: %v, want success", err)
	}
}

// TestPacedRunSkipsTickWhileDriveHeld proves PacedRun's own real-time
// ticks are skipped (not queued, not silently interleaved), not run,
// while something else owns driveMu — the property scenario.Runner's
// whole-run TryAcquireDrive hold (Start to Stop/completion) depends on
// to keep PacedRun from racing a running scenario's own advances.
func TestPacedRunSkipsTickWhileDriveHeld(t *testing.T) {
	st := store.New()
	clk := clock.NewFake(time.Now())
	proc := newTestProcessor(t, st, clk)
	r := NewRunner(New(DefaultParams(), 50), st, clk, proc)

	if !r.TryAcquireDrive() {
		t.Fatal("TryAcquireDrive on a fresh Runner returned false")
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.PacedRun(5*time.Millisecond, 1, stop)
	}()
	time.Sleep(40 * time.Millisecond)
	close(stop)
	<-done

	if hb := get(t, st, "EMS", "ems_periodic_heartbeat_indicator"); hb != 0 {
		t.Errorf("heartbeat = %v after ~40ms of PacedRun with driveMu externally held, want 0 (every tick attempt skipped)", hb)
	}

	r.ReleaseDrive()
	if err := r.TickOnce(5 * time.Millisecond); err != nil {
		t.Fatalf("TickOnce after ReleaseDrive: %v", err)
	}
	if hb := get(t, st, "EMS", "ems_periodic_heartbeat_indicator"); hb != 1 {
		t.Errorf("heartbeat = %v after ReleaseDrive + one TickOnce, want 1", hb)
	}
}

// TestFastForwardNeverBusyDueToBackgroundPacer is Blocker 1's third-
// review-round black-box reproduction, turned into a deterministic
// barrier test: with PacedRun running continuously in the background at
// a fast tick rate (maximizing the odds of an external FastForward
// landing on top of one of its ticks), every external FastForward call
// must still succeed — ErrClockBusy is only a valid outcome between two
// *external* drivers, never because the background pacer happened to be
// mid-tick. Reproduced against the pre-fix design (PacedRun contending
// for the same lock external callers used): 20/20 advances failed at
// speed=1000000; 1 in 500 failed even at speed=1.
func TestFastForwardNeverBusyDueToBackgroundPacer(t *testing.T) {
	st := store.New()
	clk := clock.NewFake(time.Now())
	proc := newTestProcessor(t, st, clk)
	r := NewRunner(New(DefaultParams(), 50), st, clk, proc)

	stop := make(chan struct{})
	pacerDone := make(chan struct{})
	go func() {
		defer close(pacerDone)
		r.PacedRun(time.Microsecond, 1000000, stop) // as fast as the ticker/scheduler allow
	}()
	defer func() {
		close(stop)
		<-pacerDone
	}()

	const n = 500
	for i := 0; i < n; i++ {
		if err := r.FastForward(time.Millisecond, time.Millisecond); err != nil {
			t.Fatalf("FastForward #%d/%d against a live background pacer: %v, want nil (pacer must yield, never cause ErrClockBusy)", i+1, n, err)
		}
	}
}

// TestExternalDriversStillConflictWithEachOtherUnderLivePacer is
// TestFastForwardNeverBusyDueToBackgroundPacer's complement: the fix for
// pacer-vs-external contention must not accidentally also suppress
// genuine external-vs-external contention. A long-running external
// driver (modeled via TryAcquireDrive directly, standing in for a
// running scenario) must still make a concurrent FastForward fail with
// ErrClockBusy, live background pacer or not.
func TestExternalDriversStillConflictWithEachOtherUnderLivePacer(t *testing.T) {
	st := store.New()
	clk := clock.NewFake(time.Now())
	proc := newTestProcessor(t, st, clk)
	r := NewRunner(New(DefaultParams(), 50), st, clk, proc)

	stop := make(chan struct{})
	pacerDone := make(chan struct{})
	go func() {
		defer close(pacerDone)
		r.PacedRun(time.Microsecond, 1000000, stop)
	}()
	defer func() {
		close(stop)
		<-pacerDone
	}()

	if !r.TryAcquireDrive() {
		t.Fatal("TryAcquireDrive on a fresh Runner returned false")
	}
	defer r.ReleaseDrive()

	if err := r.FastForward(time.Millisecond, time.Millisecond); !errors.Is(err, ErrClockBusy) {
		t.Fatalf("FastForward while another external driver holds ownership (live pacer also running): err = %v, want ErrClockBusy", err)
	}
}

// TestCheckedPaceRejectsOverflow is Significant 2's regression test: a
// bare time.Duration(float64(d)/speed) conversion is implementation-
// defined (not a panic) once the quotient falls outside int64
// nanoseconds — reproduced black-box as a valid, finite, positive speed
// (1e-300) making the live simulator run at ~1ns cadence instead of
// "almost stopped" (heartbeat reached 24,478 within seconds of real
// time). CheckedPace must reject this instead of silently producing an
// implementation-defined Duration.
//
// Fourth review round: also rejects the opposite extreme — a speed so
// large the quotient underflows to zero once truncated to whole
// nanoseconds, which previously let PacedRun fall back to a runaway
// as-fast-as-possible ticker instead of refusing to run.
func TestCheckedPaceRejectsOverflow(t *testing.T) {
	cases := []struct {
		name  string
		d     time.Duration
		speed float64
	}{
		{"extremely small speed", time.Second, 1e-300},
		{"speed makes NaN", time.Second, math.NaN()},
		{"speed is zero", time.Second, 0},
		{"extremely large finite speed underflows to zero", time.Second, 1e30},
		{"infinite speed underflows to zero", time.Second, math.Inf(1)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := CheckedPace(c.d, c.speed)
			if err == nil {
				t.Errorf("CheckedPace(%v, %v) = nil error, want a rejection", c.d, c.speed)
			}
		})
	}

	// A normal, representable case must still succeed.
	got, err := CheckedPace(time.Second, 2)
	if err != nil {
		t.Fatalf("CheckedPace(1s, 2) = %v, want nil error", err)
	}
	if got != 500*time.Millisecond {
		t.Errorf("CheckedPace(1s, 2) = %v, want 500ms", got)
	}

	// d == 0 is exempt from the underflow-to-zero rejection: a
	// zero-length remaining chunk legitimately paces to nothing at any
	// speed, not an extreme-speed symptom.
	if got, err := CheckedPace(0, 1e30); err != nil || got != 0 {
		t.Errorf("CheckedPace(0, 1e30) = (%v, %v), want (0, nil)", got, err)
	}
}

// TestPacedRunDoesNotRunawayOnExtremeSpeed is TestCheckedPaceRejectsOverflow's
// integration counterpart: PacedRun given a valid-per-validSpeed but
// pace-overflowing speed must simply not tick (degrade to "no live
// pacing"), never silently run at an implementation-defined ~1ns
// cadence.
func TestPacedRunDoesNotRunawayOnExtremeSpeed(t *testing.T) {
	st := store.New()
	clk := clock.NewFake(time.Now())
	proc := newTestProcessor(t, st, clk)
	r := NewRunner(New(DefaultParams(), 50), st, clk, proc)

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.PacedRun(time.Second, 1e-300, stop)
	}()
	time.Sleep(50 * time.Millisecond)
	close(stop)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("PacedRun did not return after stop was closed")
	}

	if hb := get(t, st, "EMS", "ems_periodic_heartbeat_indicator"); hb != 0 {
		t.Errorf("heartbeat = %v after ~50ms with an overflowing speed, want 0 (PacedRun must refuse to run away, not tick at ~1ns cadence)", hb)
	}
}

// TestPacedRunDoesNotRunawayOnHugeFiniteSpeed is the huge-speed mirror of
// TestPacedRunDoesNotRunawayOnExtremeSpeed — fourth review round: a
// speed so large the computed real interval underflows to zero
// previously fell back to a fixed 1ns ticker, an uncontrolled as-fast-
// as-possible cadence with no real pacing at all, not merely "wrong",
// but capable of consuming a full CPU core. PacedRun must refuse to run
// at all instead.
func TestPacedRunDoesNotRunawayOnHugeFiniteSpeed(t *testing.T) {
	st := store.New()
	clk := clock.NewFake(time.Now())
	proc := newTestProcessor(t, st, clk)
	r := NewRunner(New(DefaultParams(), 50), st, clk, proc)

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.PacedRun(time.Second, 1e30, stop)
	}()
	time.Sleep(50 * time.Millisecond)
	close(stop)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("PacedRun did not return after stop was closed")
	}

	if hb := get(t, st, "EMS", "ems_periodic_heartbeat_indicator"); hb != 0 {
		t.Errorf("heartbeat = %v after ~50ms with an underflowing speed, want 0 (PacedRun must refuse to run away, not tick at an uncontrolled ~1ns cadence)", hb)
	}
}
