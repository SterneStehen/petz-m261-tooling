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

// TestDefaultDeclaresAllTask6Parameters covers the §7 parameters Task 6
// added: watchdog.mode/timeout_s, modes.priority, commands.allow_dangerous
// — each must default per the table and be marked unconfirmed.
func TestDefaultDeclaresAllTask6Parameters(t *testing.T) {
	cfg := Default()

	if cfg.Watchdog.Mode.Value != "zero_after" || !cfg.Watchdog.Mode.Unconfirmed {
		t.Errorf("watchdog.mode = %+v, want value=zero_after unconfirmed=true", cfg.Watchdog.Mode)
	}
	if cfg.Watchdog.TimeoutS.Value != 60 || !cfg.Watchdog.TimeoutS.Unconfirmed {
		t.Errorf("watchdog.timeout_s = %+v, want value=60 unconfirmed=true", cfg.Watchdog.TimeoutS)
	}
	wantPriority := []string{"remote", "demand_control", "load_tracking"}
	if !reflect.DeepEqual(cfg.Modes.Priority.Value, wantPriority) || !cfg.Modes.Priority.Unconfirmed {
		t.Errorf("modes.priority = %+v, want value=%v unconfirmed=true", cfg.Modes.Priority, wantPriority)
	}
	if cfg.Commands.AllowDangerous.Value != false || !cfg.Commands.AllowDangerous.Unconfirmed {
		t.Errorf("commands.allow_dangerous = %+v, want value=false unconfirmed=true", cfg.Commands.AllowDangerous)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Default() failed its own Validate(): %v", err)
	}
}

func TestValidateRejectsBadWatchdogMode(t *testing.T) {
	cfg := Default()
	cfg.Watchdog.Mode.Value = "retry_forever"
	if err := cfg.Validate(); err == nil {
		t.Error("Validate with an out-of-enum watchdog.mode returned nil error")
	}
}

func TestValidateRejectsNonPositiveWatchdogTimeout(t *testing.T) {
	for _, v := range []int{0, -1} {
		cfg := Default()
		cfg.Watchdog.TimeoutS.Value = v
		if err := cfg.Validate(); err == nil {
			t.Errorf("Validate with watchdog.timeout_s = %d returned nil error", v)
		}
	}
}

func TestValidateRejectsMalformedModePriority(t *testing.T) {
	cases := [][]string{
		{"remote", "demand_control"},                       // too short
		{"remote", "demand_control", "load_tracking", "x"}, // too long
		{"remote", "demand_control", "nonsense"},           // unknown name
		{"remote", "remote", "load_tracking"},              // duplicate
	}
	for _, priority := range cases {
		cfg := Default()
		cfg.Modes.Priority.Value = priority
		if err := cfg.Validate(); err == nil {
			t.Errorf("Validate with modes.priority = %v returned nil error", priority)
		}
	}
}

func TestLoadParsesTask6Sections(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m261sim.yaml")
	writeFile(t, path, ""+
		"watchdog:\n  mode:\n    value: safe_state_after\n  timeout_s:\n    value: 5\n"+
		"modes:\n  priority:\n    value: [load_tracking, remote, demand_control]\n"+
		"commands:\n  allow_dangerous:\n    value: true\n")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Watchdog.Mode.Value != "safe_state_after" {
		t.Errorf("watchdog.mode = %q, want safe_state_after", cfg.Watchdog.Mode.Value)
	}
	if cfg.Watchdog.TimeoutS.Value != 5 {
		t.Errorf("watchdog.timeout_s = %d, want 5", cfg.Watchdog.TimeoutS.Value)
	}
	want := []string{"load_tracking", "remote", "demand_control"}
	if !reflect.DeepEqual(cfg.Modes.Priority.Value, want) {
		t.Errorf("modes.priority = %v, want %v", cfg.Modes.Priority.Value, want)
	}
	if !cfg.Commands.AllowDangerous.Value {
		t.Error("commands.allow_dangerous = false, want true")
	}
}

// TestRealConfigFileDeclaresTask6Defaults is TestRealConfigFileLoadsAndValidates's
// Task 6 complement — the shipped config file itself, not a synthetic one.
func TestRealConfigFileDeclaresTask6Defaults(t *testing.T) {
	cfg, err := Load(filepath.Join("..", "..", "config", "m261sim.yaml"))
	if err != nil {
		t.Fatalf("Load(simulator/config/m261sim.yaml): %v", err)
	}
	if cfg.Watchdog.Mode.Value != "zero_after" {
		t.Errorf("shipped watchdog.mode = %q, want zero_after", cfg.Watchdog.Mode.Value)
	}
	if cfg.Commands.AllowDangerous.Value {
		t.Error("shipped commands.allow_dangerous = true, want false by default")
	}
}

// TestDefaultControlAPIBindIsLoopback is Task 7 item 3's default:
// control_api.bind must default to loopback-only, matching AGENT-TASK
// §1.3's "no hardcoded IPs besides 127.0.0.1 and config values" rule.
func TestDefaultControlAPIBindIsLoopback(t *testing.T) {
	cfg := Default()
	if cfg.ControlAPI.Bind != "127.0.0.1:8081" {
		t.Errorf("control_api.bind default = %q, want 127.0.0.1:8081", cfg.ControlAPI.Bind)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Default() failed its own Validate(): %v", err)
	}
}

// TestLoadParsesControlAPISection proves control_api.bind is a real,
// loadable YAML key — the reviewed fix for the second round's rejected
// pushback (an earlier version left this flag-only, matching
// -modbus-addr/-iec104-addr; the review held that, unlike those two,
// this isn't a manufacturer-unconfirmed register-map value, so it
// belongs in this file like every other ordinary setting here).
func TestLoadParsesControlAPISection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m261sim.yaml")
	writeFile(t, path, "control_api:\n  bind: 0.0.0.0:9999\n")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ControlAPI.Bind != "0.0.0.0:9999" {
		t.Errorf("control_api.bind = %q, want 0.0.0.0:9999", cfg.ControlAPI.Bind)
	}
}

func TestValidateRejectsEmptyControlAPIBind(t *testing.T) {
	cfg := Default()
	cfg.ControlAPI.Bind = ""
	if err := cfg.Validate(); err == nil {
		t.Error("Validate with control_api.bind = \"\" returned nil error")
	}
}

// TestRealConfigFileDeclaresControlAPIBind is
// TestRealConfigFileLoadsAndValidates's control_api complement — the
// shipped config file itself, not a synthetic one.
func TestRealConfigFileDeclaresControlAPIBind(t *testing.T) {
	cfg, err := Load(filepath.Join("..", "..", "config", "m261sim.yaml"))
	if err != nil {
		t.Fatalf("Load(simulator/config/m261sim.yaml): %v", err)
	}
	if cfg.ControlAPI.Bind != "127.0.0.1:8081" {
		t.Errorf("shipped control_api.bind = %q, want 127.0.0.1:8081", cfg.ControlAPI.Bind)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
