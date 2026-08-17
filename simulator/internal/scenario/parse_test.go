package scenario_test

import (
	"errors"
	"testing"

	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/scenario"
)

// validExample is the corrected example from AGENT-TASK.md, Task 7,
// item 4 — canonical catalog keys only (EMS/set_active_power_kw, BMS/soc),
// the structured link: {protocol, mode} shape.
const validExample = `
name: charge_then_link_loss_then_overheat
clock: {start: "2026-08-12T00:00:00+03:00", speed: 60}
steps:
  - at: 0s
    write: {device: EMS, point: set_operating_mode, value: 2}
  - at: 5s
    write: {device: EMS, point: set_active_power_kw, value: -100}
  - at: 30m
    expect: {device: BMS, point: soc, min: 30}
  - at: 35m
    link: {protocol: iec104, mode: drop}
  - at: 40m
    fault: {device: BMS, point: cell_temperature_too_high, value: 1}
  - at: 45m
    expect: {device: EMS, point: discharge_prohibition_protection, value: 1}
`

func TestParseAcceptsValidExample(t *testing.T) {
	s, err := scenario.Parse([]byte(validExample))
	if err != nil {
		t.Fatalf("Parse(validExample) = %v, want accepted", err)
	}
	if s.Name != "charge_then_link_loss_then_overheat" {
		t.Errorf("Name = %q", s.Name)
	}
	if len(s.Steps) != 6 {
		t.Fatalf("len(Steps) = %d, want 6", len(s.Steps))
	}
	if s.Steps[0].Write == nil || s.Steps[0].Write.Value != 2 {
		t.Errorf("Steps[0] = %+v, want a write of value 2", s.Steps[0])
	}
	if s.Steps[2].Expect == nil || s.Steps[2].Expect.Min == nil || *s.Steps[2].Expect.Min != 30 {
		t.Errorf("Steps[2] = %+v, want an expect with min=30", s.Steps[2])
	}
	if s.Steps[3].Link == nil || s.Steps[3].Link.Protocol != "iec104" || s.Steps[3].Link.Mode != "drop" {
		t.Errorf("Steps[3] = %+v, want link{iec104, drop}", s.Steps[3])
	}
}

func TestParseRejectsUnknownTopLevelKey(t *testing.T) {
	_, err := scenario.Parse([]byte(`
name: x
clock: {start: "2026-08-12T00:00:00+03:00", speed: 1}
bogus_field: 1
steps:
  - at: 0s
    write: {device: EMS, point: set_operating_mode, value: 2}
`))
	if !errors.Is(err, scenario.ErrMalformed) {
		t.Errorf("Parse with an unknown top-level key = %v, want ErrMalformed", err)
	}
}

func TestParseRejectsUnknownStepKey(t *testing.T) {
	_, err := scenario.Parse([]byte(`
name: x
clock: {start: "2026-08-12T00:00:00+03:00", speed: 1}
steps:
  - at: 0s
    bogus: {device: EMS, point: set_operating_mode, value: 2}
`))
	if !errors.Is(err, scenario.ErrMalformed) {
		t.Errorf("Parse with an unknown step action key = %v, want ErrMalformed", err)
	}
}

func TestParseRejectsUnknownPoint(t *testing.T) {
	_, err := scenario.Parse([]byte(`
name: x
clock: {start: "2026-08-12T00:00:00+03:00", speed: 1}
steps:
  - at: 0s
    write: {device: EMS, point: this_point_does_not_exist, value: 1}
`))
	if !errors.Is(err, scenario.ErrMalformed) {
		t.Errorf("Parse with an unknown point = %v, want ErrMalformed", err)
	}
}

func TestParseRejectsStepWithNoAction(t *testing.T) {
	_, err := scenario.Parse([]byte(`
name: x
clock: {start: "2026-08-12T00:00:00+03:00", speed: 1}
steps:
  - at: 0s
`))
	if !errors.Is(err, scenario.ErrMalformed) {
		t.Errorf("Parse with a step that has no action = %v, want ErrMalformed", err)
	}
}

func TestParseRejectsStepWithTwoActions(t *testing.T) {
	_, err := scenario.Parse([]byte(`
name: x
clock: {start: "2026-08-12T00:00:00+03:00", speed: 1}
steps:
  - at: 0s
    write: {device: EMS, point: set_operating_mode, value: 2}
    fault: {device: BMS, point: cell_temperature_too_high, value: 1}
`))
	if !errors.Is(err, scenario.ErrMalformed) {
		t.Errorf("Parse with a step declaring both write and fault = %v, want ErrMalformed", err)
	}
}

func TestParseRejectsRetrogradeAt(t *testing.T) {
	_, err := scenario.Parse([]byte(`
name: x
clock: {start: "2026-08-12T00:00:00+03:00", speed: 1}
steps:
  - at: 10s
    write: {device: EMS, point: set_operating_mode, value: 2}
  - at: 5s
    write: {device: EMS, point: set_active_power_kw, value: 1}
`))
	if !errors.Is(err, scenario.ErrMalformed) {
		t.Errorf("Parse with a retrograde at: = %v, want ErrMalformed", err)
	}
}

func TestParseAllowsEqualAtOnDifferentPoints(t *testing.T) {
	_, err := scenario.Parse([]byte(`
name: x
clock: {start: "2026-08-12T00:00:00+03:00", speed: 1}
steps:
  - at: 5s
    write: {device: EMS, point: set_operating_mode, value: 2}
  - at: 5s
    write: {device: EMS, point: set_active_power_kw, value: 1}
`))
	if err != nil {
		t.Errorf("Parse with equal at: on two different points = %v, want accepted", err)
	}
}

func TestParseRejectsDuplicateSamePointSameAt(t *testing.T) {
	_, err := scenario.Parse([]byte(`
name: x
clock: {start: "2026-08-12T00:00:00+03:00", speed: 1}
steps:
  - at: 5s
    write: {device: EMS, point: set_active_power_kw, value: 1}
  - at: 5s
    write: {device: EMS, point: set_active_power_kw, value: 2}
`))
	if !errors.Is(err, scenario.ErrMalformed) {
		t.Errorf("Parse with two writes to the same point at the same at: = %v, want ErrMalformed", err)
	}
}

func TestParseRejectsExpectWithNeitherValueNorRange(t *testing.T) {
	_, err := scenario.Parse([]byte(`
name: x
clock: {start: "2026-08-12T00:00:00+03:00", speed: 1}
steps:
  - at: 0s
    expect: {device: BMS, point: soc}
`))
	if !errors.Is(err, scenario.ErrMalformed) {
		t.Errorf("Parse with an expect that has neither value nor min/max = %v, want ErrMalformed", err)
	}
}

func TestParseRejectsExpectWithBothValueAndRange(t *testing.T) {
	_, err := scenario.Parse([]byte(`
name: x
clock: {start: "2026-08-12T00:00:00+03:00", speed: 1}
steps:
  - at: 0s
    expect: {device: BMS, point: soc, value: 30, min: 10}
`))
	if !errors.Is(err, scenario.ErrMalformed) {
		t.Errorf("Parse with an expect declaring both value and min = %v, want ErrMalformed", err)
	}
}

func TestParseRejectsBadLinkProtocol(t *testing.T) {
	_, err := scenario.Parse([]byte(`
name: x
clock: {start: "2026-08-12T00:00:00+03:00", speed: 1}
steps:
  - at: 0s
    link: {protocol: bogus, mode: drop}
`))
	if !errors.Is(err, scenario.ErrMalformed) {
		t.Errorf("Parse with link.protocol=bogus = %v, want ErrMalformed", err)
	}
}

func TestParseRejectsBadLinkMode(t *testing.T) {
	_, err := scenario.Parse([]byte(`
name: x
clock: {start: "2026-08-12T00:00:00+03:00", speed: 1}
steps:
  - at: 0s
    link: {protocol: both, mode: bogus}
`))
	if !errors.Is(err, scenario.ErrMalformed) {
		t.Errorf("Parse with link.mode=bogus = %v, want ErrMalformed", err)
	}
}

func TestParseRejectsDelayModeWithoutPositiveDelayMS(t *testing.T) {
	_, err := scenario.Parse([]byte(`
name: x
clock: {start: "2026-08-12T00:00:00+03:00", speed: 1}
steps:
  - at: 0s
    link: {protocol: both, mode: delay}
`))
	if !errors.Is(err, scenario.ErrMalformed) {
		t.Errorf("Parse with link.mode=delay and no delay_ms = %v, want ErrMalformed", err)
	}
}

// TestParseRejectsOverflowingDelayMS is the fourth review round's
// duration-overflow finding applied to the scenario dialect's own
// link.delay_ms — mirrors controlapi's identical
// TestSetLinkRejectsOverflowingDelayMS for the same underlying bug:
// delay_ms * time.Millisecond must fit in a time.Duration, or it
// silently wraps (Go doesn't panic on integer overflow) instead of
// erroring.
func TestParseRejectsOverflowingDelayMS(t *testing.T) {
	_, err := scenario.Parse([]byte(`
name: x
clock: {start: "2026-08-12T00:00:00+03:00", speed: 1}
steps:
  - at: 0s
    link: {protocol: both, mode: delay, delay_ms: 9223372036854775807}
`))
	if !errors.Is(err, scenario.ErrMalformed) {
		t.Errorf("Parse with an overflowing link.delay_ms = %v, want ErrMalformed", err)
	}
}

// TestParseAcceptsExtremelySmallSpeedRegardlessOfRealStepInterval is
// blocker 1's Parse-level regression test (fifth review round): Parse no
// longer checks clock.speed against anything but its own syntax (finite,
// positive) — the real speed/stepInterval representability check now
// lives in scenario.Runner.Load (physics.ValidatePacing), against the
// Runner's real stepInterval, not an artificial 24h stand-in Parse used
// to check against. This speed would have overflowed that old surrogate
// (CheckedPace(24*time.Hour, 1e-6) overflows int64 nanoseconds) even
// though it's perfectly representable at any real, much smaller
// stepInterval (see TestLoadAcceptsValidPacingAtRealStepInterval in
// runner_test.go, at stepInterval=1s) — Parse alone must accept it.
func TestParseAcceptsExtremelySmallSpeedRegardlessOfRealStepInterval(t *testing.T) {
	_, err := scenario.Parse([]byte(`
name: x
clock: {start: "2026-08-12T00:00:00+03:00", speed: 0.000001}
steps:
  - at: 0s
    write: {device: EMS, point: set_operating_mode, value: 2}
`))
	if err != nil {
		t.Errorf("Parse with speed=1e-6 = %v, want accepted (Parse only checks syntax now)", err)
	}
}

func TestParseAcceptsLinkClear(t *testing.T) {
	s, err := scenario.Parse([]byte(`
name: x
clock: {start: "2026-08-12T00:00:00+03:00", speed: 1}
steps:
  - at: 0s
    link: {protocol: both, mode: clear}
`))
	if err != nil {
		t.Fatalf("Parse with link.mode=clear = %v, want accepted", err)
	}
	if s.Steps[0].Link.Mode != "clear" {
		t.Errorf("Steps[0].Link.Mode = %q, want clear", s.Steps[0].Link.Mode)
	}
}

func TestParseRejectsMalformedAt(t *testing.T) {
	_, err := scenario.Parse([]byte(`
name: x
clock: {start: "2026-08-12T00:00:00+03:00", speed: 1}
steps:
  - at: not-a-duration
    write: {device: EMS, point: set_operating_mode, value: 2}
`))
	if !errors.Is(err, scenario.ErrMalformed) {
		t.Errorf("Parse with a malformed at: = %v, want ErrMalformed", err)
	}
}

func TestParseRejectsEmptySteps(t *testing.T) {
	_, err := scenario.Parse([]byte(`
name: x
clock: {start: "2026-08-12T00:00:00+03:00", speed: 1}
steps: []
`))
	if !errors.Is(err, scenario.ErrMalformed) {
		t.Errorf("Parse with an empty steps list = %v, want ErrMalformed", err)
	}
}

func TestParseRejectsNonPositiveSpeed(t *testing.T) {
	for _, speed := range []string{"0", "-1"} {
		_, err := scenario.Parse([]byte(`
name: x
clock: {start: "2026-08-12T00:00:00+03:00", speed: ` + speed + `}
steps:
  - at: 0s
    write: {device: EMS, point: set_operating_mode, value: 2}
`))
		if !errors.Is(err, scenario.ErrMalformed) {
			t.Errorf("Parse with speed=%s = %v, want ErrMalformed", speed, err)
		}
	}
}
