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
}

func NewInjector(st *store.Store) *Injector {
	return &Injector{store: st}
}

// Inject validates key is a real class: alarm point and value is a
// member of its enum (where the catalog defines one — a small number of
// alarm points may not, in which case any finite value is accepted, the
// same "don't check against a list that doesn't exist" rule Task 6 item 1
// applies to setpoints), then writes it straight to the Store. No
// watchdog, no mode arbitration, no dispatched-power effect — alarms
// don't participate in any of that.
func (i *Injector) Inject(key m261points.PointKey, value float64) error {
	meta, err := i.resolveAlarm(key)
	if err != nil {
		return err
	}
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return fmt.Errorf("%w: %v is not finite", ErrInvalidValue, value)
	}
	if meta.Enum != nil {
		rounded := math.Round(value)
		if rounded != value {
			return fmt.Errorf("%w: %v is not an integer, so it cannot match an enum key", ErrInvalidValue, value)
		}
		if _, ok := meta.Enum[int(rounded)]; !ok {
			return fmt.Errorf("%w: %v is not one of the allowed enum values %v", ErrInvalidValue, value, meta.Enum)
		}
	}
	i.store.Set(key, value)
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
