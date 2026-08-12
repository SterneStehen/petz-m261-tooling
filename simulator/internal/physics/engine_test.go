package physics

import (
	"math"
	"testing"
	"time"
)

func testParams() Params {
	return DefaultParams()
}

// --- Task 5 acceptance criteria, verbatim ---------------------------------

func TestChargeTwentyToEightyIsPhysicallyPlausible(t *testing.T) {
	p := testParams()
	e := New(p, 20)
	const step = 30 * time.Second
	elapsed := time.Duration(0)
	for e.State().SoCPercent < 80 {
		e.Step(step, -p.NominalACPowerKW, 0) // negative = charge, at full nominal rate
		elapsed += step
		if elapsed > 6*time.Hour {
			t.Fatal("did not reach 80% SoC within 6 simulated hours — charge rate implausible")
		}
	}
	// Expected: 60% of 261 kWh = 156.6 kWh stored, at 130.5 kW * sqrt(0.92)
	// effective charge rate -> ~1h15m. A real system doing 60% of its
	// capacity at its own continuous nominal rating in under 15 minutes or
	// over 4 hours would not be "physically plausible" by any reading.
	if elapsed < 15*time.Minute || elapsed > 4*time.Hour {
		t.Errorf("20%%->80%% charge took %v, not in the physically plausible [15m, 4h] band", elapsed)
	}
	t.Logf("20%% -> 80%% charge at nominal power took %v", elapsed)
}

func TestSoCNeverExceedsBoundsUnderSustainedOverdrive(t *testing.T) {
	p := testParams()
	e := New(p, 50)
	// Request far more power than the battery/PCS could ever deliver, in
	// both directions, for long enough to hit both ends.
	for i := 0; i < 20000; i++ {
		e.Step(time.Second, -100000, 0) // absurd charge request
	}
	if soc := e.State().SoCPercent; soc < 0 || soc > 100 {
		t.Fatalf("SoC = %v after sustained overcharge, want within [0, 100]", soc)
	}
	for i := 0; i < 20000; i++ {
		e.Step(time.Second, 100000, 0) // absurd discharge request
	}
	if soc := e.State().SoCPercent; soc < 0 || soc > 100 {
		t.Fatalf("SoC = %v after sustained overdischarge, want within [0, 100]", soc)
	}
}

func TestVoltageNeverExceedsBounds(t *testing.T) {
	p := testParams()
	e := New(p, 0)
	for soc := 0; soc <= 100; soc++ {
		e.storedEnergyKWh = float64(soc) / 100 * p.CapacityKWh
		e.Step(time.Second, 0, 0)
		v := e.State().PackVoltageV
		if v < p.VoltageMin || v > p.VoltageMax {
			t.Fatalf("at SoC=%d%%, PackVoltageV = %v, want within [%v, %v]", soc, v, p.VoltageMin, p.VoltageMax)
		}
	}
}

func TestProhibitionFlagsSetAtSOCBoundaries(t *testing.T) {
	p := testParams()
	// Starts well outside the taper band (not right at the edge) so the
	// test exercises the actual approach-then-terminate behavior, not just
	// an immediate snap.
	e := New(p, 95)
	for i := 0; i < 5000 && !e.State().ChargeProhibited; i++ {
		e.Step(time.Second, -p.NominalACPowerKW, 0)
	}
	s := e.State()
	if !s.ChargeProhibited {
		t.Fatal("ChargeProhibited never set after driving SoC to 100%")
	}
	if s.DischargeProhibited {
		t.Error("DischargeProhibited incorrectly set at full SoC")
	}
	if s.SoCPercent != 100 {
		t.Errorf("SoC = %v at charge prohibition, want exactly 100", s.SoCPercent)
	}

	e2 := New(p, 5)
	for i := 0; i < 5000 && !e2.State().DischargeProhibited; i++ {
		e2.Step(time.Second, p.NominalACPowerKW, 0)
	}
	s2 := e2.State()
	if !s2.DischargeProhibited {
		t.Fatal("DischargeProhibited never set after driving SoC to 0%")
	}
	if s2.ChargeProhibited {
		t.Error("ChargeProhibited incorrectly set at empty SoC")
	}
	if s2.SoCPercent != 0 {
		t.Errorf("SoC = %v at discharge prohibition, want exactly 0", s2.SoCPercent)
	}
}

func TestEnergyBalanceWithinHalfPercent(t *testing.T) {
	p := testParams()
	e := New(p, 50) // mid-range, safely clear of the SoC taper band both directions
	const chargePowerKW = 50.0
	const steps = 600 // 600 x 10s = 1h40m, well short of the taper zone at this power
	const step = 10 * time.Second

	before := e.State().SoCPercent / 100 * p.CapacityKWh
	var integratedPowerKWh float64
	for i := 0; i < steps; i++ {
		actual := e.Step(step, -chargePowerKW, 0)
		integratedPowerKWh += -actual * step.Hours() // energy delivered TO the battery, unscaled by efficiency
	}
	after := e.State().SoCPercent / 100 * p.CapacityKWh

	predictedDelta := integratedPowerKWh * math.Sqrt(p.RoundTripEfficiency)
	actualDelta := after - before
	relErr := math.Abs(actualDelta-predictedDelta) / predictedDelta
	if relErr > 0.005 {
		t.Errorf(
			"energy balance error %.4f%% (integral %.4f kWh * sqrt(eff) = predicted %.4f kWh, actual stored delta %.4f kWh) — want < 0.5%%",
			relErr*100, integratedPowerKWh, predictedDelta, actualDelta,
		)
	}
}

func TestOverheatingReducesMaxPower(t *testing.T) {
	p := testParams()
	// Exaggerated relative to the realistic default (a well-cooled pack
	// isn't supposed to overheat at its own rated power) so the rise from
	// ambient to past the derate ceiling happens in well under a second of
	// simulated time, but still gradual — tens of one-second steps, not one
	// — so thermal inertia is actually exercised, not skipped over.
	p.HeatCoefficientKWPerKW2 = 0.005
	p.ThermalMassKWhPerC = 0.05
	p.ChillerCapacityKW = 0 // disable cooling so temperature climbs monotonically
	e := New(p, 50)

	nominalMax := e.State().MaxDischargeableKW
	for i := 0; i < 2000 && e.State().BatteryTempC <= p.DerateStartTempC; i++ {
		e.Step(time.Second, p.NominalACPowerKW, 0)
	}
	s := e.State()
	if s.BatteryTempC <= p.DerateStartTempC {
		t.Fatalf("battery never exceeded the %v°C derate threshold (reached %v°C) — test setup problem, not a real assertion", p.DerateStartTempC, s.BatteryTempC)
	}
	if s.MaxDischargeableKW >= nominalMax {
		t.Errorf("MaxDischargeableKW = %v at %v°C, want less than the nominal %v", s.MaxDischargeableKW, s.BatteryTempC, nominalMax)
	}
	if s.MaxChargeableKW >= nominalMax {
		t.Errorf("MaxChargeableKW = %v at %v°C, want less than the nominal %v", s.MaxChargeableKW, s.BatteryTempC, nominalMax)
	}

	// And fully above the ceiling, power must reach exactly 0.
	for i := 0; i < 5000 && e.State().BatteryTempC <= p.DerateFullAtTempC; i++ {
		e.Step(time.Second, p.NominalACPowerKW, 0)
	}
	if s := e.State(); s.BatteryTempC > p.DerateFullAtTempC && s.MaxDischargeableKW != 0 {
		t.Errorf("MaxDischargeableKW = %v at %v°C (>= derate-full %v°C), want 0", s.MaxDischargeableKW, s.BatteryTempC, p.DerateFullAtTempC)
	}
}

func TestDeterministicForFixedSeed(t *testing.T) {
	p := testParams()
	p.RNGSeed = 42

	run := func() State {
		e := New(p, 30)
		for i := 0; i < 500; i++ {
			e.Step(time.Second, -20+float64(i%7), float64(i%3))
		}
		return e.State()
	}

	a, b := run(), run()
	if a.SoCPercent != b.SoCPercent || a.BatteryTempC != b.BatteryTempC {
		t.Fatalf("two runs with the same seed diverged: SoC %v vs %v, temp %v vs %v", a.SoCPercent, b.SoCPercent, a.BatteryTempC, b.BatteryTempC)
	}
	for i := range a.CellVoltagesMV {
		if a.CellVoltagesMV[i] != b.CellVoltagesMV[i] {
			t.Fatalf("cell voltage %d diverged between two same-seed runs: %v vs %v", i, a.CellVoltagesMV[i], b.CellVoltagesMV[i])
		}
	}
	for i := range a.CellTemperaturesC {
		if a.CellTemperaturesC[i] != b.CellTemperaturesC[i] {
			t.Fatalf("cell temperature %d diverged between two same-seed runs: %v vs %v", i, a.CellTemperaturesC[i], b.CellTemperaturesC[i])
		}
	}
}

func TestDifferentSeedsProduceDifferentCellBias(t *testing.T) {
	p1, p2 := testParams(), testParams()
	p1.RNGSeed, p2.RNGSeed = 1, 2
	e1, e2 := New(p1, 50), New(p2, 50)
	s1, s2 := e1.State(), e2.State()
	same := true
	for i := range s1.CellVoltagesMV {
		if s1.CellVoltagesMV[i] != s2.CellVoltagesMV[i] {
			same = false
			break
		}
	}
	if same {
		t.Error("two different RNG seeds produced identical cell voltage bias — seed isn't being used")
	}
}

// --- Task 5's 8 numbered requirements, beyond the acceptance criteria ----

func TestCellVoltagePointCountAndSpread(t *testing.T) {
	p := testParams()
	e := New(p, 50)
	s := e.State()
	if len(s.CellVoltagesMV) != 240 {
		t.Fatalf("len(CellVoltagesMV) = %d, want 240 (§3.3)", len(s.CellVoltagesMV))
	}
	baseMV := lfpCellVoltage(50) * 1000
	for i, v := range s.CellVoltagesMV {
		if math.Abs(v-baseMV) > p.CellVoltageSpreadMV+1e-9 {
			t.Fatalf("cell %d voltage %v strays further than the configured spread %v from base %v", i, v, p.CellVoltageSpreadMV, baseMV)
		}
	}
}

func TestCellTemperaturePointCountAndSpread(t *testing.T) {
	p := testParams()
	e := New(p, 50)
	s := e.State()
	if len(s.CellTemperaturesC) != 140 {
		t.Fatalf("len(CellTemperaturesC) = %d, want 140 (§3.3)", len(s.CellTemperaturesC))
	}
	for i, temp := range s.CellTemperaturesC {
		if math.Abs(temp-s.BatteryTempC) > p.CellTempSpreadC+1e-9 {
			t.Fatalf("cell %d temperature %v strays further than the configured spread %v from battery temp %v", i, temp, p.CellTempSpreadC, s.BatteryTempC)
		}
	}
}

func TestSingleCellImbalanceOverride(t *testing.T) {
	e := New(testParams(), 50)
	e.SetCellVoltageOverrideMV(17, 2900) // clearly out of the normal spread band
	if got := e.State().CellVoltagesMV[17]; got != 2900 {
		t.Fatalf("overridden cell 17 = %v, want 2900", got)
	}
	// A step must not clobber the override.
	e.Step(time.Second, 0, 0)
	if got := e.State().CellVoltagesMV[17]; got != 2900 {
		t.Fatalf("override lost after Step: cell 17 = %v, want 2900", got)
	}
	e.ClearCellVoltageOverride(17)
	e.Step(time.Second, 0, 0)
	if got := e.State().CellVoltagesMV[17]; got == 2900 {
		t.Fatal("ClearCellVoltageOverride did not actually clear the override")
	}
}

func TestSingleCellTemperatureImbalanceOverride(t *testing.T) {
	e := New(testParams(), 50)
	e.SetCellTemperatureOverrideC(3, 80)
	if got := e.State().CellTemperaturesC[3]; got != 80 {
		t.Fatalf("overridden cell temp 3 = %v, want 80", got)
	}
	e.ClearCellTemperatureOverride(3)
	e.Step(time.Second, 0, 0)
	if got := e.State().CellTemperaturesC[3]; got == 80 {
		t.Fatal("ClearCellTemperatureOverride did not actually clear the override")
	}
}

func TestPCSElectricalsConsistentWithPower(t *testing.T) {
	p := testParams()
	e := New(p, 50)
	e.Step(time.Second, 65.25, 0) // half of nominal, discharge, no reactive
	s := e.State()

	if s.PowerFactor < 0.99 {
		t.Errorf("PowerFactor = %v with zero reactive power request, want ~1", s.PowerFactor)
	}
	if s.FrequencyHz != 50 {
		t.Errorf("FrequencyHz = %v, want the grid nominal 50", s.FrequencyHz)
	}
	for i, v := range s.PhaseVoltagesV {
		want := p.AcVoltage / math.Sqrt(3)
		if math.Abs(v-want) > 1e-6 {
			t.Errorf("phase %d voltage = %v, want %v (balanced)", i, v, want)
		}
	}
	// Doubling the requested power should roughly double the current.
	e2 := New(p, 50)
	e2.Step(time.Second, 65.25, 0)
	e3 := New(p, 50)
	e3.Step(time.Second, 130.5, 0)
	ratio := e3.State().PhaseCurrentsA[0] / e2.State().PhaseCurrentsA[0]
	if math.Abs(ratio-2) > 0.01 {
		t.Errorf("doubling power changed current by a factor of %v, want ~2", ratio)
	}
}

func TestMeterAccumulatesForwardOnDischargeReverseOnCharge(t *testing.T) {
	e := New(testParams(), 50)
	e.Step(time.Second, 10, 0) // discharge
	s := e.State()
	if s.TotalForwardEnergyKWh <= 0 || s.TotalReverseEnergyKWh != 0 {
		t.Fatalf("after discharge: forward=%v reverse=%v, want forward>0 reverse=0", s.TotalForwardEnergyKWh, s.TotalReverseEnergyKWh)
	}

	e2 := New(testParams(), 50)
	e2.Step(time.Second, -10, 0) // charge
	s2 := e2.State()
	if s2.TotalReverseEnergyKWh <= 0 || s2.TotalForwardEnergyKWh != 0 {
		t.Fatalf("after charge: forward=%v reverse=%v, want reverse>0 forward=0", s2.TotalForwardEnergyKWh, s2.TotalReverseEnergyKWh)
	}
}

func TestMeterDirectionInvertedSwapsAccumulators(t *testing.T) {
	e := New(testParams(), 50)
	e.SetMeterDirectionInverted(true)
	e.Step(time.Second, 10, 0) // discharge, but inverted
	s := e.State()
	if s.TotalReverseEnergyKWh <= 0 || s.TotalForwardEnergyKWh != 0 {
		t.Fatalf("after discharge with inverted direction: forward=%v reverse=%v, want reverse>0 forward=0", s.TotalForwardEnergyKWh, s.TotalReverseEnergyKWh)
	}
}

func TestHeartbeatIncrementsEveryStepAndOnlineIsTrue(t *testing.T) {
	e := New(testParams(), 50)
	if !e.State().Online {
		t.Error("Online = false immediately after New()")
	}
	start := e.State().HeartbeatCounter
	for i := 0; i < 10; i++ {
		e.Step(time.Second, 0, 0)
	}
	if got := e.State().HeartbeatCounter; got != start+10 {
		t.Errorf("HeartbeatCounter = %d after 10 steps, want %d", got, start+10)
	}
	if !e.State().Online {
		t.Error("Online = false after stepping")
	}
}
