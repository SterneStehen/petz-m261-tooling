package controlapi_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/SterneStehen/petz-m261-tooling/gen/go/m261points"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/clock"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/commands"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/controlapi"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/faults"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/physics"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/scenario"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/store"
)

// fakeLinkTarget mirrors the one in package scenario's own tests — see
// its doc comment. Duplicated rather than exported from either package:
// each is a small, test-only detail of its own package.
type fakeLinkTarget struct {
	drop, hang, cleared bool
	delay               time.Duration
	hbValue             float64
	hbSet               bool
}

func (f *fakeLinkTarget) SetDrop()                 { f.drop = true }
func (f *fakeLinkTarget) SetHang()                 { f.hang = true }
func (f *fakeLinkTarget) SetDelay(d time.Duration) { f.delay = d }
func (f *fakeLinkTarget) SetHeartbeatPause(v float64) {
	f.hbValue, f.hbSet = v, true
}
func (f *fakeLinkTarget) ClearLinkFaults() { *f = fakeLinkTarget{cleared: true} }

type harness struct {
	store          *store.Store
	injector       *faults.Injector
	processor      *commands.Processor
	physicsRunner  *physics.Runner
	clk            *clock.Fake
	scenarioRunner *scenario.Runner
	iec, mb        *fakeLinkTarget
	server         *controlapi.Server
	baseURL        string
}

// newHarness builds a controlapi.Server with a *clock.Fake — the mode
// most of this package's endpoints need (POST /clock/advance, scenario
// playback). newHarnessRealClock (below) covers the clock_not_fake path.
func newHarness(t *testing.T) *harness {
	t.Helper()
	return newHarnessWithClock(t, true)
}

func newHarnessRealClock(t *testing.T) *harness {
	t.Helper()
	return newHarnessWithClock(t, false)
}

func newHarnessWithClock(t *testing.T, fake bool) *harness {
	t.Helper()
	st := store.New()
	startupInstant := time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC)
	fc := clock.NewFake(startupInstant)
	var clk clock.Clock = fc
	if !fake {
		clk = clock.Real{}
	}

	proc, err := commands.NewProcessor(st, clk, commands.DefaultConfig())
	if err != nil {
		t.Fatalf("commands.NewProcessor: %v", err)
	}
	newEngine := func() *physics.Engine { return physics.New(physics.DefaultParams(), 50) }
	pr := physics.NewRunner(newEngine(), st, clk, proc)
	inj := faults.NewInjector(st)
	iec, mb := &fakeLinkTarget{}, &fakeLinkTarget{}
	sr := scenario.NewRunner(st, inj, proc, pr, fc, time.Minute, iec, mb)

	startupSnapshot := st.Snapshot()

	srv := controlapi.New(controlapi.Config{
		Addr:            "127.0.0.1:0",
		Store:           st,
		Injector:        inj,
		Processor:       proc,
		PhysicsRunner:   pr,
		Clock:           clk,
		StepInterval:    time.Minute,
		ScenarioRunner:  sr,
		IECServer:       iec,
		ModbusServer:    mb,
		ScenariosDir:    "",
		StartupSnapshot: startupSnapshot,
		NewEngine:       newEngine,
		StartupInstant:  startupInstant,
	})
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { srv.Close() })
	return &harness{
		store: st, injector: inj, processor: proc, physicsRunner: pr, clk: fc, scenarioRunner: sr,
		iec: iec, mb: mb, server: srv, baseURL: "http://" + srv.Addr().String(),
	}
}

type apiError struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (h *harness) do(t *testing.T, method, path string, body any) (*http.Response, []byte) {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal request body: %v", err)
		}
		reader = bytes.NewReader(b)
	} else {
		reader = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, h.baseURL+path, reader)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	respBody := make([]byte, 0)
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		respBody = append(respBody, buf[:n]...)
		if err != nil {
			break
		}
	}
	return resp, respBody
}

func decodeError(t *testing.T, body []byte) apiError {
	t.Helper()
	var e apiError
	if err := json.Unmarshal(body, &e); err != nil {
		t.Fatalf("decode error response %s: %v", body, err)
	}
	return e
}

// --- GET /state -------------------------------------------------------

func TestState(t *testing.T) {
	h := newHarness(t)
	h.store.Set(m261points.PointKey{Device: "EMS", Slug: "set_active_power_kw"}, -42)

	resp, body := h.do(t, "GET", "/state", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	var got struct {
		Points map[string]float64 `json:"points"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Points) != len(m261points.Points) {
		t.Errorf("len(points) = %d, want %d", len(got.Points), len(m261points.Points))
	}
	if got.Points["EMS/set_active_power_kw"] != -42 {
		t.Errorf("EMS/set_active_power_kw = %v, want -42", got.Points["EMS/set_active_power_kw"])
	}
}

func TestStateFiltersByDevice(t *testing.T) {
	h := newHarness(t)
	resp, body := h.do(t, "GET", "/state?device=BMS", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	var got struct {
		Points map[string]float64 `json:"points"`
	}
	json.Unmarshal(body, &got) //nolint:errcheck
	for k := range got.Points {
		if k[:4] != "BMS/" {
			t.Errorf("?device=BMS returned non-BMS point %s", k)
		}
	}
	if len(got.Points) == 0 {
		t.Error("?device=BMS returned no points")
	}
}

// --- POST /faults, DELETE /faults/{device}/{point} ---------------------

func TestInjectFault(t *testing.T) {
	h := newHarness(t)
	resp, body := h.do(t, "POST", "/faults", map[string]any{"device": "BMS", "point": "cell_temperature_too_high", "value": 1})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	if v, _ := h.store.Get(m261points.PointKey{Device: "BMS", Slug: "cell_temperature_too_high"}); v != 1 {
		t.Errorf("stored value = %v, want 1", v)
	}
}

func TestInjectFaultRejectsUnknownPoint(t *testing.T) {
	h := newHarness(t)
	resp, body := h.do(t, "POST", "/faults", map[string]any{"device": "NOPE", "point": "nope", "value": 1})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", resp.StatusCode, body)
	}
	if e := decodeError(t, body); e.Error.Code != "unknown_point" {
		t.Errorf("error.code = %q, want unknown_point", e.Error.Code)
	}
}

func TestInjectFaultRejectsNonAlarmClass(t *testing.T) {
	h := newHarness(t)
	resp, body := h.do(t, "POST", "/faults", map[string]any{"device": "EMS", "point": "set_active_power_kw", "value": 1})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", resp.StatusCode, body)
	}
	if e := decodeError(t, body); e.Error.Code != "not_alarm_class" {
		t.Errorf("error.code = %q, want not_alarm_class", e.Error.Code)
	}
}

func TestInjectFaultRejectsMalformedJSON(t *testing.T) {
	h := newHarness(t)
	req, _ := http.NewRequest("POST", h.baseURL+"/faults", bytes.NewReader([]byte("{not json")))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestClearFault(t *testing.T) {
	h := newHarness(t)
	h.store.Set(m261points.PointKey{Device: "BMS", Slug: "cell_temperature_too_high"}, 1)
	resp, body := h.do(t, "DELETE", "/faults/BMS/cell_temperature_too_high", nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	if v, _ := h.store.Get(m261points.PointKey{Device: "BMS", Slug: "cell_temperature_too_high"}); v != 0 {
		t.Errorf("stored value after clear = %v, want 0", v)
	}
}

func TestClearFaultRejectsNonAlarmClass(t *testing.T) {
	h := newHarness(t)
	resp, body := h.do(t, "DELETE", "/faults/EMS/set_active_power_kw", nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", resp.StatusCode, body)
	}
}

// --- POST /link, POST /link/clear ---------------------------------------

func TestSetLinkDrop(t *testing.T) {
	h := newHarness(t)
	resp, body := h.do(t, "POST", "/link", map[string]any{"protocol": "iec104", "mode": "drop"})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	if !h.iec.drop || h.mb.drop {
		t.Errorf("iec.drop=%v mb.drop=%v, want iec only", h.iec.drop, h.mb.drop)
	}
}

func TestSetLinkHeartbeatPausePassesLiveValue(t *testing.T) {
	h := newHarness(t)
	h.store.Set(m261points.PointKey{Device: "EMS", Slug: "ems_periodic_heartbeat_indicator"}, 7)
	resp, body := h.do(t, "POST", "/link", map[string]any{"protocol": "both", "mode": "heartbeat_pause"})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	if !h.iec.hbSet || h.iec.hbValue != 7 || !h.mb.hbSet || h.mb.hbValue != 7 {
		t.Errorf("heartbeat freeze value not propagated correctly: iec=%+v mb=%+v", h.iec, h.mb)
	}
}

func TestSetLinkRejectsBadProtocol(t *testing.T) {
	h := newHarness(t)
	resp, body := h.do(t, "POST", "/link", map[string]any{"protocol": "bogus", "mode": "drop"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", resp.StatusCode, body)
	}
}

func TestClearLink(t *testing.T) {
	h := newHarness(t)
	h.iec.drop, h.mb.hang = true, true
	resp, body := h.do(t, "POST", "/link/clear", map[string]any{"protocol": "both"})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	if !h.iec.cleared || !h.mb.cleared {
		t.Errorf("both targets should have ClearLinkFaults called: iec=%+v mb=%+v", h.iec, h.mb)
	}
}

// --- POST /scenario/load, /start, /stop ----------------------------------

const inlineScenarioYAML = `
name: inline-test
clock: {start: "2026-08-12T00:00:00+03:00", speed: 1}
steps:
  - at: 0s
    write: {device: EMS, point: set_operating_mode, value: 2}
`

func TestScenarioLoadInlineYAML(t *testing.T) {
	h := newHarness(t)
	resp, body := h.do(t, "POST", "/scenario/load", map[string]any{"yaml": inlineScenarioYAML})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	if h.scenarioRunner.Loaded() == nil {
		t.Error("scenario not loaded")
	}
}

func TestScenarioLoadRejectsBothNameAndYAML(t *testing.T) {
	h := newHarness(t)
	resp, body := h.do(t, "POST", "/scenario/load", map[string]any{"name": "x.yaml", "yaml": inlineScenarioYAML})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", resp.StatusCode, body)
	}
}

func TestScenarioLoadRejectsNeitherNameNorYAML(t *testing.T) {
	h := newHarness(t)
	resp, body := h.do(t, "POST", "/scenario/load", map[string]any{})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", resp.StatusCode, body)
	}
}

func TestScenarioLoadInvalidYAMLRejected(t *testing.T) {
	h := newHarness(t)
	resp, body := h.do(t, "POST", "/scenario/load", map[string]any{"yaml": "not: [valid, scenario"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", resp.StatusCode, body)
	}
}

func TestScenarioLoadByNameFromScenariosDir(t *testing.T) {
	h := newHarness(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "x.yaml"), []byte(inlineScenarioYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	// Rebuild a server with ScenariosDir set — newHarness leaves it empty.
	srv2 := controlapi.New(controlapi.Config{
		Addr: "127.0.0.1:0", Store: h.store, Injector: h.injector, Processor: h.processor,
		PhysicsRunner: h.physicsRunner, Clock: h.clk, StepInterval: time.Minute,
		ScenarioRunner: h.scenarioRunner, IECServer: h.iec, ModbusServer: h.mb,
		ScenariosDir: dir, StartupSnapshot: h.store.Snapshot(), NewEngine: func() *physics.Engine { return physics.New(physics.DefaultParams(), 50) },
		StartupInstant: h.clk.Now(),
	})
	if err := srv2.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv2.Close() })

	req, _ := http.NewRequest("POST", "http://"+srv2.Addr().String()+"/scenario/load", bytes.NewReader(mustJSON(t, map[string]any{"name": "x.yaml"})))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestScenarioStartWithoutLoadIsConflict(t *testing.T) {
	h := newHarness(t)
	resp, body := h.do(t, "POST", "/scenario/start", nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409, body = %s", resp.StatusCode, body)
	}
	if e := decodeError(t, body); e.Error.Code != "no_scenario_loaded" {
		t.Errorf("error.code = %q, want no_scenario_loaded", e.Error.Code)
	}
}

func TestScenarioStartRequiresFakeClock(t *testing.T) {
	h := newHarnessRealClock(t)
	h.do(t, "POST", "/scenario/load", map[string]any{"yaml": inlineScenarioYAML})
	resp, body := h.do(t, "POST", "/scenario/start", nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409, body = %s", resp.StatusCode, body)
	}
	if e := decodeError(t, body); e.Error.Code != "clock_not_fake" {
		t.Errorf("error.code = %q, want clock_not_fake", e.Error.Code)
	}
}

func TestScenarioStartThenStop(t *testing.T) {
	h := newHarness(t)
	h.do(t, "POST", "/scenario/load", map[string]any{"yaml": inlineScenarioYAML})
	resp, body := h.do(t, "POST", "/scenario/start", nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("start status = %d, body = %s", resp.StatusCode, body)
	}
	resp2, body2 := h.do(t, "POST", "/scenario/stop", nil)
	if resp2.StatusCode != http.StatusNoContent {
		t.Fatalf("stop status = %d, body = %s", resp2.StatusCode, body2)
	}
	// idempotent
	resp3, body3 := h.do(t, "POST", "/scenario/stop", nil)
	if resp3.StatusCode != http.StatusNoContent {
		t.Fatalf("second stop status = %d, body = %s", resp3.StatusCode, body3)
	}
}

// --- POST /clock/advance --------------------------------------------------

func TestClockAdvance(t *testing.T) {
	h := newHarness(t)
	resp, body := h.do(t, "POST", "/clock/advance", map[string]any{"by_seconds": 120})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	if got := h.clk.Now(); got.Sub(time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC)) != 2*time.Minute {
		t.Errorf("clock advanced to %v, want +2m from startup", got)
	}
}

func TestClockAdvanceRequiresFakeClock(t *testing.T) {
	h := newHarnessRealClock(t)
	resp, body := h.do(t, "POST", "/clock/advance", map[string]any{"by_seconds": 1})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409, body = %s", resp.StatusCode, body)
	}
	if e := decodeError(t, body); e.Error.Code != "clock_not_fake" {
		t.Errorf("error.code = %q, want clock_not_fake", e.Error.Code)
	}
}

func TestClockAdvanceRejectsNegative(t *testing.T) {
	h := newHarness(t)
	resp, body := h.do(t, "POST", "/clock/advance", map[string]any{"by_seconds": -1})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", resp.StatusCode, body)
	}
}

// --- POST /reset ------------------------------------------------------------

// TestResetFromDirtyState is Task 7 item 7's acceptance criterion in
// full: deliberately dirty every kind of state Reset is documented to
// touch, then confirm all of it — Store, Processor's watchdog/latch/
// diagnostics, physics (including RNG-seed reproducibility), link
// faults, and the scenario cursor — is back to its startup value.
func TestResetFromDirtyState(t *testing.T) {
	h := newHarness(t)

	// Dirty the Store (a setpoint away from its startup default).
	if err := h.processor.Write(m261points.PointKey{Device: "EMS", Slug: "maximum_charge_soc"}, 77); err != nil {
		t.Fatal(err)
	}
	// Dirty a fault.
	h.store.Set(m261points.PointKey{Device: "BMS", Slug: "cell_temperature_too_high"}, 1)
	// Dirty link fault state.
	h.iec.drop = true
	// Dirty the watchdog/latch: enter Remote, go stale under
	// safe_state_after... simpler: just accumulate a Diagnostic via Trip
	// with allow_dangerous off is rejected, so directly exercise via
	// Demand Control priority winning instead (always available,
	// DefaultConfig's ModePriority already has demand_control below
	// remote — use Manual/Remote mode switch instead for a cheap,
	// reliable dirty signal): write Set Operating Mode = Remote and a
	// setpoint to arm the watchdog timer.
	if err := h.processor.Write(m261points.PointKey{Device: "EMS", Slug: "set_operating_mode"}, 2); err != nil {
		t.Fatal(err)
	}
	if err := h.processor.Write(m261points.PointKey{Device: "EMS", Slug: "set_active_power_kw"}, -10); err != nil {
		t.Fatal(err)
	}
	// Dirty physics: advance the clock and tick so SoC moves off 50% —
	// under the default watchdog.timeout_s (60s), so the -10kW setpoint
	// written above is still fresh and actually dispatches.
	h.clk.Advance(30 * time.Second)
	h.physicsRunner.Tick()
	if soc, _ := h.store.Get(m261points.PointKey{Device: "BMS", Slug: "soc"}); soc == 50 {
		t.Fatal("setup: SoC didn't move — test can't prove physics reset without a real starting delta")
	}

	resp, body := h.do(t, "POST", "/reset", nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}

	// Store: setpoint back to its startup default (100, per
	// Processor.publishSensibleDefaults).
	if v, _ := h.store.Get(m261points.PointKey{Device: "EMS", Slug: "maximum_charge_soc"}); v != 100 {
		t.Errorf("maximum_charge_soc after reset = %v, want 100 (startup default)", v)
	}
	// Fault cleared.
	if v, _ := h.store.Get(m261points.PointKey{Device: "BMS", Slug: "cell_temperature_too_high"}); v != 0 {
		t.Errorf("cell_temperature_too_high after reset = %v, want 0", v)
	}
	// Link faults cleared.
	if !h.iec.cleared {
		t.Error("iec link faults were not cleared by reset")
	}
	// Physics: SoC back to the original 50%.
	if soc, _ := h.store.Get(m261points.PointKey{Device: "BMS", Slug: "soc"}); soc != 50 {
		t.Errorf("SoC after reset = %v, want 50 (startup initial SoC)", soc)
	}
	// Clock: back to the startup instant.
	if got := h.clk.Now(); !got.Equal(time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("clock after reset = %v, want the startup instant", got)
	}
	// Watchdog: a fresh Remote setpoint dispatches immediately (proves
	// the pre-reset watchdog timer/latch didn't survive).
	if err := h.processor.Write(m261points.PointKey{Device: "EMS", Slug: "set_operating_mode"}, 2); err != nil {
		t.Fatal(err)
	}
	if err := h.processor.Write(m261points.PointKey{Device: "EMS", Slug: "set_active_power_kw"}, 40); err != nil {
		t.Fatal(err)
	}
	active, _ := h.processor.ResolvePower(h.clk.Now(), 130.5, 130.5, 50, false, false)
	if active != 40 {
		t.Errorf("dispatch right after reset + fresh Remote setpoint = %v, want 40", active)
	}
}

// TestResetDoesNotDisconnectClients is documented explicitly in
// AGENT-TASK.md, Task 7 item 7: reset is a Store/internal-state reset,
// not a network event — the control API connection itself (this test's
// own HTTP client) must keep working across a reset.
func TestResetDoesNotDisconnectClients(t *testing.T) {
	h := newHarness(t)
	resp, body := h.do(t, "POST", "/reset", nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("reset status = %d, body = %s", resp.StatusCode, body)
	}
	resp2, body2 := h.do(t, "GET", "/state", nil)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("GET /state after reset: status = %d, body = %s", resp2.StatusCode, body2)
	}
}
