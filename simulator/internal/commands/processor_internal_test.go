package commands

// White-box (package commands, not commands_test) regression tests for
// the fifth review round's blocker 3: a physics tick's dispatch decision
// (ResolveDispatch, née ResolvePower) must never observe a torn write —
// a setpoint's new value paired with the previous write's *own* stale
// side effects, or vice versa, or one setpoint from a multi-point batch
// paired with another still-unwritten one. These use testBeforeCommit (a
// package-private seam only this file can reach) to force the exact
// interleaving deterministically — a barrier, not scheduler timing — per
// the review's explicit instruction not to rely on stress loops as proof.

import (
	"testing"
	"time"

	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/appgate"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/clock"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/store"
)

// newInternalTestProcessor mirrors processor_test.go's own newProcessor —
// duplicated here (rather than exported for reuse) because that helper
// lives in package commands_test, which this file, deliberately in
// package commands for testBeforeCommit access, cannot import without a
// cycle.
func newInternalTestProcessor(t *testing.T) (*Processor, *store.Store, *clock.Fake) {
	t.Helper()
	st := store.New()
	clk := clock.NewFake(time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC))
	p, err := NewProcessor(st, clk, DefaultConfig())
	if err != nil {
		t.Fatalf("NewProcessor: %v", err)
	}
	return p, st, clk
}

// TestResolveDispatchCannotObserveATornWatchdogSetpointPair forces Write
// to pause *after* it has updated the watchdog's lastRemoteSetpointAt
// (applySideEffects, under p.mu) but *before* it commits the new setpoint
// value to the Store — on pre-fix HEAD, ResolveDispatch/ResolvePower had
// no reason to wait for that commit (it only held Store.RLock, never
// writeMu), so it could read a freshly-refreshed watchdog timestamp
// together with the *old*, not-yet-committed setpoint value: a torn
// combination no real write ever produces. Asserts ResolveDispatch is
// genuinely blocked (does not return) for as long as the write stays
// paused at that seam, then, once released, observes the fully-committed
// new value — never anything in between.
func TestResolveDispatchCannotObserveATornWatchdogSetpointPair(t *testing.T) {
	p, _, clk := newInternalTestProcessor(t)
	if err := p.Write(emsKey("set_operating_mode"), 2 /* Remote */); err != nil {
		t.Fatalf("Write(set_operating_mode, 2): %v", err)
	}
	if err := p.Write(emsKey("set_active_power_kw"), 10); err != nil {
		t.Fatalf("Write(set_active_power_kw, 10): %v", err)
	}

	entered := make(chan struct{})
	release := make(chan struct{})
	testBeforeCommit = func() {
		close(entered)
		<-release
	}
	defer func() { testBeforeCommit = nil }()

	writeDone := make(chan struct{})
	go func() {
		defer close(writeDone)
		if err := p.Write(emsKey("set_active_power_kw"), 20); err != nil {
			t.Errorf("Write(set_active_power_kw, 20): %v", err)
		}
	}()
	<-entered // the watchdog timestamp has been refreshed; the Store still holds 10

	resolveDone := make(chan struct{})
	var gotActive float64
	go func() {
		defer close(resolveDone)
		gotActive, _, _ = p.ResolveDispatch(clk.Now(), 1e9, 1e9, 50, false, false)
	}()

	select {
	case <-resolveDone:
		t.Fatal("ResolveDispatch returned while a Write was paused between its watchdog update and its Store commit -- writeMu must exclude it until the whole write finishes")
	case <-time.After(50 * time.Millisecond):
		// Expected: ResolveDispatch is still blocked on writeMu.
	}

	close(release)
	<-writeDone
	<-resolveDone

	if gotActive != 20 {
		t.Errorf("ResolveDispatch active power = %v, want 20 (the fully-committed new value -- it could only run after the whole write finished)", gotActive)
	}
}

// TestResolveDispatchCannotObserveATornPowerDirectionPair is blocker 3's
// second required test: a single FC16-shaped batch write changing both
// Set Active Power and Energy Storage Meter Power Direction together is
// paused (via the same seam) after both have been validated but before
// WriteBatch's Store commit. On pre-fix HEAD, physics.Runner.step read
// power (via ResolvePower) and direction (via its own separate, unlocked
// store.Store.Get call) as two independent reads with nothing excluding
// a write from landing between them, so a tick could observe the *old*
// power together with the *new* direction, or vice versa. ResolveDispatch
// now returns both from one locked snapshot — this asserts that read is
// excluded from an in-flight batch exactly like the single-write case
// above, and lands on the full pre- or full post-write pair, never a mix.
func TestResolveDispatchCannotObserveATornPowerDirectionPair(t *testing.T) {
	p, _, clk := newInternalTestProcessor(t)
	if err := p.Write(emsKey("set_operating_mode"), 2 /* Remote */); err != nil {
		t.Fatalf("Write(set_operating_mode, 2): %v", err)
	}
	if err := p.Write(emsKey("set_active_power_kw"), 10); err != nil {
		t.Fatalf("Write(set_active_power_kw, 10): %v", err)
	}
	if err := p.Write(emsKey("energy_storage_meter_power_direction"), 0); err != nil {
		t.Fatalf("Write(energy_storage_meter_power_direction, 0): %v", err)
	}

	entered := make(chan struct{})
	release := make(chan struct{})
	testBeforeCommit = func() {
		close(entered)
		<-release
	}
	defer func() { testBeforeCommit = nil }()

	writeDone := make(chan struct{})
	go func() {
		defer close(writeDone)
		// Mirrors modbustcp.applyRegisterWrites' own calling convention
		// (LockWrites/UnlockWrites around WriteBatch, gate.Op held
		// outside — see its own doc comment) for a request that touches
		// both points in one all-or-nothing commit.
		p.LockWrites()
		defer p.UnlockWrites()
		p.WriteBatch([]KeyValue{
			{Key: emsKey("set_active_power_kw"), Value: 30},
			{Key: emsKey("energy_storage_meter_power_direction"), Value: 1},
		})
	}()
	<-entered // both points validated; neither is committed to the Store yet

	resolveDone := make(chan struct{})
	var gotActive float64
	var gotDirection bool
	go func() {
		defer close(resolveDone)
		gotActive, _, gotDirection = p.ResolveDispatch(clk.Now(), 1e9, 1e9, 50, false, false)
	}()

	select {
	case <-resolveDone:
		t.Fatal("ResolveDispatch returned while a two-point batch write was paused mid-transaction (validated, not yet committed) -- writeMu must exclude it until the whole batch commits")
	case <-time.After(50 * time.Millisecond):
		// Expected: ResolveDispatch is still blocked on writeMu.
	}

	close(release)
	<-writeDone
	<-resolveDone

	if gotActive != 30 || !gotDirection {
		t.Errorf("ResolveDispatch observed (active=%v, directionInverted=%v), want the fully-committed new pair (30, true) -- a torn read would report 10/false, 10/true, or 30/false, none of which this batch ever actually produced", gotActive, gotDirection)
	}
}

// TestGateOpOuterWriteMuInnerOrderAvoidsTheThreeWayDeadlock is blocker
// 3's explicit deadlock-avoidance requirement: "the current writeMu ->
// Gate path can deadlock against Tick and Reset." Rather than a stress
// loop hoping to stumble into the narrow three-way window (the review's
// own instruction against exactly that), this deliberately engineers the
// interleaving with real gate.Op/gate.Exclusive/p.writeMu acquisitions,
// sequenced through channels for the two parts that are directly
// observable (a lock has been acquired) and one short, generous sleep
// only for the one part that fundamentally isn't (a goroutine is now
// parked inside a blocking Lock call, not merely about to call it) —
// 200ms against real acquisitions that complete in well under a
// microsecond leaves five orders of magnitude of margin, not a coin
// flip.
//
//  1. A "reader" goroutine (mirrors Tick: gate.Op held for the outer
//     scope, then a call into ResolveDispatch, which needs writeMu)
//     acquires gate.Op and confirms it before anything else starts.
//  2. A "reset" goroutine then calls gate.Exclusive() — since the reader
//     already holds gate.Op, this legitimately blocks, and (a documented
//     property of sync.RWMutex, not an assumption this test invents)
//     from this point on every *new* gate.Op() attempt blocks too, until
//     Exclusive is served.
//  3. A real p.Write call now starts (the "writer"). Under the *fixed*
//     order (gate.Op outer, writeMu inner) it blocks on gate.Op — behind
//     reset, same as any other new caller — without ever touching
//     writeMu, so the reader's own pending writeMu.Lock() (inside
//     ResolveDispatch) succeeds immediately, the reader finishes and
//     releases gate.Op, reset drains and proceeds, and the writer's
//     queued gate.Op finally succeeds: everything resolves. Under the
//     *old* order (writeMu outer, gate.Op inner — reproduced directly
//     against p.writeMu below, since flipping the real Write/
//     ResolveDispatch back to that order would require reverting the
//     whole fix rather than isolating this one property) the writer
//     would instead acquire writeMu immediately (nothing holds it yet)
//     and only then block on gate.Op — completing the cycle: reader
//     blocked on writeMu (held by the writer), writer blocked on gate.Op
//     (queued behind reset), reset blocked on the reader's gate.Op
//     draining, which it now never will.
func TestGateOpOuterWriteMuInnerOrderAvoidsTheThreeWayDeadlock(t *testing.T) {
	t.Run("fixed order (through the real Write/ResolveDispatch)", func(t *testing.T) {
		p, _, clk := newInternalTestProcessor(t)
		gate := appgate.New()
		p.SetGate(gate)

		readerHasOp := make(chan struct{})
		go func() {
			done := gate.Op()
			defer done()
			close(readerHasOp)
			p.ResolveDispatch(clk.Now(), 1e9, 1e9, 50, false, false)
		}()
		<-readerHasOp

		resetDone := make(chan struct{})
		go func() {
			done := gate.Exclusive()
			done()
			close(resetDone)
		}()
		time.Sleep(50 * time.Millisecond) // let Exclusive actually start waiting/blocking new readers

		writeDone := make(chan struct{})
		go func() {
			defer close(writeDone)
			p.Write(emsKey("set_active_power_kw"), 42) //nolint:errcheck // only whether this finishes matters here
		}()

		assertBothFinish(t, resetDone, writeDone, "fixed order (Gate.Op outer, writeMu inner)")
	})

	t.Run("old order (writeMu outer, gate.Op inner, reproduced directly)", func(t *testing.T) {
		p, _, clk := newInternalTestProcessor(t)
		gate := appgate.New()
		p.SetGate(gate)

		// Step 1: the writer acquires writeMu *first* (the old, buggy
		// order) and holds it until told to proceed to gate.Op — this
		// must happen before the reader starts, or the reader's own
		// (uncontended) writeMu acquisition inside ResolveDispatch would
		// race it and could win, finishing before the writer ever
		// contends for anything and defeating the whole scenario.
		writerHasWriteMu := make(chan struct{})
		writerCanTryGateOp := make(chan struct{})
		go func() {
			p.writeMu.Lock()
			defer p.writeMu.Unlock()
			close(writerHasWriteMu)
			<-writerCanTryGateOp
			done := gate.Op()
			done()
		}()
		<-writerHasWriteMu

		// Step 2: the reader acquires gate.Op (fine — the writer doesn't
		// hold it) and then tries writeMu — now genuinely blocks, since
		// the writer holds it.
		readerHasOp := make(chan struct{})
		go func() {
			done := gate.Op()
			defer done()
			close(readerHasOp)
			p.ResolveDispatch(clk.Now(), 1e9, 1e9, 50, false, false)
		}()
		<-readerHasOp
		time.Sleep(50 * time.Millisecond) // let the reader actually park inside writeMu.Lock()

		// Step 3: reset tries gate.Exclusive() — blocks, since the reader
		// holds gate.Op.
		resetDone := make(chan struct{})
		go func() {
			done := gate.Exclusive()
			done()
			close(resetDone)
		}()
		time.Sleep(50 * time.Millisecond) // let reset actually start waiting/blocking new readers

		// Step 4: let the writer (still holding writeMu since step 1) now
		// attempt gate.Op — under the old order, this blocks too (a
		// writer, reset, is already queued), closing the cycle: reader
		// blocked on writeMu (held by the writer), the writer blocked on
		// gate.Op (queued behind reset), reset blocked on the reader's
		// gate.Op draining, which it now never will.
		close(writerCanTryGateOp)

		select {
		case <-resetDone:
			t.Fatal("expected the old (writeMu outer, gate.Op inner) order to deadlock here, but reset completed anyway -- this test no longer reproduces the regression it's named for")
		case <-time.After(time.Second):
			// Expected: genuinely deadlocked, exactly as the review
			// described. (The leaked goroutines are harmless -- this
			// subtest's own process-local state is discarded with the
			// test binary.)
		}
	})
}

// assertBothFinish requires both a and b to close within a generous
// bound — used where the *absence* of a deadlock, not a specific value,
// is the assertion. Once a channel is observed closed, it's set to nil —
// a nil channel's receive case in a select never fires — so each of the
// two loop iterations waits for a genuinely distinct signal instead of
// double-counting the same one.
func assertBothFinish(t *testing.T, a, b <-chan struct{}, label string) {
	t.Helper()
	deadline := time.After(time.Second)
	for i := 0; i < 2; i++ {
		select {
		case <-a:
			a = nil
		case <-b:
			b = nil
		case <-deadline:
			t.Fatalf("%s: reader/reset/writer did not all finish within 2s -- deadlocked", label)
		}
	}
}
