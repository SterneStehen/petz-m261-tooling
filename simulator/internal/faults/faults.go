// Package faults implements Task 7 item 1's fault injection: the one
// entry point both the control API (controlapi's POST /faults) and the
// scenario engine's fault: step funnel through, so neither has its own
// separate rule for what may be injected (AGENT-TASK.md, Task 7 item 1).
//
// Injection is restricted to class: alarm points only — not setpoint
// (those go through commands.Processor, a different domain entirely) and
// not telemetry (those are physics.Runner's output; injecting into one
// directly would silently disagree with the model the very next tick).
// An alarm has no manufacturer-documented physical cause in this
// simulator (Task 5 doesn't model any of the 284 alarm conditions), so
// setting it directly, rather than trying to simulate a triggering
// condition, is the only approach that doesn't invent behavior the
// source data doesn't contain (AGENT-TASK §1 rule 1).
package faults

import (
	"errors"
	"fmt"
	"math"

	"github.com/SterneStehen/petz-m261-tooling/gen/go/m261points"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/appgate"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/store"
)

// ErrUnknownPoint, ErrNotAlarmClass and ErrInvalidValue are Inject/Clear's
// sentinel errors — distinct causes a caller (controlapi's HTTP layer)
// maps to distinct, stable error codes rather than one generic failure.
var (
	ErrUnknownPoint  = errors.New("faults: no such point in the catalog")
	ErrNotAlarmClass = errors.New("faults: point exists but is not class=alarm")
	ErrInvalidValue  = errors.New("faults: value is not a member of the point's enum")
)

// Injector is the shared implementation behind both injection paths.
// Holds only a *store.Store — alarm injection has no interaction with
// commands.Processor (that's the setpoint-only domain, Task 6) or with
// physics.Engine (alarms have no physical model to disturb).
type Injector struct {
	store *store.Store
	gate  *appgate.Gate
}

func NewInjector(st *store.Store) *Injector {
	return &Injector{store: st}
}

// SetGate wires the process-wide reset-atomicity gate (package appgate)
// — Inject/Clear become shared (Op) operations against it once set, so a
// controlapi.Server.doReset's exclusive section can never interleave
// with one. nil (never calling SetGate) disables gating.
func (i *Injector) SetGate(g *appgate.Gate) { i.gate = g }

func (i *Injector) opDone() func() {
	if i.gate == nil {
		return func() {}
	}
	return i.gate.Op()
}

// Inject validates key is a real class: alarm point and value is
// representable and, where the catalog defines one, a member of its enum
// — then writes it straight to the Store. No watchdog, no mode
// arbitration, no dispatched-power effect — alarms don't participate in
// any of that.
func (i *Injector) Inject(key m261points.PointKey, value float64) error {
	meta, err := i.resolveAlarm(key)
	if err != nil {
		return err
	}
	if err := i.validateValue(meta, value); err != nil {
		return err
	}
	done := i.opDone()
	defer done()
	i.store.Set(key, value)
	return nil
}

// Validate is Inject's dry-run half: every check Inject applies, without
// writing to the Store — used by the scenario engine to reject a whole
// scenario at load time (AGENT-TASK.md, Task 7 item 5: malformed actions
// fail before execution) if any of its fault: steps targets a value that
// would be rejected at execution time anyway, rather than letting the
// scenario run partway and fail mid-flight for a cause fully knowable up
// front.
func (i *Injector) Validate(key m261points.PointKey, value float64) error {
	meta, err := i.resolveAlarm(key)
	if err != nil {
		return err
	}
	return i.validateValue(meta, value)
}

// validateValue implements the checks Inject and Validate share.
//
// Every real alarm point is U8 (verified against the whole catalog, not
// assumed) — representability is checked against a fixed [0, 255]
// integer domain unconditionally, not only when the point happens to
// have an enum: without this, a point with no enum (three real ones —
// EMS/custom_prompt_1/2/3) would silently accept a value like 0.5,
// writing it to the Store while every protocol readback (a boolean
// discrete input for Modbus, M_SP_NA_1 for IEC-104) reports the point as
// simply "set" (nonzero) — a real Store/protocol disagreement, not a
// hypothetical one. Enum membership (where the catalog defines one) is
// checked on top, the same "independent, both apply" layering Task 6
// item 1 uses for setpoints.
func (i *Injector) validateValue(meta m261points.PointMeta, value float64) error {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return fmt.Errorf("%w: %v is not finite", ErrInvalidValue, value)
	}
	rounded := math.Round(value)
	if rounded != value {
		return fmt.Errorf("%w: %v is not an integer", ErrInvalidValue, value)
	}
	if rounded < 0 || rounded > 255 {
		return fmt.Errorf("%w: %v is outside the U8 domain [0, 255]", ErrInvalidValue, value)
	}
	if meta.Enum != nil {
		if _, ok := meta.Enum[int(rounded)]; !ok {
			return fmt.Errorf("%w: %v is not one of the allowed enum values %v", ErrInvalidValue, value, meta.Enum)
		}
	}
	return nil
}

// Clear returns an alarm point to its enum default (0 — every alarm
// enum observed in the catalog uses 0 for "Normal"/inactive; a point
// whose enum happens not to define 0 still accepts 0 here, since Clear's
// job is "no fault", not "a valid enum member").
func (i *Injector) Clear(key m261points.PointKey) error {
	if _, err := i.resolveAlarm(key); err != nil {
		return err
	}
	done := i.opDone()
	defer done()
	i.store.Set(key, 0)
	return nil
}

func (i *Injector) resolveAlarm(key m261points.PointKey) (m261points.PointMeta, error) {
	meta, ok := m261points.Points[key]
	if !ok {
		return m261points.PointMeta{}, fmt.Errorf("%w: %s/%s", ErrUnknownPoint, key.Device, key.Slug)
	}
	if meta.Class != m261points.ClassAlarm {
		return m261points.PointMeta{}, fmt.Errorf("%w: %s/%s is %s", ErrNotAlarmClass, key.Device, key.Slug, meta.Class)
	}
	return meta, nil
}
