package scenario

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/SterneStehen/petz-m261-tooling/gen/go/m261points"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/physics"
)

// ErrMalformed is Parse's sentinel for every load-time rejection —
// unknown keys, an unresolvable action shape, an unknown device/point, a
// bad link protocol/mode, a non-monotonic or duplicate at: (AGENT-TASK.md,
// Task 7 item 5) — deliberately one error kind, not one per cause: the
// contract callers rely on is "the whole file was rejected as a whole,
// before any step ran" (item 5's "reject the whole thing, don't apply it
// partially"), not which specific rule fired.
var ErrMalformed = errors.New("scenario: malformed")

// maxDelayMS is the largest link.delay_ms that, multiplied by
// time.Millisecond, still fits in a time.Duration — see controlapi's
// identical constant (simulator/internal/controlapi/handlers.go) for the
// overflow-wraps-to-a-negative-duration bug this guards against.
const maxDelayMS = int64(math.MaxInt64) / int64(time.Millisecond)

// raw* mirror the YAML shape exactly (AGENT-TASK.md, Task 7 item 4's
// example) — kept separate from the semantic Scenario/Step/*Action types
// so parsing (strings, optional pointers) and the validated, executable
// result (time.Duration, required fields) don't have to compromise on
// either's ideal shape.
type rawScenario struct {
	Name  string    `yaml:"name"`
	Clock rawClock  `yaml:"clock"`
	Steps []rawStep `yaml:"steps"`
}

type rawClock struct {
	Start string  `yaml:"start"`
	Speed float64 `yaml:"speed"`
}

type rawStep struct {
	At     string         `yaml:"at"`
	Write  *rawPointValue `yaml:"write"`
	Fault  *rawPointValue `yaml:"fault"`
	Expect *rawExpect     `yaml:"expect"`
	Link   *rawLink       `yaml:"link"`
}

// Value is a pointer, not a plain float64 — a missing value: key must be
// a load-time rejection (AGENT-TASK.md, Task 7 item 5: malformed actions
// fail before execution), not silently decode to the zero value and get
// executed as if the author had written value: 0.
type rawPointValue struct {
	Device string   `yaml:"device"`
	Point  string   `yaml:"point"`
	Value  *float64 `yaml:"value"`
}

type rawExpect struct {
	Device string   `yaml:"device"`
	Point  string   `yaml:"point"`
	Value  *float64 `yaml:"value"`
	Min    *float64 `yaml:"min"`
	Max    *float64 `yaml:"max"`
}

type rawLink struct {
	Protocol string `yaml:"protocol"`
	Mode     string `yaml:"mode"`
	DelayMS  int    `yaml:"delay_ms"`
}

// Parse decodes and fully validates data as a scenario file, per
// AGENT-TASK.md, Task 7 item 4-5. Every rejection wraps ErrMalformed.
// Nothing about a step is executed here — Parse only builds the
// in-memory Scenario a Runner later plays back.
func Parse(data []byte) (*Scenario, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true) // Task 7 item 5: unknown keys fail before execution
	var raw rawScenario
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	// A second document in the same file (a "---"-separated multi-doc
	// YAML stream) is rejected outright rather than silently ignored —
	// decoding only the first document and discarding the rest without
	// telling the caller would violate the same "one scenario, fully
	// valid or fully rejected" contract everything else in Parse follows.
	var trailing yaml.Node
	if err := dec.Decode(&trailing); err == nil {
		return nil, fmt.Errorf("%w: file contains more than one YAML document", ErrMalformed)
	} else if !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	if raw.Name == "" {
		return nil, fmt.Errorf("%w: name is required", ErrMalformed)
	}
	start, err := time.Parse(time.RFC3339, raw.Clock.Start)
	if err != nil {
		return nil, fmt.Errorf("%w: clock.start: %v", ErrMalformed, err)
	}
	// speed <= 0 alone doesn't reject NaN (every comparison against NaN
	// is false in Go, so "NaN <= 0" is false and a YAML .nan literal
	// would slip through) or +Inf (Inf <= 0 is also false) — both are
	// checked explicitly, matching physics.validSpeed's identical guard
	// on the -speed flag (main.go) for the same reason: a scenario's own
	// clock.speed reaches the exact same physics.Runner.
	// PacedFastForwardLocked call that flag ultimately feeds.
	if math.IsNaN(raw.Clock.Speed) || math.IsInf(raw.Clock.Speed, 0) {
		return nil, fmt.Errorf("%w: clock.speed must be finite, got %v", ErrMalformed, raw.Clock.Speed)
	}
	if raw.Clock.Speed <= 0 {
		return nil, fmt.Errorf("%w: clock.speed must be positive, got %v", ErrMalformed, raw.Clock.Speed)
	}
	// A finite, positive speed can still make stepInterval/speed overflow
	// a time.Duration at run time — an extreme value like 1e-300 turns
	// even a 1s step into an implementation-defined ~1ns real interval
	// instead of "almost stopped" (third review round, reproduced against
	// the live simulator; see physics.CheckedPace). This scenario's own
	// stepInterval isn't known yet at Parse time (it's a Runner
	// construction parameter, not a scenario field), so this checks
	// against a generously large stand-in (24h) that would never
	// legitimately appear as a real stepInterval — anything that
	// overflows even that bound is rejected now, at load time, rather
	// than discovered mid-run; physics.Runner.PacedFastForwardLocked
	// performs the authoritative check against the real stepInterval
	// every time this scenario actually ticks.
	if _, err := physics.CheckedPace(24*time.Hour, raw.Clock.Speed); err != nil {
		return nil, fmt.Errorf("%w: clock.speed %v: %v", ErrMalformed, raw.Clock.Speed, err)
	}
	if len(raw.Steps) == 0 {
		return nil, fmt.Errorf("%w: steps is empty", ErrMalformed)
	}

	steps := make([]Step, 0, len(raw.Steps))
	type target struct {
		key m261points.PointKey
		at  time.Duration
	}
	seenTargets := make(map[target]bool, len(raw.Steps))
	var lastAt time.Duration
	haveLastAt := false

	for i, rs := range raw.Steps {
		at, err := time.ParseDuration(rs.At)
		if err != nil {
			return nil, fmt.Errorf("%w: step %d: at: %v", ErrMalformed, i, err)
		}
		if at < 0 {
			return nil, fmt.Errorf("%w: step %d: at: %s is negative", ErrMalformed, i, at)
		}
		if haveLastAt && at < lastAt {
			return nil, fmt.Errorf("%w: step %d: at: %s is retrograde (step %d was at: %s)", ErrMalformed, i, at, i-1, lastAt)
		}
		lastAt, haveLastAt = at, true

		step, targetKey, hasTarget, err := parseStep(i, rs)
		if err != nil {
			return nil, err
		}
		step.At = at
		steps = append(steps, step)

		if hasTarget {
			t := target{key: targetKey, at: at}
			if seenTargets[t] {
				return nil, fmt.Errorf(
					"%w: step %d: another write/fault step already targets %s/%s at the same at: %s",
					ErrMalformed, i, targetKey.Device, targetKey.Slug, at,
				)
			}
			seenTargets[t] = true
		}
	}

	return &Scenario{
		Name:  raw.Name,
		Clock: ClockSpec{Start: start, Speed: raw.Clock.Speed},
		Steps: steps,
	}, nil
}

// parseStep validates exactly one of Write/Fault/Expect/Link is present
// and resolves it into a Step. Returns the (device, point) a Write or
// Fault targets, for Parse's same-point-same-at: duplicate check —
// Expect and Link don't participate (see Parse's doc comment on that
// rule's scope).
func parseStep(i int, rs rawStep) (Step, m261points.PointKey, bool, error) {
	present := 0
	if rs.Write != nil {
		present++
	}
	if rs.Fault != nil {
		present++
	}
	if rs.Expect != nil {
		present++
	}
	if rs.Link != nil {
		present++
	}
	if present != 1 {
		return Step{}, m261points.PointKey{}, false, fmt.Errorf(
			"%w: step %d: exactly one of write/fault/expect/link is required, got %d", ErrMalformed, i, present,
		)
	}

	switch {
	case rs.Write != nil:
		key, err := resolvePoint(i, "write", rs.Write.Device, rs.Write.Point)
		if err != nil {
			return Step{}, m261points.PointKey{}, false, err
		}
		if rs.Write.Value == nil {
			return Step{}, m261points.PointKey{}, false, fmt.Errorf("%w: step %d: write: value is required", ErrMalformed, i)
		}
		if err := requireFinite(i, "write: value", *rs.Write.Value); err != nil {
			return Step{}, m261points.PointKey{}, false, err
		}
		return Step{Write: &WriteAction{Device: rs.Write.Device, Point: rs.Write.Point, Value: *rs.Write.Value}}, key, true, nil

	case rs.Fault != nil:
		key, err := resolvePoint(i, "fault", rs.Fault.Device, rs.Fault.Point)
		if err != nil {
			return Step{}, m261points.PointKey{}, false, err
		}
		if rs.Fault.Value == nil {
			return Step{}, m261points.PointKey{}, false, fmt.Errorf("%w: step %d: fault: value is required", ErrMalformed, i)
		}
		if err := requireFinite(i, "fault: value", *rs.Fault.Value); err != nil {
			return Step{}, m261points.PointKey{}, false, err
		}
		return Step{Fault: &FaultAction{Device: rs.Fault.Device, Point: rs.Fault.Point, Value: *rs.Fault.Value}}, key, true, nil

	case rs.Expect != nil:
		if _, err := resolvePoint(i, "expect", rs.Expect.Device, rs.Expect.Point); err != nil {
			return Step{}, m261points.PointKey{}, false, err
		}
		hasValue := rs.Expect.Value != nil
		hasRange := rs.Expect.Min != nil || rs.Expect.Max != nil
		if hasValue == hasRange { // both false, or both true — either way, ambiguous/missing
			return Step{}, m261points.PointKey{}, false, fmt.Errorf(
				"%w: step %d: expect requires exactly one of value or min/max", ErrMalformed, i,
			)
		}
		for name, v := range map[string]*float64{"expect: value": rs.Expect.Value, "expect: min": rs.Expect.Min, "expect: max": rs.Expect.Max} {
			if v != nil {
				if err := requireFinite(i, name, *v); err != nil {
					return Step{}, m261points.PointKey{}, false, err
				}
			}
		}
		if rs.Expect.Min != nil && rs.Expect.Max != nil && *rs.Expect.Min > *rs.Expect.Max {
			return Step{}, m261points.PointKey{}, false, fmt.Errorf(
				"%w: step %d: expect: min (%v) > max (%v)", ErrMalformed, i, *rs.Expect.Min, *rs.Expect.Max,
			)
		}
		return Step{Expect: &ExpectAction{
			Device: rs.Expect.Device, Point: rs.Expect.Point,
			Value: rs.Expect.Value, Min: rs.Expect.Min, Max: rs.Expect.Max,
		}}, m261points.PointKey{}, false, nil

	default: // rs.Link != nil
		switch rs.Link.Protocol {
		case "iec104", "modbus", "both":
		default:
			return Step{}, m261points.PointKey{}, false, fmt.Errorf(
				"%w: step %d: link.protocol %q is not one of iec104/modbus/both", ErrMalformed, i, rs.Link.Protocol,
			)
		}
		switch rs.Link.Mode {
		case "drop", "hang", "heartbeat_pause", "clear":
		case "delay":
			if rs.Link.DelayMS <= 0 {
				return Step{}, m261points.PointKey{}, false, fmt.Errorf(
					"%w: step %d: link.mode: delay requires a positive delay_ms, got %d", ErrMalformed, i, rs.Link.DelayMS,
				)
			}
			// See controlapi's identical maxDelayMS check: delay_ms *
			// time.Millisecond must fit in a time.Duration, or it
			// silently wraps (Go doesn't panic on integer overflow)
			// instead of erroring — accepted, but the resulting "delay"
			// defeats the request rather than honoring it.
			if int64(rs.Link.DelayMS) > maxDelayMS {
				return Step{}, m261points.PointKey{}, false, fmt.Errorf(
					"%w: step %d: link.delay_ms %d exceeds the maximum representable duration (%d ms)", ErrMalformed, i, rs.Link.DelayMS, maxDelayMS,
				)
			}
		default:
			return Step{}, m261points.PointKey{}, false, fmt.Errorf(
				"%w: step %d: link.mode %q is not one of drop/hang/delay/heartbeat_pause/clear", ErrMalformed, i, rs.Link.Mode,
			)
		}
		return Step{Link: &LinkAction{Protocol: rs.Link.Protocol, Mode: rs.Link.Mode, DelayMS: rs.Link.DelayMS}},
			m261points.PointKey{}, false, nil
	}
}

// requireFinite rejects a NaN/±Inf value at load time — YAML's .nan/.inf/
// -.inf literals decode to real IEEE-754 NaN/Inf float64s, and every
// value-carrying field here would otherwise reach commands.Processor.
// Validate/faults.Injector.Validate's own finite check anyway, just
// after Load already has to fail (validateAll), one step later than
// necessary and with a less specific error.
func requireFinite(i int, name string, v float64) error {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return fmt.Errorf("%w: step %d: %s: %v is not finite", ErrMalformed, i, name, v)
	}
	return nil
}

func resolvePoint(i int, actionName, device, point string) (m261points.PointKey, error) {
	if device == "" || point == "" {
		return m261points.PointKey{}, fmt.Errorf("%w: step %d: %s: device and point are both required", ErrMalformed, i, actionName)
	}
	key := m261points.PointKey{Device: device, Slug: point}
	if _, ok := m261points.Points[key]; !ok {
		return key, fmt.Errorf("%w: step %d: %s: unknown point %s/%s", ErrMalformed, i, actionName, device, point)
	}
	return key, nil
}
