package main

import (
	"encoding/binary"
	"io"
	"math"
	"net"
	"testing"
	"time"

	gomodbus "github.com/goburrow/modbus"

	"github.com/SterneStehen/petz-m261-tooling/gen/go/m261points"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/clock"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/commands"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/config"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/iec104"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/modbustcp"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/physics"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/store"
)

// newRemoteCommandsProcessor builds a commands.Processor on st/clk (Task
// 6) and puts it in Remote mode (Set Operating Mode = 2) — the tests that
// use this predate Task 6 and are exercising the physics/protocol
// plumbing (does a requested power reach the engine and come back out
// through both protocols), not mode arbitration itself, which has its own
// dedicated coverage in simulator/internal/commands and in this file's
// Task 6 acceptance tests below.
func newRemoteCommandsProcessor(t *testing.T, st *store.Store, clk clock.Clock) *commands.Processor {
	t.Helper()
	p, err := commands.NewProcessor(st, clk, commands.DefaultConfig())
	if err != nil {
		t.Fatalf("commands.NewProcessor: %v", err)
	}
	if err := p.Write(m261points.PointKey{Device: "EMS", Slug: "set_operating_mode"}, 2); err != nil {
		t.Fatalf("Write(set_operating_mode, 2): %v", err)
	}
	return p
}

func TestResolveByteOrderDefaultsToConfigValue(t *testing.T) {
	// No CLI override: whatever the loaded config says wins — this is the
	// default/no-override half of the behavior the config file exists for.
	order, resolved, err := resolveByteOrder(config.Default(), "")
	if err != nil || order != m261points.BigEndian {
		t.Fatalf("resolveByteOrder(Default(), \"\") = %v, %v; want BigEndian, nil", order, err)
	}
	if resolved.Modbus.ByteOrder.Value != "big" {
		t.Errorf("resolved config value = %q, want %q", resolved.Modbus.ByteOrder.Value, "big")
	}
}

func TestResolveByteOrderCLIOverridesConfig(t *testing.T) {
	order, resolved, err := resolveByteOrder(config.Default(), "little_word_swap")
	if err != nil || order != m261points.LittleEndianWordSwap {
		t.Fatalf("resolveByteOrder(Default(), \"little_word_swap\") = %v, %v; want LittleEndianWordSwap, nil", order, err)
	}
	if resolved.Modbus.ByteOrder.Value != "little_word_swap" {
		t.Errorf("resolved config value = %q, want %q", resolved.Modbus.ByteOrder.Value, "little_word_swap")
	}
}

func TestResolveByteOrderRejectsInvalidOverride(t *testing.T) {
	if _, _, err := resolveByteOrder(config.Default(), "nonsense"); err == nil {
		t.Error("resolveByteOrder with an out-of-enum override returned nil error")
	}
}

// TestBothServersShareOneStore is the full Task 4 wiring test: one store,
// both servers, exactly as main() constructs them — a Modbus write is
// read back via a real IEC-104 client, and an IEC-104 write is read back
// via a real Modbus client. Each direction is also covered within
// modbustcp's and iec104's own package tests against a bare store; this
// proves the two servers are actually wired to the SAME store together,
// not just individually correct in isolation.
func TestBothServersShareOneStore(t *testing.T) {
	st := store.New()

	mb := modbustcp.New(st, modbustcp.Config{Addr: "127.0.0.1:0", ByteOrder: m261points.BigEndian})
	if err := mb.Start(); err != nil {
		t.Fatalf("modbustcp Start: %v", err)
	}
	t.Cleanup(func() { mb.Close() })

	iec := iec104.New(st, iec104.Config{Addr: "127.0.0.1:0"})
	if err := iec.Start(); err != nil {
		t.Fatalf("iec104 Start: %v", err)
	}
	t.Cleanup(func() { iec.Close() })

	t.Run("modbus write visible via IEC-104", func(t *testing.T) {
		handler := gomodbus.NewTCPClientHandler(mb.Addr().String())
		handler.SlaveId = 1 // EMS
		handler.Timeout = 2 * time.Second
		if err := handler.Connect(); err != nil {
			t.Fatalf("modbus Connect: %v", err)
		}
		defer handler.Close()
		client := gomodbus.NewClient(handler)

		buf := make([]byte, 4)
		binary.BigEndian.PutUint32(buf, math.Float32bits(-55))
		// EMS Set Active Power: modbus_addr 40153 -> wire address 40153-40001=152
		if _, err := client.WriteMultipleRegisters(152, 2, buf); err != nil {
			t.Fatalf("WriteMultipleRegisters: %v", err)
		}

		// EMS Set Active Power's readback is IEC-104 address 16487.
		c := dialRawIEC(t, iec.Addr().String())
		c.startDT()
		c.sendGeneralInterrogation(1)
		got, ok := c.waitForFloat(16487)
		if !ok || got != -55 {
			t.Fatalf("IEC-104 readback = %v, %v; want -55, true", got, ok)
		}
	})

	t.Run("IEC-104 write visible via Modbus", func(t *testing.T) {
		c := dialRawIEC(t, iec.Addr().String())
		c.startDT()
		c.sendSetpointCommand(1, 25165, 63.5) // EMS Set Active Power
		c.expectActivationConfirmation()

		handler := gomodbus.NewTCPClientHandler(mb.Addr().String())
		handler.SlaveId = 1
		handler.Timeout = 2 * time.Second
		if err := handler.Connect(); err != nil {
			t.Fatalf("modbus Connect: %v", err)
		}
		defer handler.Close()
		client := gomodbus.NewClient(handler)

		regs, err := client.ReadHoldingRegisters(152, 2)
		if err != nil {
			t.Fatalf("ReadHoldingRegisters: %v", err)
		}
		if got := math.Float32frombits(binary.BigEndian.Uint32(regs)); got != 63.5 {
			t.Fatalf("Modbus readback = %v, want 63.5", got)
		}
	})
}

// TestPhysicsTickIsVisibleThroughBothProtocols is the review-requested
// proof that the physics model actually drives what the running simulator
// reports — before this, nothing ever called physics.Engine.Step from
// main(), so m261sim served static zero values regardless of the physics
// package's own (correct, well-tested) behavior in isolation. This wires
// exactly what main() wires (store, both protocol servers, a
// physics.Runner) and asserts the change through real protocol clients,
// not by reading the store directly.
func TestPhysicsTickIsVisibleThroughBothProtocols(t *testing.T) {
	st := store.New()

	mb := modbustcp.New(st, modbustcp.Config{Addr: "127.0.0.1:0", ByteOrder: m261points.BigEndian})
	if err := mb.Start(); err != nil {
		t.Fatalf("modbustcp Start: %v", err)
	}
	t.Cleanup(func() { mb.Close() })

	iec := iec104.New(st, iec104.Config{Addr: "127.0.0.1:0"})
	if err := iec.Start(); err != nil {
		t.Fatalf("iec104 Start: %v", err)
	}
	t.Cleanup(func() { iec.Close() })

	const startingSOC = 50.0
	engine := physics.New(physics.DefaultParams(), startingSOC)
	clk := clock.NewFake(time.Now())
	cmds := newRemoteCommandsProcessor(t, st, clk)
	runner := physics.NewRunner(engine, st, clk, cmds)

	// A discharge request (+50 kW, positive per §4.5) so SoC and delivered
	// power move in an unambiguous, checkable direction.
	if err := cmds.Write(m261points.PointKey{Device: "EMS", Slug: "set_active_power_kw"}, 50); err != nil {
		t.Fatal(err)
	}

	// Baseline, before any tick: heartbeat and delivered power both read
	// their zero-value defaults through IEC-104, exactly like the reviewer
	// observed against the unfixed binary.
	c := dialRawIEC(t, iec.Addr().String())
	c.startDT()
	c.sendGeneralInterrogation(1) // EMS
	heartbeatBefore, ok := c.waitForFloat(16400)
	if !ok {
		t.Fatal("EMS Periodic Heartbeat Indicator (16400) not found in the pre-tick interrogation")
	}
	if heartbeatBefore != 0 {
		t.Fatalf("heartbeat before any tick = %v, want 0 (unstarted baseline)", heartbeatBefore)
	}

	clk.Advance(time.Second)
	runner.Tick()

	// Heartbeat and delivered power, via one IEC-104 general interrogation
	// pass (16396 "Last Charge/Discharge Power" and 16400 "Heartbeat",
	// looked up together since interrogation sends points in ascending IOA
	// order and 16396 < 16400 — see waitForFloats).
	c2 := dialRawIEC(t, iec.Addr().String())
	c2.startDT()
	c2.sendGeneralInterrogation(1)
	after := c2.waitForFloats(16396, 16400)

	heartbeatAfter, ok := after[16400]
	if !ok || heartbeatAfter <= heartbeatBefore {
		t.Fatalf("heartbeat after one tick = %v, %v; want > %v (moved off the pre-tick baseline)", heartbeatAfter, ok, heartbeatBefore)
	}
	deliveredPower, ok := after[16396] // EMS Last Charge/Discharge Power (kW)
	if !ok || deliveredPower <= 0 {
		t.Fatalf("delivered power after a +50kW discharge request = %v, %v; want > 0", deliveredPower, ok)
	}

	// SoC, via a real Modbus client reading BMS (Unit ID 34) — a
	// completely different device and protocol than the IEC-104 checks
	// above, proving the same physics tick reached both.
	handler := gomodbus.NewTCPClientHandler(mb.Addr().String())
	handler.SlaveId = 34 // BMS
	handler.Timeout = 2 * time.Second
	if err := handler.Connect(); err != nil {
		t.Fatalf("modbus Connect: %v", err)
	}
	defer handler.Close()
	client := gomodbus.NewClient(handler)

	// BMS SOC (%): modbus_addr 30003, class 4 -> wire address 30003-30001=2
	regs, err := client.ReadInputRegisters(2, 2)
	if err != nil {
		t.Fatalf("ReadInputRegisters: %v", err)
	}
	soc := math.Float32frombits(binary.BigEndian.Uint32(regs))
	if soc >= startingSOC {
		t.Fatalf("SoC via Modbus after a 1s discharge = %v, want < the starting %v", soc, startingSOC)
	}
	if soc <= 0 {
		t.Fatalf("SoC via Modbus after a single 1s discharge tick = %v, want a small but positive drop from %v", soc, startingSOC)
	}
}

// TestMeterDirectionInvertedIsAppliedEachTick is the code-review-requested
// protocol-level proof that "Energy Storage Meter Power Direction" (§4.4)
// is actually read and applied — previously the runner never read that
// setpoint at all, so inverting it through a real Modbus write had no
// effect on which accumulator discharge energy went into.
func TestMeterDirectionInvertedIsAppliedEachTick(t *testing.T) {
	st := store.New()
	clk := clock.NewFake(time.Now())
	cmds := newRemoteCommandsProcessor(t, st, clk)

	mb := modbustcp.New(st, modbustcp.Config{Addr: "127.0.0.1:0", ByteOrder: m261points.BigEndian, Commands: cmds})
	if err := mb.Start(); err != nil {
		t.Fatalf("modbustcp Start: %v", err)
	}
	t.Cleanup(func() { mb.Close() })

	engine := physics.New(physics.DefaultParams(), 50)
	runner := physics.NewRunner(engine, st, clk, cmds)

	handler := gomodbus.NewTCPClientHandler(mb.Addr().String())
	handler.SlaveId = 1 // EMS
	handler.Timeout = 2 * time.Second
	if err := handler.Connect(); err != nil {
		t.Fatalf("modbus Connect: %v", err)
	}
	defer handler.Close()
	client := gomodbus.NewClient(handler)

	// Energy Storage Meter Power Direction = 1 (inverted): modbus_addr
	// 40079 -> wire address 40079-40001=78.
	dirBuf := make([]byte, 4)
	binary.BigEndian.PutUint32(dirBuf, 1)
	if _, err := client.WriteMultipleRegisters(78, 2, dirBuf); err != nil {
		t.Fatalf("write energy_storage_meter_power_direction: %v", err)
	}

	// Set Active Power = +10 kW (discharge): wire address 40153-40001=152.
	powerBuf := make([]byte, 4)
	binary.BigEndian.PutUint32(powerBuf, math.Float32bits(10))
	if _, err := client.WriteMultipleRegisters(152, 2, powerBuf); err != nil {
		t.Fatalf("write set_active_power_kw: %v", err)
	}

	clk.Advance(time.Second)
	runner.Tick()

	readF32 := func(wireAddr uint16) float32 {
		t.Helper()
		regs, err := client.ReadInputRegisters(wireAddr, 2)
		if err != nil {
			t.Fatalf("ReadInputRegisters(%d): %v", wireAddr, err)
		}
		return math.Float32frombits(binary.BigEndian.Uint32(regs))
	}
	// Total Forward/Reverse Energy: modbus 30043/30045 -> wire 42/44.
	forward := readF32(42)
	reverse := readF32(44)

	if reverse <= 0 {
		t.Errorf("reverse energy = %v after a discharge with direction inverted, want > 0", reverse)
	}
	if forward != 0 {
		t.Errorf("forward energy = %v after a discharge with direction inverted, want 0 (all of it should land in reverse instead)", forward)
	}
}

// TestPCSMeterDeviceIsPopulatedThroughModbus is the code-review-requested
// proof that the dedicated "Energy Storage Meter" device (PCS_METER, Unit
// ID 163, §4.1) is no longer left entirely static while EMS/PCS report
// real values — read through Unit ID 163 specifically, as asked.
func TestPCSMeterDeviceIsPopulatedThroughModbus(t *testing.T) {
	st := store.New()

	mb := modbustcp.New(st, modbustcp.Config{Addr: "127.0.0.1:0", ByteOrder: m261points.BigEndian})
	if err := mb.Start(); err != nil {
		t.Fatalf("modbustcp Start: %v", err)
	}
	t.Cleanup(func() { mb.Close() })

	engine := physics.New(physics.DefaultParams(), 50)
	clk := clock.NewFake(time.Now())
	cmds := newRemoteCommandsProcessor(t, st, clk)
	runner := physics.NewRunner(engine, st, clk, cmds)
	if err := cmds.Write(m261points.PointKey{Device: "EMS", Slug: "set_active_power_kw"}, 20); err != nil { // discharge
		t.Fatal(err)
	}
	clk.Advance(time.Second)
	runner.Tick()

	handler := gomodbus.NewTCPClientHandler(mb.Addr().String())
	handler.SlaveId = 163 // PCS_METER ("Energy Storage Meter"), §4.1
	handler.Timeout = 2 * time.Second
	if err := handler.Connect(); err != nil {
		t.Fatalf("modbus Connect: %v", err)
	}
	defer handler.Close()
	client := gomodbus.NewClient(handler)

	readF32 := func(wireAddr uint16) float32 {
		t.Helper()
		regs, err := client.ReadInputRegisters(wireAddr, 2)
		if err != nil {
			t.Fatalf("ReadInputRegisters(%d): %v", wireAddr, err)
		}
		return math.Float32frombits(binary.BigEndian.Uint32(regs))
	}
	// online_status's catalog data_type is I16, widened to I32 on the wire
	// (§2.2) — unlike the F32 points above, reading it as a float would
	// reinterpret the integer 1's raw bits as ~1e-45.
	readI32 := func(wireAddr uint16) int32 {
		t.Helper()
		regs, err := client.ReadInputRegisters(wireAddr, 2)
		if err != nil {
			t.Fatalf("ReadInputRegisters(%d): %v", wireAddr, err)
		}
		return int32(binary.BigEndian.Uint32(regs))
	}

	if v := readF32(0); v <= 0 { // phase_a_voltage_v: modbus 30001 -> wire 0
		t.Errorf("PCS_METER phase_a_voltage_v = %v, want > 0", v)
	}
	if p := readF32(12); p <= 0 { // total_active_power_kw: modbus 30013 -> wire 12
		t.Errorf("PCS_METER total_active_power_kw = %v after a discharge, want > 0", p)
	}
	if e := readF32(44); e <= 0 { // forward_active_total_energy_kwh: modbus 30045 -> wire 44
		t.Errorf("PCS_METER forward_active_total_energy_kwh = %v, want > 0", e)
	}
	if o := readI32(60); o != 1 { // online_status: modbus 30061 -> wire 60
		t.Errorf("PCS_METER online_status = %v, want 1", o)
	}
}

// TestInitialStateIsPublishedBeforeFirstTick is the code-review-requested
// proof that a client connecting before the first Tick fires sees the
// engine's configured initial state, not the store's zero defaults — the
// window between server startup and the first tick isn't always
// negligible (physics-step is configurable), and clients reading zero
// SoC/voltage/offline status in that window would be reading a physically
// impossible state.
func TestInitialStateIsPublishedBeforeFirstTick(t *testing.T) {
	st := store.New()

	mb := modbustcp.New(st, modbustcp.Config{Addr: "127.0.0.1:0", ByteOrder: m261points.BigEndian})
	if err := mb.Start(); err != nil {
		t.Fatalf("modbustcp Start: %v", err)
	}
	t.Cleanup(func() { mb.Close() })

	const configuredSOC = 73.0
	engine := physics.New(physics.DefaultParams(), configuredSOC)
	clk := clock.NewFake(time.Now())
	// NewRunner only — deliberately never call Tick, matching a client
	// that connects in the gap before the first real tick.
	physics.NewRunner(engine, st, clk, newRemoteCommandsProcessor(t, st, clk))

	handlerBMS := gomodbus.NewTCPClientHandler(mb.Addr().String())
	handlerBMS.SlaveId = 34 // BMS
	handlerBMS.Timeout = 2 * time.Second
	if err := handlerBMS.Connect(); err != nil {
		t.Fatalf("modbus Connect (BMS): %v", err)
	}
	defer handlerBMS.Close()
	bms := gomodbus.NewClient(handlerBMS)

	readF32 := func(client gomodbus.Client, wireAddr uint16) float32 {
		t.Helper()
		regs, err := client.ReadInputRegisters(wireAddr, 2)
		if err != nil {
			t.Fatalf("ReadInputRegisters(%d): %v", wireAddr, err)
		}
		return math.Float32frombits(binary.BigEndian.Uint32(regs))
	}

	// BMS SOC (%): modbus 30003 -> wire 2.
	if soc := readF32(bms, 2); soc != configuredSOC {
		t.Errorf("SoC before any Tick = %v, want the configured initial %v, not a zero default", soc, configuredSOC)
	}
	// BMS Battery Total Voltage (V): modbus 30007 -> wire 6 — must already
	// be on the LFP curve for configuredSOC, never 0.
	if v := readF32(bms, 6); v < 676 || v > 936 {
		t.Errorf("battery_total_voltage_v before any Tick = %v, want within [676, 936] (the curve's value at %v%% SoC), not 0", v, configuredSOC)
	}

	handlerEMS := gomodbus.NewTCPClientHandler(mb.Addr().String())
	handlerEMS.SlaveId = 1 // EMS
	handlerEMS.Timeout = 2 * time.Second
	if err := handlerEMS.Connect(); err != nil {
		t.Fatalf("modbus Connect (EMS): %v", err)
	}
	defer handlerEMS.Close()
	ems := gomodbus.NewClient(handlerEMS)

	// EMS Online Status: modbus 30051 -> wire 50. data_type I16, widened to
	// I32 on the wire (§2.2) — not F32 like soc/voltage above.
	regs, err := ems.ReadInputRegisters(50, 2)
	if err != nil {
		t.Fatalf("ReadInputRegisters(50): %v", err)
	}
	if online := int32(binary.BigEndian.Uint32(regs)); online != 1 {
		t.Errorf("online_status before any Tick = %v, want 1, not offline", online)
	}
}

// newAllowDangerousCommandsProcessor is newRemoteCommandsProcessor's twin
// with allow_dangerous=true — used only by
// TestAll148SetpointsWriteAndReadbackThroughBothProtocols below, which
// must be able to write Trip/Clear Protection too (the only two of the
// 148 flagged Dangerous:true) to genuinely cover all 148, not 146 plus a
// separate carve-out.
func newAllowDangerousCommandsProcessor(t *testing.T, st *store.Store, clk clock.Clock) *commands.Processor {
	t.Helper()
	cfg := commands.DefaultConfig()
	cfg.AllowDangerous = true
	p, err := commands.NewProcessor(st, clk, cfg)
	if err != nil {
		t.Fatalf("commands.NewProcessor: %v", err)
	}
	if err := p.Write(m261points.PointKey{Device: "EMS", Slug: "set_operating_mode"}, 2); err != nil {
		t.Fatalf("Write(set_operating_mode, 2): %v", err)
	}
	return p
}

// float32Eq compares two engineering values by their canonical
// float32-representable form. Every IEC-104 monitored value (M_ME_NC_1)
// and every Modbus F32-typed register pair round-trips through a real
// float32 on the wire (§2.2) — exact float64 equality is the wrong
// comparison once a value has actually gone over either protocol.
func float32Eq(a, b float64) bool {
	return float32(a) == float32(b)
}

// representativeValue picks one valid, in-range engineering value for
// meta: deterministic, and safe for every one of the 148 real setpoints
// (none of which has a business range today — AGENT-TASK §6 item 1). 7.1
// is deliberately not exactly float32-representable, so a test asserting
// equality against an F32 point's round-tripped value is forced to use
// float32Eq rather than get away with exact float64 comparison by luck.
func representativeValue(meta m261points.PointMeta) float64 {
	if meta.Enum != nil {
		min, first := 0, true
		for k := range meta.Enum {
			if first || k < min {
				min, first = k, false
			}
		}
		return float64(min)
	}
	if meta.DataType == m261points.DataTypeF32 {
		return 7.1
	}
	return 7
}

type setpointExpectation struct {
	meta        m261points.PointMeta
	value       float64
	wireAddr    uint16
	readbackIOA int
}

func all148SetpointExpectations(t *testing.T) []setpointExpectation {
	t.Helper()
	var out []setpointExpectation
	for _, meta := range m261points.Points {
		if meta.Device != "EMS" || meta.Class != m261points.ClassSetpoint {
			continue
		}
		out = append(out, setpointExpectation{
			meta:        meta,
			value:       representativeValue(meta),
			wireAddr:    uint16(*meta.ModbusAddr - 40001),
			readbackIOA: *meta.ReadbackIEC104Addr,
		})
	}
	if len(out) != 148 {
		t.Fatalf("found %d EMS setpoints in the catalog, want 148", len(out))
	}
	return out
}

// TestAll148SetpointsWriteAndReadbackThroughBothProtocols is Task 6's
// "readback works for all 148 points", exercised through real protocol
// clients rather than the store directly
// (simulator/internal/commands.TestWriteReadbackForEvery148Setpoint is the
// store-level equivalent): every one of the 148 EMS setpoints is written
// via a real Modbus client, then confirmed both via a Modbus read-back at
// the same address and via a single IEC-104 general interrogation pass
// reading every one of the 148 readback IOAs at once — a real client
// wouldn't run 148 separate interrogations any more than this test does.
func TestAll148SetpointsWriteAndReadbackThroughBothProtocols(t *testing.T) {
	st := store.New()
	clk := clock.NewFake(time.Now())
	cmds := newAllowDangerousCommandsProcessor(t, st, clk)

	mb := modbustcp.New(st, modbustcp.Config{Addr: "127.0.0.1:0", ByteOrder: m261points.BigEndian, Commands: cmds})
	if err := mb.Start(); err != nil {
		t.Fatalf("modbustcp Start: %v", err)
	}
	t.Cleanup(func() { mb.Close() })

	iec := iec104.New(st, iec104.Config{Addr: "127.0.0.1:0", Commands: cmds})
	if err := iec.Start(); err != nil {
		t.Fatalf("iec104 Start: %v", err)
	}
	t.Cleanup(func() { iec.Close() })

	handler := gomodbus.NewTCPClientHandler(mb.Addr().String())
	handler.SlaveId = 1 // EMS
	handler.Timeout = 10 * time.Second
	if err := handler.Connect(); err != nil {
		t.Fatalf("modbus Connect: %v", err)
	}
	defer handler.Close()
	client := gomodbus.NewClient(handler)

	setpoints := all148SetpointExpectations(t)

	for _, sp := range setpoints {
		buf := make([]byte, 4)
		if sp.meta.DataType == m261points.DataTypeF32 {
			binary.BigEndian.PutUint32(buf, math.Float32bits(float32(sp.value)))
		} else {
			binary.BigEndian.PutUint32(buf, uint32(int32(sp.value)))
		}
		if _, err := client.WriteMultipleRegisters(sp.wireAddr, 2, buf); err != nil {
			t.Fatalf("write %s/%s (wire %d): %v", sp.meta.Device, sp.meta.Slug, sp.wireAddr, err)
		}
	}

	for _, sp := range setpoints {
		regs, err := client.ReadHoldingRegisters(sp.wireAddr, 2)
		if err != nil {
			t.Fatalf("read %s/%s (wire %d): %v", sp.meta.Device, sp.meta.Slug, sp.wireAddr, err)
		}
		if sp.meta.DataType == m261points.DataTypeF32 {
			got := float64(math.Float32frombits(binary.BigEndian.Uint32(regs)))
			if !float32Eq(got, sp.value) {
				t.Errorf("%s/%s Modbus read-back = %v, want %v (canonical float32)", sp.meta.Device, sp.meta.Slug, got, sp.value)
			}
		} else if got := int32(binary.BigEndian.Uint32(regs)); got != int32(sp.value) {
			t.Errorf("%s/%s Modbus read-back = %v, want %v", sp.meta.Device, sp.meta.Slug, got, int32(sp.value))
		}
	}

	c := dialRawIEC(t, iec.Addr().String())
	c.startDT()
	c.sendGeneralInterrogation(1) // EMS
	ioas := make([]int, len(setpoints))
	for i, sp := range setpoints {
		ioas[i] = sp.readbackIOA
	}
	got := c.waitForFloats(ioas...)
	if len(got) != len(setpoints) {
		t.Fatalf("general interrogation returned %d/%d readback IOAs", len(got), len(setpoints))
	}
	for _, sp := range setpoints {
		gotVal := float64(got[sp.readbackIOA])
		if !float32Eq(gotVal, sp.value) {
			t.Errorf("%s/%s readback via IEC-104 (IOA %d) = %v, want %v (canonical float32)", sp.meta.Device, sp.meta.Slug, sp.readbackIOA, gotVal, sp.value)
		}
	}
}

// TestRejectedWriteDoesNotChangeSetpointOrReadbackThroughBothProtocols
// proves an invalid write is fully rejected end-to-end through each real
// protocol, not just at the commands.Processor unit level
// (simulator/internal/commands already covers that in isolation) — the
// setpoint and its readback both stay at their prior value.
func TestRejectedWriteDoesNotChangeSetpointOrReadbackThroughBothProtocols(t *testing.T) {
	st := store.New()
	clk := clock.NewFake(time.Now())
	cmds := newRemoteCommandsProcessor(t, st, clk)

	mb := modbustcp.New(st, modbustcp.Config{Addr: "127.0.0.1:0", ByteOrder: m261points.BigEndian, Commands: cmds})
	if err := mb.Start(); err != nil {
		t.Fatalf("modbustcp Start: %v", err)
	}
	t.Cleanup(func() { mb.Close() })

	iec := iec104.New(st, iec104.Config{Addr: "127.0.0.1:0", Commands: cmds})
	if err := iec.Start(); err != nil {
		t.Fatalf("iec104 Start: %v", err)
	}
	t.Cleanup(func() { iec.Close() })

	handler := gomodbus.NewTCPClientHandler(mb.Addr().String())
	handler.SlaveId = 1
	handler.Timeout = 2 * time.Second
	if err := handler.Connect(); err != nil {
		t.Fatalf("modbus Connect: %v", err)
	}
	defer handler.Close()
	client := gomodbus.NewClient(handler)

	// Baseline, valid write via Modbus: Set Operating Mode = 1 (Auto
	// Strategy), wire 40009-40001=8.
	buf := make([]byte, 4)
	binary.BigEndian.PutUint32(buf, 1)
	if _, err := client.WriteMultipleRegisters(8, 2, buf); err != nil {
		t.Fatalf("baseline write: %v", err)
	}

	// Modbus: an out-of-enum value (5 — only 0/1/2 are valid) must be
	// rejected as a Modbus exception, and must not move the stored value.
	badBuf := make([]byte, 4)
	binary.BigEndian.PutUint32(badBuf, 5)
	if _, err := client.WriteMultipleRegisters(8, 2, badBuf); err == nil {
		t.Error("Modbus write of an out-of-enum set_operating_mode value did not return an error")
	}
	regs, err := client.ReadHoldingRegisters(8, 2)
	if err != nil {
		t.Fatalf("ReadHoldingRegisters: %v", err)
	}
	if got := int32(binary.BigEndian.Uint32(regs)); got != 1 {
		t.Errorf("set_operating_mode after a rejected Modbus write = %v, want unchanged 1", got)
	}

	// IEC-104: the same rejection, through a real, independent raw client.
	c := dialRawIEC(t, iec.Addr().String())
	c.startDT()
	c.sendSetpointCommand(1, 25093, 5) // Set Operating Mode, IOA 25093
	found := false
	for i := 0; i < 50; i++ {
		asdu := c.nextI()
		if asdu[0] == 50 { // C_SE_NC_1
			if asdu[2]&0x40 == 0 { // negative (P/N) flag must be set
				t.Errorf("expected a negative confirmation for an out-of-enum IEC-104 write, got cot=%d", asdu[2])
			}
			found = true
			break
		}
	}
	if !found {
		t.Fatal("no C_SE_NC_1 confirmation observed after 50 frames")
	}

	c.sendGeneralInterrogation(1)
	after, ok := c.waitForFloat(16415) // set_operating_mode's readback IOA
	if !ok || after != 1 {
		t.Errorf("set_operating_mode readback after a rejected IEC-104 write = %v, %v; want 1, true (unchanged)", after, ok)
	}
}

// --- minimal, self-contained raw IEC-104 client for this integration test ---

type rawIEC struct {
	t       *testing.T
	nc      net.Conn
	sendSeq uint16
	recvSeq uint16
}

func dialRawIEC(t *testing.T, addr string) *rawIEC {
	t.Helper()
	nc, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	nc.SetDeadline(time.Now().Add(5 * time.Second))
	t.Cleanup(func() { nc.Close() })
	return &rawIEC{t: t, nc: nc}
}

func (c *rawIEC) writeRaw(control, asdu []byte) {
	c.t.Helper()
	buf := make([]byte, 2+len(control)+len(asdu))
	buf[0], buf[1] = 0x68, byte(len(control)+len(asdu))
	copy(buf[2:], control)
	copy(buf[2+len(control):], asdu)
	if _, err := c.nc.Write(buf); err != nil {
		c.t.Fatalf("write: %v", err)
	}
}

func (c *rawIEC) readFrame() (control, asdu []byte) {
	c.t.Helper()
	header := make([]byte, 2)
	if _, err := io.ReadFull(c.nc, header); err != nil {
		c.t.Fatalf("read header: %v", err)
	}
	rest := make([]byte, header[1])
	if _, err := io.ReadFull(c.nc, rest); err != nil {
		c.t.Fatalf("read body: %v", err)
	}
	return rest[0:4], rest[4:]
}

func (c *rawIEC) startDT() {
	c.writeRaw([]byte{0x07, 0x00, 0x00, 0x00}, nil)
	control, _ := c.readFrame()
	if control[0] != 0x0B {
		c.t.Fatalf("expected STARTDT_CON, got control 0x%02x", control[0])
	}
}

func (c *rawIEC) sendI(asdu []byte) {
	sLo, sHi := byte(c.sendSeq<<1), byte(c.sendSeq>>7)
	rLo, rHi := byte(c.recvSeq<<1), byte(c.recvSeq>>7)
	c.writeRaw([]byte{sLo, sHi, rLo, rHi}, asdu)
	c.sendSeq++
}

func (c *rawIEC) nextI() []byte {
	c.t.Helper()
	for {
		control, asdu := c.readFrame()
		if control[0]&0x01 == 0 {
			c.recvSeq = ((uint16(control[1])<<8 | uint16(control[0])) >> 1) + 1
			return asdu
		}
	}
}

func (c *rawIEC) sendGeneralInterrogation(commonAddr int) {
	asdu := make([]byte, 10)
	asdu[0], asdu[1], asdu[2] = 100, 1, 6
	binary.LittleEndian.PutUint16(asdu[4:6], uint16(commonAddr))
	asdu[9] = 20
	c.sendI(asdu)
}

func (c *rawIEC) sendSetpointCommand(commonAddr, ioa int, value float32) {
	asdu := make([]byte, 14)
	asdu[0], asdu[1], asdu[2] = 50, 1, 6
	binary.LittleEndian.PutUint16(asdu[4:6], uint16(commonAddr))
	asdu[6], asdu[7], asdu[8] = byte(ioa), byte(ioa>>8), byte(ioa>>16)
	binary.LittleEndian.PutUint32(asdu[9:13], math.Float32bits(value))
	c.sendI(asdu)
}

// expectActivationConfirmation scans past anything else (a spontaneous
// transmission from the readback mirror can legitimately race with the
// command's own confirmation on this same connection — a real client
// can't assume its ack is literally the next frame) to find the C_SE_NC_1
// activation confirmation.
func (c *rawIEC) expectActivationConfirmation() {
	c.t.Helper()
	for i := 0; i < 50; i++ {
		asdu := c.nextI()
		if asdu[0] == 50 { // C_SE_NC_1
			if asdu[2] != 7 { // COT activation confirmation
				c.t.Fatalf("expected C_SE_NC_1 activation confirmation, got cot=%d", asdu[2])
			}
			return
		}
	}
	c.t.Fatal("did not see a C_SE_NC_1 confirmation after 50 frames")
}

// waitForFloat drains I-frames (general interrogation response) until it
// finds an M_ME_NC_1 for the given IOA, or the interrogation ends. General
// interrogation sends points in ascending IOA order (server.go sorts by
// IEC104Addr), and this only scans forward — looking up a smaller IOA
// after a larger one on the same connection will never find it, since
// it's already gone by. Use waitForFloats to look up more than one IOA
// from a single interrogation pass without hitting that trap.
func (c *rawIEC) waitForFloat(ioa int) (float32, bool) {
	c.t.Helper()
	got := c.waitForFloats(ioa)
	v, ok := got[ioa]
	return v, ok
}

// waitForFloats scans one interrogation response for every requested IOA
// in a single forward pass (order-independent from the caller's point of
// view), stopping once all are found or the interrogation ends.
//
// It only accepts frames with COT 20 (interrogated-by-station). The server
// broadcasts spontaneous updates (COT 3) on this same connection whenever
// the store changes elsewhere, and those can legitimately interleave with
// a general-interrogation response in flight — a real client parsing "the
// answer to my interrogation" has to make the same distinction, or it can
// pick up a stale value a spontaneous frame delivered before the
// interrogated one for the same IOA arrived (observed intermittently once
// physics.NewRunner started publishing a large initial burst of Changes:
// a leftover queued spontaneous frame for an IOA could be read as if it
// were that IOA's interrogation answer).
func (c *rawIEC) waitForFloats(ioas ...int) map[int]float32 {
	c.t.Helper()
	const cotInterrogatedByStation = 20
	want := make(map[int]bool, len(ioas))
	for _, ioa := range ioas {
		want[ioa] = true
	}
	got := make(map[int]float32, len(ioas))
	for len(got) < len(want) {
		asdu := c.nextI()
		if asdu[0] == 100 {
			if asdu[2] == 10 { // activation termination
				return got
			}
			continue
		}
		if asdu[2] != cotInterrogatedByStation {
			continue // e.g. a spontaneous update (COT 3) interleaved with the GI response
		}
		gotIOA := int(asdu[6]) | int(asdu[7])<<8 | int(asdu[8])<<16
		if asdu[0] == 13 && want[gotIOA] { // M_ME_NC_1
			got[gotIOA] = math.Float32frombits(binary.LittleEndian.Uint32(asdu[9:13]))
		}
	}
	return got
}
