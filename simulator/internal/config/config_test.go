package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/SterneStehen/petz-m261-tooling/gen/go/m261points"
)

func TestDefaultIsBigEndianUnconfirmed(t *testing.T) {
	cfg := Default()
	if cfg.Modbus.ByteOrder.Value != "big" {
		t.Errorf("default value = %q, want %q", cfg.Modbus.ByteOrder.Value, "big")
	}
	if !cfg.Modbus.ByteOrder.Unconfirmed {
		t.Error("default modbus.byte_order.unconfirmed = false, want true (§7: not confirmed by the manufacturer)")
	}
	want := []string{"big", "little", "big_word_swap", "little_word_swap"}
	if len(cfg.Modbus.ByteOrder.Allowed) != len(want) {
		t.Fatalf("allowed values = %v, want %v", cfg.Modbus.ByteOrder.Allowed, want)
	}
	for i, v := range want {
		if cfg.Modbus.ByteOrder.Allowed[i] != v {
			t.Errorf("allowed[%d] = %q, want %q", i, cfg.Modbus.ByteOrder.Allowed[i], v)
		}
	}
	order, err := cfg.ModbusByteOrder()
	if err != nil || order != m261points.BigEndian {
		t.Errorf("ModbusByteOrder() = %v, %v; want BigEndian, nil", order, err)
	}
}

func TestLoadMissingFileFallsBackToDefault(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "does-not-exist.yaml"))
	if err != nil {
		t.Fatalf("Load on a missing file returned an error: %v", err)
	}
	if !reflect.DeepEqual(cfg, Default()) {
		t.Errorf("Load on a missing file = %+v, want Default()", cfg)
	}
}

func TestLoadOverridesValueKeepingRestOfDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m261sim.yaml")
	writeFile(t, path, "modbus:\n  byte_order:\n    value: little_word_swap\n")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Modbus.ByteOrder.Value != "little_word_swap" {
		t.Errorf("value = %q, want %q", cfg.Modbus.ByteOrder.Value, "little_word_swap")
	}
	// allowed/unconfirmed weren't in the override file — must still be the defaults.
	if !cfg.Modbus.ByteOrder.Unconfirmed {
		t.Error("unconfirmed flag lost after a partial override")
	}
	if len(cfg.Modbus.ByteOrder.Allowed) != 4 {
		t.Errorf("allowed values lost after a partial override: %v", cfg.Modbus.ByteOrder.Allowed)
	}
	order, err := cfg.ModbusByteOrder()
	if err != nil || order != m261points.LittleEndianWordSwap {
		t.Errorf("ModbusByteOrder() = %v, %v; want LittleEndianWordSwap, nil", order, err)
	}
}

func TestLoadRejectsValueOutsideAllowed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m261sim.yaml")
	writeFile(t, path, "modbus:\n  byte_order:\n    value: middle_endian\n")

	if _, err := Load(path); err == nil {
		t.Error("Load with an out-of-enum value returned nil error")
	}
}

func TestAllFourAllowedValuesLoadAndTranslate(t *testing.T) {
	want := map[string]m261points.ByteOrder{
		"big": m261points.BigEndian, "little": m261points.LittleEndian,
		"big_word_swap": m261points.BigEndianWordSwap, "little_word_swap": m261points.LittleEndianWordSwap,
	}
	for value, wantOrder := range want {
		path := filepath.Join(t.TempDir(), "m261sim.yaml")
		writeFile(t, path, "modbus:\n  byte_order:\n    value: "+value+"\n")
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("Load(%q): %v", value, err)
		}
		order, err := cfg.ModbusByteOrder()
		if err != nil || order != wantOrder {
			t.Errorf("value=%q: ModbusByteOrder() = %v, %v; want %v, nil", value, order, err, wantOrder)
		}
	}
}

func TestRealConfigFileLoadsAndValidates(t *testing.T) {
	// The actual file the simulator ships with — must parse and default to big.
	cfg, err := Load(filepath.Join("..", "..", "config", "m261sim.yaml"))
	if err != nil {
		t.Fatalf("Load(simulator/config/m261sim.yaml): %v", err)
	}
	if cfg.Modbus.ByteOrder.Value != "big" {
		t.Errorf("shipped config value = %q, want %q", cfg.Modbus.ByteOrder.Value, "big")
	}
	if !cfg.Modbus.ByteOrder.Unconfirmed {
		t.Error("shipped config: modbus.byte_order.unconfirmed = false, want true")
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
