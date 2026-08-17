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
// is polled once per physics tick (Runner calls ResolveDispatch) for the
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
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/appgate"
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

	gate *appgate.Gate // see SetGate

	// writeMu serializes every operation that determines a setpoint's
	// next value and commits it — a full-replace write (Write) or a
	// partial, register-level read-modify-write (modbustcp's
	// applyRegisterWrites, via LockWrites/UnlockWrites) — against every
	// other one, *and* against ResolvePower/ResolveDispatch's own read of
	// the same state (fifth review round). Fourth-review-round fix:
	// gate.Op is a *shared* lock, so wrapping applyRegisterWrites' whole
	// read-modify-write-validate-commit transaction in gate.Op (third
	// review round) excluded it from a concurrent Reset (which needs
	// gate.Exclusive) but never from another concurrent writer also
	// holding gate.Op — a second FC06/FC16 request touching the same
	// point, or an IEC-104 setpoint write to it, could still interleave
	// with an in-flight read-modify-write and produce a classic lost
	// update. Write acquiring writeMu itself, even though a full-replace
	// write doesn't need read-then-write protection for its *own* sake,
	// is what closes that window: it makes every writer participate in
	// the same mutual-exclusion domain a read-modify-write needs to be
	// safe, not just protect read-modify-writes from each other.
	//
	// Fifth-review-round fix: this lock's position relative to gate.Op
	// *inverted*. It used to be the outer lock (writeMu, then gate.Op),
	// which is exactly backwards for a reader like ResolvePower — Tick
	// already holds gate.Op (as the shared/Op side) for the whole tick
	// before ResolvePower ever runs, so ResolvePower acquiring writeMu
	// under that gate.Op hold, while every writer acquired writeMu
	// *before* gate.Op, is the textbook AB-BA lock-order inversion: a
	// concurrent Reset (gate.Exclusive) queued between the two blocks
	// every *new* gate.Op acquisition (Go's sync.RWMutex gives a waiting
	// writer priority over new readers) — so a writer already holding
	// writeMu and now blocked acquiring gate.Op waits on Reset, Reset
	// waits for Tick's already-held gate.Op to drain, and Tick (inside
	// ResolvePower) waits on writeMu the blocked writer is holding — a
	// genuine three-way deadlock, not a hypothetical one (reproduced by
	// the reviewer). The fix is the order itself, not a workaround:
	// gate.Op is now always acquired *first* (outer) by every caller —
	// Write, applyRegisterWrites (via LockWrites), and Tick (before it
	// ever calls ResolvePower) — and writeMu is always the *inner* lock,
	// with the Store's own mutex innermost of all
	// (Gate.Op -> writeMu -> Store lock, matching linkfault.Coordinator's
	// own documented relationship to gate.Op). Reset keeps using
	// gate.Exclusive only, unchanged: since nothing acquires gate.Op
	// *after* already holding writeMu/the Store lock under the new order,
	// Reset's drain-then-exclude wait can never be blocked by an inner
	// lock that is itself waiting on Reset.
	writeMu sync.Mutex
}

// LockWrites/UnlockWrites let a caller (modbustcp's FC06/FC16 handler)
// hold the same write-serialization lock Write uses internally, for its
// own multi-step read-modify-write-validate-commit transaction — see
// writeMu's own doc comment for the lost-update race this closes, and for
// the fifth-review-round lock-order fix this must follow: the caller
// must acquire gate.Op *first* (outer) and call LockWrites only after —
// never the other way around — then hold writeMu for the *entire*
// transaction, from the first read-modify-write seed read through the
// final WriteBatch commit.
func (p *Processor) LockWrites()   { p.writeMu.Lock() }
func (p *Processor) UnlockWrites() { p.writeMu.Unlock() }

// SetGate wires the process-wide reset-atomicity gate (package appgate)
// — Write becomes a shared (Op) operation against it once set, so a
// controlapi.Server.doReset's exclusive section can never interleave
// with one. nil (never calling SetGate) disables gating.
func (p *Processor) SetGate(g *appgate.Gate) { p.gate = g }

func (p *Processor) opDone() func() {
	if p.gate == nil {
		return func() {}
	}
	return p.gate.Op()
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
// wrapping, no silent rounding of a genuinely fractional engineering
// value: 1.4 at scale=1 is rejected outright, never silently treated as
// enum value 1 or truncated to 1. Integrality is checked with a small,
// fixed-ULP-count tolerance rather than exact equality (see
// snapToIntegerWithinTolerance / integralityToleranceULPs) — a decimal,
// non-power-of-two scale like 0.1 makes raw = value/scale land one float64
// ULP off the true integer even when the mathematical quotient is exact
// (3276.7/0.1 evaluates to 32766.999999999996 in float64, not 32767.0) —
// exact equality would reject that valid boundary. The tolerance is
// measured in ULPs, not a fixed or relative epsilon, specifically because
// a relative epsilon loose enough to absorb that division's rounding
// error at small magnitudes is, at the I16 boundary (raw≈32767), loose
// enough to also swallow a genuinely fractional value like 32767.00001 as
// if it were the integer 32767 — snapToIntegerWithinTolerance's own doc
// comment has the measurements.
//
// Enum: only for points the catalog actually gives an Enum for (25 of
// 148). A point without one isn't checked against a list that doesn't
// exist. Membership is checked against raw (post-representability-check,
// so already confirmed within tolerance of an integer, then snapped to
// it) — never math.Round(value), which would accept e.g. 1.4 as if it
// were 1 regardless of scale.
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
		rounded, ok := snapToIntegerWithinTolerance(raw)
		if !ok {
			return fmt.Errorf("raw value %v is not an integer (I16 does not round fractional values, it rejects them)", raw)
		}
		raw = rounded
		if raw < -32768 || raw > 32767 {
			return fmt.Errorf("raw value %v is outside the I16 domain [-32768, 32767]", raw)
		}
	case m261points.DataTypeF32:
		if math.Abs(raw) > math.MaxFloat32 {
			return fmt.Errorf("raw value %v overflows float32 (max %v) — ordinary float32 rounding/precision loss within range is fine, this is not that", raw, math.MaxFloat32)
		}
	}

	if meta.Enum != nil {
		// raw is already snapped to an exact integer for I16 above
		// (every real enum-bearing point is I16 today); the same
		// tolerant snap is applied again here for a hypothetical non-I16
		// enum point — none exist in the catalog today, but nothing in
		// the schema forbids one — so enum membership never rounds a
		// genuinely fractional value.
		rounded, ok := snapToIntegerWithinTolerance(raw)
		if !ok {
			return fmt.Errorf("raw value %v is not an integer, so it cannot match an enum key (no rounding)", raw)
		}
		raw = rounded
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
	done := p.opDone() // outer lock — see writeMu's own doc comment for the fifth-review-round order fix
	defer done()
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	p.applySideEffects(key, value)
	if testBeforeCommit != nil {
		testBeforeCommit()
	}
	p.store.Set(key, value)
	return nil
}

// testBeforeCommit, when non-nil, runs from within Write/WriteBatch right
// after the write's side effects (watchdog bookkeeping, diagnostics) have
// been applied but before the corresponding Store commit — the exact
// seam a torn read (a concurrent ResolvePower observing the *new*
// watchdog state paired with the *old* setpoint value, or vice versa)
// would have to land in. Exists purely so package-internal tests can
// force that seam open deterministically (a barrier, not scheduler
// timing) and prove writeMu now excludes ResolvePower/ResolveDispatch
// from it — see processor_internal_test.go. Always nil in production;
// never set outside a test.
var testBeforeCommit func()

// KeyValue is one already-validated (key, value) pair for WriteBatch — a
// type alias (not a new type) for store.KeyValue, so a caller building
// its own batch for store.Store.SetBatch never needs to convert between
// two identically-shaped types.
type KeyValue = store.KeyValue

// WriteBatch commits every pair in writes as a single atomic operation —
// for a caller (modbustcp's FC16/FC06 handler) that has already
// validated every touched point via Validate and now needs to commit all
// of them without a concurrent Reset, or a concurrent reader, ever
// observing the batch partially applied. Unlike Write, WriteBatch does
// *not* validate: it's a low-level commit primitive for a caller that
// has already done so itself and computed its own "reject the whole
// request, don't apply it partially" decision beforehand (the same
// all-or-nothing rule Task 6 item 1 requires for a multi-register FC16
// write spanning more than one catalog point) — calling it with an
// unvalidated pair skips Validate entirely, unlike Write.
//
// Deliberately ungated (does *not* call gate.Op or writeMu itself,
// unlike every other mutating method here) — third-review-round fix: the
// caller (modbustcp.applyRegisterWrites) needs the *entire* transaction —
// reading each touched point's current value to build the read-modify-
// write seed for an unaligned FC06 half-write, decoding, Validate, and
// this final commit — to run under one gate.Op, not just the commit
// half; WriteBatch taking its own nested gate.Op on top of that outer
// one is exactly the "outer RLock plus nested inner Op acquisition" the
// review warned can deadlock against a queued exclusive Reset. The
// actual Store mutation is also now one store.Store.SetBatch call, not
// a loop of individual Store.Set calls — SetBatch holds the Store's own
// mutex for the whole batch, so even a concurrent reader that also holds
// gate.Op (a shared lock, so it doesn't itself exclude another gate.Op
// holder) can never observe only part of a multi-point batch applied.
//
// Fourth-review-round addition: the caller must *also* hold writeMu
// (LockWrites/UnlockWrites) for that same entire transaction — see
// writeMu's own doc comment for the lost-update race (another concurrent
// writer to the same point, not just Reset) this closes. Fifth-review-
// round fix: the caller must acquire gate.Op *before* LockWrites, not
// after — writeMu is now the inner lock, gate.Op the outer one; see
// writeMu's own doc comment for the deadlock the previous order (writeMu
// outer) risked against Tick and Reset. WriteBatch itself still doesn't
// acquire either lock; it assumes the caller already holds both, in that
// order.
func (p *Processor) WriteBatch(writes []KeyValue) {
	for _, kv := range writes {
		p.applySideEffects(kv.Key, kv.Value)
	}
	if testBeforeCommit != nil {
		testBeforeCommit()
	}
	p.store.SetBatch(writes)
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

// ResolvePower is ResolveDispatch's convenience wrapper for every caller
// that only cares about active/reactive power (every current caller
// except physics.Runner.step itself, which needs the meter direction
// too — see ResolveDispatch). Kept as a separate, narrower entry point
// rather than changing its signature so that the many existing tests and
// controlapi's own diagnostic use of it don't have to name a return value
// they have no use for.
func (p *Processor) ResolvePower(now time.Time, bmsMaxChargeKW, bmsMaxDischargeKW, socPercent float64, chargeProhibited, dischargeProhibited bool) (activeKW, reactiveKW float64) {
	activeKW, reactiveKW, _ = p.ResolveDispatch(now, bmsMaxChargeKW, bmsMaxDischargeKW, socPercent, chargeProhibited, dischargeProhibited)
	return activeKW, reactiveKW
}

// ResolveDispatch is Task 6's dispatch decision, called once per physics
// tick by physics.Runner.step in place of the raw store read it used
// before this package existed. bmsMaxChargeKW/bmsMaxDischargeKW/
// socPercent are the physics engine's own current-tick output (physics.
// State's MaxChargeableKW/MaxDischargeableKW/SoCPercent); chargeProhibited/
// dischargeProhibited are physics.State's ChargeProhibited/
// DischargeProhibited — the engine's own hard 0%/100% SoC boundary flags,
// checked here explicitly and redundantly with the MaxChargeable/
// DischargeableKW==0 taper they imply, the same defense-in-depth already
// used elsewhere in this codebase (e.g. iec104's writePoint re-checking
// EMS/ClassSetpoint even though address space already guarantees it).
// Task 5's Engine.Step remains responsible for clipping to what's
// physically possible right now; ResolveDispatch is responsible for
// everything upstream of that (mode arbitration, EMS-level limits,
// watchdog, Power On/Off, meter direction).
//
// meterDirectionInverted is Energy Storage Meter Power Direction
// (physics.Runner.step used to read this separately, via its own
// unlocked store.Store.Get call, *after* calling ResolvePower — fifth-
// review-round fix: a single legal FC16 request changing both Set Active
// Power and this point together could land in the gap between the two
// reads, so one Tick could apply the *old* power together with the *new*
// direction, or vice versa, a combination the client never actually
// requested). Folding it into this same locked snapshot closes that: a
// tick's power and direction are now read together, under the same
// writeMu+Store.RLock hold, so they can only ever be the full pre-write
// pair or the full post-write pair, never a mix.
//
// Holds p.writeMu, then p.store.RLock(), for its *entire* body — see
// writeMu's own doc comment for the fifth-review-round lock-order fix
// this depends on (gate.Op, held by the caller — Tick — before it ever
// reaches here, is always the outer lock; writeMu is acquired only after
// that, never before). Fourth-review-round fix (Store.RLock): this reads
// a dozen-plus separate setpoints (set_operating_mode, the Remote-mode
// power pair, both SoC/power limits, and — in Auto Strategy — up to ten
// Strategy Periods' worth of schedule fields) across several of its own
// helper methods (resolvePriorityDriver, scheduleLookup,
// applyPowerLimits), each previously via its own independently-locked
// store.Store.Get call. A concurrent multi-point Modbus batch commit
// (store.Store.SetBatch) landing partway through used to be able to mix
// pre- and post-batch setpoint values into one dispatch decision — e.g.
// reading the *old* operating mode together with the *new* power
// setpoint from the same just-committed batch, a combination the client
// never actually requested. One RLock for the whole call closes that;
// writeMu (fifth review round) additionally excludes a Write/WriteBatch
// transaction's own watchdog/diagnostic side effects (applySideEffects,
// applied *before* the Store commit — see Write) from being observed
// half-done, paired with a setpoint value that doesn't match yet.
func (p *Processor) ResolveDispatch(now time.Time, bmsMaxChargeKW, bmsMaxDischargeKW, socPercent float64, chargeProhibited, dischargeProhibited bool) (activeKW, reactiveKW float64, meterDirectionInverted bool) {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	p.store.RLock()
	defer p.store.RUnlock()

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

	// Energy Storage Meter Power Direction (§4.4/Task 5 item 7): read
	// every call, not just once at startup, since it's a live setpoint a
	// client can change at any time — folded into this same locked
	// snapshot, not a separate later read, per this function's own doc
	// comment.
	meterDirectionInverted = p.get("energy_storage_meter_power_direction") != 0

	return activeKW, reactiveKW, meterDirectionInverted
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

// Reset returns the Processor's own internal bookkeeping — not the Store
// itself, callers restore that separately (e.g. store.Store.Restore) —
// to what it was immediately after NewProcessor (Task 7 item 7): the
// watchdog's last-Remote-setpoint timer cleared, the safe_state_after
// latch released exactly as if the process had just started (not
// "released as if Remote had just been re-entered" — ResolvePower's own
// mode-transition detection handles that distinction on its own the next
// time it runs), every accumulated Diagnostic discarded, and the
// Remote-mode re-entry tracking reset to its unreachable-until-first-call
// sentinel.
func (p *Processor) Reset() {
	p.mu.Lock()
	p.lastRemoteSetpointAt = time.Time{}
	p.safeStateLatched = false
	p.lastObservedMode = -1
	p.mu.Unlock()

	p.diagMu.Lock()
	p.diagnostics = nil // matches NewProcessor's own unset-until-first-recordDiagnostic state
	p.diagMu.Unlock()
}

// get reads one EMS setpoint — assumes the caller already holds
// p.store.RLock() for the whole multi-point read transaction it's part
// of (every current caller is within ResolvePower's own call graph,
// which wraps its entire body in one RLock/RUnlock pair — see
// ResolvePower's doc comment). Uses GetLocked, not Get: Get takes its
// own RLock internally, which would be an unsafe nested RLock on top of
// the one ResolvePower already holds (see store.Store.RLock's own doc
// comment for the deadlock risk that guards against).
func (p *Processor) get(slug string) float64 {
	v, _ := p.store.GetLocked(emsKey(slug))
	return v
}

func (p *Processor) getInt(slug string) int {
	return int(math.Round(p.get(slug)))
}

// integralityToleranceULPs bounds how far raw = engineering_value / scale
// may sit from the nearest integer, in units of one float64 ULP at that
// integer's own magnitude, and still be treated as exactly that integer.
//
// A single float64 division rounds its mathematically exact result to the
// nearest representable float64 — introducing error of at most (and, by
// direct measurement across the scales/values this codebase actually
// divides by, exactly) one ULP relative to the true quotient. That's what
// turns a decimal, non-power-of-two scale like 0.1 into a value one ULP
// off the integer it mathematically equals (3276.7/0.1 == 32766.999999999996
// in float64 — one ULP below the true 32767; 0.7/0.001 and 3276.7/0.001
// measure the same one-ULP gap).
//
// A *relative* tolerance — this package's first approach, 1e-9 * |raw| —
// cannot express that bound correctly across magnitudes: tight enough to
// reject a genuinely fractional value near raw=1 (1e-9 of 1), it widens
// to roughly nine million ULPs at the I16 boundary (raw≈32767), which is
// loose enough to silently accept an actually-fractional value like
// 32767.00001 (measured at ~2.75 million ULPs away — well inside that
// nine-million-ULP window) as if it were the integer 32767. A small,
// fixed ULP count doesn't have that problem: it's exactly as tight, in
// the unit that actually bounds division rounding error, at every
// magnitude raw can take — including far outside the I16 domain, for the
// hypothetical non-I16 enum point this same helper also serves.
//
// 4 is a small integer multiple of the one-ULP error actually measured,
// leaving headroom for a future compounding rounding step (e.g. a scale
// itself reaching this division after its own decimal-literal round-trip)
// without approaching the ~2.75-million-ULP distance of a genuinely
// fractional value at the same magnitude — the two are separated by six
// orders of magnitude, so this margin is not a close call.
const integralityToleranceULPs = 4

// snapToIntegerWithinTolerance reports whether raw is within
// integralityToleranceULPs float64 ULPs of an integer and, if so, returns
// that integer (as a float64, still to be range/domain-checked by the
// caller) — never the caller's job to re-derive the rounding decision.
func snapToIntegerWithinTolerance(raw float64) (rounded float64, ok bool) {
	rounded = math.Round(raw)
	if raw == rounded {
		return rounded, true
	}
	// ulp is the gap to the next float64 above rounded — the size of one
	// "last place" step at rounded's own magnitude. math.Nextafter, not a
	// fixed epsilon, so this scales correctly whether rounded is 1 or
	// 1e30 (relevant because this helper is also reached, via the Enum
	// branch, for a hypothetical non-I16 — including F32-range — enum
	// point). Sign of rounded doesn't need special-casing: float64
	// spacing is symmetric around zero.
	ulp := math.Nextafter(rounded, math.Inf(1)) - rounded
	tolerance := integralityToleranceULPs * ulp
	if math.Abs(raw-rounded) > tolerance {
		return 0, false
	}
	return rounded, true
}
