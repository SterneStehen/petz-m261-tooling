// Package commands implements Task 6: everything AGENT-TASK §6 describes
// as sitting between a protocol write and the physics model —
// range/enum validation of all 148 EMS write points, Set Operating Mode
// arbitration (Manual/Auto Strategy/Remote), Strategy Period schedule
// execution, the EMS-level power limits layered on top of physics.Engine's
// own physical clipping, a configurable watchdog on a stale Remote
// setpoint, mode-priority arbitration between Remote/Demand
// Control/Load Tracking, and dangerous-command gating (Trip, Clear
// Protection).
//
// A Processor sits in front of store.Store for every EMS setpoint write
// (protocol servers call Write/Validate instead of the store directly) and
// is polled once per physics tick (Runner calls ResolvePower) for the
// power Task 5's Engine.Step should actually be given — replacing the raw,
// unarbitrated store read runner.go used before this package existed.
package commands

import (
	"fmt"
	"log"
	"math"
	"sync"
	"time"

	"github.com/SterneStehen/petz-m261-tooling/gen/go/m261points"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/clock"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/store"
)

// Set Operating Mode's documented enum values (§4.4).
const (
	modeManual       = 0
	modeAutoStrategy = 1
	modeRemote       = 2
)

const numStrategyPeriods = 10

func emsKey(slug string) m261points.PointKey {
	return m261points.PointKey{Device: "EMS", Slug: slug}
}

// Processor is the Task 6 command/mode-arbitration layer. Safe for
// concurrent use: Write/Validate are called from protocol server
// goroutines (possibly several at once), ResolvePower from the physics
// Runner's own goroutine.
type Processor struct {
	store *store.Store
	clock clock.Clock
	cfg   Config

	mu                   sync.Mutex
	lastRemoteSetpointAt time.Time // zero until the first Set Active/Reactive Power write
	safeStateLatched     bool      // watchdog.mode=safe_state_after only, see applyWatchdog
	lastObservedMode     int       // -1 until the first ResolvePower call

	diagMu      sync.Mutex
	diagnostics map[m261points.PointKey]Diagnostic
}

// NewProcessor builds a Processor and publishes the EMS setpoint defaults
// described on publishSensibleDefaults. Returns an error if cfg is
// malformed (bad watchdog mode, non-positive timeout, or a modes.priority
// that isn't a permutation of remote/demand_control/load_tracking).
func NewProcessor(st *store.Store, clk clock.Clock, cfg Config) (*Processor, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	p := &Processor{store: st, clock: clk, cfg: cfg, lastObservedMode: -1}
	p.publishSensibleDefaults()
	return p, nil
}

// publishSensibleDefaults writes the out-of-the-box values for the EMS
// setpoints that gate dispatch. store.New leaves every point at 0 — for
// most of the 148 setpoints that's a fine, inert default, but for these
// specific ones 0 is a *meaningful, obstructive* value: Power On/Off=0
// means off, Maximum Charge SOC=0%/Minimum Discharge SOC=0% would forbid
// essentially all charging/discharging, and System Maximum
// Charge/Discharge Power=0 would cap every dispatch at 0 kW regardless of
// what else is configured. Left unpublished, a freshly built simulator
// would silently reject every power request no matter the mode, which is
// not a "safe default" — it's simply broken, and would fail Task 6's own
// "Remote mode Set Active Power changes power and SoC" acceptance
// criterion out of the box.
//
// The register map documents no manufacturer power-on default for any of
// these, so these values are a modeling choice (a plant commissioned and
// ready to dispatch, not a doubly-locked-out one), the same kind of
// necessary choice physics.NewRunner already makes by eagerly publishing
// the engine's initial state instead of leaving the store at zero. A
// scenario (Task 7) or a test that wants a specific starting configuration
// overwrites these the same way an operator would on real hardware.
func (p *Processor) publishSensibleDefaults() {
	p.store.Set(emsKey("power_on_off"), 1)
	p.store.Set(emsKey("maximum_charge_soc"), 100)
	p.store.Set(emsKey("minimum_discharge_soc"), 0)
	p.store.Set(emsKey("system_maximum_charge_power"), p.cfg.NominalPowerKW)
	p.store.Set(emsKey("system_maximum_discharge_power"), p.cfg.NominalPowerKW)
}

// Validate checks whether value (an engineering-unit value — the same
// units the store holds and a protocol client reads/writes) would be
// accepted for key, without writing it — used by modbustcp to pre-validate
// every point a single FC16 request touches before committing any of them
// (Task 6 item 1: reject, don't partially apply), and by Write itself.
func (p *Processor) Validate(key m261points.PointKey, value float64) error {
	meta, ok := m261points.Points[key]
	if !ok || meta.Device != "EMS" || meta.Class != m261points.ClassSetpoint {
		return fmt.Errorf("%w: %+v", ErrNotWritable, key)
	}
	if meta.Dangerous && !p.cfg.AllowDangerous {
		return fmt.Errorf("%w: %s", ErrDangerous, meta.Slug)
	}
	if err := validateValue(meta, value); err != nil {
		return fmt.Errorf("%w: %s: %v", ErrInvalidValue, meta.Slug, err)
	}
	return nil
}

// validateValue implements Task 6 item 1's three independent checks —
// enum, logical-type representability, and business range. "Independent"
// means every check that applies to a point always runs, in full, for
// that point: an enum-bearing point still gets the finite/scale/
// logical-type/range checks (nothing about having an Enum exempts it from
// them), and enum membership itself is never reached via rounding.
//
// Representability: checked against raw = engineering_value / scale — the
// same formula gen/go/m261points/codec.go's EncodeValue already applies —
// not against engineering_value directly, and not against Modbus's
// widened 32-bit wire slot (§2.2: an I16 catalog point still occupies a
// 2-register/I32-shaped slot on the wire; that widening doesn't expand the
// logical I16 domain the catalog actually declares). No clamping, no
// wrapping, no silent rounding: raw for I16 must be an exact integer, not
// merely "close enough to round" — and that includes I16 enum-backed
// points (e.g. Set Operating Mode): 1.4 is rejected outright, never
// silently treated as enum value 1.
//
// Enum: only for points the catalog actually gives an Enum for (25 of
// 148). A point without one isn't checked against a list that doesn't
// exist. Membership is checked against raw (post-representability-check,
// so already confirmed an exact integer) — never math.Round(value), which
// would accept e.g. 1.4 as if it were 1.
//
// Business range: meta.Range, when the catalog actually has one (every
// point today has none — see AGENT-TASK §6 item 1). Compared against
// engineering_value, not raw, since Range's bounds are documented as
// engineering values. A range-less point that's in-range for its wire
// type but exceeds a physical/EMS limit (BMS headroom, System Maximum
// Charge/Discharge Power, SoC bounds) is still accepted here and clipped
// later, at dispatch (ResolvePower) — Task 6's own acceptance criterion
// distinguishes "the write is rejected" from "the setpoint is clipped,
// not executed literally", and conflating the two would fail it.
func validateValue(meta m261points.PointMeta, value float64) error {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return fmt.Errorf("engineering value %v is not finite", value)
	}
	scale := meta.Scale
	if math.IsNaN(scale) || math.IsInf(scale, 0) || scale == 0 {
		return fmt.Errorf("catalog scale %v is not finite and non-zero (metadata bug, not a value the client sent)", scale)
	}
	raw := value / scale
	if math.IsNaN(raw) || math.IsInf(raw, 0) {
		return fmt.Errorf("raw value %v is not finite", raw)
	}

	switch meta.DataType {
	case m261points.DataTypeI16:
		if raw != math.Trunc(raw) {
			return fmt.Errorf("raw value %v is not an integer (I16 does not round fractional values, it rejects them)", raw)
		}
		if raw < -32768 || raw > 32767 {
			return fmt.Errorf("raw value %v is outside the I16 domain [-32768, 32767]", raw)
		}
	case m261points.DataTypeF32:
		if math.Abs(raw) > math.MaxFloat32 {
			return fmt.Errorf("raw value %v overflows float32 (max %v) — ordinary float32 rounding/precision loss within range is fine, this is not that", raw, math.MaxFloat32)
		}
	}

	if meta.Enum != nil {
		// raw's integrality is already confirmed for I16 above; enforced
		// again here (redundantly, for I16, but not for a hypothetical
		// non-I16 enum point — none exist in the catalog today, but
		// nothing in the schema forbids one) so enum membership itself
		// never rounds.
		if raw != math.Trunc(raw) {
			return fmt.Errorf("raw value %v is not an integer, so it cannot match an enum key (no rounding)", raw)
		}
		if _, ok := meta.Enum[int(raw)]; !ok {
			return fmt.Errorf("%v is not one of the allowed enum values %v", value, meta.Enum)
		}
	}

	if meta.Range != nil {
		if meta.Range.Min != nil && value < *meta.Range.Min {
			return fmt.Errorf("engineering value %v is below the confirmed minimum %v", value, *meta.Range.Min)
		}
		if meta.Range.Max != nil && value > *meta.Range.Max {
			return fmt.Errorf("engineering value %v is above the confirmed maximum %v", value, *meta.Range.Max)
		}
	}
	return nil
}

// Write validates value for key (see Validate) and, if accepted, applies
// watchdog bookkeeping and commits it to the store — which also
// mechanically mirrors it onto the point's readback twin (store.Set),
// satisfying Task 6 item 8 for every one of the 148 setpoints uniformly,
// not just the ones this package models further behavior for. A rejected
// value leaves the store, readback, dispatched power, and every piece of
// Processor's own internal state completely untouched, and is logged
// (Task 6 item 1: "the fact is recorded in the log") — Validate runs
// first and returns before anything else in Write executes.
//
// Trip and Clear Protection (Task 6 item 7): accepting either (i.e.
// commands.allow_dangerous is true — Validate already rejects them
// otherwise) never changes dispatched power and never models a latch,
// physical E-stop, circuit breaker, BMS power action, or protection
// reset — accepted_but_unsupported is recorded instead, see Diagnostics.
func (p *Processor) Write(key m261points.PointKey, value float64) error {
	if err := p.Validate(key, value); err != nil {
		log.Printf("commands: rejected write %s/%s = %v: %v", key.Device, key.Slug, value, err)
		return err
	}
	p.applySideEffects(key, value)
	p.store.Set(key, value)
	return nil
}

// applySideEffects updates the Processor's own watchdog/diagnostic state
// for the handful of setpoints Task 6 models further behavior for. Only
// called after Validate has already accepted the write.
func (p *Processor) applySideEffects(key m261points.PointKey, value float64) {
	if key.Device != "EMS" {
		return
	}
	switch key.Slug {
	case "set_active_power_kw", "set_reactive_power_kvar":
		// Both setpoints are treated as one paired "remote dispatch
		// command" for watchdog purposes — refreshing either counts as a
		// live update.
		p.mu.Lock()
		p.lastRemoteSetpointAt = p.clock.Now()
		p.mu.Unlock()
	case "trip", "clear_protection":
		// Reaching here at all means Validate already required
		// allow_dangerous=true (both are Dangerous:true in the catalog).
		// No latch, no dispatched-power effect — see Write's doc comment.
		p.recordDiagnostic(Diagnostic{Code: DiagCodeAcceptedButUnsupported, PointKey: key, AcceptedValue: value})
	}
}

// ResolvePower is Task 6's dispatch decision, called once per physics tick
// by physics.Runner in place of the raw store read it used before this
// package existed. bmsMaxChargeKW/bmsMaxDischargeKW/socPercent are the
// physics engine's own current-tick output (physics.State's
// MaxChargeableKW/MaxDischargeableKW/SoCPercent); chargeProhibited/
// dischargeProhibited are physics.State's ChargeProhibited/
// DischargeProhibited — the engine's own hard 0%/100% SoC boundary flags,
// checked here explicitly and redundantly with the MaxChargeable/
// DischargeableKW==0 taper they imply, the same defense-in-depth already
// used elsewhere in this codebase (e.g. iec104's writePoint re-checking
// EMS/ClassSetpoint even though address space already guarantees it).
// Task 5's Engine.Step remains responsible for clipping to what's
// physically possible right now; ResolvePower is responsible for
// everything upstream of that (mode arbitration, EMS-level limits,
// watchdog, Power On/Off).
func (p *Processor) ResolvePower(now time.Time, bmsMaxChargeKW, bmsMaxDischargeKW, socPercent float64, chargeProhibited, dischargeProhibited bool) (activeKW, reactiveKW float64) {
	mode := p.getInt("set_operating_mode")

	p.mu.Lock()
	if mode == modeRemote && p.lastObservedMode != modeRemote {
		// Re-entering Remote mode explicitly re-arms a latched
		// safe-state watchdog trip — see applyWatchdog's doc comment.
		p.safeStateLatched = false
	}
	p.lastObservedMode = mode
	p.mu.Unlock()

	switch mode {
	case modeManual:
		// "Setpoints are not automatically executed" (§6 item 2) — Manual
		// is the safe, do-nothing default, matching Set Operating Mode's
		// own store.New zero value.
		activeKW, reactiveKW = 0, 0
	case modeAutoStrategy:
		activeKW, reactiveKW = p.scheduleLookup(now), 0
	case modeRemote:
		// resolvePriorityDriver's return value only decides which
		// Diagnostic (if any) gets recorded — see its doc comment for why
		// dispatch itself always falls through to Set Active Power
		// regardless of which name wins: Demand Control/Load Tracking have
		// zero effect on dispatched power, which means "as if the
		// unsupported mode were absent", not "force dispatch to zero".
		p.resolvePriorityDriver()
		activeKW = p.get("set_active_power_kw")
		reactiveKW = p.get("set_reactive_power_kvar")
		activeKW, reactiveKW = p.applyWatchdog(now, activeKW, reactiveKW)
	default:
		// Write already rejects anything outside the documented 0/1/2
		// enum, so this is unreachable via a real write — treated as
		// Manual's safe default rather than a panic, in case the store
		// is ever seeded some other way (a test, a future scenario).
		activeKW, reactiveKW = 0, 0
	}

	activeKW = p.applyPowerLimits(activeKW, socPercent, bmsMaxChargeKW, bmsMaxDischargeKW)
	if (activeKW < 0 && chargeProhibited) || (activeKW > 0 && dischargeProhibited) {
		activeKW = 0
	}

	if p.getInt("power_on_off") == 0 {
		activeKW, reactiveKW = 0, 0
	}
	return activeKW, reactiveKW
}

// resolvePriorityDriver implements Task 6 item 6: which of Remote, Demand
// Control, and Load Tracking wins when more than one is enabled, per
// modes.priority. Only meaningful from within ResolvePower's modeRemote
// case (Remote is being considered specifically because Set Operating
// Mode == 2; Demand Control/Load Tracking have no documented effect in
// Manual or Auto Strategy, which already have their own unambiguous
// dispatch mechanisms).
//
// Demand Control and Load Tracking have no manufacturer-documented power
// computation anywhere in the register map (no formula, and no modeled
// Demand/Load meter input to compute one from — §6.8 of the M261 manual
// documents an external RS-485 meter at the on-site transformer, but no
// such device exists anywhere in the current catalog) — inventing a
// formula would fabricate behavior the source data doesn't contain
// (AGENT-TASK §1 rule 1). So when either outranks "remote" in
// modes.priority and is enabled, this records an accepted_but_unsupported
// Diagnostic naming which mode won — but per AGENT-TASK §6 item 6,
// dispatch must behave "as if the unsupported mode were absent, not as a
// silent forced zero": the caller (ResolvePower) always dispatches Set
// Active Power regardless of what this function returns. The return
// value exists only to pick which name (if any) the recorded Diagnostic
// names as the winner — it never gates dispatch itself.
func (p *Processor) resolvePriorityDriver() string {
	enabled := map[string]bool{
		"remote":         true, // this is only ever called from the modeRemote branch
		"demand_control": p.getInt("demand_control") != 0,
		"load_tracking":  p.getInt("enable_load_tracking") != 0,
	}
	winner := "remote" // unreachable fallback once cfg.validate() has run
	for _, name := range p.cfg.ModePriority {
		if enabled[name] {
			winner = name
			break
		}
	}
	if slug, ok := unsupportedModeSlug[winner]; ok {
		key := emsKey(slug)
		p.recordDiagnostic(Diagnostic{
			Code:          DiagCodeAcceptedButUnsupported,
			PointKey:      key,
			AcceptedValue: p.get(slug),
			SelectedMode:  winner,
		})
	}
	return winner
}

// unsupportedModeSlug maps a resolvePriorityDriver winner name to the
// catalog slug of the setpoint that selects it — "load_tracking" (the
// priority-name convention, matching modes.priority's own vocabulary)
// isn't itself a slug; the real point is "enable_load_tracking".
var unsupportedModeSlug = map[string]string{
	"demand_control": "demand_control",
	"load_tracking":  "enable_load_tracking",
}

// applyWatchdog implements Task 6 item 5. Only called for Remote-mode
// dispatch — Auto Strategy's schedule is self-driven by the simulator's
// own clock and needs no external refresh, and Manual already dispatches
// nothing.
//
// hold: no expiry, ever.
//
// zero_after: purely a function of "how long since the last Set Active/
// Reactive Power write" at this instant — dispatch resumes the moment a
// fresh write lands, no memory of the stale period.
//
// safe_state_after: AGENT-TASK §6 item 5 itself describes the "safe
// state" as something to be agreed with the manufacturer, not something
// already specified — i.e., explicitly not yet agreed, which is exactly
// why it's a separate config option rather than a spec. The only
// universally-defensible safe action derivable without inventing
// manufacturer behavior is "stop moving power", so this produces the
// same immediate 0/0 as zero_after — but, unlike zero_after, it
// *latches*: once tripped, dispatch stays at 0 even across subsequent
// Set Active Power writes, until the operator explicitly leaves and
// re-enters Remote mode (a transition detected in ResolvePower). That's
// the one difference between the two modes that's possible to implement
// without guessing manufacturer specifics: hold vs. don't hold on comms
// recovery, i.e. does a resumed heartbeat alone self-heal dispatch, or
// does it require a deliberate re-arm.
func (p *Processor) applyWatchdog(now time.Time, activeKW, reactiveKW float64) (float64, float64) {
	p.mu.Lock()
	lastAt := p.lastRemoteSetpointAt
	latched := p.safeStateLatched
	p.mu.Unlock()

	stale := !lastAt.IsZero() && now.Sub(lastAt) >= p.cfg.WatchdogTimeout

	switch p.cfg.WatchdogMode {
	case WatchdogHold:
		return activeKW, reactiveKW
	case WatchdogZeroAfter:
		if stale {
			return 0, 0
		}
		return activeKW, reactiveKW
	case WatchdogSafeStateAfter:
		if stale && !latched {
			p.mu.Lock()
			p.safeStateLatched = true
			p.mu.Unlock()
			latched = true
		}
		if latched {
			return 0, 0
		}
		return activeKW, reactiveKW
	default:
		return activeKW, reactiveKW // unreachable once cfg.validate() has run
	}
}

// scheduleLookup implements Task 6 item 3: find the Strategy Period active
// at now (by hour:minute of day, in whatever zone the injected Clock
// produces — the register map's Start/End Hour/Minute fields carry no
// timezone of their own), apply its Execution Power, idle (0) outside
// every period. If more than one of the 10 periods is configured to
// overlap, the lowest-numbered one wins — a deterministic, documented
// tie-break for a configuration the register map doesn't say is even
// supposed to happen.
func (p *Processor) scheduleLookup(now time.Time) float64 {
	curMinutes := now.Hour()*60 + now.Minute()
	for period := 1; period <= numStrategyPeriods; period++ {
		prefix := fmt.Sprintf("strategy_period_%d_", period)
		start := p.getInt(prefix+"start_hour")*60 + p.getInt(prefix+"start_minute")
		end := p.getInt(prefix+"end_hour")*60 + p.getInt(prefix+"end_minute")
		if periodActive(curMinutes, start, end) {
			return p.get(prefix + "execution_power_charge_discharge")
		}
	}
	return 0
}

// periodActive reports whether curMinutes (0-1439, minutes since
// midnight) falls in [start, end). start == end (including the all-zero
// default of a period the operator never configured) is treated as
// zero-width/inactive, never "active all day" — a partially-configured
// schedule must not accidentally dispatch around the clock. start > end
// wraps past midnight (e.g. 23:00-01:00), the standard reading of an
// overnight window.
func periodActive(curMinutes, start, end int) bool {
	if start == end {
		return false
	}
	if start < end {
		return curMinutes >= start && curMinutes < end
	}
	return curMinutes >= start || curMinutes < end
}

// applyPowerLimits implements Task 6 item 4: Maximum Charge SOC/Minimum
// Discharge SOC (EMS-configured, generally tighter than the engine's own
// hard 0%/100% physical bounds) and System Maximum Charge/Discharge Power
// (an EMS-level cap independent of, and possibly tighter than, the
// physical/BMS headroom) — "the most restrictive applies" against
// bmsMaxChargeKW/bmsMaxDischargeKW, Task 5's own already-physical limits.
// Reactive power isn't limited here — the register map documents no
// reactive-power equivalent of System Maximum Charge/Discharge Power or an
// SoC-driven reactive bound.
//
// chargeCapKW/dischargeCapKW are magnitudes and are floored at 0: System
// Maximum Charge/Discharge Power is representable as negative on the wire
// (no confirmed business range exists to reject it as out-of-range at
// write time — see validateValue), but a "negative maximum magnitude" is
// nonsensical, and treating it as a literal negative cap would let
// -chargeCapKW/dischargeCapKW flip sign in the clamps below and turn a
// charge request into discharge (or vice versa) — an accepted, merely
// unconfirmed limit value must never reverse the requested power's
// direction. A negative configured limit floors to 0 instead: "no power
// allowed in this direction", the most restrictive, safe reading.
func (p *Processor) applyPowerLimits(activeKW, socPercent, bmsMaxChargeKW, bmsMaxDischargeKW float64) float64 {
	maxChargeSOC := p.get("maximum_charge_soc")
	minDischargeSOC := p.get("minimum_discharge_soc")
	chargeCapKW := math.Max(0, math.Min(p.get("system_maximum_charge_power"), bmsMaxChargeKW))
	dischargeCapKW := math.Max(0, math.Min(p.get("system_maximum_discharge_power"), bmsMaxDischargeKW))

	switch {
	case activeKW < 0: // charging (§4.5: negative = charge)
		if socPercent >= maxChargeSOC {
			return 0
		}
		return math.Max(activeKW, -chargeCapKW)
	case activeKW > 0: // discharging
		if socPercent <= minDischargeSOC {
			return 0
		}
		return math.Min(activeKW, dischargeCapKW)
	default:
		return 0
	}
}

func (p *Processor) get(slug string) float64 {
	v, _ := p.store.Get(emsKey(slug))
	return v
}

func (p *Processor) getInt(slug string) int {
	return int(math.Round(p.get(slug)))
}
