package main

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	gomodbus "github.com/goburrow/modbus"

	"github.com/SterneStehen/petz-m261-tooling/gen/go/m261points"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/clock"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/commands"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/controlapi"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/faults"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/iec104"
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
	store         *store.Store
	clk           *clock.Fake
	processor     *commands.Processor
	physicsRunner *physics.Runner
	injector      *faults.Injector
	mb            *modbustcp.Server
	iec           *iec104.Server
	capi          *controlapi.Server
}

func newTask7Sim(t *testing.T) *task7Sim {
	t.Helper()
	st := store.New()
	startupInstant := time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC)
	clk := clock.NewFake(startupInstant)

	proc, err := commands.NewProcessor(st, clk, commands.DefaultConfig())
	if err != nil {
		t.Fatalf("commands.NewProcessor: %v", err)
	}
	params := physics.DefaultParams()
	newEngine := func() *physics.Engine { return physics.New(params, 50) }
	pr := physics.NewRunner(newEngine(), st, clk, proc)

	mb := modbustcp.New(st, modbustcp.Config{Addr: "127.0.0.1:0", ByteOrder: m261points.BigEndian, Commands: proc})
	if err := mb.Start(); err != nil {
		t.Fatalf("modbustcp Start: %v", err)
	}
	t.Cleanup(func() { mb.Close() })

	iec := iec104.New(st, iec104.Config{Addr: "127.0.0.1:0", Commands: proc})
	if err := iec.Start(); err != nil {
		t.Fatalf("iec104 Start: %v", err)
	}
	t.Cleanup(func() { iec.Close() })

	inj := faults.NewInjector(st)
	sr := scenario.NewRunner(st, inj, proc, pr, clk, 5*time.Minute, iec, mb)

	capi := controlapi.New(controlapi.Config{
		Addr: "127.0.0.1:0", Store: st, Injector: inj, Processor: proc, PhysicsRunner: pr,
		Clock: clk, StepInterval: 5 * time.Minute, ScenarioRunner: sr, IECServer: iec, ModbusServer: mb,
		StartupSnapshot: st.Snapshot(), NewEngine: newEngine, StartupInstant: startupInstant,
	})
	if err := capi.Start(); err != nil {
		t.Fatalf("controlapi Start: %v", err)
	}
	t.Cleanup(func() { capi.Close() })

	return &task7Sim{store: st, clk: clk, processor: proc, physicsRunner: pr, injector: inj, mb: mb, iec: iec, capi: capi}
}

func (s *task7Sim) apiURL(path string) string { return "http://" + s.capi.Addr().String() + path }

// TestAllAlarmsInjectableAndVisibleThroughBothProtocols is Task 7's
// headline acceptance criterion: every one of the 284 class:alarm points
// is injectable through the control API and visible through both real
// protocol clients — not the internal faults.Injector directly (that's
// package faults's own job) and not this package's own encode/decode
// code (an "external client" test, matching Task 4's own convention).
func TestAllAlarmsInjectableAndVisibleThroughBothProtocols(t *testing.T) {
	sim := newTask7Sim(t)

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

	for _, a := range alarms {
		resp := postJSON(t, sim.apiURL("/faults"), map[string]any{"device": a.key.Device, "point": a.key.Slug, "value": 1})
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("POST /faults for %s/%s: status %d", a.key.Device, a.key.Slug, resp.StatusCode)
		}
		resp.Body.Close()
	}

	// Visible via Modbus (FC02, discrete inputs) — one client per device
	// Unit ID, matching modbustcp's own §4.1 addressing.
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

	// Visible via IEC-104 (general interrogation, M_SP_NA_1) — one
	// connection/interrogation per device common address.
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

// TestControlAPIReachableDuringProtocolLinkFault is AGENT-TASK.md, Task 7
// item 2's explicit requirement: an active link fault on IEC-104/Modbus
// must never make the control API itself unreachable — proven here
// against the real protocol servers, not the fakeLinkTarget mocks
// package controlapi's own unit tests use.
func TestControlAPIReachableDuringProtocolLinkFault(t *testing.T) {
	sim := newTask7Sim(t)

	resp := postJSON(t, sim.apiURL("/link"), map[string]any{"protocol": "both", "mode": "drop"})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("POST /link: status %d", resp.StatusCode)
	}
	resp.Body.Close()

	// The real IEC-104 server is now refusing traffic — modbustcp's and
	// iec104's own packages already have dedicated tests proving exactly
	// how drop manifests (existing connections force-closed, new ones
	// refused); what this test is actually about is the next assertion.
	nc, dialErr := net.DialTimeout("tcp", sim.iec.Addr().String(), 300*time.Millisecond)
	if dialErr == nil {
		nc.Close()
	}

	// ...but the control API, on its own independent port, keeps working.
	resp2, err := http.Get(sim.apiURL("/state"))
	if err != nil {
		t.Fatalf("GET /state while IEC-104/Modbus are dropped: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("GET /state while dropped: status %d", resp2.StatusCode)
	}
}

// Test72HourNoGapsInHeartbeat is Task 7 item 8: the 72-hour continuous-
// monitoring criterion runs on accelerated/fake time — this whole test
// completes in well under a second of real time — and proves no gaps in
// the model-time heartbeat sequence: HeartbeatCounter increments exactly
// once per physics tick, so 72h at a 1-minute step must read exactly
// 72*60 = 4320, not more (double-counted) and not less (a skipped tick).
func Test72HourNoGapsInHeartbeat(t *testing.T) {
	sim := newTask7Sim(t)
	const stepInterval = 5 * time.Minute

	before, _ := sim.store.Get(m261points.PointKey{Device: "EMS", Slug: "ems_periodic_heartbeat_indicator"})
	if before != 0 {
		t.Fatalf("setup: heartbeat = %v before any advance, want 0", before)
	}

	resp := postJSON(t, sim.apiURL("/clock/advance"), map[string]any{"by_seconds": int64(72 * 60 * 60)})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("POST /clock/advance: status %d", resp.StatusCode)
	}
	resp.Body.Close()

	after, _ := sim.store.Get(m261points.PointKey{Device: "EMS", Slug: "ems_periodic_heartbeat_indicator"})
	want := float64(72 * 60 * 60 / int(stepInterval.Seconds()))
	if after != want {
		t.Errorf("heartbeat after 72h advance at a %s step = %v, want exactly %v (no gaps, no double-counting)", stepInterval, after, want)
	}
}

func postJSON(t *testing.T, url string, body map[string]any) *http.Response {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	resp, err := http.Post(url, "application/json", bytes.NewReader(b))
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
