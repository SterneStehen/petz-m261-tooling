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

// TestWriteRejectsF32AboveMaxFloat32 is distinct from
// TestWriteRejectsNonFiniteF32: 1e40 is a perfectly finite float64 (well
// under float64's own ~1.8e308 ceiling), but it overflows float32's
// ~3.4e38 ceiling — casting it to float32 silently produces +Inf, which
// EncodeF32 would then encode as a real, on-wire infinity if this weren't
// caught first. NaN/Inf-in-float64 and finite-float64-that-overflows-
// float32 are two different failure shapes, both required to be rejected.
func TestWriteRejectsF32AboveMaxFloat32(t *testing.T) {
	p, _, _ := newProcessor(t)
	for _, v := range []float64{1e40, -1e40} {
		if err := p.Write(emsKey("set_active_power_kw"), v); err == nil {
			t.Errorf("Write(set_active_power_kw, %v) accepted a value finite in float64 but overflowing float32", v)
		} else if !errors.Is(err, commands.ErrInvalidValue) {
			t.Errorf("value %v: error = %v, want ErrInvalidValue", v, err)
		}
	}
}

// TestWriteRejectsFractionalI16Value is the review-required proof that a
// non-enum I16 point rejects a fractional value outright rather than
// rounding it — System Maximum Charge Power has no Enum, so this
// specifically exercises the representability check's own integrality
// rule, independent of enum membership.
func TestWriteRejectsFractionalI16Value(t *testing.T) {
	p, st, _ := newProcessor(t)
	before, _ := st.Get(emsKey("system_maximum_charge_power"))
	if err := p.Write(emsKey("system_maximum_charge_power"), 5.5); err == nil {
		t.Error("Write(system_maximum_charge_power, 5.5) accepted a fractional I16 value")
	} else if !errors.Is(err, commands.ErrInvalidValue) {
		t.Errorf("error = %v, want ErrInvalidValue", err)
	}
	if after, _ := st.Get(emsKey("system_maximum_charge_power")); after != before {
		t.Errorf("store changed to %v after a rejected fractional write, want unchanged %v", after, before)
	}
}

// TestWriteI16ExactBoundaries checks underflow and overflow as separate,
// exact cases on both sides of the valid [-32768, 32767] domain — not
// just "some out-of-range value is rejected somewhere".
func TestWriteI16ExactBoundaries(t *testing.T) {
	cases := []struct {
		value float64
		want  bool // true = accepted
	}{
		{32767, true},   // max valid
		{32768, false},  // one past max: overflow
		{-32768, true},  // min valid
		{-32769, false}, // one past min: underflow
	}
	for _, c := range cases {
		p, _, _ := newProcessor(t)
		err := p.Write(emsKey("system_maximum_charge_power"), c.value)
		accepted := err == nil
		if accepted != c.want {
			t.Errorf("Write(system_maximum_charge_power, %v): accepted=%v, want %v (err=%v)", c.value, accepted, c.want, err)
		}
	}
}

// TestWriteRejectsFractionalEnumValue is the review-required fix
// verification: Set Operating Mode = 1.4 must be rejected outright, never
// silently accepted as enum value 1 via rounding.
func TestWriteRejectsFractionalEnumValue(t *testing.T) {
	p, st, _ := newProcessor(t)
	before, _ := st.Get(emsKey("set_operating_mode"))
	if err := p.Write(emsKey("set_operating_mode"), 1.4); err == nil {
		t.Error("Write(set_operating_mode, 1.4) accepted a fractional value (silently rounded to enum 1)")
	} else if !errors.Is(err, commands.ErrInvalidValue) {
		t.Errorf("error = %v, want ErrInvalidValue", err)
	}
	if after, _ := st.Get(emsKey("set_operating_mode")); after != before {
		t.Errorf("store changed to %v after a rejected fractional enum write, want unchanged %v", after, before)
	}
}

// TestWriteRejectsNaNAndInfForEnumPoint is the review-required fix
// verification for the early-return bug: an enum-bearing point used to
// skip the finite check entirely (int(math.Round(NaN)) is
// implementation-defined in Go, and could coincidentally match a real
// enum key like 0). NaN/+Inf/-Inf must all be rejected for an enum point
// exactly like any other.
func TestWriteRejectsNaNAndInfForEnumPoint(t *testing.T) {
	p, st, _ := newProcessor(t)
	before, _ := st.Get(emsKey("set_operating_mode"))
	for _, v := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if err := p.Write(emsKey("set_operating_mode"), v); err == nil {
			t.Errorf("Write(set_operating_mode, %v) accepted a non-finite value on an enum point", v)
		} else if !errors.Is(err, commands.ErrInvalidValue) {
			t.Errorf("value %v: error = %v, want ErrInvalidValue", v, err)
		}
	}
	if after, _ := st.Get(emsKey("set_operating_mode")); after != before {
		t.Errorf("store changed to %v after rejected non-finite writes, want unchanged %v", after, before)
	}
}

// --- isolated metadata fixtures --------------------------------------------
//
// The real catalog gives every one of the 148 setpoints scale=1 and
// range=nil — there is no real point to exercise a non-unit scale or a
// confirmed business range against. m261points.Points is an exported
// package-level map, so withTemporaryMeta overwrites one real point's
// metadata for the lifetime of a single test (restored on cleanup) rather
// than touching the generated catalog file itself — the Go-side
// equivalent of tests/test_range_propagation.py's temporary
// catalog/overrides.yaml fixtures on the Python side. Safe here because
// this file never calls t.Parallel(): tests run strictly sequentially, so
// there's no window for one test's fixture to leak into another's.

func withTemporaryMeta(t *testing.T, key m261points.PointKey, mutate func(*m261points.PointMeta)) {
	t.Helper()
	original, ok := m261points.Points[key]
	if !ok {
		t.Fatalf("withTemporaryMeta: %+v is not a real catalog point", key)
	}
	modified := original
	mutate(&modified)
	m261points.Points[key] = modified
	t.Cleanup(func() { m261points.Points[key] = original })
}

func f64(v float64) *float64 { return &v }

func TestValidateRejectsZeroScale(t *testing.T) {
	p, st, _ := newProcessor(t)
	key := emsKey("start_charge_power_kw")
	withTemporaryMeta(t, key, func(m *m261points.PointMeta) { m.Scale = 0 })

	before, _ := st.Get(key)
	if err := p.Write(key, 5); !errors.Is(err, commands.ErrInvalidValue) {
		t.Errorf("Write with scale=0 = %v, want ErrInvalidValue", err)
	}
	if after, _ := st.Get(key); after != before {
		t.Errorf("store changed to %v after a rejected zero-scale write, want unchanged %v", after, before)
	}
}

func TestValidateRejectsNonFiniteScale(t *testing.T) {
	for _, scale := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		p, _, _ := newProcessor(t)
		key := emsKey("start_discharge_power_kw")
		withTemporaryMeta(t, key, func(m *m261points.PointMeta) { m.Scale = scale })

		if err := p.Write(key, 5); !errors.Is(err, commands.ErrInvalidValue) {
			t.Errorf("scale=%v: Write = %v, want ErrInvalidValue", scale, err)
		}
	}
}

// TestValidateNonUnitScaleBoundary confirms raw = engineering_value/scale
// is what's actually checked against the I16 domain, not
// engineering_value directly — a fixture the real catalog (scale=1
// everywhere) can never exercise on its own. Scale 0.5 is picked
// deliberately: it's an exact power of two, so division by it introduces
// no IEEE-754 rounding error (unlike, say, 0.1, which isn't exactly
// representable in binary and would make 3276.7/0.1 land a hair off
// 32767 for reasons that have nothing to do with the code under test).
// engineering 16383.5 -> raw 32767 (valid, the exact I16 max);
// engineering 16384.0 -> raw 32768 (invalid, one past it).
func TestValidateNonUnitScaleBoundary(t *testing.T) {
	key := emsKey("adjustment_interval_seconds")

	p, st, _ := newProcessor(t)
	withTemporaryMeta(t, key, func(m *m261points.PointMeta) { m.Scale = 0.5 })
	if err := p.Write(key, 16383.5); err != nil {
		t.Errorf("Write(%v) with scale=0.5 (raw=32767, exactly I16 max) = %v, want accepted", 16383.5, err)
	}
	if v, _ := st.Get(key); v != 16383.5 {
		t.Errorf("stored value = %v, want 16383.5 (engineering value, not raw)", v)
	}

	p2, _, _ := newProcessor(t)
	withTemporaryMeta(t, key, func(m *m261points.PointMeta) { m.Scale = 0.5 })
	if err := p2.Write(key, 16384.0); !errors.Is(err, commands.ErrInvalidValue) {
		t.Errorf("Write(%v) with scale=0.5 (raw=32768, one past I16 max) = %v, want ErrInvalidValue", 16384.0, err)
	}
}

// TestValidateDecimalScaleBoundary is
// TestValidateNonUnitScaleBoundary's review-required decimal-scale
// complement: 0.5 is an exact power of two, so raw = value/scale
// introduces no floating-point rounding of its own — it doesn't actually
// exercise the tolerance snapToIntegerWithinTolerance adds. 0.1 is not
// exactly representable in binary, so 3276.7/0.1 evaluates to
// 32766.999999999996 in float64, a few ULPs off the true 32767 even
// though the mathematical quotient is exact — this must still be
// accepted. A genuinely fractional neighbor (3276.75, raw≈32767.5, half
// an integer away — nowhere near the ULP-scale tolerance) must still be
// rejected at the very same scale, proving the tolerance doesn't
// swallow real fractional intent just because the scale is decimal.
func TestValidateDecimalScaleBoundary(t *testing.T) {
	key := emsKey("adjustment_interval_seconds")

	p, st, _ := newProcessor(t)
	withTemporaryMeta(t, key, func(m *m261points.PointMeta) { m.Scale = 0.1 })
	if err := p.Write(key, 3276.7); err != nil {
		t.Errorf("Write(%v) with scale=0.1 (raw=32767 mathematically, 32766.999999999996 in float64) = %v, want accepted", 3276.7, err)
	}
	if v, _ := st.Get(key); v != 3276.7 {
		t.Errorf("stored value = %v, want 3276.7 (engineering value, not raw)", v)
	}

	p2, _, _ := newProcessor(t)
	withTemporaryMeta(t, key, func(m *m261points.PointMeta) { m.Scale = 0.1 })
	if err := p2.Write(key, 3276.75); !errors.Is(err, commands.ErrInvalidValue) {
		t.Errorf("Write(%v) with scale=0.1 (raw≈32767.5, genuinely fractional, not a rounding artifact) = %v, want ErrInvalidValue", 3276.75, err)
	}
}

// TestWriteRejectsNearIntegerButFractionalI16Value is the third review's
// required negative case for snapToIntegerWithinTolerance: the tolerance
// must be tight enough, in absolute (ULP) terms, that a value merely
// close to an integer -- not a division-rounding artifact of one -- is
// still rejected as fractional, not silently snapped to that integer.
//
// This is exactly the failure mode the earlier 1e-9-relative-tolerance
// version had: 1e-9 * |raw| is about 3.28e-5 at raw=32767, and
// 32767.00001 is only 1e-5 away from 32767 -- inside that window, so the
// old code snapped it to 32767 and accepted it. Measured in the ULPs the
// fixed version actually bounds tolerance by, 32767.00001 sits roughly
// 2.75 million float64 ULPs from 32767 -- nothing like the single-ULP gap
// a real division rounding error produces (see
// TestValidateDecimalScaleBoundary) -- so it must be rejected outright.
//
// Uses scale=1 (system_maximum_charge_power's real, unmodified catalog
// scale, like TestWriteI16ExactBoundaries) rather than a fixture: at
// scale=1, raw == value exactly (division by 1 is exact, no float64
// rounding of its own), so this isolates the tolerance check itself from
// any scale-division rounding, which is deliberately exercised separately
// above. The negative boundary (-32767.00001, the same magnitude mirrored
// across zero) is included rather than assumed: float64 ULP spacing is
// symmetric around zero, but snapToIntegerWithinTolerance's use of
// math.Nextafter(rounded, +Inf) for a negative rounded is exactly the
// kind of sign-handling detail worth a direct assertion instead of a
// belief.
func TestWriteRejectsNearIntegerButFractionalI16Value(t *testing.T) {
	for _, v := range []float64{32767.00001, -32767.00001} {
		p, st, _ := newProcessor(t)
		before, _ := st.Get(emsKey("system_maximum_charge_power"))
		if err := p.Write(emsKey("system_maximum_charge_power"), v); err == nil {
			t.Errorf("Write(system_maximum_charge_power, %v) accepted a value ~1e-5 off an integer as if it were that integer", v)
		} else if !errors.Is(err, commands.ErrInvalidValue) {
			t.Errorf("value %v: error = %v, want ErrInvalidValue", v, err)
		}
		if after, _ := st.Get(emsKey("system_maximum_charge_power")); after != before {
			t.Errorf("value %v: store changed to %v after a rejected write, want unchanged %v", v, after, before)
		}
	}
}

func TestValidateRangeMinOnly(t *testing.T) {
	p, st, _ := newProcessor(t)
	key := emsKey("anti_reverse_power_margin_kw")
	withTemporaryMeta(t, key, func(m *m261points.PointMeta) { m.Range = &m261points.Range{Min: f64(10)} })

	if err := p.Write(key, 9); !errors.Is(err, commands.ErrInvalidValue) {
		t.Errorf("Write(9) below confirmed min 10 = %v, want ErrInvalidValue", err)
	}
	if err := p.Write(key, 10); err != nil {
		t.Errorf("Write(10) at the confirmed min = %v, want accepted", err)
	}
	if err := p.Write(key, 1000); err != nil {
		t.Errorf("Write(1000), min-only range has no upper bound, = %v, want accepted", err)
	}
	if v, _ := st.Get(key); v != 1000 {
		t.Errorf("stored value = %v, want 1000", v)
	}
}

func TestValidateRangeMaxOnly(t *testing.T) {
	p, _, _ := newProcessor(t)
	key := emsKey("adjustment_interval_seconds")
	withTemporaryMeta(t, key, func(m *m261points.PointMeta) { m.Range = &m261points.Range{Max: f64(300)} })

	if err := p.Write(key, 301); !errors.Is(err, commands.ErrInvalidValue) {
		t.Errorf("Write(301) above confirmed max 300 = %v, want ErrInvalidValue", err)
	}
	if err := p.Write(key, 300); err != nil {
		t.Errorf("Write(300) at the confirmed max = %v, want accepted", err)
	}
	if err := p.Write(key, -1000); err != nil {
		t.Errorf("Write(-1000), max-only range has no lower bound, = %v, want accepted", err)
	}
}

func TestValidateRangeMinAndMax(t *testing.T) {
	p, _, _ := newProcessor(t)
	key := emsKey("start_charge_power_kw")
	withTemporaryMeta(t, key, func(m *m261points.PointMeta) { m.Range = &m261points.Range{Min: f64(0), Max: f64(100)} })

	for _, v := range []float64{-1, 101} {
		if err := p.Write(key, v); !errors.Is(err, commands.ErrInvalidValue) {
			t.Errorf("Write(%v) outside [0, 100] = %v, want ErrInvalidValue", v, err)
		}
	}
	for _, v := range []float64{0, 50, 100} {
		if err := p.Write(key, v); err != nil {
			t.Errorf("Write(%v) inside [0, 100] = %v, want accepted", v, err)
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

// TestNegativeSystemMaxChargePowerBlocksRatherThanReversesCharge is the
// review-required fix verification: System Maximum Charge Power is
// representable as negative on the wire (no confirmed business range
// exists to reject it as out-of-range at write time), and a negative
// configured cap must never flip a charge request into discharge. Before
// the fix: chargeCapKW = min(-10, 130.5) = -10, then
// math.Max(-50, -(-10)) = math.Max(-50, 10) = +10 — a charge request
// became a discharge result. The correct behavior is "no charging
// allowed" (0), not sign reversal.
func TestNegativeSystemMaxChargePowerBlocksRatherThanReversesCharge(t *testing.T) {
	p, st, clk := newProcessor(t)
	setMode(t, p, modeRemote)
	st.Set(emsKey("system_maximum_charge_power"), -10)
	if err := p.Write(emsKey("set_active_power_kw"), -50); err != nil {
		t.Fatal(err)
	}
	active, _ := p.ResolvePower(clk.Now(), 130.5, 130.5, 50, false, false)
	if active > 0 {
		t.Fatalf("dispatched active power = %v, a charge request must never become positive (discharge)", active)
	}
	if active != 0 {
		t.Errorf("dispatched active power = %v, want 0 (a negative configured limit blocks charging, not a reversed +10)", active)
	}
}

// TestNegativeSystemMaxDischargePowerBlocksRatherThanReversesDischarge is
// TestNegativeSystemMaxChargePowerBlocksRatherThanReversesCharge's
// discharge-side twin: dischargeCapKW = min(-10, 130.5) = -10, then
// math.Min(50, -10) = -10 would turn a discharge request into a charge
// result.
func TestNegativeSystemMaxDischargePowerBlocksRatherThanReversesDischarge(t *testing.T) {
	p, st, clk := newProcessor(t)
	setMode(t, p, modeRemote)
	st.Set(emsKey("system_maximum_discharge_power"), -10)
	if err := p.Write(emsKey("set_active_power_kw"), 50); err != nil {
		t.Fatal(err)
	}
	active, _ := p.ResolvePower(clk.Now(), 130.5, 130.5, 50, false, false)
	if active < 0 {
		t.Fatalf("dispatched active power = %v, a discharge request must never become negative (charge)", active)
	}
	if active != 0 {
		t.Errorf("dispatched active power = %v, want 0 (a negative configured limit blocks discharging, not a reversed -10)", active)
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
// demand_control outranks remote still records a Diagnostic naming the
// winner, even though — per AGENT-TASK §6 item 6 — dispatch itself must
// stay exactly as if demand_control were absent (Set Active Power, not a
// forced 0): there is no confirmed formula to compute a power value from
// demand_control, so the only defensible behavior is to leave dispatch
// alone and flag the fact, not to invent a zero.
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
	if active != 33 {
		t.Errorf("dispatch with demand_control outranking remote (and enabled) = %v, want 33 (unaffected — no confirmed demand-control formula, so dispatch stays as if it were absent)", active)
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
	if active != 33 {
		t.Errorf("dispatch with load_tracking outranking remote (and enabled) = %v, want 33 (unaffected)", active)
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

// --- Reset (Task 7 item 7) -------------------------------------------------

// TestResetClearsWatchdogLatchAndDiagnostics dirties every piece of
// internal state Reset is documented to clear, then confirms all three
// are gone: the watchdog timer (a fresh Remote-mode dispatch after Reset
// must not immediately look stale just because a write happened long
// before Reset), the safe_state_after latch, and accumulated Diagnostics.
func TestResetClearsWatchdogLatchAndDiagnostics(t *testing.T) {
	p, clk := newProcessorWithWatchdog(t, commands.WatchdogSafeStateAfter, 10*time.Second)

	// Latch the watchdog.
	clk.Advance(10 * time.Second)
	if active, _ := p.ResolvePower(clk.Now(), 130.5, 130.5, 50, false, false); active != 0 {
		t.Fatalf("setup: dispatch after timeout = %v, want 0 (latched)", active)
	}

	// Accumulate a Diagnostic (Trip, accepted_but_unsupported).
	dangerousP, _, _ := newAllowDangerousProcessor(t)
	if err := dangerousP.Write(emsKey("trip"), 1); err != nil {
		t.Fatalf("Write(trip, 1): %v", err)
	}
	if len(dangerousP.Diagnostics()) == 0 {
		t.Fatal("setup: expected at least one Diagnostic after Write(trip, 1)")
	}
	dangerousP.Reset()
	if got := dangerousP.Diagnostics(); len(got) != 0 {
		t.Errorf("Diagnostics() after Reset = %v, want empty", got)
	}

	p.Reset()

	// Watchdog: re-entering Remote and writing a fresh setpoint must
	// dispatch immediately — if the latch or timer had survived Reset,
	// this would still read 0.
	setMode(t, p, modeRemote)
	if err := p.Write(emsKey("set_active_power_kw"), 40); err != nil {
		t.Fatal(err)
	}
	if active, _ := p.ResolvePower(clk.Now(), 130.5, 130.5, 50, false, false); active != 40 {
		t.Errorf("dispatch immediately after Reset + fresh Remote setpoint = %v, want 40 (watchdog/latch cleared)", active)
	}
}

// TestResetDoesNotTouchStore confirms Reset's documented scope: it resets
// the Processor's own internal bookkeeping only — the caller (Task 7's
// controlapi) is responsible for restoring Store values separately (e.g.
// store.Store.Restore). A setpoint written before Reset must still read
// back the same value after.
func TestResetDoesNotTouchStore(t *testing.T) {
	p, st, _ := newProcessor(t)
	if err := p.Write(emsKey("maximum_charge_soc"), 77); err != nil {
		t.Fatal(err)
	}
	p.Reset()
	if v, _ := st.Get(emsKey("maximum_charge_soc")); v != 77 {
		t.Errorf("maximum_charge_soc after Reset = %v, want unchanged 77 — Reset must not touch the Store", v)
	}
}
