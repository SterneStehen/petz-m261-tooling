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

type ModbusConfig struct {
	ByteOrder EnumParam `yaml:"byte_order"`
}

type Config struct {
	Modbus ModbusConfig `yaml:"modbus"`
}

// Default returns the §7 default: modbus.byte_order = big, unconfirmed.
func Default() Config {
	return Config{
		Modbus: ModbusConfig{
			ByteOrder: EnumParam{
				Value:       "big",
				Allowed:     []string{"big", "little", "big_word_swap", "little_word_swap"},
				Unconfirmed: true,
			},
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
