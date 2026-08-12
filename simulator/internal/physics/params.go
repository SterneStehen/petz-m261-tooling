// Package physics implements the M261 battery physical model (Task 5):
// SoC integration, an LFP voltage curve, per-cell voltage/temperature
// spread, a thermal model with derating, and the PCS/meter telemetry
// that follows from the resulting power flow.
//
// Engine.Step is a pure function of (dt, requested power) — it never
// reads a clock itself (AGENT-TASK §1.5: no time.Now() in business
// logic). A Runner (runner.go) is the only piece that talks to a
// simulator/internal/clock.Clock, and only to compute dt between ticks.
package physics

// Params holds the physical constants driving the model. Values default
// to the manufacturer figures in AGENT-TASK §4.8 ("use as given, don't
// re-derive") where §4.8 states one directly; the rest (thermal
// coefficients, cell spread, SoC taper band) are modeling choices with no
// source in the register map, called out individually below.
type Params struct {
	// --- §4.8, taken as given ---
	CapacityKWh       float64 // 261 kWh usable system capacity
	NominalVoltage    float64 // 832 V
	VoltageMin        float64 // 676 V
	VoltageMax        float64 // 936 V
	SeriesCells       int     // 260 (1P260S)
	NominalACPowerKW  float64 // 130.5 kW continuous
	AcVoltage         float64 // 400 V, 3-phase
	ChillerCapacityKW float64 // 5 kW
	AmbientTempC      float64 // assumed enclosure ambient, not in §4.8 — a
	// reasonable indoor/enclosure figure, not a manufacturer-stated value.
	DerateStartTempC  float64 // 45 °C — §4.8: "power derating above 45°C"
	DerateFullAtTempC float64 // temperature at which power reaches 0. §4.8
	// gives the *operating range* upper bound as 55°C but doesn't say power
	// reaches exactly zero there — using 55°C as that point anyway is the
	// most defensible reading (it's the documented ceiling of the operating
	// range), not an invented number beyond what §4.8 states.

	// --- round-trip efficiency, Task 5 item 1: "default 0.92" ---
	RoundTripEfficiency float64

	// --- modeling choices with no source in the register map, all
	// configurable rather than hardcoded so they can be tuned without a
	// code change ---
	StepDuration         float64 // seconds per Step call the Runner uses by default
	NumCellVoltagePoints int     // 240, §3.3
	NumCellTempPoints    int     // 140, §3.3
	CellVoltageSpreadMV  float64 // max +/- per-cell manufacturing bias
	CellTempSpreadC      float64 // max +/- per-cell manufacturing bias
	SOCTaperBandPercent  float64 // width of the SoC band near 0%/100% over
	// which Max{Chargeable,Dischargeable}Power tapers linearly to 0, rather
	// than stepping discontinuously at the boundary
	HeatCoefficientKWPerKW2 float64 // heat_kW = coefficient * power_kW^2
	ThermalMassKWhPerC      float64 // battery thermal mass (heat capacity)
	PassiveLossKWPerC       float64 // passive heat loss/gain to ambient per °C difference
	RNGSeed                 int64   // AGENT-TASK: "deterministic for a fixed RNG seed"
}

// DefaultParams returns Params with every §4.8 figure at its documented
// value and reasonable defaults for everything else.
func DefaultParams() Params {
	return Params{
		CapacityKWh:      261,
		NominalVoltage:   832,
		VoltageMin:       676,
		VoltageMax:       936,
		SeriesCells:      260,
		NominalACPowerKW: 130.5,
		AcVoltage:        400,

		ChillerCapacityKW: 5,
		AmbientTempC:      25,
		DerateStartTempC:  45,
		DerateFullAtTempC: 55,

		RoundTripEfficiency: 0.92,

		StepDuration:            1,
		NumCellVoltagePoints:    240,
		NumCellTempPoints:       140,
		CellVoltageSpreadMV:     5,
		CellTempSpreadC:         1,
		SOCTaperBandPercent:     2,
		HeatCoefficientKWPerKW2: 0.00012, // ~2 kW (~1.5%) of I^2R-equivalent loss at nominal power
		ThermalMassKWhPerC:      2,
		PassiveLossKWPerC:       0.05,
		RNGSeed:                 1,
	}
}
