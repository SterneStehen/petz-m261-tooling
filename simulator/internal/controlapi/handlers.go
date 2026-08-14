package controlapi

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/SterneStehen/petz-m261-tooling/gen/go/m261points"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/clock"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/faults"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/linkfault"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/scenario"
)

// --- GET /state ---------------------------------------------------------

// handleState answers with every point's value, or one device's if
// ?device=<device> is given — filtered against the live Snapshot rather
// than store.SnapshotDevice (which is keyed by numeric device address,
// not the device name this API uses everywhere else — EMS/BMS/…). An
// unrecognized ?device= is a 400, not a silently empty {"points":{}} —
// a caller with a typo'd device name deserves to be told, not to read
// "no points" as "this device really has none".
func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	device := r.URL.Query().Get("device")
	if device != "" && !s.knownDevice(device) {
		writeError(w, http.StatusBadRequest, "unknown_device", fmt.Errorf("unknown device %q", device))
		return
	}
	snap := s.cfg.Store.Snapshot()
	points := make(map[string]float64, len(snap))
	for k, v := range snap {
		if device != "" && k.Device != device {
			continue
		}
		points[k.Device+"/"+k.Slug] = v
	}
	writeJSON(w, http.StatusOK, map[string]any{"points": points})
}

// --- POST /faults, DELETE /faults/{device}/{point} ---------------------

// Value is a pointer — a request with no value field at all must be
// rejected outright, not silently treated as value: 0 (the same
// required-field discipline scenario.Parse applies to write:/fault:
// steps, for the same reason: Task 7 item 5's "reject the whole thing,
// don't apply it partially" applies to a malformed request just as much
// as a malformed scenario step).
type faultRequest struct {
	Device string   `json:"device"`
	Point  string   `json:"point"`
	Value  *float64 `json:"value"`
}

func (s *Server) handleFaults(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var req faultRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", fmt.Errorf("malformed JSON body: %w", err))
		return
	}
	if req.Value == nil {
		writeError(w, http.StatusBadRequest, "invalid_request", errors.New("value is required"))
		return
	}
	key := m261points.PointKey{Device: req.Device, Slug: req.Point}
	if err := s.cfg.Injector.Inject(key, *req.Value); err != nil {
		writeFaultsError(w, err)
		return
	}
	writeNoContent(w)
}

func (s *Server) handleFaultByPath(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodDelete) {
		return
	}
	key := m261points.PointKey{Device: r.PathValue("device"), Slug: r.PathValue("point")}
	if err := s.cfg.Injector.Clear(key); err != nil {
		writeFaultsError(w, err)
		return
	}
	writeNoContent(w)
}

func writeFaultsError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, faults.ErrUnknownPoint):
		writeError(w, http.StatusBadRequest, "unknown_point", err)
	case errors.Is(err, faults.ErrNotAlarmClass):
		writeError(w, http.StatusBadRequest, "not_alarm_class", err)
	default:
		writeError(w, http.StatusBadRequest, "invalid_request", err)
	}
}

// --- POST /link, POST /link/clear ---------------------------------------

type linkRequest struct {
	Protocol string `json:"protocol"`
	Mode     string `json:"mode"`
	DelayMS  int    `json:"delay_ms"`
}

func (s *Server) handleLink(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var req linkRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", fmt.Errorf("malformed JSON body: %w", err))
		return
	}
	// Mirrors scenario.Parse's own rule for link: {mode: delay} (Task 7
	// item 2/4) — a control-API request must not be looser than the
	// scenario dialect for the identical action.
	if req.Mode == string(linkfault.ModeDelay) && req.DelayMS <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_request", fmt.Errorf("mode: delay requires a positive delay_ms, got %d", req.DelayMS))
		return
	}
	var hbValue float64
	if req.Mode == string(linkfault.ModeHeartbeatPause) {
		hbValue, _ = s.cfg.Store.Get(linkfault.HeartbeatKey)
	}
	delay := time.Duration(req.DelayMS) * time.Millisecond
	err := linkfault.Apply(s.cfg.IECServer, s.cfg.ModbusServer, linkfault.Protocol(req.Protocol), linkfault.Mode(req.Mode), delay, hbValue)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err)
		return
	}
	writeNoContent(w)
}

type linkClearRequest struct {
	Protocol string `json:"protocol"`
}

func (s *Server) handleLinkClear(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var req linkClearRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", fmt.Errorf("malformed JSON body: %w", err))
		return
	}
	err := linkfault.Apply(s.cfg.IECServer, s.cfg.ModbusServer, linkfault.Protocol(req.Protocol), linkfault.ModeClear, 0, 0)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err)
		return
	}
	writeNoContent(w)
}

// --- POST /scenario/load, /start, /stop ----------------------------------

type scenarioLoadRequest struct {
	Name string `json:"name"`
	YAML string `json:"yaml"`
}

// handleScenarioLoad accepts either {"name": "<file in scenarios/>"} or
// {"yaml": "<inline text>"} — exactly one, matching every other action
// shape in this package's own scenario YAML dialect (AGENT-TASK.md,
// Task 7 item 4).
func (s *Server) handleScenarioLoad(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var req scenarioLoadRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", fmt.Errorf("malformed JSON body: %w", err))
		return
	}
	haveName := req.Name != ""
	haveYAML := req.YAML != ""
	if haveName == haveYAML {
		writeError(w, http.StatusBadRequest, "invalid_request", errors.New("exactly one of name or yaml is required"))
		return
	}

	var data []byte
	if haveName {
		if s.cfg.ScenariosDir == "" {
			writeError(w, http.StatusBadRequest, "invalid_request", errors.New("name form is unavailable: no scenarios directory configured"))
			return
		}
		// filepath.Base strips any directory component the client might
		// smuggle in (e.g. "../../etc/passwd") — name is only ever a
		// bare filename within ScenariosDir, never an arbitrary path.
		path := filepath.Join(s.cfg.ScenariosDir, filepath.Base(req.Name))
		b, err := os.ReadFile(path)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", fmt.Errorf("reading %s: %w", req.Name, err))
			return
		}
		data = b
	} else {
		data = []byte(req.YAML)
	}

	parsed, err := scenario.Parse(data)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err)
		return
	}
	if err := s.cfg.ScenarioRunner.Load(parsed); err != nil {
		writeScenarioError(w, err)
		return
	}
	writeNoContent(w)
}

func (s *Server) handleScenarioStart(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	if err := s.cfg.ScenarioRunner.Start(); err != nil {
		writeScenarioError(w, err)
		return
	}
	writeNoContent(w)
}

func (s *Server) handleScenarioStop(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	s.cfg.ScenarioRunner.Stop() // idempotent — stopping an already-stopped runner is a no-op
	writeNoContent(w)
}

func writeScenarioError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, scenario.ErrNoScenarioLoaded):
		writeError(w, http.StatusConflict, "no_scenario_loaded", err)
	case errors.Is(err, scenario.ErrAlreadyRunning), errors.Is(err, scenario.ErrLoadWhileRunning):
		writeError(w, http.StatusConflict, "scenario_running", err)
	default:
		writeError(w, http.StatusBadRequest, "invalid_request", err)
	}
}

// --- POST /clock/advance --------------------------------------------------

// BySeconds is a pointer for the same reason faultRequest.Value is — a
// request with no by_seconds field is rejected outright, not silently
// treated as a zero-length (no-op) advance.
type clockAdvanceRequest struct {
	BySeconds *int64 `json:"by_seconds"`
}

// handleClockAdvance suspends physics.Runner's own real-time pacing for
// the duration of the request (physics.Runner.SuspendPacing) — without
// this, PacedRun's independent ticks could interleave with FastForward's
// own, making the exact tick sequence Task 7 item 8 needs unpredictable.
func (s *Server) handleClockAdvance(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var req clockAdvanceRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", fmt.Errorf("malformed JSON body: %w", err))
		return
	}
	if req.BySeconds == nil {
		writeError(w, http.StatusBadRequest, "invalid_request", errors.New("by_seconds is required"))
		return
	}
	if *req.BySeconds < 0 {
		writeError(w, http.StatusBadRequest, "invalid_request", fmt.Errorf("by_seconds must be non-negative, got %d", *req.BySeconds))
		return
	}
	total := time.Duration(*req.BySeconds) * time.Second

	resume := s.cfg.PhysicsRunner.SuspendPacing()
	defer resume()
	if err := s.cfg.PhysicsRunner.FastForward(total, s.cfg.StepInterval); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err)
		return
	}
	writeNoContent(w)
}

// --- POST /reset -----------------------------------------------------------

// handleReset implements AGENT-TASK.md, Task 7 item 7 in full — see
// doReset.
func (s *Server) handleReset(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	if err := s.doReset(); err != nil {
		writeError(w, http.StatusInternalServerError, "reset_failed", err)
		return
	}
	writeNoContent(w)
}

// doReset returns every piece of state this package coordinates to
// exactly what it was right after the simulator finished its own
// startup initialization — atomically, at the application level, with
// respect to physics ticks and every other mutating API/protocol action
// (AGENT-TASK.md, Task 7 item 7). The whole sequence runs inside one
// s.cfg.Gate.Exclusive() section (package appgate): every ordinary
// mutation elsewhere (commands.Processor.Write, faults.Injector.Inject/
// Clear, physics.Runner.Tick/TickOnce — every one of them takes the
// gate's shared side) is blocked from starting until this function
// returns, and this function can't start until every currently in-flight
// one has finished. Reviewed gap this closes: the previous version ran
// its six steps with no lock spanning all of them, so a write or a tick
// could land between two steps and observe/produce neither the pre- nor
// the post-reset state.
//
//  1. Stop the scenario runner, blocking until it has actually exited
//     (scenario.Runner.Stop's guarantee).
//  2. Rewind the shared clock (always a *clock.Fake — see main.go) to
//     StartupInstant.
//  3. Rebuild physics.Runner's Engine from scratch (same params, same
//     initial SoC, same RNG seed — NewEngine, not a new random one) and
//     rebase its dt baseline to StartupInstant.
//  4. Reset commands.Processor's own internal bookkeeping (watchdog
//     timer, safe_state_after latch, diagnostics).
//  5. Restore every Store value to StartupSnapshot as one atomic
//     operation (store.Store.Restore).
//  6. Clear every active link fault on both protocol servers.
func (s *Server) doReset() error {
	fc, ok := s.cfg.Clock.(*clock.Fake)
	if !ok {
		return fmt.Errorf("controlapi: reset requires a *clock.Fake shared clock, got %T", s.cfg.Clock)
	}

	// Stop the scenario runner first, *outside* the exclusive gate
	// section below — ResetPlayback blocks until the run() goroutine has
	// actually exited (scenario.Runner.Stop's guarantee), and that
	// goroutine's own in-flight TickOnce/Write/Inject call needs the
	// gate's shared side to complete. Acquiring the exclusive side first
	// would deadlock: this function holding it and waiting on the
	// goroutine, the goroutine blocked acquiring the shared side this
	// function is holding exclusively.
	s.cfg.ScenarioRunner.ResetPlayback()

	release := s.cfg.Gate.Exclusive()
	defer release()

	fc.Set(s.cfg.StartupInstant)
	s.cfg.PhysicsRunner.Reset(s.cfg.NewEngine(), s.cfg.StartupInstant)
	s.cfg.Processor.Reset()
	s.cfg.Store.Restore(s.cfg.StartupSnapshot)
	s.cfg.IECServer.ClearLinkFaults()
	s.cfg.ModbusServer.ClearLinkFaults()
	return nil
}
