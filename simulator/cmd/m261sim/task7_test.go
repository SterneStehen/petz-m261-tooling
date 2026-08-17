package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	gomodbus "github.com/goburrow/modbus"

	"github.com/SterneStehen/petz-m261-tooling/gen/go/m261points"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/appgate"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/clock"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/commands"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/controlapi"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/faults"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/iec104"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/linkfault"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/modbustcp"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/physics"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/scenario"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/store"
)

// task7Sim is the full wiring main() builds, for Task 7's integration
// tests — one shared store, both real protocol servers, and controlapi,
// exactly as main() constructs them (modulo the fixed loopback:0 test
// addresses).
type task7Sim struct {
	store          *store.Store
	clk            *clock.Fake
	processor      *commands.Processor
	physicsRunner  *physics.Runner
	injector       *faults.Injector
	mb             *modbustcp.Server
	iec            *iec104.Server
	capi           *controlapi.Server
	startupInstant time.Time
}

func newTask7Sim(t *testing.T) *task7Sim {
	t.Helper()
	st := store.New()
	startupInstant := time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC)
	clk := clock.NewFake(startupInstant)
	gate := appgate.New()
	linkCoord := linkfault.NewCoordinator()

	proc, err := commands.NewProcessor(st, clk, commands.DefaultConfig())
	if err != nil {
		t.Fatalf("commands.NewProcessor: %v", err)
	}
	proc.SetGate(gate)
	params := physics.DefaultParams()
	newEngine := func() *physics.Engine { return physics.New(params, 50) }
	pr := physics.NewRunner(newEngine(), st, clk, proc)
	pr.SetGate(gate)

	// Every gate/coordinator below is wired exactly as main.go wires it —
	// third-review-round fix: this harness previously built mb/iec/sr
	// without SetGate/SetLinkCoordinator at all, so every one of this
	// file's own "atomic against reset" tests exercised none of the
	// atomicity they claimed to verify (nil *appgate.Gate/*linkfault.
	// Coordinator disable gating entirely, by design, for exactly this
	// kind of package-local test that doesn't care — but this package's
	// tests very much do).
	mb := modbustcp.New(st, modbustcp.Config{Addr: "127.0.0.1:0", ByteOrder: m261points.BigEndian, Commands: proc})
	mb.SetGate(gate)
	mb.SetLinkCoordinator(linkCoord)
	if err := mb.Start(); err != nil {
		t.Fatalf("modbustcp Start: %v", err)
	}
	t.Cleanup(func() { mb.Close() })

	iec := iec104.New(st, iec104.Config{Addr: "127.0.0.1:0", Commands: proc})
	iec.SetGate(gate)
	iec.SetLinkCoordinator(linkCoord)
	if err := iec.Start(); err != nil {
		t.Fatalf("iec104 Start: %v", err)
	}
	t.Cleanup(func() { iec.Close() })

	inj := faults.NewInjector(st)
	inj.SetGate(gate)
	sr := scenario.NewRunner(st, inj, proc, pr, clk, 5*time.Minute, iec, mb)
	sr.SetLinkCoordinator(linkCoord)

	capi := controlapi.New(controlapi.Config{
		Addr: "127.0.0.1:0", Store: st, Injector: inj, Processor: proc, PhysicsRunner: pr,
		Clock: clk, StepInterval: 5 * time.Minute, ScenarioRunner: sr, IECServer: iec, ModbusServer: mb,
		Gate: gate, LinkCoordinator: linkCoord, StartupSnapshot: st.Snapshot(), NewEngine: newEngine, StartupInstant: startupInstant,
	})
	if err := capi.Start(); err != nil {
		t.Fatalf("controlapi Start: %v", err)
	}
	t.Cleanup(func() { capi.Close() })

	return &task7Sim{
		store: st, clk: clk, processor: proc, physicsRunner: pr, injector: inj,
		mb: mb, iec: iec, capi: capi, startupInstant: startupInstant,
	}
}

func (s *task7Sim) apiURL(path string) string { return "http://" + s.capi.Addr().String() + path }

func allAlarms(t *testing.T) []struct {
	key  m261points.PointKey
	meta m261points.PointMeta
} {
	t.Helper()
	var alarms []struct {
		key  m261points.PointKey
		meta m261points.PointMeta
	}
	for key, meta := range m261points.Points {
		if meta.Class == m261points.ClassAlarm {
			alarms = append(alarms, struct {
				key  m261points.PointKey
				meta m261points.PointMeta
			}{key, meta})
		}
	}
	if len(alarms) != 284 {
		t.Fatalf("found %d class:alarm points, want 284", len(alarms))
	}
	// Deterministic order — both this file's two alarm tests build
	// requests/YAML by iterating this slice, and a stable order makes a
	// failure's step index or Modbus/IEC-104 ordering reproducible
	// between runs instead of depending on Go's randomized map order.
	sort.Slice(alarms, func(i, j int) bool {
		if alarms[i].key.Device != alarms[j].key.Device {
			return alarms[i].key.Device < alarms[j].key.Device
		}
		return alarms[i].key.Slug < alarms[j].key.Slug
	})
	return alarms
}

// assertAllAlarmsVisible reads every alarm in alarms back through a real
// Modbus client (FC02, discrete inputs) and a real IEC-104 client
// (general interrogation, M_SP_NA_1) — shared by both
// TestAllAlarmsInjectableAndVisibleThroughBothProtocols (control-API
// injection) and TestAllAlarmsInjectableViaScenarioFaultStep (scenario
// fault: injection), so both prove the exact same "visible through both
// real protocols" criterion rather than one of them checking something
// weaker.
func assertAllAlarmsVisible(t *testing.T, sim *task7Sim, alarms []struct {
	key  m261points.PointKey
	meta m261points.PointMeta
}) {
	t.Helper()

	mbClients := map[int]gomodbus.Client{}
	for _, a := range alarms {
		unit := a.meta.DeviceAddr
		client, ok := mbClients[unit]
		if !ok {
			handler := gomodbus.NewTCPClientHandler(sim.mb.Addr().String())
			handler.SlaveId = byte(unit)
			handler.Timeout = 5 * time.Second
			if err := handler.Connect(); err != nil {
				t.Fatalf("modbus Connect (unit %d): %v", unit, err)
			}
			t.Cleanup(func() { handler.Close() })
			client = gomodbus.NewClient(handler)
			mbClients[unit] = client
		}
		wireAddr := *a.meta.ModbusAddr - 10001 // discrete input base, §2.2
		bits, err := client.ReadDiscreteInputs(uint16(wireAddr), 1)
		if err != nil {
			t.Fatalf("ReadDiscreteInputs %s/%s (wire %d): %v", a.key.Device, a.key.Slug, wireAddr, err)
		}
		if bits[0]&0x01 != 1 {
			t.Errorf("%s/%s via Modbus = %v, want set (1)", a.key.Device, a.key.Slug, bits[0])
		}
	}

	byDevice := map[int][]int{} // deviceAddr -> IOAs
	for _, a := range alarms {
		byDevice[a.meta.DeviceAddr] = append(byDevice[a.meta.DeviceAddr], a.meta.IEC104Addr)
	}
	for deviceAddr, ioas := range byDevice {
		c := dialRawIEC(t, sim.iec.Addr().String())
		c.startDT()
		c.sendGeneralInterrogation(deviceAddr)
		got := c.waitForBools(ioas...)
		for _, ioa := range ioas {
			v, ok := got[ioa]
			if !ok {
				t.Errorf("common addr %d IOA %d: not seen in general interrogation", deviceAddr, ioa)
				continue
			}
			if !v {
				t.Errorf("common addr %d IOA %d via IEC-104 = false, want true", deviceAddr, ioa)
			}
		}
	}
}

// TestAllAlarmsInjectableAndVisibleThroughBothProtocols is Task 7's
// headline acceptance criterion: every one of the 284 class:alarm points
// is injectable through the control API and visible through both real
// protocol clients — not the internal faults.Injector directly (that's
// package faults's own job) and not this package's own encode/decode
// code (an "external client" test, matching Task 4's own convention).
func TestAllAlarmsInjectableAndVisibleThroughBothProtocols(t *testing.T) {
	sim := newTask7Sim(t)
	alarms := allAlarms(t)

	for _, a := range alarms {
		resp := postJSON(t, sim.apiURL("/faults"), map[string]any{"device": a.key.Device, "point": a.key.Slug, "value": 1})
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("POST /faults for %s/%s: status %d", a.key.Device, a.key.Slug, resp.StatusCode)
		}
		resp.Body.Close()
	}

	assertAllAlarmsVisible(t, sim, alarms)
}

// TestAllAlarmsInjectableViaScenarioFaultStep is the scenario-engine half
// of the same acceptance criterion — Task 7 item 1 names *two* equally
// valid injection paths (control API, scenario fault: step), and a
// reviewed gap in an earlier version proved only the first one for all
// 284 points; the scenario path was only exercised for one or two
// examples. Builds one scenario with 284 fault: steps, all at: 0s (a
// legitimate case per Task 7 item 5 — same at:, different points, see
// scenario.TestRunnerSameTimestampDifferentPointsExecuteInDeclarationOrder
// — not a workaround), loads and runs it through POST /scenario/load
// and /start, then re-uses the identical both-protocols check the
// control-API test above does.
func TestAllAlarmsInjectableViaScenarioFaultStep(t *testing.T) {
	sim := newTask7Sim(t)
	alarms := allAlarms(t)

	var b strings.Builder
	b.WriteString("name: inject-every-alarm\n")
	b.WriteString(`clock: {start: "2026-08-12T00:00:00+03:00", speed: 1000000}` + "\n")
	b.WriteString("steps:\n")
	for _, a := range alarms {
		fmt.Fprintf(&b, "  - at: 0s\n    fault: {device: %s, point: %s, value: 1}\n", a.key.Device, a.key.Slug)
	}

	resp := postJSON(t, sim.apiURL("/scenario/load"), map[string]any{"yaml": b.String()})
	if resp.StatusCode != http.StatusNoContent {
		body := readAll(t, resp)
		t.Fatalf("POST /scenario/load: status %d, body %s", resp.StatusCode, body)
	}
	resp.Body.Close()

	resp = postJSON(t, sim.apiURL("/scenario/start"), nil)
	if resp.StatusCode != http.StatusNoContent {
		body := readAll(t, resp)
		t.Fatalf("POST /scenario/start: status %d, body %s", resp.StatusCode, body)
	}
	resp.Body.Close()

	deadline := time.Now().Add(30 * time.Second)
	for {
		stateResp, err := http.Get(sim.apiURL("/state"))
		if err != nil {
			t.Fatalf("GET /state: %v", err)
		}
		stateResp.Body.Close()
		// No dedicated "is the scenario still running" endpoint in Task
		// 7's approved API surface — poll the last alarm's own value
		// instead, since fault: steps apply synchronously in declaration
		// order and this one is last.
		last := alarms[len(alarms)-1]
		v, _ := sim.store.Get(last.key)
		if v == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("scenario did not finish within 30s (real time) — last alarm %s/%s never landed", last.key.Device, last.key.Slug)
		}
		time.Sleep(time.Millisecond)
	}

	assertAllAlarmsVisible(t, sim, alarms)
}

func readAll(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	defer resp.Body.Close()
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			break
		}
	}
	return buf
}

// TestControlAPIReachableDuringProtocolLinkFault is AGENT-TASK.md, Task 7
// item 2's explicit requirement: an active link fault on IEC-104/Modbus
// must never make the control API itself unreachable — proven here
// against the real protocol servers, not the fakeLinkTarget mocks
// package controlapi's own unit tests use.
func TestControlAPIReachableDuringProtocolLinkFault(t *testing.T) {
	// All four independent modes (AGENT-TASK.md, Task 7 item 2), each
	// against a real modbustcp/iec104.Server via a real controlapi
	// request — modbustcp's and iec104's own packages already have
	// dedicated tests proving exactly how each mode manifests on the
	// wire (existing connections force-closed for drop, no response for
	// hang/delay, a frozen value for heartbeat_pause); this table is
	// specifically about the one thing those per-protocol tests can't
	// check: the control API staying reachable throughout, on its own
	// independent port, for every one of them.
	for _, body := range []map[string]any{
		{"protocol": "both", "mode": "drop"},
		{"protocol": "both", "mode": "hang"},
		{"protocol": "both", "mode": "delay", "delay_ms": 200},
		{"protocol": "both", "mode": "heartbeat_pause"},
	} {
		t.Run(body["mode"].(string), func(t *testing.T) {
			sim := newTask7Sim(t)

			resp := postJSON(t, sim.apiURL("/link"), body)
			if resp.StatusCode != http.StatusNoContent {
				respBody := readAll(t, resp)
				t.Fatalf("POST /link %v: status %d, body %s", body, resp.StatusCode, respBody)
			}
			resp.Body.Close()

			resp2, err := http.Get(sim.apiURL("/state"))
			if err != nil {
				t.Fatalf("GET /state while %s is active: %v", body["mode"], err)
			}
			defer resp2.Body.Close()
			if resp2.StatusCode != http.StatusOK {
				t.Fatalf("GET /state while %s is active: status %d", body["mode"], resp2.StatusCode)
			}

			// Also confirm control-API-driven fault injection still works
			// — not just GET, a mutating request too.
			resp3 := postJSON(t, sim.apiURL("/faults"), map[string]any{"device": "EMS", "point": "manual_protection", "value": 1})
			defer resp3.Body.Close()
			if resp3.StatusCode != http.StatusNoContent {
				t.Errorf("POST /faults while %s is active: status %d", body["mode"], resp3.StatusCode)
			}
		})
	}
}

// TestResetDoesNotRequireProtocolClientReconnect strengthens
// TestResetDoesNotDisconnectClients (package controlapi, which only
// proves the control API's own connection survives) with real Modbus and
// IEC-104 clients: both dial and complete a normal exchange *before*
// POST /reset, then — without reconnecting — read again afterward and
// see the reset (not stale, not an error) value, confirming AGENT-TASK.md
// Task 7 item 7's "reset does not disconnect protocol clients" against
// the actual protocol servers, not just the control API's own listener.
func TestResetDoesNotRequireProtocolClientReconnect(t *testing.T) {
	sim := newTask7Sim(t)

	if err := sim.processor.Write(m261points.PointKey{Device: "EMS", Slug: "maximum_charge_soc"}, 77); err != nil {
		t.Fatal(err)
	}

	mbHandler := gomodbus.NewTCPClientHandler(sim.mb.Addr().String())
	mbHandler.SlaveId = 1
	mbHandler.Timeout = 5 * time.Second
	if err := mbHandler.Connect(); err != nil {
		t.Fatalf("modbus Connect: %v", err)
	}
	t.Cleanup(func() { mbHandler.Close() })
	mbClient := gomodbus.NewClient(mbHandler)

	// maximum_charge_soc: F32, modbus_addr 40019 (class 3) -> wire 40019-40001=18.
	const wireAddr = 18
	regsBefore, err := mbClient.ReadHoldingRegisters(wireAddr, 2)
	if err != nil {
		t.Fatalf("ReadHoldingRegisters before reset: %v", err)
	}
	if got := math.Float32frombits(binary.BigEndian.Uint32(regsBefore)); got != 77 {
		t.Fatalf("Modbus read before reset = %v, want 77 (setup)", got)
	}

	iecClient := dialRawIEC(t, sim.iec.Addr().String())
	iecClient.startDT()

	resp := postJSON(t, sim.apiURL("/reset"), nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("POST /reset: status %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Same Modbus connection, no reconnect — must still answer, with the
	// post-reset value (100, the startup default).
	regsAfter, err := mbClient.ReadHoldingRegisters(wireAddr, 2)
	if err != nil {
		t.Fatalf("ReadHoldingRegisters after reset (same connection): %v", err)
	}
	if got := math.Float32frombits(binary.BigEndian.Uint32(regsAfter)); got != 100 {
		t.Errorf("Modbus read after reset (same connection) = %v, want 100 (reset default)", got)
	}

	// Same IEC-104 connection, no reconnect — a general interrogation
	// must still complete normally.
	iecClient.sendGeneralInterrogation(1) // EMS
	if _, ok := iecClient.waitForFloat(16400); !ok {
		// 16400 = EMS Periodic Heartbeat Indicator's IOA, always present.
		t.Error("general interrogation on the same IEC-104 connection after reset returned nothing")
	}
}

// TestResetIsAtomicAgainstConcurrentProtocolReads is Blocker 2's real-
// protocol half: a real Modbus FC03 read and a real IEC-104 general
// interrogation, both racing a real POST /reset, over actual TCP
// connections — not the in-process controlapi harness's own GET /state
// equivalent. Both servers now take gate.Op around their whole read
// (modbustcp.Server.handleReadRegisters, iec104.Server.
// handleGeneralInterrogation), so every response observed here must be
// either fully pre-reset or fully post-reset, never a value combination
// no single instant ever had.
func TestResetIsAtomicAgainstConcurrentProtocolReads(t *testing.T) {
	sim := newTask7Sim(t)
	key := m261points.PointKey{Device: "EMS", Slug: "maximum_charge_soc"}
	preResetValue, _ := sim.store.Get(key)
	const dirty = 88.0
	if err := sim.processor.Write(key, dirty); err != nil {
		t.Fatal(err)
	}

	const wireAddr = 18 // maximum_charge_soc: F32, modbus_addr 40019 -> wire 40019-40001

	const n = 30
	var wg sync.WaitGroup
	wg.Add(2 * n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			// One independent connection per goroutine — see dialModbus's
			// own doc comment for why a shared client/connection here
			// would never actually reach the server concurrently.
			mbClient, closeClient, err := dialModbus(sim.mb.Addr().String(), 1)
			if err != nil {
				t.Errorf("modbus Connect: %v", err)
				return
			}
			defer closeClient()
			regs, err := mbClient.ReadHoldingRegisters(wireAddr, 2)
			if err != nil {
				t.Errorf("ReadHoldingRegisters: %v", err)
				return
			}
			got := math.Float32frombits(binary.BigEndian.Uint32(regs))
			if float64(got) != dirty && float64(got) != preResetValue {
				t.Errorf("Modbus read = %v, want either %v (pre-reset) or %v (post-reset default)", got, dirty, preResetValue)
			}
		}()
		go func() {
			defer wg.Done()
			c := dialRawIEC(t, sim.iec.Addr().String())
			c.startDT()
			c.sendGeneralInterrogation(1)    // EMS
			got, ok := c.waitForFloat(16400) // EMS Periodic Heartbeat Indicator's IOA, always present
			if !ok {
				t.Error("general interrogation returned nothing for the heartbeat IOA")
			}
			_ = got // presence, not value, is what a torn/aborted response would fail
		}()
	}
	resp := postJSON(t, sim.apiURL("/reset"), nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("POST /reset: status %d", resp.StatusCode)
	}
	resp.Body.Close()
	wg.Wait()
}

// TestFC16BatchWriteAtomicAgainstConcurrentReset is Blocker 2's FC16
// half: a single Modbus FC16 request spanning two adjacent catalog
// points (EMS/set_active_power_kw and EMS/set_reactive_power_kvar,
// wire-adjacent — see commands.Processor.WriteBatch's doc comment)
// racing a real POST /reset. Before WriteBatch, applyRegisterWrites
// committed each touched point through a *separate* Processor.Write/
// gate.Op call, so a reset could interleave between them and leave one
// point at its newly-written value while the other had already been
// reset back to its default — read back here as a same-request register
// pair that must always agree with each other, never split.
func TestFC16BatchWriteAtomicAgainstConcurrentReset(t *testing.T) {
	sim := newTask7Sim(t)
	if err := sim.processor.Write(m261points.PointKey{Device: "EMS", Slug: "set_operating_mode"}, 2); err != nil {
		t.Fatal(err)
	}

	mbClient, closeClient, err := dialModbus(sim.mb.Addr().String(), 1)
	if err != nil {
		t.Fatalf("modbus Connect: %v", err)
	}
	t.Cleanup(closeClient)

	// wire 152-153 = set_active_power_kw (40153), wire 154-155 =
	// set_reactive_power_kvar (40155) — one contiguous 4-register FC16
	// request spans both points.
	const wireAddr = 152
	const activeKW, reactiveKvar = 42.5, -17.5
	buf := make([]byte, 8)
	binary.BigEndian.PutUint32(buf[0:4], math.Float32bits(activeKW))
	binary.BigEndian.PutUint32(buf[4:8], math.Float32bits(reactiveKvar))

	preActiveBits, err := mbClient.ReadHoldingRegisters(wireAddr, 2)
	if err != nil {
		t.Fatalf("ReadHoldingRegisters (setup, active): %v", err)
	}
	preReactiveBits, err := mbClient.ReadHoldingRegisters(wireAddr+2, 2)
	if err != nil {
		t.Fatalf("ReadHoldingRegisters (setup, reactive): %v", err)
	}
	preActive := float64(math.Float32frombits(binary.BigEndian.Uint32(preActiveBits)))
	preReactive := float64(math.Float32frombits(binary.BigEndian.Uint32(preReactiveBits)))

	// One independent connection per writer goroutine — see dialModbus's
	// own doc comment for why a shared client/connection here would never
	// actually reach the server concurrently.
	const n = 30
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			writerClient, closeWriter, err := dialModbus(sim.mb.Addr().String(), 1)
			if err != nil {
				t.Errorf("modbus Connect: %v", err)
				return
			}
			defer closeWriter()
			if _, err := writerClient.WriteMultipleRegisters(wireAddr, 4, buf); err != nil {
				// A rejected write (e.g. mode reset mid-flight, or a
				// transient race with the concurrent reset closing/
				// reopening nothing — reset never disconnects clients)
				// is not itself a failure here; only a *split* commit is.
				return
			}
		}()
	}
	resp := postJSON(t, sim.apiURL("/reset"), nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("POST /reset: status %d", resp.StatusCode)
	}
	resp.Body.Close()
	wg.Wait()

	activeBits, err := mbClient.ReadHoldingRegisters(wireAddr, 2)
	if err != nil {
		t.Fatalf("ReadHoldingRegisters (final, active): %v", err)
	}
	reactiveBits, err := mbClient.ReadHoldingRegisters(wireAddr+2, 2)
	if err != nil {
		t.Fatalf("ReadHoldingRegisters (final, reactive): %v", err)
	}
	active := math.Float32frombits(binary.BigEndian.Uint32(activeBits))
	reactive := math.Float32frombits(binary.BigEndian.Uint32(reactiveBits))

	bothWritten := float64(active) == activeKW && float64(reactive) == reactiveKvar
	bothDefault := float64(active) == preActive && float64(reactive) == preReactive
	if !bothWritten && !bothDefault {
		t.Errorf("active=%v reactive=%v after racing FC16 writes vs reset — want both at the written values (%v, %v) or both at their pre-write defaults (%v, %v), never a split pair",
			active, reactive, activeKW, reactiveKvar, preActive, preReactive)
	}
}

// TestFC06HalfWriteNeverCommitsStaleReadAcrossReset is Blocker 2's exact
// reproduction schedule, made deterministic (not just a stress test) by
// computing every reachable outcome up front and asserting the observed
// final value is one of exactly those, never a third, only-reachable-via
// -the-bug value:
//
//  1. maximum_charge_soc (F32, two Modbus registers) is 77.
//  2. FC06 (WriteSingleRegister) touches only the *low* register — a
//     read-modify-write against whatever the high register currently is.
//  3. A concurrent POST /reset restores the point to 100 (the real
//     default — see TestResetDoesNotRequireProtocolClientReconnect).
//
// Before this fix, applyRegisterWrites read the pre-reset pair (encoding
// 77), composed the new low register onto it, and committed that stale
// composite *after* reset had already run — a result matching neither
// "write completed, then reset" (100, since reset unconditionally
// overwrites the whole point) nor "reset completed, then write" (the low
// register composed onto 100's own encoding, computed below as
// wantResetFirst). Racing many such FC06 requests against many resets,
// the observed final value must always be exactly one of those two.
func TestFC06HalfWriteNeverCommitsStaleReadAcrossReset(t *testing.T) {
	const dirty = 77.0
	const resetDefault = 100.0
	const wireAddr = 18 // maximum_charge_soc: F32, modbus_addr 40019 -> wire 40019-40001
	const lowRegWireAddr = wireAddr + 1
	var lowRegPattern uint16 = 0x1234
	lowRegHi, lowRegLo := byte(lowRegPattern>>8), byte(lowRegPattern)

	dirtyBytes := m261points.EncodeF32(dirty, m261points.BigEndian)
	staleComposite := math.Float32frombits(binary.BigEndian.Uint32(append(append([]byte(nil), dirtyBytes[0:2]...), lowRegHi, lowRegLo)))

	resetBytes := m261points.EncodeF32(resetDefault, m261points.BigEndian)
	wantResetFirst := math.Float32frombits(binary.BigEndian.Uint32(append(append([]byte(nil), resetBytes[0:2]...), lowRegHi, lowRegLo)))

	key := m261points.PointKey{Device: "EMS", Slug: "maximum_charge_soc"}

	for iter := 0; iter < 20; iter++ {
		sim := newTask7Sim(t)
		if err := sim.processor.Write(key, dirty); err != nil {
			t.Fatalf("iter %d: setup Write: %v", iter, err)
		}

		// One independent connection per writer goroutine — see
		// dialModbus's own doc comment for why a shared client/connection
		// here would never actually reach the server concurrently (a
		// genuine gap in an earlier version of this test: goburrow/
		// modbus's own client serializes every request over one
		// connection via an internal mutex, so N goroutines sharing one
		// client never actually raced the server at all).
		const n = 10
		var wg sync.WaitGroup
		wg.Add(n)
		for i := 0; i < n; i++ {
			go func() {
				defer wg.Done()
				writerClient, closeWriter, err := dialModbus(sim.mb.Addr().String(), 1)
				if err != nil {
					t.Errorf("iter %d: modbus Connect: %v", iter, err)
					return
				}
				defer closeWriter()
				writerClient.WriteSingleRegister(lowRegWireAddr, lowRegPattern) //nolint:errcheck // a rejected/erroring write is fine here; only a *committed stale* one is a failure
			}()
		}
		resp := postJSON(t, sim.apiURL("/reset"), nil)
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("iter %d: POST /reset: status %d", iter, resp.StatusCode)
		}
		resp.Body.Close()
		wg.Wait()

		got, _ := sim.store.Get(key)
		gotF32 := float32(got)
		if gotF32 == staleComposite {
			t.Fatalf("iter %d: maximum_charge_soc = %v — the stale-read composite (a pre-reset base register pair, committed after reset), want %v (reset-first) or %v (reset default)", iter, got, wantResetFirst, resetDefault)
		}
		if gotF32 != float32(resetDefault) && gotF32 != wantResetFirst {
			t.Fatalf("iter %d: maximum_charge_soc = %v, want exactly %v (reset default, write-then-reset) or %v (reset-then-write composite)", iter, got, resetDefault, wantResetFirst)
		}
	}
}

// TestFC06AndIECWriteToSameSetpointNeverLoseAnUpdate is Blocker 2's
// write-vs-write case (not write-vs-reset): commands.Processor.writeMu
// makes a register-level read-modify-write (FC06) and a full-replace
// write (an IEC-104 setpoint command) to the *same* point mutually
// exclusive, not just each independently protected from a concurrent
// Reset. Without it, gate.Op alone (a shared lock) let FC06's own read
// land before a concurrent IEC write, while its own commit (built from
// that now-stale base) landed after — silently discarding the IEC write
// entirely, a classic lost update.
//
// Races one FC06 write (touching only the low register) against one
// IEC-104 setpoint write to the same point, repeatedly. Exactly two
// final values are reachable once writeMu makes the two operations
// mutually exclusive as a whole (IEC's write is always either fully
// before or fully after FC06's entire read-modify-write, never straddled
// by it): the IEC value outright (IEC ran last), or FC06's low register
// composed onto IEC's own value (FC06 ran last, having read IEC's
// already-committed base). The only value that requires the bug to
// reach — FC06's low register composed onto the point's *original*,
// pre-IEC-write base — must never appear.
func TestFC06AndIECWriteToSameSetpointNeverLoseAnUpdate(t *testing.T) {
	const original = 77.0
	const iecValue = 55.0
	const wireAddr = 18 // maximum_charge_soc: F32, modbus_addr 40019 -> wire 40019-40001
	const lowRegWireAddr = wireAddr + 1
	const commonAddr, ioa = 1, 25098 // EMS device address, maximum_charge_soc's own IOA
	var lowRegPattern uint16 = 0xABCD
	lowRegHi, lowRegLo := byte(lowRegPattern>>8), byte(lowRegPattern)

	key := m261points.PointKey{Device: "EMS", Slug: "maximum_charge_soc"}

	originalBytes := m261points.EncodeF32(original, m261points.BigEndian)
	forbidden := math.Float32frombits(binary.BigEndian.Uint32(append(append([]byte(nil), originalBytes[0:2]...), lowRegHi, lowRegLo)))

	iecBytes := m261points.EncodeF32(iecValue, m261points.BigEndian)
	fc06WinsComposite := math.Float32frombits(binary.BigEndian.Uint32(append(append([]byte(nil), iecBytes[0:2]...), lowRegHi, lowRegLo)))

	for iter := 0; iter < 20; iter++ {
		sim := newTask7Sim(t)
		if err := sim.processor.Write(key, original); err != nil {
			t.Fatalf("iter %d: setup Write: %v", iter, err)
		}

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			mbClient, closeClient, err := dialModbus(sim.mb.Addr().String(), 1)
			if err != nil {
				t.Errorf("iter %d: modbus Connect: %v", iter, err)
				return
			}
			defer closeClient()
			mbClient.WriteSingleRegister(lowRegWireAddr, lowRegPattern) //nolint:errcheck // a rejected write is fine; only a lost update is a failure
		}()
		go func() {
			defer wg.Done()
			c := dialRawIEC(t, sim.iec.Addr().String())
			c.startDT()
			c.sendSetpointCommand(commonAddr, ioa, float32(iecValue))
			c.expectActivationConfirmation() // block until the server has actually committed this write, not just accepted the bytes on the wire
		}()
		wg.Wait()

		got, _ := sim.store.Get(key)
		gotF32 := float32(got)
		if gotF32 == forbidden {
			t.Fatalf("iter %d: maximum_charge_soc = %v -- FC06's committed composite was built from the *original* pre-IEC-write base, meaning the concurrent IEC write was lost entirely (its own commit landed between FC06's read and commit)", iter, got)
		}
		if gotF32 != float32(iecValue) && gotF32 != fc06WinsComposite {
			t.Fatalf("iter %d: maximum_charge_soc = %v, want exactly %v (IEC won) or %v (FC06 won, composed on top of IEC's own already-committed value)", iter, got, iecValue, fc06WinsComposite)
		}
	}
}

// TestNoDeadlockBetweenTickWriteAndResetUnderReversedLockOrder is a
// broad concurrency smoke test for blocker 3's lock reordering (Gate.Op
// -> writeMu -> Store lock, fifth review round), run against the *real*,
// fully-wired objects (physics.Runner.TickOnce, commands.Processor.Write,
// controlapi's real POST /reset over HTTP) rather than the bare lock
// primitives commands.TestGateOpOuterWriteMuInnerOrderAvoidsTheThreeWay
// Deadlock manually orchestrates. It is deliberately *not* relied on as
// proof of deadlock-freedom by itself — measured directly, undelayed
// concurrent load essentially never lands in the exact narrow three-way
// window that test engineers on purpose (confirmed by hand: this test
// still passed, every time, when the old writeMu-outer order was
// temporarily restored), which is exactly why the review calls out
// stress loops as insufficient proof and asks for a deterministic one —
// commands.TestGateOpOuterWriteMuInnerOrderAvoidsTheThreeWayDeadlock is
// that proof. What this test adds on top: confidence that the real
// end-to-end objects, not just the bare locks, cooperate under load
// without hanging or panicking. It fails fast (5s) instead of relying on
// t's own much longer default panic-on-timeout to notice a hang.
func TestNoDeadlockBetweenTickWriteAndResetUnderReversedLockOrder(t *testing.T) {
	sim := newTask7Sim(t)
	if err := sim.processor.Write(m261points.PointKey{Device: "EMS", Slug: "set_operating_mode"}, 2); err != nil {
		t.Fatal(err)
	}

	const iterations = 200
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			sim.physicsRunner.TickOnce(10 * time.Millisecond) //nolint:errcheck // a busy drive lock just means this tick is skipped -- not a failure here
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		key := m261points.PointKey{Device: "EMS", Slug: "set_active_power_kw"}
		for i := 0; i < iterations; i++ {
			sim.processor.Write(key, float64(i%10)) //nolint:errcheck // a rejected write is fine; a hang is the only failure this test checks for
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iterations/10; i++ {
			resp, err := http.Post(sim.apiURL("/reset"), "application/json", nil)
			if err != nil {
				t.Errorf("POST /reset: %v", err)
				return
			}
			resp.Body.Close()
		}
	}()

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Tick/Write/POST-reset did not all finish within 5s (real time) -- likely deadlocked under the Gate.Op -> writeMu -> Store lock order")
	}
}

// Test72HourNoGapsInHeartbeat is Task 7 item 8: the 72-hour continuous-
// monitoring criterion runs on accelerated/fake time — this whole test
// completes in a few real seconds — and proves no gaps in the *entire*
// model-time heartbeat sequence, not just its endpoints. Advances the
// clock one stepInterval at a time via repeated POST /clock/advance
// calls (the same real control-API surface a monitoring client would
// poll through — deliberately not store.Store.Subscribe, whose small
// fixed buffer (64) would silently drop most of ~864 ticks' worth of
// Changes for points other than the one being watched, long before
// reaching the heartbeat's own), and requires the value after tick N to
// be exactly N — no skip (a missed tick) and no repeat (a double-counted
// one) anywhere in the full 864-tick sequence. Reviewed gap this closes:
// an earlier version only compared the value before and after one big
// 72h advance, which a single skipped tick followed by a compensating
// double-tick — or any other pair of errors that cancel out — would not
// have caught.
func Test72HourNoGapsInHeartbeat(t *testing.T) {
	sim := newTask7Sim(t)
	const stepInterval = 5 * time.Minute
	wantTicks := 72 * 60 * 60 / int(stepInterval.Seconds())

	heartbeatKey := m261points.PointKey{Device: "EMS", Slug: "ems_periodic_heartbeat_indicator"}
	before, _ := sim.store.Get(heartbeatKey)
	if before != 0 {
		t.Fatalf("setup: heartbeat = %v before any advance, want 0", before)
	}

	for i := 1; i <= wantTicks; i++ {
		resp := postJSON(t, sim.apiURL("/clock/advance"), map[string]any{"by_seconds": int64(stepInterval.Seconds())})
		if resp.StatusCode != http.StatusNoContent {
			body := readAll(t, resp)
			t.Fatalf("POST /clock/advance (tick %d): status %d, body %s", i, resp.StatusCode, body)
		}
		resp.Body.Close()

		got, _ := sim.store.Get(heartbeatKey)
		if got != float64(i) {
			t.Fatalf("heartbeat after tick %d = %v, want exactly %d — sequence gap or duplicate at this point", i, got, i)
		}
	}
}

// dialModbus opens a *fresh, independent* TCP connection to addr and
// returns a client bound to it. Fourth-review-round fix: goburrow/modbus's
// own tcpTransporter.Send holds an internal mutex for the whole request-
// response round trip, so N goroutines sharing *one* client/connection
// never actually reach the server concurrently — each request completes
// before the next one is even sent, no matter how many goroutines are
// "racing" to call it. A test that wants genuinely concurrent server-side
// handling (to actually exercise an interleaving a fix is supposed to
// prevent, not just fire a serialized sequence of requests against a live
// reset) needs one real connection per concurrent caller, so the
// server's own acceptLoop spawns one handleConn goroutine per connection
// and they can genuinely run at the same time.
//
// Returns a plain error, not calling t.Fatalf itself — this is routinely
// called from spawned goroutines in these tests, and t.Fatalf/FailNow is
// documented as unsafe to call from any goroutine but the one running
// the test function itself; only the caller, on the right goroutine,
// knows whether Fatalf or Errorf is safe here.
func dialModbus(addr string, unitID byte) (gomodbus.Client, func(), error) {
	handler := gomodbus.NewTCPClientHandler(addr)
	handler.SlaveId = unitID
	handler.Timeout = 5 * time.Second
	if err := handler.Connect(); err != nil {
		return nil, nil, err
	}
	return gomodbus.NewClient(handler), func() { handler.Close() }, nil
}

func postJSON(t *testing.T, url string, body map[string]any) *http.Response {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		reader = bytes.NewReader(b)
	} else {
		reader = bytes.NewReader(nil)
	}
	resp, err := http.Post(url, "application/json", reader)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	return resp
}

// waitForBools mirrors rawIEC's own waitForFloats but for M_SP_NA_1
// (alarms) instead of M_ME_NC_1 (telemetry) — see waitForFloats's doc
// comment for the COT-filtering rationale, identical here.
func (c *rawIEC) waitForBools(ioas ...int) map[int]bool {
	c.t.Helper()
	const cotInterrogatedByStation = 20
	const typeMSPNA1 = 1
	want := make(map[int]bool, len(ioas))
	for _, ioa := range ioas {
		want[ioa] = true
	}
	got := make(map[int]bool, len(ioas))
	for len(got) < len(want) {
		asdu := c.nextI()
		if asdu[0] == 100 {
			if asdu[2] == 10 { // activation termination
				return got
			}
			continue
		}
		if asdu[2] != cotInterrogatedByStation {
			continue
		}
		gotIOA := int(asdu[6]) | int(asdu[7])<<8 | int(asdu[8])<<16
		if asdu[0] == typeMSPNA1 && want[gotIOA] {
			got[gotIOA] = asdu[9]&0x01 == 1
		}
	}
	return got
}

// TestBaseScenariosPassInCI is the "all base scenarios pass in CI"
// acceptance criterion (AGENT-TASK.md, Task 7 item 6/acceptance criteria):
// every scenario file this repository ships loads, parses, and runs to
// completion without a rejected write/fault or a failed expect — proving
// the files aren't just illustrative YAML that happens to look right,
// they actually execute correctly against the real engine.
func TestBaseScenariosPassInCI(t *testing.T) {
	dir := filepath.Join("..", "..", "..", "scenarios")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	var files []string
	for _, e := range entries {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".yaml" {
			files = append(files, e.Name())
		}
	}
	// The base set §6 lists by category, plus active_alarm for item 1's
	// injection criterion — not literally 8 filenames (potentially more
	// than one file per category is fine), but at least 8 confirms none
	// were silently dropped.
	if len(files) < 8 {
		t.Fatalf("found %d scenario files in %s, want at least 8", len(files), dir)
	}

	for _, name := range files {
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				t.Fatalf("ReadFile: %v", err)
			}
			s, err := scenario.Parse(data)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}

			st := store.New()
			startupInstant := time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC)
			clk := clock.NewFake(startupInstant)
			proc, err := commands.NewProcessor(st, clk, commands.DefaultConfig())
			if err != nil {
				t.Fatalf("commands.NewProcessor: %v", err)
			}
			pr := physics.NewRunner(physics.New(physics.DefaultParams(), 50), st, clk, proc)
			inj := faults.NewInjector(st)
			iecFake, mbFake := &noopLinkTarget{}, &noopLinkTarget{}

			// 72_hour_monitoring spans 72h of at: offsets — a coarse step
			// keeps it fast without changing what's being proven (see the
			// file's own comment); everything else stays well under a
			// production-realistic 1s step's reach.
			stepInterval := time.Second
			if name == "72_hour_monitoring.yaml" {
				stepInterval = 5 * time.Minute
			}

			sr := scenario.NewRunner(st, inj, proc, pr, clk, stepInterval, iecFake, mbFake)
			if err := sr.Load(s); err != nil {
				t.Fatalf("Load: %v", err)
			}
			if err := sr.Start(); err != nil {
				t.Fatalf("Start: %v", err)
			}
			deadline := time.Now().Add(30 * time.Second)
			for sr.Running() {
				if time.Now().After(deadline) {
					t.Fatal("scenario did not finish within 30s (real time)")
				}
				time.Sleep(time.Millisecond)
			}
			if err := sr.LastError(); err != nil {
				t.Fatalf("scenario failed: %v", err)
			}
			if got := sr.Cursor(); got != len(s.Steps) {
				t.Errorf("Cursor after completion = %d, want %d (all steps)", got, len(s.Steps))
			}
		})
	}
}

// noopLinkTarget is a linkfault.Target that does nothing — the base
// scenarios' link: steps only need somewhere valid to land, not a real
// protocol server (modbustcp/iec104 have their own link-fault tests
// against the real thing).
type noopLinkTarget struct{}

func (*noopLinkTarget) SetDrop()                  {}
func (*noopLinkTarget) SetHang()                  {}
func (*noopLinkTarget) SetDelay(time.Duration)    {}
func (*noopLinkTarget) SetHeartbeatPause(float64) {}
func (*noopLinkTarget) ClearLinkFaults()          {}
func (*noopLinkTarget) FenceHeartbeat()           {}
