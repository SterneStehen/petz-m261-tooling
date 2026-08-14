package faults_test

import (
	"errors"
	"math"
	"testing"

	"github.com/SterneStehen/petz-m261-tooling/gen/go/m261points"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/faults"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/store"
)

// TestInjectAndClearEveryAlarm is the Task 7 item 1/acceptance criteria
// requirement in its most literal form: every one of the 284 real
// class: alarm points accepts Inject and Clear, round-tripping through
// the Store. Table-driven over the real catalog rather than 284
// hand-written cases — the assertion is identical for every one of them.
func TestInjectAndClearEveryAlarm(t *testing.T) {
	st := store.New()
	inj := faults.NewInjector(st)

	count := 0
	for key, meta := range m261points.Points {
		if meta.Class != m261points.ClassAlarm {
			continue
		}
		count++
		t.Run(key.Device+"/"+key.Slug, func(t *testing.T) {
			if err := inj.Inject(key, 1); err != nil {
				t.Fatalf("Inject(%v, 1) = %v, want accepted", key, err)
			}
			if v, _ := st.Get(key); v != 1 {
				t.Fatalf("stored value after Inject(1) = %v, want 1", v)
			}
			if err := inj.Clear(key); err != nil {
				t.Fatalf("Clear(%v) = %v, want accepted", key, err)
			}
			if v, _ := st.Get(key); v != 0 {
				t.Fatalf("stored value after Clear = %v, want 0", v)
			}
		})
	}
	if count != 284 {
		t.Fatalf("iterated %d class:alarm points, want exactly 284 (AGENT-CODE-REVIEW.md, Scope invariant)", count)
	}
}

// TestInjectRejectsUnknownPoint proves an unresolvable (device, point)
// pair is rejected without touching the Store — it can't touch the Store
// since there's nothing to touch, but ErrUnknownPoint specifically (not
// some other error) is the contract callers (controlapi) map to the
// unknown_point error code.
func TestInjectRejectsUnknownPoint(t *testing.T) {
	st := store.New()
	inj := faults.NewInjector(st)
	err := inj.Inject(m261points.PointKey{Device: "NOPE", Slug: "nope"}, 1)
	if !errors.Is(err, faults.ErrUnknownPoint) {
		t.Errorf("Inject on an unknown point = %v, want ErrUnknownPoint", err)
	}
}

// TestInjectRejectsNonAlarmClass covers both other classes explicitly —
// a setpoint (which has its own, entirely different entry point,
// commands.Processor) and a telemetry point (physics.Runner's output) —
// neither is injectable through this package.
func TestInjectRejectsNonAlarmClass(t *testing.T) {
	st := store.New()
	inj := faults.NewInjector(st)

	for _, key := range []m261points.PointKey{
		{Device: "EMS", Slug: "set_active_power_kw"}, // setpoint
		{Device: "BMS", Slug: "soc"},                 // telemetry
	} {
		before, _ := st.Get(key)
		err := inj.Inject(key, 1)
		if !errors.Is(err, faults.ErrNotAlarmClass) {
			t.Errorf("Inject(%v, 1) = %v, want ErrNotAlarmClass", key, err)
		}
		if after, _ := st.Get(key); after != before {
			t.Errorf("%v changed from %v to %v after a rejected Inject", key, before, after)
		}
	}
}

// TestInjectRejectsValueOutsideEnum proves enum membership is actually
// checked, not just "any finite value accepted because the point has an
// enum" — 1 is a valid Fault/Trip-style value for a two-state alarm
// enum, 7 isn't.
func TestInjectRejectsValueOutsideEnum(t *testing.T) {
	st := store.New()
	inj := faults.NewInjector(st)
	key := m261points.PointKey{Device: "BMS", Slug: "cell_temperature_too_high"}
	meta := m261points.Points[key]
	if meta.Enum == nil {
		t.Fatalf("%v has no enum in the real catalog — fixture assumption broken, pick a different point", key)
	}
	if _, ok := meta.Enum[7]; ok {
		t.Fatalf("%v's enum unexpectedly contains 7 — pick a different out-of-range value", key)
	}

	before, _ := st.Get(key)
	err := inj.Inject(key, 7)
	if !errors.Is(err, faults.ErrInvalidValue) {
		t.Errorf("Inject(%v, 7) = %v, want ErrInvalidValue", key, err)
	}
	if after, _ := st.Get(key); after != before {
		t.Errorf("%v changed from %v to %v after a rejected Inject", key, before, after)
	}
}

// TestInjectRejectsNonFiniteValue mirrors commands.validateValue's own
// unconditional finite check (Task 6 item 1) — alarms get the same
// discipline even though they have no scale/data_type representability
// domain to reason about.
func TestInjectRejectsNonFiniteValue(t *testing.T) {
	st := store.New()
	inj := faults.NewInjector(st)
	key := m261points.PointKey{Device: "BMS", Slug: "cell_temperature_too_high"}
	for _, v := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if err := inj.Inject(key, v); !errors.Is(err, faults.ErrInvalidValue) {
			t.Errorf("Inject(%v, %v) = %v, want ErrInvalidValue", key, v, err)
		}
	}
}

// TestClearRejectsNonAlarmClass proves Clear enforces the same
// class-scoping Inject does — it isn't a generic "zero out any point"
// escape hatch.
func TestClearRejectsNonAlarmClass(t *testing.T) {
	st := store.New()
	inj := faults.NewInjector(st)
	key := m261points.PointKey{Device: "EMS", Slug: "set_active_power_kw"}
	if err := inj.Clear(key); !errors.Is(err, faults.ErrNotAlarmClass) {
		t.Errorf("Clear(%v) = %v, want ErrNotAlarmClass", key, err)
	}
}

// TestInjectRestrictsNoEnumAlarmPointsToBoolean is the reviewed fix: the
// three real class:alarm points with no catalog enum
// (EMS/custom_prompt_1/2/3) are wire-Boolean on both protocols (a Modbus
// discrete input, IEC-104 M_SP_NA_1) — a value like 2 used to pass the
// general [0, 255] U8 range check and land in the Store, even though no
// protocol readback can ever report anything but 0 or 1 for it, silently
// disagreeing with whatever Inject just wrote.
func TestInjectRestrictsNoEnumAlarmPointsToBoolean(t *testing.T) {
	noEnumAlarms := []m261points.PointKey{
		{Device: "EMS", Slug: "custom_prompt_1"},
		{Device: "EMS", Slug: "custom_prompt_2"},
		{Device: "EMS", Slug: "custom_prompt_3"},
	}
	for _, key := range noEnumAlarms {
		meta, ok := m261points.Points[key]
		if !ok {
			t.Fatalf("%v not found in the real catalog — fixture assumption broken", key)
		}
		if meta.Class != m261points.ClassAlarm {
			t.Fatalf("%v is class %s, want alarm — fixture assumption broken", key, meta.Class)
		}
		if meta.Enum != nil {
			t.Fatalf("%v unexpectedly has an enum — fixture assumption broken, this test needs a no-enum alarm point", key)
		}

		st := store.New()
		inj := faults.NewInjector(st)

		for _, v := range []float64{0, 1} {
			if err := inj.Inject(key, v); err != nil {
				t.Errorf("Inject(%v, %v) = %v, want nil (0 and 1 are the Boolean domain)", key, v, err)
			}
		}
		before, _ := st.Get(key)
		for _, v := range []float64{2, 7, 255, -1, 0.5} {
			if err := inj.Inject(key, v); !errors.Is(err, faults.ErrInvalidValue) {
				t.Errorf("Inject(%v, %v) = %v, want ErrInvalidValue (outside {0, 1})", key, v, err)
			}
		}
		if after, _ := st.Get(key); after != before {
			t.Errorf("%v changed from %v to %v after a rejected Inject", key, before, after)
		}
	}
}
