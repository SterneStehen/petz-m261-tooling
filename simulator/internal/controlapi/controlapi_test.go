package controlapi_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SterneStehen/petz-m261-tooling/gen/go/m261points"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/appgate"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/clock"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/commands"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/controlapi"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/faults"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/linkfault"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/physics"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/scenario"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/store"
)

// fakeLinkTarget mirrors the one in package scenario's own tests — see
// its doc comment. Duplicated rather than exported from either package:
// each is a small, test-only detail of its own package.
//
// Guarded by its own mutex (unlike the real modbustcp/iec104 linkState,
// this doesn't need one for production correctness — a test double is
// normally driven from one goroutine) because
// TestResetIsAtomicAgainstConcurrentLinkBothAction deliberately drives it
// from many: gate.Op is a *shared* lock (multiple ordinary link actions
// are meant to run genuinely concurrently, serialized only against a
// POST /reset's Exclusive section, never against each other), so two
// concurrent POST /link requests reaching this same fake target is a
// real, intended possibility this file's own tests must not introduce a
// spurious data race for.
type fakeLinkTarget struct {
	mu                  sync.Mutex
	drop, hang, cleared bool
	delay               time.Duration
	hbValue             float64
	hbSet               bool
}

func (f *fakeLinkTarget) SetDrop() { f.mu.Lock(); f.drop = true; f.mu.Unlock() }
func (f *fakeLinkTarget) SetHang() { f.mu.Lock(); f.hang = true; f.mu.Unlock() }
func (f *fakeLinkTarget) SetDelay(d time.Duration) {
	f.mu.Lock()
	f.delay = d
	f.mu.Unlock()
}
func (f *fakeLinkTarget) SetHeartbeatPause(v float64) {
	f.mu.Lock()
	f.hbValue, f.hbSet = v, true
	f.mu.Unlock()
}
func (f *fakeLinkTarget) ClearLinkFaults() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.drop, f.hang, f.delay, f.hbValue, f.hbSet = false, false, 0, 0, false
	f.cleared = true
}
func (f *fakeLinkTarget) FenceHeartbeat() {}

// linkTargetState is fakeLinkTarget's field values without its mutex --
// fakeLinkTarget itself must never be copied by value (go vet's copylocks
// check, correctly: it embeds a sync.Mutex), so snapshot returns this
// instead, for tests that assert on the final state after racing
// concurrent mutators.
type linkTargetState struct {
	drop, hang, cleared bool
	delay               time.Duration
	hbValue             float64
	hbSet               bool
}

func (f *fakeLinkTarget) snapshot() linkTargetState {
	f.mu.Lock()
	defer f.mu.Unlock()
	return linkTargetState{drop: f.drop, hang: f.hang, cleared: f.cleared, delay: f.delay, hbValue: f.hbValue, hbSet: f.hbSet}
}

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
	startupInstant time.Time
	ready          *bool
}

// newHarness builds a controlapi.Server exactly as main.go wires one —
// a single *clock.Fake, every component sharing one appgate.Gate.
func newHarness(t *testing.T) *harness {
	t.Helper()
	st := store.New()
	startupInstant := time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC)
	fc := clock.NewFake(startupInstant)
	gate := appgate.New()
	linkCoord := linkfault.NewCoordinator()

	proc, err := commands.NewProcessor(st, fc, commands.DefaultConfig())
	if err != nil {
		t.Fatalf("commands.NewProcessor: %v", err)
	}
	proc.SetGate(gate)
	newEngine := func() *physics.Engine { return physics.New(physics.DefaultParams(), 50) }
	pr := physics.NewRunner(newEngine(), st, fc, proc)
	pr.SetGate(gate)
	inj := faults.NewInjector(st)
	inj.SetGate(gate)
	iec, mb := &fakeLinkTarget{}, &fakeLinkTarget{}
	sr := scenario.NewRunner(st, inj, proc, pr, fc, time.Minute, iec, mb)
	sr.SetLinkCoordinator(linkCoord)

	startupSnapshot := st.Snapshot()
	ready := true

	srv := controlapi.New(controlapi.Config{
		Addr:            "127.0.0.1:0",
		Store:           st,
		Injector:        inj,
		Processor:       proc,
		PhysicsRunner:   pr,
		Clock:           fc,
		StepInterval:    time.Minute,
		ScenarioRunner:  sr,
		IECServer:       iec,
		ModbusServer:    mb,
		ScenariosDir:    "",
		Gate:            gate,
		LinkCoordinator: linkCoord,
		StartupSnapshot: startupSnapshot,
		NewEngine:       newEngine,
		StartupInstant:  startupInstant,
		Ready:           func() bool { return ready },
	})
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { srv.Close() })
	return &harness{
		store: st, injector: inj, processor: proc, physicsRunner: pr, clk: fc, scenarioRunner: sr,
		iec: iec, mb: mb, server: srv, baseURL: "http://" + srv.Addr().String(), startupInstant: startupInstant,
		ready: &ready,
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

func TestV1CatalogStateAndCommands(t *testing.T) {
	h := newHarness(t)
	resp, body := h.do(t, http.MethodGet, "/api/v1/catalog", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("catalog status = %d: %s", resp.StatusCode, body)
	}
	var catalog struct {
		Points []json.RawMessage `json:"points"`
	}
	if err := json.Unmarshal(body, &catalog); err != nil || len(catalog.Points) != len(m261points.Points) {
		t.Fatalf("catalog = %d points, err=%v", len(catalog.Points), err)
	}
	resp, body = h.do(t, http.MethodGet, "/api/v1/state", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("state status = %d: %s", resp.StatusCode, body)
	}
	var state struct {
		Revision uint64            `json:"revision"`
		Points   []json.RawMessage `json:"points"`
	}
	if err := json.Unmarshal(body, &state); err != nil || len(state.Points) != len(m261points.Points) {
		t.Fatalf("state err=%v points=%d", err, len(state.Points))
	}
	resp, body = h.do(t, http.MethodPost, "/api/v1/commands", map[string]any{"device": "EMS", "slug": "set_operating_mode", "value": 2})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("command status = %d: %s", resp.StatusCode, body)
	}
	resp, body = h.do(t, http.MethodPost, "/api/v1/commands", map[string]any{"device": "BMS", "slug": "soc", "value": 2})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("non-setpoint status = %d: %s", resp.StatusCode, body)
	}
	resp, body = h.do(t, http.MethodPost, "/api/v1/demo/prepare", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("demo prepare status = %d: %s", resp.StatusCode, body)
	}
}

func TestV1StatusHealthAndReadEndpoints(t *testing.T) {
	h := newHarness(t)
	for _, path := range []string{"/api/v1/status", "/api/v1/scenarios", "/api/v1/scenario/status", "/api/v1/diagnostics", "/api/v1/health/live", "/api/v1/health/ready"} {
		resp, body := h.do(t, http.MethodGet, path, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s: status=%d body=%s", path, resp.StatusCode, body)
		}
	}
	*h.ready = false
	resp, body := h.do(t, http.MethodGet, "/api/v1/health/ready", nil)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("not-ready status=%d body=%s", resp.StatusCode, body)
	}
	if got := decodeError(t, body).Error.Code; got != "not_ready" {
		t.Fatalf("not-ready code=%q", got)
	}
	resp, body = h.do(t, http.MethodPost, "/api/v1/demo/prepare", nil)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("demo not-ready status=%d body=%s", resp.StatusCode, body)
	}
}

func TestV1DemoPrepareIsDeterministic(t *testing.T) {
	h := newHarness(t)
	key := m261points.PointKey{Device: "EMS", Slug: "set_operating_mode"}
	if err := h.processor.Write(key, 2); err != nil {
		t.Fatal(err)
	}
	resp, body := h.do(t, http.MethodPost, "/api/v1/demo/prepare", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("first prepare=%d: %s", resp.StatusCode, body)
	}
	first := h.store.Snapshot()
	if err := h.processor.Write(key, 1); err != nil {
		t.Fatal(err)
	}
	resp, body = h.do(t, http.MethodPost, "/api/v1/demo/prepare", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("second prepare=%d: %s", resp.StatusCode, body)
	}
	second := h.store.Snapshot()
	if !reflect.DeepEqual(first, second) {
		t.Fatal("two demo prepare calls produced different Store snapshots")
	}
}

func TestV1EventsBootstrapsAndCoalescesTelemetry(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.baseURL+"/api/v1/events?initial_state=true", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("SSE status=%d", resp.StatusCode)
	}
	reader := bufio.NewReader(resp.Body)
	if typ, _ := readSSEEvent(t, reader); typ != "snapshot" {
		t.Fatalf("first SSE event=%q, want snapshot", typ)
	}
	key := m261points.PointKey{Device: "EMS", Slug: "desired_active_power_kw"}
	if !h.store.Set(key, 10) || !h.store.Set(key, 20) {
		t.Fatal("Store.Set failed")
	}
	type received struct {
		typ  string
		data []byte
	}
	got := make(chan received, 1)
	go func() { typ, data := readSSEEvent(t, reader); got <- received{typ, data} }()
	select {
	case event := <-got:
		if event.typ != "telemetry" {
			t.Fatalf("second SSE event=%q, want telemetry", event.typ)
		}
		var payload struct {
			Payload struct {
				Changes []struct {
					Device string  `json:"device"`
					Slug   string  `json:"slug"`
					Value  float64 `json:"value"`
				} `json:"changes"`
			} `json:"payload"`
			Revision uint64 `json:"revision"`
		}
		if err := json.Unmarshal(event.data, &payload); err != nil {
			t.Fatal(err)
		}
		if payload.Revision != h.store.CurrentRevision() || len(payload.Payload.Changes) != 1 || payload.Payload.Changes[0].Value != 20 {
			t.Fatalf("coalesced telemetry=%s", event.data)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for coalesced telemetry")
	}
}

func readSSEEvent(t *testing.T, reader *bufio.Reader) (string, []byte) {
	t.Helper()
	typ := ""
	var data []byte
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read SSE: %v", err)
		}
		line = strings.TrimSuffix(line, "\n")
		if line == "" {
			return typ, data
		}
		if strings.HasPrefix(line, "event: ") {
			typ = strings.TrimPrefix(line, "event: ")
		}
		if strings.HasPrefix(line, "data: ") {
			data = []byte(strings.TrimPrefix(line, "data: "))
		}
	}
}

// --- routing/method/JSON contract ------------------------------------------

func TestUnknownRouteIsJSON404(t *testing.T) {
	h := newHarness(t)
	resp, body := h.do(t, "GET", "/api/v1/nope", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body = %s", resp.StatusCode, body)
	}
	if e := decodeError(t, body); e.Error.Code != "not_found" {
		t.Errorf("error.code = %q, want not_found (body: %s)", e.Error.Code, body)
	}
}

func TestEmbeddedWebUIAndAPIRouting(t *testing.T) {
	h := newHarness(t)
	resp, body := h.do(t, http.MethodGet, "/", nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "M261 Simulator Console") {
		t.Fatalf("web UI status=%d body=%s", resp.StatusCode, body)
	}
	resp, body = h.do(t, http.MethodGet, "/api/v1/status", nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(resp.Header.Get("Content-Type"), "application/json") {
		t.Fatalf("API route was intercepted: status=%d content-type=%q body=%s", resp.StatusCode, resp.Header.Get("Content-Type"), body)
	}
}

func TestWrongMethodIsJSON405(t *testing.T) {
	h := newHarness(t)
	resp, body := h.do(t, "POST", "/state", nil)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405, body = %s", resp.StatusCode, body)
	}
	if e := decodeError(t, body); e.Error.Code != "method_not_allowed" {
		t.Errorf("error.code = %q, want method_not_allowed (body: %s)", e.Error.Code, body)
	}
	if got := resp.Header.Get("Allow"); got != "GET" {
		t.Errorf("Allow header = %q, want GET", got)
	}
}

func TestTrailingJSONDataRejected(t *testing.T) {
	h := newHarness(t)
	req, _ := http.NewRequest("POST", h.baseURL+"/faults", bytes.NewReader(
		[]byte(`{"device":"BMS","point":"cell_temperature_too_high","value":1}{"device":"BMS","point":"cell_temperature_too_high","value":0}`),
	))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (a second JSON value in the body must be rejected, not silently ignored)", resp.StatusCode)
	}
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

func TestStateRejectsUnknownDevice(t *testing.T) {
	h := newHarness(t)
	resp, body := h.do(t, "GET", "/state?device=NOPE", nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", resp.StatusCode, body)
	}
	if e := decodeError(t, body); e.Error.Code != "unknown_device" {
		t.Errorf("error.code = %q, want unknown_device", e.Error.Code)
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

func TestInjectFaultRequiresValue(t *testing.T) {
	h := newHarness(t)
	resp, body := h.do(t, "POST", "/faults", map[string]any{"device": "BMS", "point": "cell_temperature_too_high"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (value is missing, must not default to 0), body = %s", resp.StatusCode, body)
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

// TestSetLinkDelayRequiresPositiveDelayMS mirrors scenario.Parse's own
// rule for link: {mode: delay} — a control-API request for the identical
// action must not be looser (accepting 0/negative/absent delay_ms as if
// it meant something) than the scenario dialect.
func TestSetLinkDelayRequiresPositiveDelayMS(t *testing.T) {
	h := newHarness(t)
	for _, body := range []map[string]any{
		{"protocol": "iec104", "mode": "delay"}, // absent
		{"protocol": "iec104", "mode": "delay", "delay_ms": 0},
		{"protocol": "iec104", "mode": "delay", "delay_ms": -5},
	} {
		resp, respBody := h.do(t, "POST", "/link", body)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("delay_ms=%v: status = %d, want 400, body = %s", body["delay_ms"], resp.StatusCode, respBody)
		}
	}
	if h.iec.delay != 0 {
		t.Errorf("iec.delay = %v, want unchanged 0 — no invalid delay request should have taken effect", h.iec.delay)
	}
}

// TestSetLinkRejectsOverflowingDelayMS is the fourth review round's
// duration-overflow finding applied to link.delay_ms: delay_ms *
// time.Millisecond must fit in a time.Duration (an int64 count of
// nanoseconds), or it silently wraps (Go doesn't panic on integer
// overflow) instead of erroring — reproduced black-box as delay_ms:
// MaxInt64 getting a 204 with the resulting "delay" wrapped to a
// negative duration, silently disabling the delay the request asked
// for instead of being rejected.
func TestSetLinkRejectsOverflowingDelayMS(t *testing.T) {
	h := newHarness(t)
	resp, body := h.do(t, "POST", "/link", map[string]any{
		"protocol": "iec104", "mode": "delay", "delay_ms": math.MaxInt64,
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("delay_ms=MaxInt64: status = %d, want 400, body = %s", resp.StatusCode, body)
	}
	if h.iec.snapshot().delay != 0 {
		t.Errorf("iec.delay = %v, want unchanged 0 — an overflowing delay_ms must not take effect", h.iec.snapshot().delay)
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
clock: {start: "2026-08-12T00:00:00+03:00", speed: 1000000}
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

// TestScenarioStartAvailableByDefault is the fix for a reviewed
// architectural gap: an earlier version defaulted to a clock.Real shared
// clock, which made every scenario/clock-advance endpoint permanently
// 409 unless an operator remembered a non-default flag. newHarness wires
// exactly what main.go now always wires (a single *clock.Fake) — this
// just confirms scenario start succeeds against it with no special
// setup.
func TestScenarioStartAvailableByDefault(t *testing.T) {
	h := newHarness(t)
	h.do(t, "POST", "/scenario/load", map[string]any{"yaml": inlineScenarioYAML})
	resp, body := h.do(t, "POST", "/scenario/start", nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204, body = %s", resp.StatusCode, body)
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
	if got := h.clk.Now(); got.Sub(h.startupInstant) != 2*time.Minute {
		t.Errorf("clock advanced to %v, want +2m from startup", got)
	}
}

func TestClockAdvanceAvailableByDefault(t *testing.T) {
	h := newHarness(t)
	resp, body := h.do(t, "POST", "/clock/advance", map[string]any{"by_seconds": 1})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204, body = %s", resp.StatusCode, body)
	}
}

func TestClockAdvanceRejectsNegative(t *testing.T) {
	h := newHarness(t)
	resp, body := h.do(t, "POST", "/clock/advance", map[string]any{"by_seconds": -1})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", resp.StatusCode, body)
	}
}

func TestClockAdvanceRequiresBySeconds(t *testing.T) {
	h := newHarness(t)
	resp, body := h.do(t, "POST", "/clock/advance", map[string]any{})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (by_seconds is missing, must not default to 0), body = %s", resp.StatusCode, body)
	}
}

// TestClockAdvanceConflictsWithRunningScenario is the "pacer-handoff-vs
// -scenario-start" case from the second review round: a scenario claims
// physics.Runner's drive lock for its whole run (Start to Stop/
// completion), so a POST /clock/advance landing while one is running
// must be rejected with 409 clock_busy — never silently interleave its
// own ticks with the scenario's, and never block until the scenario
// happens to finish.
func TestClockAdvanceConflictsWithRunningScenario(t *testing.T) {
	h := newHarness(t)
	resp, body := h.do(t, "POST", "/scenario/load", map[string]any{"yaml": `
name: long
clock: {start: "2026-08-12T00:00:00+03:00", speed: 1000000}
steps:
  - at: 1h
    write: {device: EMS, point: set_operating_mode, value: 2}
`})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("scenario/load status = %d, body = %s", resp.StatusCode, body)
	}
	resp, body = h.do(t, "POST", "/scenario/start", nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("scenario/start status = %d, body = %s", resp.StatusCode, body)
	}
	t.Cleanup(func() { h.scenarioRunner.Stop() })

	resp, body = h.do(t, "POST", "/clock/advance", map[string]any{"by_seconds": 1})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("clock/advance status while scenario running = %d, want 409, body = %s", resp.StatusCode, body)
	}
	e := decodeError(t, body)
	if e.Error.Code != "clock_busy" {
		t.Errorf("error code = %q, want clock_busy", e.Error.Code)
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
	// Dirty the watchdog/latch: enter Remote and refresh a setpoint to
	// arm the watchdog timer.
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
	if got := h.clk.Now(); !got.Equal(h.startupInstant) {
		t.Errorf("clock after reset = %v, want the startup instant %v", got, h.startupInstant)
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

// TestResetIsAtomicAgainstConcurrentWrites stresses the reviewed gap
// directly: many goroutines writing through commands.Processor
// concurrently with POST /reset, run under -race so any interleaving
// that isn't properly serialized by appgate.Gate surfaces as a detected
// race, not just a flaky assertion. The functional check afterward
// (dispatch behaves correctly for a fresh Remote setpoint) would fail if
// Processor's internal state ended up torn — part reset, part not.
func TestResetIsAtomicAgainstConcurrentWrites(t *testing.T) {
	h := newHarness(t)
	const n = 100
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			h.processor.Write(m261points.PointKey{Device: "EMS", Slug: "maximum_charge_soc"}, float64(50+i%50)) //nolint:errcheck
		}(i)
	}
	resp, body := h.do(t, "POST", "/reset", nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	wg.Wait()

	if err := h.processor.Write(m261points.PointKey{Device: "EMS", Slug: "set_operating_mode"}, 2); err != nil {
		t.Fatal(err)
	}
	if err := h.processor.Write(m261points.PointKey{Device: "EMS", Slug: "set_active_power_kw"}, 40); err != nil {
		t.Fatal(err)
	}
	active, _ := h.processor.ResolvePower(h.clk.Now(), 130.5, 130.5, 50, false, false)
	if active != 40 {
		t.Errorf("dispatch after concurrent writes racing reset = %v, want 40 (Processor state must stay internally consistent)", active)
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

// TestResetIsAtomicAgainstConcurrentStateReads is Blocker 2 from the
// second review round: GET /state used to read Store.Snapshot with no
// gate at all, so a request landing partway through POST /reset's
// six-step sequence could observe a state no single instant before or
// after reset ever actually had (e.g. physics already rebuilt but Store
// not yet fully restored). GET /state now takes gate.Op around its own
// Snapshot call, making that impossible: every response is a complete,
// internally consistent snapshot from either strictly before or strictly
// after the whole reset, verified here by racing many concurrent readers
// against one real POST /reset under -race and checking each response's
// specific dirtied point is one of exactly the two valid values, never
// anything else.
func TestResetIsAtomicAgainstConcurrentStateReads(t *testing.T) {
	h := newHarness(t)
	key := m261points.PointKey{Device: "EMS", Slug: "maximum_charge_soc"}
	preResetValue, _ := h.store.Get(key)
	const dirty = 77.0
	if err := h.processor.Write(key, dirty); err != nil {
		t.Fatalf("dirty write: %v", err)
	}

	type result struct {
		status int
		value  float64
		ok     bool
	}
	const n = 50
	results := make(chan result, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			resp, body := h.do(t, "GET", "/state", nil)
			if resp.StatusCode != http.StatusOK {
				results <- result{status: resp.StatusCode}
				return
			}
			var decoded struct {
				Points map[string]float64 `json:"points"`
			}
			if err := json.Unmarshal(body, &decoded); err != nil {
				t.Errorf("decode GET /state response: %v", err)
				return
			}
			v, ok := decoded.Points["EMS/maximum_charge_soc"]
			results <- result{status: resp.StatusCode, value: v, ok: ok}
		}()
	}
	resp, body := h.do(t, "POST", "/reset", nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("reset status = %d, body = %s", resp.StatusCode, body)
	}
	wg.Wait()
	close(results)

	for r := range results {
		if r.status != http.StatusOK {
			t.Errorf("GET /state status = %d, want 200", r.status)
			continue
		}
		if !r.ok {
			t.Error("EMS/maximum_charge_soc missing from GET /state response")
			continue
		}
		if r.value != dirty && r.value != preResetValue {
			t.Errorf("EMS/maximum_charge_soc = %v, want either %v (pre-reset dirty) or %v (post-reset default) — never anything else", r.value, dirty, preResetValue)
		}
	}
}

// TestResetIsAtomicAgainstConcurrentLinkBothAction is Blocker 3's link
// half: POST /link with protocol: both mutates iec104's and modbus's
// link state sequentially, not as one atomic step, and so did POST
// /reset's own clear — without LinkCoordinator spanning the whole
// two-target Apply call for *both* operations, the two sequential pairs
// could interleave and leave one protocol faulted while the other was
// cleared, a combination neither "reset happened" nor "reset didn't
// happen" produces on its own. Raced under -race for many iterations;
// the exact timing of which side (the n concurrent drops, or the reset)
// lands last is inherently non-deterministic, but whichever does, the
// two targets must always agree with each other — asserted on every
// iteration, not just "no panic/no race".
func TestResetIsAtomicAgainstConcurrentLinkBothAction(t *testing.T) {
	for iter := 0; iter < 20; iter++ {
		h := newHarness(t)
		var wg sync.WaitGroup
		const n = 20
		wg.Add(n)
		for i := 0; i < n; i++ {
			go func() {
				defer wg.Done()
				h.do(t, "POST", "/link", map[string]any{"protocol": "both", "mode": "drop"})
			}()
		}
		resp, body := h.do(t, "POST", "/reset", nil)
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("iter %d: reset status = %d, body = %s", iter, resp.StatusCode, body)
		}
		wg.Wait()

		// iec.cleared/mb.cleared are sticky ("ClearLinkFaults ran at least
		// once on this target", never reset back to false by a later
		// SetDrop) — not "currently cleared" — so only drop (the field
		// that actually flips back and forth as these two kinds of
		// operations race) is a meaningful "did the two targets end up
		// agreeing" check here.
		iec, mb := h.iec.snapshot(), h.mb.snapshot()
		if iec.drop != mb.drop {
			t.Fatalf("iter %d: iec.drop=%v mb.drop=%v after racing protocol:both drop vs reset, want equal (never split)", iter, iec.drop, mb.drop)
		}
	}
}

// TestConcurrentLinkBothOperationsNeverSplit is Blocker 3's link-vs-link
// case (not link-vs-reset): two concurrent protocol: both operations
// (one drop, one clear) racing each other, with no reset involved at
// all — LinkCoordinator must serialize them against *each other* too,
// not only against reset, since appgate.Gate.Op (the pre-fix mechanism)
// is a shared lock that never excluded two concurrent Op holders from
// each other.
func TestConcurrentLinkBothOperationsNeverSplit(t *testing.T) {
	for iter := 0; iter < 20; iter++ {
		h := newHarness(t)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			h.do(t, "POST", "/link", map[string]any{"protocol": "both", "mode": "drop"})
		}()
		go func() {
			defer wg.Done()
			h.do(t, "POST", "/link/clear", map[string]any{"protocol": "both"})
		}()
		wg.Wait()

		iec, mb := h.iec.snapshot(), h.mb.snapshot()
		if iec.drop != mb.drop || iec.cleared != mb.cleared {
			t.Fatalf("iter %d: iec=%+v mb=%+v after racing two concurrent protocol:both operations, want the two targets to always agree", iter, iec, mb)
		}
	}
}

// TestHeartbeatPauseNeverSurvivesResetWithStaleValue is Blocker 3's
// heartbeat_pause-vs-reset case: heartbeat_pause used to read the Store's
// current heartbeat value *before* acquiring any lock at all, so a
// concurrent POST /reset landing between that read and the eventual
// Apply could produce a pause holding a stale pre-reset value — one the
// just-reset Store no longer has, and (since reset's own clear only ran
// once, before this racing heartbeat_pause's own Apply) one that then
// survives the reset indefinitely. Dirties the heartbeat to a
// distinctive value no post-reset state can legitimately produce, races
// many heartbeat_pause requests against one reset, and asserts every
// resulting final state is either "not paused" or "paused at the
// current (post-reset) value" — never paused at the pre-reset dirty
// value.
func TestHeartbeatPauseNeverSurvivesResetWithStaleValue(t *testing.T) {
	const dirtyHeartbeat = 999999.0
	for iter := 0; iter < 20; iter++ {
		h := newHarness(t)
		h.store.Set(m261points.PointKey{Device: "EMS", Slug: "ems_periodic_heartbeat_indicator"}, dirtyHeartbeat)

		var wg sync.WaitGroup
		const n = 20
		wg.Add(n)
		for i := 0; i < n; i++ {
			go func() {
				defer wg.Done()
				h.do(t, "POST", "/link", map[string]any{"protocol": "both", "mode": "heartbeat_pause"})
			}()
		}
		resp, body := h.do(t, "POST", "/reset", nil)
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("iter %d: reset status = %d, body = %s", iter, resp.StatusCode, body)
		}
		wg.Wait()

		freshValue, _ := h.store.Get(m261points.PointKey{Device: "EMS", Slug: "ems_periodic_heartbeat_indicator"})
		for _, target := range []struct {
			name string
			snap linkTargetState
		}{{"iec", h.iec.snapshot()}, {"mb", h.mb.snapshot()}} {
			if target.snap.hbSet && target.snap.hbValue == dirtyHeartbeat {
				t.Fatalf("iter %d: %s paused at %v (the pre-reset dirty value) after racing heartbeat_pause vs reset — a stale pause survived the reset", iter, target.name, target.snap.hbValue)
			}
			if target.snap.hbSet && target.snap.hbValue != freshValue {
				t.Fatalf("iter %d: %s paused at %v, want either not paused or paused at the current post-reset value %v", iter, target.name, target.snap.hbValue, freshValue)
			}
		}
	}
}
