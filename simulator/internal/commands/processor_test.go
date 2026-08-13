package commands_test

import (
	"errors"
	"math"
	"strconv"
	"testing"
	"time"

	"github.com/SterneStehen/petz-m261-tooling/gen/go/m261points"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/clock"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/commands"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/store"
)

func newProcessor(t *testing.T) (*commands.Processor, *store.Store, *clock.Fake) {
	t.Helper()
	st := store.New()
	clk := clock.NewFake(time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC))
	p, err := commands.NewProcessor(st, clk, commands.DefaultConfig())
	if err != nil {
		t.Fatalf("NewProcessor: %v", err)
	}
	return p, st, clk
}

func emsKey(slug string) m261points.PointKey { return m261points.PointKey{Device: "EMS", Slug: slug} }

func setMode(t *testing.T, p *commands.Processor, mode float64) {
	t.Helper()
	if err := p.Write(emsKey("set_operating_mode"), mode); err != nil {
		t.Fatalf("Write(set_operating_mode, %v): %v", mode, err)
	}
}

// --- NewProcessor / config validation -------------------------------------

func TestNewProcessorRejectsBadConfig(t *testing.T) {
	st := store.New()
	clk := clock.NewFake(time.Now())
	cases := []commands.Config{
		func() commands.Config { c := commands.DefaultConfig(); c.WatchdogMode = "retry_forever"; return c }(),
		func() commands.Config { c := commands.DefaultConfig(); c.WatchdogTimeout = 0; return c }(),
		func() commands.Config { c := commands.DefaultConfig(); c.ModePriority = []string{"remote"}; return c }(),
		func() commands.Config {
			c := commands.DefaultConfig()
			c.ModePriority = []string{"remote", "remote", "load_tracking"}
			return c
		}(),
	}
	for i, cfg := range cases {
		if _, err := commands.NewProcessor(st, clk, cfg); err == nil {
			t.Errorf("case %d: NewProcessor accepted an invalid config %+v", i, cfg)
		}
	}
}

// TestNewProcessorPublishesSensibleDefaults is the fix for the trap where
// store.New's zero default (Power On/Off=0, Maximum/Minimum SOC=0,
// System Max Charge/Discharge Power=0) would block all dispatch out of
// the box.
func TestNewProcessorPublishesSensibleDefaults(t *testing.T) {
	_, st, _ := newProcessor(t)
	cases := map[string]float64{
		"power_on_off":                   1,
		"maximum_charge_soc":             100,
		"minimum_discharge_soc":          0,
		"system_maximum_charge_power":    130.5,
		"system_maximum_discharge_power": 130.5,
	}
	for slug, want := range cases {
		got, ok := st.Get(emsKey(slug))
		if !ok || got != want {
			t.Errorf("%s = %v, %v; want %v, true", slug, got, ok, want)
		}
	}
}

// --- validation: enum, range, readback ------------------------------------

func TestWriteRejectsOutOfEnumValue(t *testing.T) {
	p, st, _ := newProcessor(t)
	before, _ := st.Get(emsKey("set_operating_mode"))
	if err := p.Write(emsKey("set_operating_mode"), 3); err == nil {
		t.Error("Write(set_operating_mode, 3) accepted an out-of-enum value")
	} else if !errors.Is(err, commands.ErrInvalidValue) {
		t.Errorf("error = %v, want ErrInvalidValue", err)
	}
	after, _ := st.Get(emsKey("set_operating_mode"))
	if after != before {
		t.Errorf("store changed to %v after a rejected write, want unchanged %v", after, before)
	}
}

func TestWriteAcceptsInEnumValue(t *testing.T) {
	p, st, _ := newProcessor(t)
	if err := p.Write(emsKey("set_operating_mode"), 1); err != nil {
		t.Fatalf("Write(set_operating_mode, 1): %v", err)
	}
	if v, _ := st.Get(emsKey("set_operating_mode")); v != 1 {
		t.Errorf("set_operating_mode = %v, want 1", v)
	}
}

func TestWriteRejectsOutOfI16Range(t *testing.T) {
	p, _, _ := newProcessor(t)
	// System Maximum Charge Power is I16, no enum.
	for _, v := range []float64{32768, -32769, 1e9} {
		if err := p.Write(emsKey("system_maximum_charge_power"), v); err == nil {
			t.Errorf("Write(system_maximum_charge_power, %v) accepted an out-of-I16-range value", v)
		} else if !errors.Is(err, commands.ErrInvalidValue) {
			t.Errorf("value %v: error = %v, want ErrInvalidValue", v, err)
		}
	}
}

func TestWriteRejectsNonFiniteF32(t *testing.T) {
	p, _, _ := newProcessor(t)
	for _, v := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if err := p.Write(emsKey("set_active_power_kw"), v); err == nil {
			t.Errorf("Write(set_active_power_kw, %v) accepted a non-finite value", v)
		}
	}
}

func TestWriteRejectsNonSetpointPoints(t *testing.T) {
	p, _, _ := newProcessor(t)
	cases := []m261points.PointKey{
		{Device: "EMS", Slug: "online_status"},     // EMS, but telemetry (RO)
		{Device: "BMS", Slug: "soc"},               // not EMS at all
		{Device: "PCS", Slug: "phase_a_voltage_v"}, // not EMS at all
		{Device: "EMS", Slug: "does_not_exist_in_the_catalog_at_all"},
	}
	for _, key := range cases {
		if err := p.Write(key, 1); !errors.Is(err, commands.ErrNotWritable) {
			t.Errorf("Write(%+v, 1) = %v, want ErrNotWritable", key, err)
		}
	}
}

// TestWriteReadbackForEvery148Setpoint is Task 6's "readback works for
// all 148 points" acceptance criterion, exercised against the real
// catalog rather than a hand-picked sample — every point Task 1 generated
// as an EMS setpoint, not a guess at how many there are.
func TestWriteReadbackForEvery148Setpoint(t *testing.T) {
	p, st, _ := newProcessor(t)
	count := 0
	for key, meta := range m261points.Points {
		if meta.Device != "EMS" || meta.Class != m261points.ClassSetpoint {
			continue
		}
		count++
		if meta.Dangerous {
			continue // covered separately by the dangerous-gating tests below
		}
		value := 1.0
		if meta.Enum != nil {
			for k := range meta.Enum {
				value = float64(k)
				break
			}
		}
		if err := p.Write(key, value); err != nil {
			t.Fatalf("Write(%s/%s, %v): %v", key.Device, key.Slug, value, err)
		}
		if meta.ReadbackIEC104Addr == nil {
			t.Fatalf("%s has no ReadbackIEC104Addr — Task 1/3 invariant broken", meta.Slug)
		}
		rbAddr := store.IECAddr{CommonAddr: meta.DeviceAddr, ObjAddr: *meta.ReadbackIEC104Addr}
		rbKey, rbValue, ok := st.GetByIEC(rbAddr)
		if !ok {
			t.Fatalf("%s: no point at its own documented readback address %v", meta.Slug, rbAddr)
		}
		if rbValue != value {
			t.Errorf("%s: readback (%s) = %v, want %v", meta.Slug, rbKey.Slug, rbValue, value)
		}
	}
	if count != 148 {
		t.Errorf("found %d EMS setpoints in the catalog, want 148 (AGENT-TASK §2/§4.2)", count)
	}
}

// --- dangerous gating (Trip, Clear Protection) ----------------------------

func TestTripRejectedWithoutAllowDangerous(t *testing.T) {
	st := store.New()
	clk := clock.NewFake(time.Now())
	p, err := commands.NewProcessor(st, clk, commands.DefaultConfig()) // AllowDangerous: false
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Write(emsKey("trip"), 1); !errors.Is(err, commands.ErrDangerous) {
		t.Errorf("Write(trip, 1) = %v, want ErrDangerous", err)
	}
	if v, _ := st.Get(emsKey("trip")); v != 0 {
		t.Errorf("trip = %v after a rejected write, want unchanged 0", v)
	}
}

func TestClearProtectionRejectedWithoutAllowDangerous(t *testing.T) {
	p, st, _ := newProcessor(t)
	if err := p.Write(emsKey("clear_protection"), 1); !errors.Is(err, commands.ErrDangerous) {
		t.Errorf("Write(clear_protection, 1) = %v, want ErrDangerous", err)
	}
	if v, _ := st.Get(emsKey("clear_protection")); v != 0 {
		t.Errorf("clear_protection = %v after a rejected write, want unchanged 0", v)
	}
}

// newAllowDangerousProcessor builds a Processor with allow_dangerous=true —
// used only by the accepted-Trip/Clear-Protection tests below; the
// rejected-without-permission tests use the DefaultConfig() (false) helper
// newProcessor instead.
func newAllowDangerousProcessor(t *testing.T) (*commands.Processor, *store.Store, *clock.Fake) {
	t.Helper()
	st := store.New()
	clk := clock.NewFake(time.Now())
	cfg := commands.DefaultConfig()
	cfg.AllowDangerous = true
	p, err := commands.NewProcessor(st, clk, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return p, st, clk
}

// assertAcceptedButUnsupported checks key's recorded Diagnostic matches the
// approved AGENT-TASK §6 item 7 contract: code accepted_but_unsupported,
// the point key, and the accepted value — no SelectedMode (that's only
// for Demand Control/Load Tracking, tested separately below).
func assertAcceptedButUnsupported(t *testing.T, p *commands.Processor, key m261points.PointKey, wantValue float64) {
	t.Helper()
	d, ok := p.DiagnosticFor(key)
	if !ok {
		t.Fatalf("no Diagnostic recorded for %+v", key)
	}
	if d.Code != commands.DiagCodeAcceptedButUnsupported {
		t.Errorf("Diagnostic.Code = %q, want %q", d.Code, commands.DiagCodeAcceptedButUnsupported)
	}
	if d.PointKey != key {
		t.Errorf("Diagnostic.PointKey = %+v, want %+v", d.PointKey, key)
	}
	if d.AcceptedValue != wantValue {
		t.Errorf("Diagnostic.AcceptedValue = %v, want %v", d.AcceptedValue, wantValue)
	}
}

// TestTripAcceptedWithAllowDangerousDoesNotAffectDispatch is the approved
// AGENT-TASK §6 item 7 contract for Trip specifically: accepted and
// mirrored to readback, but dispatched power is completely unaffected —
// no latch, no forced zero, nothing modeled — plus the structured
// accepted_but_unsupported diagnostic.
func TestTripAcceptedWithAllowDangerousDoesNotAffectDispatch(t *testing.T) {
	p, st, clk := newAllowDangerousProcessor(t)
	setMode(t, p, modeRemote)
	if err := p.Write(emsKey("set_active_power_kw"), 50); err != nil {
		t.Fatal(err)
	}

	if err := p.Write(emsKey("trip"), 1); err != nil {
		t.Fatalf("Write(trip, 1) with allow_dangerous=true: %v", err)
	}
	if v, _ := st.Get(emsKey("trip")); v != 1 {
		t.Errorf("stored trip = %v, want 1 (accepted)", v)
	}
	// trip has no readback_iec104_addr of its own to check via the store
	// (it's an EMS setpoint like any other — see readback test below for
	// the general 148-point proof); the dispatch-unaffected assertion is
	// the point of this test.
	active, reactive := p.ResolvePower(clk.Now(), 130.5, 130.5, 50, false, false)
	if active != 50 || reactive != 0 {
		t.Errorf("dispatch after accepted Trip = %v, %v; want 50, 0 (unaffected)", active, reactive)
	}
	assertAcceptedButUnsupported(t, p, emsKey("trip"), 1)
}

// TestClearProtectionAcceptedWithAllowDangerousDoesNotAffectDispatch mirrors
// the Trip test above for Clear Protection — tested separately per the
// review's explicit requirement, even though the code path is identical.
func TestClearProtectionAcceptedWithAllowDangerousDoesNotAffectDispatch(t *testing.T) {
	p, st, clk := newAllowDangerousProcessor(t)
	setMode(t, p, modeRemote)
	if err := p.Write(emsKey("set_active_power_kw"), 50); err != nil {
		t.Fatal(err)
	}

	if err := p.Write(emsKey("clear_protection"), 1); err != nil {
		t.Fatalf("Write(clear_protection, 1) with allow_dangerous=true: %v", err)
	}
	if v, _ := st.Get(emsKey("clear_protection")); v != 1 {
		t.Errorf("stored clear_protection = %v, want 1 (accepted)", v)
	}
	active, reactive := p.ResolvePower(clk.Now(), 130.5, 130.5, 50, false, false)
	if active != 50 || reactive != 0 {
		t.Errorf("dispatch after accepted Clear Protection = %v, %v; want 50, 0 (unaffected)", active, reactive)
	}
	assertAcceptedButUnsupported(t, p, emsKey("clear_protection"), 1)
}

// --- mode arbitration: Manual / Auto Strategy / Remote --------------------

const (
	modeManual       = 0
	modeAutoStrategy = 1
	modeRemote       = 2
)

func TestManualModeDoesNotDispatchSetpoint(t *testing.T) {
	p, _, clk := newProcessor(t)
	setMode(t, p, modeManual)
	if err := p.Write(emsKey("set_active_power_kw"), 80); err != nil {
		t.Fatal(err)
	}
	active, reactive := p.ResolvePower(clk.Now(), 130.5, 130.5, 50, false, false)
	if active != 0 || reactive != 0 {
		t.Errorf("Manual-mode dispatch = %v, %v; want 0, 0 (setpoints not auto-executed)", active, reactive)
	}
}

// TestRemoteModeDispatchesSetActivePower is half of Task 6's "Set Active
// Power in Remote mode changes power and SoC" acceptance criterion — this
// package's dispatch-resolution half; cmd/m261sim's
// TestPhysicsTickIsVisibleThroughBothProtocols covers the "and SoC" half
// through the real physics engine and both real protocol clients.
func TestRemoteModeDispatchesSetActivePower(t *testing.T) {
	p, _, clk := newProcessor(t)
	setMode(t, p, modeRemote)
	if err := p.Write(emsKey("set_active_power_kw"), -42); err != nil {
		t.Fatal(err)
	}
	if err := p.Write(emsKey("set_reactive_power_kvar"), 7); err != nil {
		t.Fatal(err)
	}
	active, reactive := p.ResolvePower(clk.Now(), 130.5, 130.5, 50, false, false)
	if active != -42 || reactive != 7 {
		t.Errorf("Remote-mode dispatch = %v, %v; want -42, 7", active, reactive)
	}
}

// --- Auto Strategy schedule execution --------------------------------------

func writePeriod(t *testing.T, p *commands.Processor, n int, startH, startM, endH, endM int, power float64) {
	t.Helper()
	prefix := "strategy_period_"
	writes := map[string]float64{
		"start_hour":                       float64(startH),
		"start_minute":                     float64(startM),
		"end_hour":                         float64(endH),
		"end_minute":                       float64(endM),
		"execution_power_charge_discharge": power,
	}
	for suffix, v := range writes {
		slug := prefix + strconv.Itoa(n) + "_" + suffix
		if err := p.Write(emsKey(slug), v); err != nil {
			t.Fatalf("Write(%s, %v): %v", slug, v, err)
		}
	}
}

func TestAutoStrategyExecutesActivePeriodBySign(t *testing.T) {
	p, _, clk := newProcessor(t)
	writePeriod(t, p, 1, 1, 0, 2, 0, -30) // 01:00-02:00, charge 30kW
	writePeriod(t, p, 2, 3, 0, 4, 0, 55)  // 03:00-04:00, discharge 55kW
	setMode(t, p, modeAutoStrategy)

	cases := []struct {
		hour, minute int
		want         float64
	}{
		{0, 30, 0},   // before period 1: idle
		{1, 0, -30},  // period 1 start (inclusive)
		{1, 59, -30}, // still inside period 1
		{2, 0, 0},    // period 1 end (exclusive): idle
		{2, 30, 0},   // between periods: idle
		{3, 30, 55},  // inside period 2
		{4, 0, 0},    // period 2 end (exclusive): idle
	}
	for _, c := range cases {
		clk.Set(time.Date(2026, 8, 12, c.hour, c.minute, 0, 0, time.UTC))
		active, reactive := p.ResolvePower(clk.Now(), 130.5, 130.5, 50, false, false)
		if active != c.want {
			t.Errorf("%02d:%02d: active = %v, want %v", c.hour, c.minute, active, c.want)
		}
		if reactive != 0 {
			t.Errorf("%02d:%02d: reactive = %v, want 0 (Auto Strategy has no reactive schedule)", c.hour, c.minute, reactive)
		}
	}
}

func TestAutoStrategyUnconfiguredPeriodsAreInactive(t *testing.T) {
	// Periods 2-10 are left at their store.New zero value (all fields 0) —
	// must not be read as an always-active 00:00-00:00 period.
	p, _, clk := newProcessor(t)
	writePeriod(t, p, 1, 10, 0, 11, 0, 20)
	setMode(t, p, modeAutoStrategy)

	clk.Set(time.Date(2026, 8, 12, 15, 0, 0, 0, time.UTC))
	active, _ := p.ResolvePower(clk.Now(), 130.5, 130.5, 50, false, false)
	if active != 0 {
		t.Errorf("active outside period 1, with periods 2-10 unconfigured = %v, want 0", active)
	}
}

func TestAutoStrategyWrapsPastMidnight(t *testing.T) {
	p, _, clk := newProcessor(t)
	writePeriod(t, p, 1, 23, 0, 1, 0, -15) // 23:00-01:00, overnight charge
	setMode(t, p, modeAutoStrategy)

	for _, hm := range [][2]int{{23, 30}, {0, 30}} {
		clk.Set(time.Date(2026, 8, 12, hm[0], hm[1], 0, 0, time.UTC))
		active, _ := p.ResolvePower(clk.Now(), 130.5, 130.5, 50, false, false)
		if active != -15 {
			t.Errorf("%02d:%02d inside an overnight period = %v, want -15", hm[0], hm[1], active)
		}
	}
	clk.Set(time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC))
	if active, _ := p.ResolvePower(clk.Now(), 130.5, 130.5, 50, false, false); active != 0 {
		t.Errorf("midday, outside the overnight period = %v, want 0", active)
	}
}

func TestAutoStrategyLowestNumberedOverlappingPeriodWins(t *testing.T) {
	p, _, clk := newProcessor(t)
	writePeriod(t, p, 1, 8, 0, 18, 0, 10)
	writePeriod(t, p, 2, 8, 0, 18, 0, 999) // fully overlapping period 1
	setMode(t, p, modeAutoStrategy)

	clk.Set(time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC))
	if active, _ := p.ResolvePower(clk.Now(), 130.5, 130.5, 50, false, false); active != 10 {
		t.Errorf("overlapping periods 1 and 2 = %v, want 10 (period 1, lowest-numbered, wins)", active)
	}
}

// --- limits: SoC bounds, System Maximum Charge/Discharge Power, BMS -------

// TestBMSLimitClipsRatherThanRejects is Task 6 acceptance criterion 3: "a
// setpoint exceeding the BMS limit is clipped, not executed literally" —
// the write itself succeeds and reads back unclipped; only dispatch is
// clipped.
func TestBMSLimitClipsRatherThanRejects(t *testing.T) {
	p, st, clk := newProcessor(t)
	setMode(t, p, modeRemote)
	if err := p.Write(emsKey("set_active_power_kw"), 200); err != nil {
		t.Fatal(err)
	}
	if v, _ := st.Get(emsKey("set_active_power_kw")); v != 200 {
		t.Errorf("stored/readback set_active_power_kw = %v, want the literal 200 written", v)
	}
	active, _ := p.ResolvePower(clk.Now(), 130.5 /* bmsMaxCharge */, 90 /* bmsMaxDischarge */, 50, false, false)
	if active != 90 {
		t.Errorf("dispatched active power = %v, want clipped to the BMS discharge limit 90", active)
	}
}

func TestSystemMaxPowerClipsTighterThanBMS(t *testing.T) {
	p, st, clk := newProcessor(t)
	setMode(t, p, modeRemote)
	st.Set(emsKey("system_maximum_discharge_power"), 40) // tighter than the 130.5 BMS default
	if err := p.Write(emsKey("set_active_power_kw"), 100); err != nil {
		t.Fatal(err)
	}
	if active, _ := p.ResolvePower(clk.Now(), 130.5, 130.5, 50, false, false); active != 40 {
		t.Errorf("dispatched active power = %v, want clipped to System Maximum Discharge Power 40", active)
	}
}

func TestChargeLimitAppliesOnChargeSide(t *testing.T) {
	p, st, clk := newProcessor(t)
	setMode(t, p, modeRemote)
	st.Set(emsKey("system_maximum_charge_power"), 25)
	if err := p.Write(emsKey("set_active_power_kw"), -100); err != nil {
		t.Fatal(err)
	}
	if active, _ := p.ResolvePower(clk.Now(), 130.5, 130.5, 50, false, false); active != -25 {
		t.Errorf("dispatched active power = %v, want clipped to -25 (System Maximum Charge Power)", active)
	}
}

func TestMaximumChargeSOCBlocksFurtherCharging(t *testing.T) {
	p, st, clk := newProcessor(t)
	setMode(t, p, modeRemote)
	st.Set(emsKey("maximum_charge_soc"), 80)
	if err := p.Write(emsKey("set_active_power_kw"), -30); err != nil {
		t.Fatal(err)
	}
	if active, _ := p.ResolvePower(clk.Now(), 130.5, 130.5, 85 /* already above the 80% cap */, false, false); active != 0 {
		t.Errorf("dispatched charge power at 85%% SoC with an 80%% Maximum Charge SOC = %v, want 0", active)
	}
	if active, _ := p.ResolvePower(clk.Now(), 130.5, 130.5, 79, false, false); active != -30 {
		t.Errorf("dispatched charge power at 79%% SoC with an 80%% Maximum Charge SOC = %v, want -30 (unclipped)", active)
	}
}

func TestMinimumDischargeSOCBlocksFurtherDischarging(t *testing.T) {
	p, st, clk := newProcessor(t)
	setMode(t, p, modeRemote)
	st.Set(emsKey("minimum_discharge_soc"), 20)
	if err := p.Write(emsKey("set_active_power_kw"), 30); err != nil {
		t.Fatal(err)
	}
	if active, _ := p.ResolvePower(clk.Now(), 130.5, 130.5, 15 /* already below the 20% floor */, false, false); active != 0 {
		t.Errorf("dispatched discharge power at 15%% SoC with a 20%% Minimum Discharge SOC = %v, want 0", active)
	}
	if active, _ := p.ResolvePower(clk.Now(), 130.5, 130.5, 21, false, false); active != 30 {
		t.Errorf("dispatched discharge power at 21%% SoC with a 20%% Minimum Discharge SOC = %v, want 30 (unclipped)", active)
	}
}

// TestChargeProhibitedFlagZeroesChargeEvenWithBMSHeadroom proves the
// explicit chargeProhibited gate (physics.State.ChargeProhibited, the
// engine's own hard SoC-boundary flag) does real, independent work —
// distinct from bmsMaxChargeKW/bmsMaxDischargeKW's taper, which normally
// reaches 0 at the same boundary but isn't the only thing enforcing it.
// Passing a nonzero bmsMaxChargeKW alongside chargeProhibited=true (not
// realistic from the real engine, but exactly what a synthetic unit test
// should do to isolate this one code path) confirms the flag alone is
// sufficient to zero dispatch.
func TestChargeProhibitedFlagZeroesChargeEvenWithBMSHeadroom(t *testing.T) {
	p, _, clk := newProcessor(t)
	setMode(t, p, modeRemote)
	if err := p.Write(emsKey("set_active_power_kw"), -30); err != nil {
		t.Fatal(err)
	}
	active, _ := p.ResolvePower(clk.Now(), 50 /* bmsMaxChargeKW, nonzero on purpose */, 130.5, 50, true /* chargeProhibited */, false)
	if active != 0 {
		t.Errorf("charge dispatch with chargeProhibited=true = %v, want 0 (regardless of bmsMaxChargeKW=50)", active)
	}
}

func TestDischargeProhibitedFlagZeroesDischargeEvenWithBMSHeadroom(t *testing.T) {
	p, _, clk := newProcessor(t)
	setMode(t, p, modeRemote)
	if err := p.Write(emsKey("set_active_power_kw"), 30); err != nil {
		t.Fatal(err)
	}
	active, _ := p.ResolvePower(clk.Now(), 130.5, 50 /* bmsMaxDischargeKW, nonzero on purpose */, 50, false, true /* dischargeProhibited */)
	if active != 0 {
		t.Errorf("discharge dispatch with dischargeProhibited=true = %v, want 0 (regardless of bmsMaxDischargeKW=50)", active)
	}
}

// TestSystemMaxChargePowerDefaultIsNeverAmbiguousWithAnExplicitZero is the
// review's required "explicitly define and test how the simulator
// distinguishes an unset default from a real zero limit". The answer:
// "unset" never actually exists as an observable store state —
// NewProcessor's publishSensibleDefaults publishes a real, non-zero
// default before any dispatch decision can happen, so a stored 0 can only
// ever mean an operator's deliberate write, never "nobody configured this
// yet". This test proves both halves: the un-configured value dispatches
// as if there were no additional cap (the non-zero default in effect),
// and an operator's explicit 0 really does block dispatch — it isn't
// mistaken for the unconfigured case.
func TestSystemMaxChargePowerDefaultIsNeverAmbiguousWithAnExplicitZero(t *testing.T) {
	p, _, clk := newProcessor(t)
	setMode(t, p, modeRemote)
	if err := p.Write(emsKey("set_active_power_kw"), -20); err != nil {
		t.Fatal(err)
	}
	if active, _ := p.ResolvePower(clk.Now(), 130.5, 130.5, 50, false, false); active != -20 {
		t.Fatalf("dispatch under the out-of-the-box default System Maximum Charge Power = %v, want -20 (unclipped default, not 0)", active)
	}

	if err := p.Write(emsKey("system_maximum_charge_power"), 0); err != nil {
		t.Fatal(err)
	}
	if active, _ := p.ResolvePower(clk.Now(), 130.5, 130.5, 50, false, false); active != 0 {
		t.Errorf("dispatch after an operator explicitly wrote System Maximum Charge Power=0 = %v, want 0 (a real limit, not confused with \"unset\")", active)
	}
}

func TestPowerOffForcesZeroDispatchRegardlessOfMode(t *testing.T) {
	p, st, clk := newProcessor(t)
	setMode(t, p, modeRemote)
	if err := p.Write(emsKey("set_active_power_kw"), 60); err != nil {
		t.Fatal(err)
	}
	st.Set(emsKey("power_on_off"), 0)
	active, reactive := p.ResolvePower(clk.Now(), 130.5, 130.5, 50, false, false)
	if active != 0 || reactive != 0 {
		t.Errorf("dispatch with Power Off = %v, %v; want 0, 0", active, reactive)
	}
}

// --- watchdog: hold / zero_after / safe_state_after ------------------------

func newProcessorWithWatchdog(t *testing.T, mode commands.WatchdogMode, timeout time.Duration) (*commands.Processor, *clock.Fake) {
	t.Helper()
	st := store.New()
	clk := clock.NewFake(time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC))
	cfg := commands.DefaultConfig()
	cfg.WatchdogMode = mode
	cfg.WatchdogTimeout = timeout
	p, err := commands.NewProcessor(st, clk, cfg)
	if err != nil {
		t.Fatal(err)
	}
	setMode(t, p, modeRemote)
	if err := p.Write(emsKey("set_active_power_kw"), 40); err != nil {
		t.Fatal(err)
	}
	return p, clk
}

func TestWatchdogHoldNeverExpires(t *testing.T) {
	p, clk := newProcessorWithWatchdog(t, commands.WatchdogHold, 10*time.Second)
	clk.Advance(10 * time.Hour)
	if active, _ := p.ResolvePower(clk.Now(), 130.5, 130.5, 50, false, false); active != 40 {
		t.Errorf("watchdog.mode=hold dispatch after 10h with no refresh = %v, want still 40", active)
	}
}

func TestWatchdogZeroAfterExpiresThenResumesOnFreshWrite(t *testing.T) {
	p, clk := newProcessorWithWatchdog(t, commands.WatchdogZeroAfter, 10*time.Second)

	clk.Advance(5 * time.Second)
	if active, _ := p.ResolvePower(clk.Now(), 130.5, 130.5, 50, false, false); active != 40 {
		t.Errorf("before timeout: dispatch = %v, want 40", active)
	}

	clk.Advance(10 * time.Second) // total 15s since the write, timeout is 10s
	if active, _ := p.ResolvePower(clk.Now(), 130.5, 130.5, 50, false, false); active != 0 {
		t.Errorf("after timeout: dispatch = %v, want 0 (zero_after)", active)
	}

	if err := p.Write(emsKey("set_active_power_kw"), 40); err != nil {
		t.Fatal(err)
	}
	if active, _ := p.ResolvePower(clk.Now(), 130.5, 130.5, 50, false, false); active != 40 {
		t.Errorf("immediately after a fresh write: dispatch = %v, want 40 (zero_after resumes immediately)", active)
	}
}

// TestWatchdogZeroAfterExactTimeoutBoundary pins the exact edge: one tick
// before the configured timeout must still dispatch normally, exactly at
// the timeout must already be stale (applyWatchdog compares with >=, not
// >), and one tick past it must obviously still be stale too.
func TestWatchdogZeroAfterExactTimeoutBoundary(t *testing.T) {
	const timeout = 10 * time.Second

	t.Run("one tick before timeout", func(t *testing.T) {
		p, clk := newProcessorWithWatchdog(t, commands.WatchdogZeroAfter, timeout)
		clk.Advance(timeout - time.Nanosecond)
		if active, _ := p.ResolvePower(clk.Now(), 130.5, 130.5, 50, false, false); active != 40 {
			t.Errorf("dispatch at timeout-1ns = %v, want 40 (not yet stale)", active)
		}
	})

	t.Run("exactly at timeout", func(t *testing.T) {
		p, clk := newProcessorWithWatchdog(t, commands.WatchdogZeroAfter, timeout)
		clk.Advance(timeout)
		if active, _ := p.ResolvePower(clk.Now(), 130.5, 130.5, 50, false, false); active != 0 {
			t.Errorf("dispatch at exactly timeout = %v, want 0 (already stale)", active)
		}
	})

	t.Run("one tick after timeout", func(t *testing.T) {
		p, clk := newProcessorWithWatchdog(t, commands.WatchdogZeroAfter, timeout)
		clk.Advance(timeout + time.Nanosecond)
		if active, _ := p.ResolvePower(clk.Now(), 130.5, 130.5, 50, false, false); active != 0 {
			t.Errorf("dispatch at timeout+1ns = %v, want 0 (stale)", active)
		}
	})
}

// TestWatchdogZeroAfterRefreshBeforeTimeoutResetsTheClock proves a refresh
// landing before the deadline pushes the deadline out by a full timeout
// from the refresh — not from the original write — so dispatch is still
// live at a point that would have been stale relative to the first write.
func TestWatchdogZeroAfterRefreshBeforeTimeoutResetsTheClock(t *testing.T) {
	p, clk := newProcessorWithWatchdog(t, commands.WatchdogZeroAfter, 10*time.Second)

	clk.Advance(9 * time.Second) // 1s before the original deadline
	if err := p.Write(emsKey("set_active_power_kw"), 40); err != nil {
		t.Fatal(err)
	}

	clk.Advance(9 * time.Second) // 18s since the original write, but only 9s since the refresh
	if active, _ := p.ResolvePower(clk.Now(), 130.5, 130.5, 50, false, false); active != 40 {
		t.Errorf("dispatch 9s after a refresh (18s after the original write) = %v, want 40 (refresh reset the deadline)", active)
	}

	clk.Advance(time.Second) // now 10s since the refresh
	if active, _ := p.ResolvePower(clk.Now(), 130.5, 130.5, 50, false, false); active != 0 {
		t.Errorf("dispatch 10s after the refresh = %v, want 0 (stale relative to the refresh, not the original write)", active)
	}
}

// TestWatchdogZeroAfterRepeatedRefreshNeverExpires proves a setpoint
// refreshed on a cadence shorter than the timeout never goes stale, no
// matter how much wall-clock time (simulated via the fake clock, no real
// sleep) passes in total.
func TestWatchdogZeroAfterRepeatedRefreshNeverExpires(t *testing.T) {
	p, clk := newProcessorWithWatchdog(t, commands.WatchdogZeroAfter, 10*time.Second)

	for i := 0; i < 20; i++ {
		clk.Advance(7 * time.Second) // always < the 10s timeout
		if err := p.Write(emsKey("set_active_power_kw"), 40); err != nil {
			t.Fatal(err)
		}
		if active, _ := p.ResolvePower(clk.Now(), 130.5, 130.5, 50, false, false); active != 40 {
			t.Fatalf("refresh %d: dispatch = %v, want 40 (never should have gone stale)", i, active)
		}
	}
}

// TestWatchdogSafeStateAfterExactTimeoutBoundary is
// TestWatchdogZeroAfterExactTimeoutBoundary's safe_state_after twin — the
// latch only engages once staleness is actually observed, so the same
// just-before/at boundary applies before the latch has ever tripped.
func TestWatchdogSafeStateAfterExactTimeoutBoundary(t *testing.T) {
	const timeout = 10 * time.Second

	t.Run("one tick before timeout", func(t *testing.T) {
		p, clk := newProcessorWithWatchdog(t, commands.WatchdogSafeStateAfter, timeout)
		clk.Advance(timeout - time.Nanosecond)
		if active, _ := p.ResolvePower(clk.Now(), 130.5, 130.5, 50, false, false); active != 40 {
			t.Errorf("dispatch at timeout-1ns = %v, want 40 (not yet stale, not latched)", active)
		}
	})

	t.Run("exactly at timeout", func(t *testing.T) {
		p, clk := newProcessorWithWatchdog(t, commands.WatchdogSafeStateAfter, timeout)
		clk.Advance(timeout)
		if active, _ := p.ResolvePower(clk.Now(), 130.5, 130.5, 50, false, false); active != 0 {
			t.Errorf("dispatch at exactly timeout = %v, want 0 (latched)", active)
		}
	})
}

func TestWatchdogSafeStateAfterLatchesUntilModeReentry(t *testing.T) {
	p, clk := newProcessorWithWatchdog(t, commands.WatchdogSafeStateAfter, 10*time.Second)

	clk.Advance(10 * time.Second)
	if active, _ := p.ResolvePower(clk.Now(), 130.5, 130.5, 50, false, false); active != 0 {
		t.Errorf("after timeout: dispatch = %v, want 0 (safe_state_after)", active)
	}

	// Unlike zero_after, a fresh setpoint write alone does not undo the trip.
	if err := p.Write(emsKey("set_active_power_kw"), 40); err != nil {
		t.Fatal(err)
	}
	if active, _ := p.ResolvePower(clk.Now(), 130.5, 130.5, 50, false, false); active != 0 {
		t.Errorf("after a fresh write, still latched: dispatch = %v, want 0", active)
	}

	// Leaving and re-entering Remote mode re-arms it.
	setMode(t, p, modeManual)
	p.ResolvePower(clk.Now(), 130.5, 130.5, 50, false, false) // observe the mode change
	setMode(t, p, modeRemote)
	if active, _ := p.ResolvePower(clk.Now(), 130.5, 130.5, 50, false, false); active != 40 {
		t.Errorf("after leaving and re-entering Remote mode: dispatch = %v, want 40 (re-armed)", active)
	}
}

// --- mode priority: Remote / Demand Control / Load Tracking ---------------

func TestModePriorityRemoteFirstIsUnaffectedByOtherFlags(t *testing.T) {
	p, _, clk := newProcessor(t) // default priority: remote, demand_control, load_tracking
	setMode(t, p, modeRemote)
	if err := p.Write(emsKey("set_active_power_kw"), 33); err != nil {
		t.Fatal(err)
	}
	if err := p.Write(emsKey("demand_control"), 1); err != nil {
		t.Fatal(err)
	}
	if err := p.Write(emsKey("enable_load_tracking"), 1); err != nil {
		t.Fatal(err)
	}
	if active, _ := p.ResolvePower(clk.Now(), 130.5, 130.5, 50, false, false); active != 33 {
		t.Errorf("dispatch with remote first in priority = %v, want 33 (remote still wins)", active)
	}
}

// TestModePriorityDemandControlOutranksRemote proves modes.priority is a
// genuine, live config parameter (§7) and not dead code: reordering it so
// demand_control outranks remote changes dispatch even though Set Active
// Power itself is unchanged and still valid.
func TestModePriorityDemandControlOutranksRemote(t *testing.T) {
	st := store.New()
	clk := clock.NewFake(time.Now())
	cfg := commands.DefaultConfig()
	cfg.ModePriority = []string{"demand_control", "remote", "load_tracking"}
	p, err := commands.NewProcessor(st, clk, cfg)
	if err != nil {
		t.Fatal(err)
	}
	setMode(t, p, modeRemote)
	if err := p.Write(emsKey("set_active_power_kw"), 33); err != nil {
		t.Fatal(err)
	}
	if err := p.Write(emsKey("demand_control"), 1); err != nil {
		t.Fatal(err)
	}
	active, _ := p.ResolvePower(clk.Now(), 130.5, 130.5, 50, false, false)
	if active != 0 {
		t.Errorf("dispatch with demand_control outranking remote (and enabled) = %v, want 0 (no confirmed demand-control formula)", active)
	}

	d, ok := p.DiagnosticFor(emsKey("demand_control"))
	if !ok {
		t.Fatal("no Diagnostic recorded for demand_control winning priority")
	}
	if d.Code != commands.DiagCodeAcceptedButUnsupported {
		t.Errorf("Diagnostic.Code = %q, want %q", d.Code, commands.DiagCodeAcceptedButUnsupported)
	}
	if d.AcceptedValue != 1 {
		t.Errorf("Diagnostic.AcceptedValue = %v, want 1", d.AcceptedValue)
	}
	if d.SelectedMode != "demand_control" {
		t.Errorf("Diagnostic.SelectedMode = %q, want %q", d.SelectedMode, "demand_control")
	}
}

// TestModePriorityLoadTrackingOutranksRemote is
// TestModePriorityDemandControlOutranksRemote's Load Tracking twin —
// separately exercised since the two points (and their diagnostic's
// SelectedMode) are distinct, not just two names for the same code path.
func TestModePriorityLoadTrackingOutranksRemote(t *testing.T) {
	st := store.New()
	clk := clock.NewFake(time.Now())
	cfg := commands.DefaultConfig()
	cfg.ModePriority = []string{"load_tracking", "remote", "demand_control"}
	p, err := commands.NewProcessor(st, clk, cfg)
	if err != nil {
		t.Fatal(err)
	}
	setMode(t, p, modeRemote)
	if err := p.Write(emsKey("set_active_power_kw"), 33); err != nil {
		t.Fatal(err)
	}
	if err := p.Write(emsKey("enable_load_tracking"), 1); err != nil {
		t.Fatal(err)
	}
	active, _ := p.ResolvePower(clk.Now(), 130.5, 130.5, 50, false, false)
	if active != 0 {
		t.Errorf("dispatch with load_tracking outranking remote (and enabled) = %v, want 0", active)
	}

	d, ok := p.DiagnosticFor(emsKey("enable_load_tracking"))
	if !ok {
		t.Fatal("no Diagnostic recorded for load_tracking winning priority")
	}
	if d.SelectedMode != "load_tracking" {
		t.Errorf("Diagnostic.SelectedMode = %q, want %q", d.SelectedMode, "load_tracking")
	}
}

// TestDemandControlAndLoadTrackingWriteUpdatesStoreAndReadbackRegardless
// confirms item 6's "the write is accepted; only dispatch is unaffected"
// distinction — enabling one of these must not be rejected or fail to
// mirror to readback just because it has no modeled power effect.
func TestDemandControlAndLoadTrackingWriteUpdatesStoreAndReadbackRegardless(t *testing.T) {
	p, st, _ := newProcessor(t)
	for _, slug := range []string{"demand_control", "enable_load_tracking"} {
		if err := p.Write(emsKey(slug), 1); err != nil {
			t.Fatalf("Write(%s, 1): %v", slug, err)
		}
		if v, _ := st.Get(emsKey(slug)); v != 1 {
			t.Errorf("%s stored value = %v, want 1", slug, v)
		}
		meta := m261points.Points[emsKey(slug)]
		rbAddr := store.IECAddr{CommonAddr: meta.DeviceAddr, ObjAddr: *meta.ReadbackIEC104Addr}
		_, rbValue, ok := st.GetByIEC(rbAddr)
		if !ok || rbValue != 1 {
			t.Errorf("%s readback = %v, %v; want 1, true", slug, rbValue, ok)
		}
	}
}
