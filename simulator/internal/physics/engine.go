package physics

import (
	"math"
	"math/rand"
	"time"
)

// State is the full observable output of one Step — everything a caller
// (a Runner writing into the shared store) needs to update telemetry.
type State struct {
	SoCPercent   float64
	PackVoltageV float64

	CellVoltagesMV    []float64 // len == Params.NumCellVoltagePoints
	CellTemperaturesC []float64 // len == Params.NumCellTempPoints

	BatteryTempC float64

	MaxChargeableKW     float64 // Task 5 item 5, always >= 0
	MaxDischargeableKW  float64 // always >= 0
	ChargeProhibited    bool
	DischargeProhibited bool

	RequestedPowerKW  float64
	ActualPowerKW     float64 // after physical clipping — what the battery actually did
	ReactivePowerKvar float64

	PhaseVoltagesV [3]float64
	PhaseCurrentsA [3]float64
	FrequencyHz    float64
	PowerFactor    float64

	TotalForwardEnergyKWh  float64
	TotalReverseEnergyKWh  float64
	MeterDirectionInverted bool

	HeartbeatCounter int64
	Online           bool
}

// Engine is the M261 battery/PCS physical model. Step is a pure function
// of (dt, requested power); Engine holds no clock and calls time.Now()
// nowhere (AGENT-TASK §1.5) — determinism only depends on Params.RNGSeed
// and the sequence of Step calls, both fully controlled by the caller.
type Engine struct {
	params Params
	state  State
	rng    *rand.Rand

	cellVoltageBiasMV []float64 // fixed per-cell manufacturing bias, drawn once at New()
	cellTempBiasC     []float64

	cellVoltageOverrideMV map[int]float64 // manual per-cell imbalance overrides
	cellTempOverrideC     map[int]float64

	storedEnergyKWh float64 // internal precision store; State.SoCPercent is derived from this
}

// New builds an Engine at the given initial SoC (0-100).
func New(params Params, initialSOCPercent float64) *Engine {
	e := &Engine{
		params:                params,
		rng:                   rand.New(rand.NewSource(params.RNGSeed)), //nolint:gosec // deterministic simulator RNG, not cryptographic
		cellVoltageOverrideMV: make(map[int]float64),
		cellTempOverrideC:     make(map[int]float64),
	}
	e.storedEnergyKWh = clamp(initialSOCPercent, 0, 100) / 100 * params.CapacityKWh
	e.state.SoCPercent = e.storedEnergyKWh / params.CapacityKWh * 100

	e.cellVoltageBiasMV = make([]float64, params.NumCellVoltagePoints)
	for i := range e.cellVoltageBiasMV {
		e.cellVoltageBiasMV[i] = (e.rng.Float64()*2 - 1) * params.CellVoltageSpreadMV
	}
	e.cellTempBiasC = make([]float64, params.NumCellTempPoints)
	for i := range e.cellTempBiasC {
		e.cellTempBiasC[i] = (e.rng.Float64()*2 - 1) * params.CellTempSpreadC
	}

	e.state.CellVoltagesMV = make([]float64, params.NumCellVoltagePoints)
	e.state.CellTemperaturesC = make([]float64, params.NumCellTempPoints)
	e.state.BatteryTempC = params.AmbientTempC
	e.state.Online = true
	e.recompute(0, 0, 0)
	return e
}

// State returns a snapshot of the current output — safe for the caller
// to retain (slices are copied).
func (e *Engine) State() State {
	s := e.state
	s.CellVoltagesMV = append([]float64(nil), e.state.CellVoltagesMV...)
	s.CellTemperaturesC = append([]float64(nil), e.state.CellTemperaturesC...)
	return s
}

// SetCellVoltageOverrideMV pins one cell's reported voltage regardless of
// SoC/bias — for injecting a specific cell imbalance (Task 5 item 3, Task 7
// fault injection will build on this).
func (e *Engine) SetCellVoltageOverrideMV(index int, mv float64) {
	e.cellVoltageOverrideMV[index] = mv
	e.applyCellValues()
}

func (e *Engine) ClearCellVoltageOverride(index int) {
	delete(e.cellVoltageOverrideMV, index)
	e.applyCellValues()
}

func (e *Engine) SetCellTemperatureOverrideC(index int, c float64) {
	e.cellTempOverrideC[index] = c
	e.applyCellValues()
}

func (e *Engine) ClearCellTemperatureOverride(index int) {
	delete(e.cellTempOverrideC, index)
	e.applyCellValues()
}

// SetMeterDirectionInverted mirrors the "Energy Storage Meter Power
// Direction" setpoint (§4.4): 0 normal, 1 inverted swaps which
// accumulator (forward/reverse) charge vs discharge energy adds to.
func (e *Engine) SetMeterDirectionInverted(inverted bool) {
	e.state.MeterDirectionInverted = inverted
}

// Step advances the model by dt given a requested active/reactive power
// (kW/kvar, sign per §4.5: negative charges, positive discharges).
// Returns the actual power delivered after clipping to what the battery
// can physically do right now (SoC headroom, thermal derating) — Task 6
// is responsible for everything upstream of "physically possible"
// (Power On/Off, System Max Charge/Discharge Power, mode arbitration).
func (e *Engine) Step(dt time.Duration, requestedPowerKW, requestedReactiveKvar float64) float64 {
	dtHours := dt.Hours()

	actualPowerKW := clamp(requestedPowerKW, -e.state.MaxChargeableKW, e.state.MaxDischargeableKW)
	e.integrateSOC(dtHours, actualPowerKW)
	e.updateThermal(dtHours, actualPowerKW)
	e.updateMeter(dtHours, actualPowerKW)
	e.state.HeartbeatCounter++

	e.recompute(actualPowerKW, requestedReactiveKvar, requestedPowerKW)
	return actualPowerKW
}

// integrateSOC applies Task 5 item 1: power integrated against capacity, with
// round-trip efficiency split symmetrically (sqrt on each direction, so a
// full charge/discharge cycle nets the full round-trip figure) since §4.8
// gives only a single round-trip number, not a charge/discharge split.
func (e *Engine) integrateSOC(dtHours, actualPowerKW float64) {
	eff := math.Sqrt(e.params.RoundTripEfficiency)
	switch {
	case actualPowerKW < 0: // charging
		e.storedEnergyKWh += -actualPowerKW * dtHours * eff
	case actualPowerKW > 0: // discharging
		e.storedEnergyKWh -= actualPowerKW * dtHours / eff
	}
	e.storedEnergyKWh = clamp(e.storedEnergyKWh, 0, e.params.CapacityKWh)
	soc := e.storedEnergyKWh / e.params.CapacityKWh * 100

	// recompute's taperFactor linearly reduces charge/discharge power as
	// SoC nears the 0%/100% boundary, so a plain integration only ever
	// asymptotically approaches the exact boundary — the same shape as a
	// real battery's CC-CV taper, which never reaches "full" by integrating
	// forever either. A real BMS instead declares the battery full/empty
	// once the tapered current drops below a small threshold; snapping SoC
	// to the exact boundary once within a small margin models that same
	// termination behavior instead of waiting on floating-point underflow.
	const boundaryEpsilonPercent = 0.01
	switch {
	case soc > 100-boundaryEpsilonPercent:
		soc = 100
		e.storedEnergyKWh = e.params.CapacityKWh
	case soc < boundaryEpsilonPercent:
		soc = 0
		e.storedEnergyKWh = 0
	}
	e.state.SoCPercent = soc
}

// updateThermal implements Task 5 item 4: heat generation proportional to
// I^2 (approximated as proportional to power^2 at roughly constant
// voltage), a fixed-capacity chiller once above ambient, passive loss to
// ambient, and first-order thermal inertia (no instant jumps).
func (e *Engine) updateThermal(dtHours, actualPowerKW float64) {
	heatGenKW := e.params.HeatCoefficientKWPerKW2 * actualPowerKW * actualPowerKW
	heatRemovedKW := 0.0
	if e.state.BatteryTempC > e.params.AmbientTempC {
		heatRemovedKW = e.params.ChillerCapacityKW
	}
	passiveKW := (e.state.BatteryTempC - e.params.AmbientTempC) * e.params.PassiveLossKWPerC

	netKW := heatGenKW - heatRemovedKW - passiveKW
	e.state.BatteryTempC += netKW * dtHours / e.params.ThermalMassKWhPerC
}

func (e *Engine) updateMeter(dtHours, actualPowerKW float64) {
	forward, reverse := actualPowerKW > 0, actualPowerKW < 0 // forward: discharge (export); reverse: charge (import)
	if e.state.MeterDirectionInverted {
		forward, reverse = reverse, forward
	}
	energy := math.Abs(actualPowerKW) * dtHours
	switch {
	case forward:
		e.state.TotalForwardEnergyKWh += energy
	case reverse:
		e.state.TotalReverseEnergyKWh += energy
	}
}

// recompute derives every value that's purely a function of the current
// SoC/temperature/power (as opposed to something integrated over time) —
// called once at New() and again at the end of every Step.
func (e *Engine) recompute(actualPowerKW, reactiveKvar, requestedPowerKW float64) {
	e.state.PackVoltageV = clamp(lfpCellVoltage(e.state.SoCPercent)*float64(e.params.SeriesCells), e.params.VoltageMin, e.params.VoltageMax)
	e.applyCellValues()

	derate := thermalDerateFactor(e.state.BatteryTempC, e.params.DerateStartTempC, e.params.DerateFullAtTempC)
	chargeSOCFactor := taperFactor(100-e.state.SoCPercent, e.params.SOCTaperBandPercent)
	dischargeSOCFactor := taperFactor(e.state.SoCPercent, e.params.SOCTaperBandPercent)
	e.state.MaxChargeableKW = e.params.NominalACPowerKW * derate * chargeSOCFactor
	e.state.MaxDischargeableKW = e.params.NominalACPowerKW * derate * dischargeSOCFactor
	e.state.ChargeProhibited = e.state.SoCPercent >= 100
	e.state.DischargeProhibited = e.state.SoCPercent <= 0

	e.state.RequestedPowerKW = requestedPowerKW
	e.state.ActualPowerKW = actualPowerKW
	e.state.ReactivePowerKvar = reactiveKvar
	e.updatePCSElectricals(actualPowerKW, reactiveKvar)
}

// thermalDerateFactor is 1 below DerateStartTempC, 0 at/above
// DerateFullAtTempC, linear in between (Task 5 item 4/§4.8: "power
// derating above 45°C", ceiling of the operating range is 55°C).
func thermalDerateFactor(tempC, start, full float64) float64 {
	if tempC <= start {
		return 1
	}
	if tempC >= full {
		return 0
	}
	return 1 - (tempC-start)/(full-start)
}

// taperFactor is 1 while headroom is >= band, tapering linearly to 0 as
// headroom (distance from the SoC boundary being approached) reaches 0 —
// avoids a discontinuous jump straight to the hard SoC limit.
func taperFactor(headroomPercent, bandPercent float64) float64 {
	if bandPercent <= 0 {
		if headroomPercent <= 0 {
			return 0
		}
		return 1
	}
	return clamp(headroomPercent/bandPercent, 0, 1)
}

func (e *Engine) applyCellValues() {
	baseMV := lfpCellVoltage(e.state.SoCPercent) * 1000
	for i := 0; i < e.params.NumCellVoltagePoints; i++ {
		if v, ok := e.cellVoltageOverrideMV[i]; ok {
			e.state.CellVoltagesMV[i] = v
			continue
		}
		e.state.CellVoltagesMV[i] = baseMV + e.cellVoltageBiasMV[i]
	}
	for i := 0; i < e.params.NumCellTempPoints; i++ {
		if c, ok := e.cellTempOverrideC[i]; ok {
			e.state.CellTemperaturesC[i] = c
			continue
		}
		e.state.CellTemperaturesC[i] = e.state.BatteryTempC + e.cellTempBiasC[i]
	}
}

// updatePCSElectricals derives phase voltages/currents, frequency, and
// power factor consistent with actualPowerKW (Task 5 item 6). A grid-tied
// PCS holds frequency at the grid's nominal value regardless of load —
// unlike an islanded/frequency-forming source, load doesn't shift
// frequency here, so "consistent with current power" is satisfied by
// frequency staying put while voltage/current move with power, not by
// inventing a power-frequency coupling that wouldn't exist in this mode.
func (e *Engine) updatePCSElectricals(actualPowerKW, reactiveKvar float64) {
	const nominalFrequencyHz = 50.0
	lineVoltage := e.params.AcVoltage
	apparentKVA := math.Hypot(actualPowerKW, reactiveKvar)
	powerFactor := 1.0
	if apparentKVA > 0 {
		powerFactor = clamp(math.Abs(actualPowerKW)/apparentKVA, 0, 1)
	}
	lineCurrent := 0.0
	if lineVoltage > 0 {
		lineCurrent = apparentKVA * 1000 / (math.Sqrt(3) * lineVoltage)
	}
	for i := 0; i < 3; i++ {
		e.state.PhaseVoltagesV[i] = lineVoltage / math.Sqrt(3) // phase-to-neutral, balanced
		e.state.PhaseCurrentsA[i] = lineCurrent
	}
	e.state.FrequencyHz = nominalFrequencyHz
	e.state.PowerFactor = powerFactor
}
