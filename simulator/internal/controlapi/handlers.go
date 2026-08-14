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
// not the device name this API uses everywhere else — EMS/BMS/…).
func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	device := r.URL.Query().Get("device")
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

type faultRequest struct {
	Device string  `json:"device"`
	Point  string  `json:"point"`
	Value  float64 `json:"value"`
}

func (s *Server) handleInjectFault(w http.ResponseWriter, r *http.Request) {
	var req faultRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", fmt.Errorf("malformed JSON body: %w", err))
		return
	}
	key := m261points.PointKey{Device: req.Device, Slug: req.Point}
	if err := s.cfg.Injector.Inject(key, req.Value); err != nil {
		writeFaultsError(w, err)
		return
	}
	writeNoContent(w)
}

func (s *Server) handleClearFault(w http.ResponseWriter, r *http.Request) {
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

func (s *Server) handleSetLink(w http.ResponseWriter, r *http.Request) {
	var req linkRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", fmt.Errorf("malformed JSON body: %w", err))
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

func (s *Server) handleClearLink(w http.ResponseWriter, r *http.Request) {
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
	if !s.requireFakeClock(w) {
		return
	}
	if err := s.cfg.ScenarioRunner.Start(); err != nil {
		writeScenarioError(w, err)
		return
	}
	writeNoContent(w)
}

func (s *Server) handleScenarioStop(w http.ResponseWriter, r *http.Request) {
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

type clockAdvanceRequest struct {
	BySeconds int64 `json:"by_seconds"`
}

func (s *Server) handleClockAdvance(w http.ResponseWriter, r *http.Request) {
	if !s.requireFakeClock(w) {
		return
	}
	var req clockAdvanceRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", fmt.Errorf("malformed JSON body: %w", err))
		return
	}
	if req.BySeconds < 0 {
		writeError(w, http.StatusBadRequest, "invalid_request", fmt.Errorf("by_seconds must be non-negative, got %d", req.BySeconds))
		return
	}
	total := time.Duration(req.BySeconds) * time.Second
	if err := s.cfg.PhysicsRunner.FastForward(total, s.cfg.StepInterval); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err)
		return
	}
	writeNoContent(w)
}

// requireFakeClock writes a 409 and returns false if the simulator's
// shared clock isn't a *clock.Fake — POST /clock/advance and
// POST /scenario/start both need to fast-forward/replay against a
// controllable clock, which clock.Real (wired by default in production,
// main.go's -clock=real) can never be (AGENT-TASK §1.5).
func (s *Server) requireFakeClock(w http.ResponseWriter) bool {
	if _, ok := s.cfg.Clock.(*clock.Fake); !ok {
		writeError(w, http.StatusConflict, "clock_not_fake", errors.New(
			"this endpoint requires the simulator to be running with a fake, controllable clock (-clock=fake)",
		))
		return false
	}
	return true
}

// --- POST /reset -----------------------------------------------------------

// handleReset implements AGENT-TASK.md, Task 7 item 7 in full — see
// doReset.
func (s *Server) handleReset(w http.ResponseWriter, r *http.Request) {
	s.doReset()
	writeNoContent(w)
}

// doReset returns every piece of state this package coordinates to
// exactly what it was right after the simulator finished its own
// startup initialization — atomically with respect to physics ticks and
// concurrent API/protocol actions:
//
//  1. Stop the scenario runner first, and block until it has actually
//     exited (scenario.Runner.Stop's guarantee) — nothing below is safe
//     to do while a scenario step could still be executing concurrently.
//  2. If the shared Clock is a *clock.Fake, rewind it to StartupInstant.
//     A clock.Real is left alone; there's nothing to rewind.
//  3. Rebuild physics.Runner's Engine from scratch (same params, same
//     initial SoC, same RNG seed — NewEngine, not a new random one) and
//     rebase its dt baseline to StartupInstant, under physics.Runner's
//     own Tick-exclusion lock (Reset), so no concurrent Tick can
//     interleave with this.
//  4. Reset commands.Processor's own internal bookkeeping (watchdog
//     timer, safe_state_after latch, diagnostics).
//  5. Restore every Store value to StartupSnapshot as one atomic
//     operation (store.Store.Restore).
//  6. Clear every active link fault on both protocol servers.
//
// Steps 3-6 touch disjoint state (Engine internals, Processor internals,
// Store values, protocol server link state respectively) — their
// relative order doesn't matter for correctness, only that step 1
// (stop the scenario) happens first and blocks until real, and that
// physics.Runner.Reset's own internal lock (not this function) is what
// makes step 3 atomic against a concurrent Tick from the *production*
// real-clock ticker (physics.Runner.Run), which reset does not stop —
// AGENT-TASK.md, Task 7 item 7 requires reset to work in normal
// operation too, not just mid-scenario.
func (s *Server) doReset() {
	s.cfg.ScenarioRunner.ResetPlayback()

	if fc, ok := s.cfg.Clock.(*clock.Fake); ok {
		fc.Set(s.cfg.StartupInstant)
	}

	s.cfg.PhysicsRunner.Reset(s.cfg.NewEngine(), s.cfg.StartupInstant)
	s.cfg.Processor.Reset()
	s.cfg.Store.Restore(s.cfg.StartupSnapshot)
	s.cfg.IECServer.ClearLinkFaults()
	s.cfg.ModbusServer.ClearLinkFaults()
}
