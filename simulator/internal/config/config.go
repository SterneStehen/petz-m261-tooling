// Package config loads simulator/config/m261sim.yaml — the declaration
// point for every parameter AGENT-TASK §7 lists as unconfirmed by the
// manufacturer. Each such parameter carries its allowed values, a
// default, and an explicit Unconfirmed flag, so that a real value learned
// during Stage 0 on-site testing only ever changes this file, never Go
// source.
package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"

	"github.com/SterneStehen/petz-m261-tooling/gen/go/m261points"
)

// EnumParam is a config value constrained to a documented set of allowed
// strings, per §7's table shape (parameter / allowed values / default).
type EnumParam struct {
	Value       string   `yaml:"value"`
	Allowed     []string `yaml:"allowed"`
	Unconfirmed bool     `yaml:"unconfirmed"`
}

func (p EnumParam) validate(name string) error {
	for _, a := range p.Allowed {
		if a == p.Value {
			return nil
		}
	}
	return fmt.Errorf("config: %s = %q is not one of %v", name, p.Value, p.Allowed)
}

// IntParam is a config value with no enum constraint (§7's watchdog.timeout_s
// is documented as a plain integer, not a set of allowed strings).
type IntParam struct {
	Value       int  `yaml:"value"`
	Unconfirmed bool `yaml:"unconfirmed"`
}

// BoolParam is a plain boolean §7 parameter (commands.allow_dangerous).
type BoolParam struct {
	Value       bool `yaml:"value"`
	Unconfirmed bool `yaml:"unconfirmed"`
}

// PriorityParam is modes.priority (§7): an explicit order over exactly the
// three mode drivers named in the table — a permutation of "remote",
// "demand_control", "load_tracking", not an open-ended string list.
type PriorityParam struct {
	Value       []string `yaml:"value"`
	Unconfirmed bool     `yaml:"unconfirmed"`
}

var modePriorityNames = []string{"remote", "demand_control", "load_tracking"}

func (p PriorityParam) validate(name string) error {
	want := make(map[string]bool, len(modePriorityNames))
	for _, n := range modePriorityNames {
		want[n] = true
	}
	if len(p.Value) != len(modePriorityNames) {
		return fmt.Errorf("config: %s must list exactly %v, got %v", name, modePriorityNames, p.Value)
	}
	seen := make(map[string]bool, len(p.Value))
	for _, v := range p.Value {
		if !want[v] {
			return fmt.Errorf("config: %s contains %q, want one of %v", name, v, modePriorityNames)
		}
		if seen[v] {
			return fmt.Errorf("config: %s lists %q more than once", name, v)
		}
		seen[v] = true
	}
	return nil
}

type ModbusConfig struct {
	ByteOrder EnumParam `yaml:"byte_order"`
}
type ScalingConfig struct {
	Source EnumParam `yaml:"source"`
}
type SetpointsConfig struct {
	Apply EnumParam `yaml:"apply"`
}
type BatteryConfig struct {
	CellAddressLayout EnumParam `yaml:"cell_address_layout"`
}

// WatchdogConfig is §7's watchdog.mode/watchdog.timeout_s — Task 6 item 5:
// three modes because the real EMS's actual watchdog behavior on a stale
// remote setpoint isn't confirmed.
type WatchdogConfig struct {
	Mode     EnumParam `yaml:"mode"`
	TimeoutS IntParam  `yaml:"timeout_s"`
}

// ModesConfig is §7's modes.priority — Task 6 item 6: which mode wins when
// Remote, Demand Control, and Load Tracking are simultaneously enabled,
// unspecified by the register map.
type ModesConfig struct {
	Priority PriorityParam `yaml:"priority"`
}

// CommandsConfig is §7's commands.allow_dangerous — Task 6 item 7: Trip
// (and Clear Protection, also flagged dangerous per Task 1 item 8) are
// rejected unless explicitly allowed.
type CommandsConfig struct {
	AllowDangerous BoolParam `yaml:"allow_dangerous"`
}

// ControlAPIConfig is control_api.bind — the address Task 7's control
// API listens on (AGENT-TASK.md, Task 7 item 3). Unlike every other §7
// parameter above, this isn't a manufacturer-unconfirmed register-map
// value: no such API exists in the real M261 at all, so there's nothing
// to confirm against on-site — it's an ordinary application setting that
// belongs in this file for the same reason as the rest, so an operator
// can repoint it without a rebuild. -control-addr (main.go) remains
// available as a flag override, matching -modbus-byte-order's
// relationship to modbus.byte_order.
type ControlAPIConfig struct {
	Bind string `yaml:"bind"`
}

type Config struct {
	Modbus     ModbusConfig     `yaml:"modbus"`
	Scaling    ScalingConfig    `yaml:"scaling"`
	Watchdog   WatchdogConfig   `yaml:"watchdog"`
	Modes      ModesConfig      `yaml:"modes"`
	Commands   CommandsConfig   `yaml:"commands"`
	Setpoints  SetpointsConfig  `yaml:"setpoints"`
	Battery    BatteryConfig    `yaml:"battery"`
	ControlAPI ControlAPIConfig `yaml:"control_api"`
}

// Default returns every §7 parameter declared so far at its documented
// default, all marked unconfirmed (the table itself says none of these are
// confirmed by the manufacturer as of writing).
func Default() Config {
	return Config{
		Modbus: ModbusConfig{
			ByteOrder: EnumParam{
				Value:       "big",
				Allowed:     []string{"big", "little", "big_word_swap", "little_word_swap"},
				Unconfirmed: true,
			},
		},
		Scaling: ScalingConfig{Source: EnumParam{Value: "override", Allowed: []string{"iec104", "modbus", "override"}, Unconfirmed: true}},
		Watchdog: WatchdogConfig{
			Mode: EnumParam{
				Value:       "zero_after",
				Allowed:     []string{"hold", "zero_after", "safe_state_after"},
				Unconfirmed: true,
			},
			TimeoutS: IntParam{Value: 60, Unconfirmed: true},
		},
		Modes: ModesConfig{
			Priority: PriorityParam{Value: append([]string(nil), modePriorityNames...), Unconfirmed: true},
		},
		Commands: CommandsConfig{
			AllowDangerous: BoolParam{Value: false, Unconfirmed: true},
		},
		Setpoints: SetpointsConfig{Apply: EnumParam{Value: "immediate", Allowed: []string{"immediate", "on_restart"}, Unconfirmed: true}},
		Battery:   BatteryConfig{CellAddressLayout: EnumParam{Value: "single_device", Allowed: []string{"single_device", "multi_device"}, Unconfirmed: true}},
		ControlAPI: ControlAPIConfig{
			// Loopback-only default, per AGENT-TASK §1.3: code doesn't
			// hardcode IPs other than 127.0.0.1 and config values, and
			// this is the config value.
			Bind: "127.0.0.1:8081",
		},
	}
}

// Load reads path and unmarshals it over Default() — any field the file
// doesn't mention keeps its default, so a config that only overrides
// modbus.byte_order.value still gets the right Allowed/Unconfirmed. A
// missing file is not an error: Default() applies as-is, matching §7's
// "each parameter has a default" rule.
func Load(path string) (Config, error) {
	cfg := Default()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return Config{}, fmt.Errorf("config: read %s: %w", path, err)
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("config: parse %s: %w", path, err)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) Validate() error {
	if err := c.Modbus.ByteOrder.validate("modbus.byte_order"); err != nil {
		return err
	}
	if err := c.Scaling.Source.validate("scaling.source"); err != nil {
		return err
	}
	if err := c.Watchdog.Mode.validate("watchdog.mode"); err != nil {
		return err
	}
	if c.Watchdog.TimeoutS.Value <= 0 {
		return fmt.Errorf("config: watchdog.timeout_s = %d, must be positive", c.Watchdog.TimeoutS.Value)
	}
	if err := c.Modes.Priority.validate("modes.priority"); err != nil {
		return err
	}
	if err := c.Setpoints.Apply.validate("setpoints.apply"); err != nil {
		return err
	}
	if err := c.Battery.CellAddressLayout.validate("battery.cell_address_layout"); err != nil {
		return err
	}
	if c.ControlAPI.Bind == "" {
		return fmt.Errorf("config: control_api.bind must not be empty")
	}
	return nil
}

// ModbusByteOrder translates the validated string value to the codec
// type gen/go/m261points' Encode/Decode functions take.
func (c Config) ModbusByteOrder() (m261points.ByteOrder, error) {
	switch c.Modbus.ByteOrder.Value {
	case "big":
		return m261points.BigEndian, nil
	case "little":
		return m261points.LittleEndian, nil
	case "big_word_swap":
		return m261points.BigEndianWordSwap, nil
	case "little_word_swap":
		return m261points.LittleEndianWordSwap, nil
	default:
		// unreachable if Validate() has already run, kept as a safety net
		return 0, fmt.Errorf("config: unknown modbus.byte_order %q", c.Modbus.ByteOrder.Value)
	}
}
