package commands

import (
	"fmt"
	"time"
)

// WatchdogMode selects how a stale Remote-mode setpoint (Set Active
// Power/Set Reactive Power not refreshed) is handled — AGENT-TASK §7:
// unconfirmed which of the three the real EMS actually implements.
type WatchdogMode string

const (
	WatchdogHold           WatchdogMode = "hold"             // the setpoint persists forever
	WatchdogZeroAfter      WatchdogMode = "zero_after"       // dispatched power zeroes after Timeout with no write
	WatchdogSafeStateAfter WatchdogMode = "safe_state_after" // as zero_after, but latched — see Processor's doc comment
)

// Config is the commands package's own resolved configuration — plain Go
// values, not simulator/internal/config's YAML-shaped EnumParam/IntParam
// wrappers. main.go translates a loaded config.Config into this; tests
// build one directly (DefaultConfig) without touching YAML at all.
type Config struct {
	// WatchdogMode/WatchdogTimeout: §7 watchdog.mode/watchdog.timeout_s,
	// Task 6 item 5.
	WatchdogMode    WatchdogMode
	WatchdogTimeout time.Duration

	// ModePriority: §7 modes.priority, Task 6 item 6 — a permutation of
	// exactly "remote", "demand_control", "load_tracking", highest
	// priority first.
	ModePriority []string

	// AllowDangerous: §7 commands.allow_dangerous, Task 6 item 7 — gates
	// every point the catalog flags Dangerous (Trip and Clear Protection,
	// Task 1 item 8), not just Trip specifically.
	AllowDangerous bool

	// NominalPowerKW is the plant's nameplate AC power (§4.8: 130.5 kW),
	// used only as the out-of-the-box default for the "System Maximum
	// Charge/Discharge Power" setpoints — see Processor's
	// publishSensibleDefaults doc comment for why a default is needed at
	// all. Deliberately a plain float rather than an import of
	// physics.Params (commands has no reason to depend on physics), so a
	// caller wiring a non-default physics.Params must pass the matching
	// figure here too — main.go does.
	NominalPowerKW float64
}

// DefaultConfig returns §7's documented defaults, plus §4.8's nameplate
// power for NominalPowerKW. Tests that don't care about Task 6's
// configuration surface use this directly, matching physics.DefaultParams's
// role for the physics package.
func DefaultConfig() Config {
	return Config{
		WatchdogMode:    WatchdogZeroAfter,
		WatchdogTimeout: 60 * time.Second,
		ModePriority:    []string{"remote", "demand_control", "load_tracking"},
		AllowDangerous:  false,
		NominalPowerKW:  130.5,
	}
}

func (c Config) validate() error {
	switch c.WatchdogMode {
	case WatchdogHold, WatchdogZeroAfter, WatchdogSafeStateAfter:
	default:
		return fmt.Errorf("commands: WatchdogMode %q is not one of hold/zero_after/safe_state_after", c.WatchdogMode)
	}
	if c.WatchdogTimeout <= 0 {
		return fmt.Errorf("commands: WatchdogTimeout must be positive, got %s", c.WatchdogTimeout)
	}
	want := map[string]bool{"remote": true, "demand_control": true, "load_tracking": true}
	if len(c.ModePriority) != len(want) {
		return fmt.Errorf("commands: ModePriority must list exactly %v, got %v", want, c.ModePriority)
	}
	seen := make(map[string]bool, len(c.ModePriority))
	for _, name := range c.ModePriority {
		if !want[name] {
			return fmt.Errorf("commands: ModePriority contains %q, want one of remote/demand_control/load_tracking", name)
		}
		if seen[name] {
			return fmt.Errorf("commands: ModePriority lists %q more than once", name)
		}
		seen[name] = true
	}
	return nil
}
