package controlapi

import (
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/SterneStehen/petz-m261-tooling/gen/go/m261points"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/clock"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/faults"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/linkfault"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/physics"
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
	// gate.Op held only around the snapshot capture — see
	// iec104.Server.handleGeneralInterrogation's identical comment for
	// why that's enough: once Snapshot returns, this response is already
	// fully determined from one atomically-captured map, so a concurrent
	// POST /reset can never make it observe a mix of pre- and post-reset
	// values.
	release := s.cfg.Gate.Op()
	snap := s.cfg.Store.Snapshot()
	release()
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
	rev := s.cfg.Store.CurrentRevision()
	s.events.publish("fault", s.cfg.Clock.Now(), &rev, map[string]any{"device": key.Device, "slug": key.Slug, "value": *req.Value})
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
	rev := s.cfg.Store.CurrentRevision()
	s.events.publish("fault", s.cfg.Clock.Now(), &rev, map[string]any{"device": key.Device, "slug": key.Slug, "value": 0})
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

// maxDelayMS is the largest delay_ms that, multiplied by time.Millisecond,
// still fits in a time.Duration (an int64 count of nanoseconds) — mirrors
// maxAdvanceSeconds's own rationale: a request past this silently wraps
// around instead of erroring (Go doesn't panic on integer overflow),
// which is how a fourth-review-round reproduction (delay_ms: MaxInt64)
// got a 204 with the delay silently wrapping to a negative duration —
// accepted, but the resulting "delay" is none at all, defeating the
// request outright rather than being rejected. ~292,471 years in
// milliseconds — far past any legitimate use of this endpoint.
const maxDelayMS = int64(math.MaxInt64) / int64(time.Millisecond)

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
	if int64(req.DelayMS) > maxDelayMS {
		writeError(w, http.StatusBadRequest, "invalid_request", fmt.Errorf("delay_ms %d exceeds the maximum representable duration (%d ms)", req.DelayMS, maxDelayMS))
		return
	}
	delay := time.Duration(req.DelayMS) * time.Millisecond
	// linkfault.ApplyCoordinated holds LinkCoordinator for the whole
	// heartbeat-capture-plus-Apply sequence — see its own doc comment for
	// the races this closes: protocol: both mutating iec104's and
	// modbus's link state as two independent, uncoordinated calls (which
	// a concurrent POST /reset's own clear, or another link operation,
	// could interleave with), and a heartbeat_pause's own current-value
	// read landing before a concurrent reset, with the eventual Apply
	// (and its now-stale captured value) landing after.
	err := linkfault.ApplyCoordinated(s.cfg.LinkCoordinator, s.cfg.Store, s.cfg.IECServer, s.cfg.ModbusServer, linkfault.Protocol(req.Protocol), linkfault.Mode(req.Mode), delay)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err)
		return
	}
	// Fifth-review-round fix: called *after* ApplyCoordinated has already
	// released LinkCoordinator, never while still holding it — see
	// linkfault.Target.FenceHeartbeat's own doc comment for why (a slow
	// connection's own transport write must never be able to freeze link
	// operations for every other connection/protocol). This response
	// (204) only completes once every heartbeat frame admitted before
	// this call has actually been sent — the fencing guarantee Task 7
	// item 2's heartbeat_pause needs.
	s.cfg.IECServer.FenceHeartbeat()
	s.cfg.ModbusServer.FenceHeartbeat()
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
	// See handleLink's identical comment on why this needs LinkCoordinator
	// held for the whole (potentially protocol: both) Apply call.
	err := linkfault.ApplyCoordinated(s.cfg.LinkCoordinator, s.cfg.Store, s.cfg.IECServer, s.cfg.ModbusServer, linkfault.Protocol(req.Protocol), linkfault.ModeClear, 0)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err)
		return
	}
	// See handleLink's identical comment: fence outside LinkCoordinator's
	// hold, after it has already been released by ApplyCoordinated.
	s.cfg.IECServer.FenceHeartbeat()
	s.cfg.ModbusServer.FenceHeartbeat()
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
		if errors.Is(err, physics.ErrClockBusy) {
			// Reviewed gap: this used to fall through to
			// writeScenarioError's default case (400 invalid_request),
			// despite this package's own doc comments elsewhere promising
			// 409 for exactly this conflict (POST /clock/advance's
			// identical mapping) — a real, expected conflict between two
			// legitimate requests (another external driver already owns
			// the clock), not a malformed one.
			writeError(w, http.StatusConflict, "clock_busy", err)
			return
		}
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

// maxAdvanceSeconds is the largest by_seconds that, multiplied by
// time.Second, still fits in a time.Duration (an int64 count of
// nanoseconds) — a request past this silently wraps around instead of
// erroring (Go doesn't panic on integer overflow), which is how a
// third-review-round reproduction (by_seconds: 36028797018963968) got a
// 204 without actually advancing the clock at all instead of a clear
// rejection. ~292 years — far past any legitimate use of this endpoint.
const maxAdvanceSeconds = int64(math.MaxInt64) / int64(time.Second)

// handleClockAdvance calls physics.Runner.FastForward, which owns its
// own exclusive drive-lock for the whole request (physics.Runner.
// TryAcquireDrive/driveMu) — no separate suspend/resume needed here.
// FastForward returns physics.ErrClockBusy, mapped to 409 (not 400: this
// is a real conflict with another legitimate in-flight operation — a
// running scenario or a concurrent advance — not a malformed request),
// rather than blocking or silently interleaving with it.
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
	if *req.BySeconds > maxAdvanceSeconds {
		writeError(w, http.StatusBadRequest, "invalid_request", fmt.Errorf("by_seconds %d exceeds the maximum representable duration (%d seconds)", *req.BySeconds, maxAdvanceSeconds))
		return
	}
	total := time.Duration(*req.BySeconds) * time.Second

	if err := s.cfg.PhysicsRunner.FastForward(total, s.cfg.StepInterval); err != nil {
		if errors.Is(err, physics.ErrClockBusy) {
			writeError(w, http.StatusConflict, "clock_busy", err)
			return
		}
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
	rev := s.cfg.Store.CurrentRevision()
	s.events.publish("reset", s.cfg.Clock.Now(), &rev, map[string]any{})
	writeNoContent(w)
}

// doReset returns every piece of state this package coordinates to
// exactly what it was right after the simulator finished its own
// startup initialization — atomically, at the application level, with
// respect to physics ticks and every other mutating API/protocol action
// (AGENT-TASK.md, Task 7 item 7).
//
//  1. Stop the scenario runner and rewind its cursor, keeping its
//     lifecycle (Load/Start/Stop/ResetPlayback) locked until this whole
//     function returns (scenario.Runner.LockForReset).
//  2. Claim LinkCoordinator for the rest of this function — see below.
//  3. Rewind the shared clock (always a *clock.Fake — see main.go) to
//     StartupInstant.
//  4. Rebuild physics.Runner's Engine from scratch (same params, same
//     initial SoC, same RNG seed — NewEngine, not a new random one) and
//     rebase its dt baseline to StartupInstant.
//  5. Reset commands.Processor's own internal bookkeeping (watchdog
//     timer, safe_state_after latch, diagnostics).
//  6. Restore every Store value to StartupSnapshot as one atomic
//     operation (store.Store.Restore).
//  7. Clear every active link fault on both protocol servers, as one
//     linkfault.Apply(..., ModeClear) call.
//  8. Fence the heartbeat (fifth review round — see FenceHeartbeat's own
//     doc comment), *after* releasing every lock steps 2-7 ran under:
//     wait for every heartbeat frame admitted before step 7's clear to
//     actually be sent, so this function's own 204 can't complete before
//     that happens, without holding LinkCoordinator or Gate.Exclusive
//     across a connection's own transport write to do it.
//
// Steps 1-7 (resetLocked) run inside one s.cfg.Gate.Exclusive() section
// (package appgate): every ordinary mutation elsewhere (commands.
// Processor.Write, faults.Injector.Inject/Clear, physics.Runner.Tick/
// TickOnce — every one of them takes the gate's shared side) is blocked
// from starting until resetLocked returns, and resetLocked can't start
// until every currently in-flight one has finished. Reviewed gap this
// closes: the previous version ran its six steps with no lock spanning
// all of them, so a write or a tick could land between two steps and
// observe/produce neither the pre- nor the post-reset state.
//
// LinkCoordinator is held for the whole of resetLocked, not just step 7 —
// third-review-round fix: a heartbeat_pause request's own "read the live
// heartbeat, then Apply" sequence (linkfault.ApplyCoordinated) needs to
// be excluded from the *entire* window during which this function might
// change the heartbeat's Store value (step 6) and clear the link servers'
// own frozen-value bookkeeping (step 7) — holding the coordinator for
// only step 7 would leave a heartbeat_pause free to capture a stale
// pre-reset value in the gap right after step 6 restores the Store but
// before step 7 clears the link servers, producing a pause that survives
// the reset with a value the just-reset Store no longer has.
func (s *Server) doReset() error {
	fc, ok := s.cfg.Clock.(*clock.Fake)
	if !ok {
		return fmt.Errorf("controlapi: reset requires a *clock.Fake shared clock, got %T", s.cfg.Clock)
	}

	// Stop the scenario runner first, *outside* the exclusive gate
	// section below — LockForReset blocks until the run() goroutine has
	// actually exited (scenario.Runner.Stop's guarantee), and that
	// goroutine's own in-flight TickOnce/Write/Inject call needs the
	// gate's shared side to complete. Acquiring the exclusive side first
	// would deadlock: this function holding it and waiting on the
	// goroutine, the goroutine blocked acquiring the shared side this
	// function is holding exclusively.
	//
	// Unlike the ResetPlayback this used to call, LockForReset keeps the
	// scenario lifecycle locked (not just stopped) until unlockScenario
	// runs — closing a reviewed gap where a POST /scenario/start racing
	// in right after the stop, but before the rest of this function had
	// finished resetting physics/Store/link state, could start a new run
	// against state still being reset out from under it.
	unlockScenario := s.cfg.ScenarioRunner.LockForReset()
	defer unlockScenario()

	if err := s.resetLocked(fc); err != nil {
		return err
	}
	// Fifth-review-round fix: fencing runs *after* resetLocked has
	// already released both LinkCoordinator and Gate.Exclusive (it
	// returned, so its own defers already ran) — never while either is
	// still held, for the identical reason handleLink/handleLinkClear
	// fence only after ApplyCoordinated returns: waiting here can take as
	// long as a connection's own slow transport write, and neither lock
	// may be held across that. This still gives POST /reset the same
	// cutoff guarantee a plain heartbeat_pause gets: the 204 response
	// only completes once every heartbeat frame admitted before this
	// reset's own clear (step 7 below) has actually been sent, so a
	// client can never observe a stale, pre-reset heartbeat frame arrive
	// after the reset was already reported complete.
	s.cfg.IECServer.FenceHeartbeat()
	s.cfg.ModbusServer.FenceHeartbeat()
	return nil
}

// resetLocked is doReset's own locked section, split out so fencing
// (above) can run after both locks below are released — see doReset's
// own comment on why. Everything about the atomicity guarantee this used
// to provide as part of doReset itself is unchanged: LinkCoordinator and
// Gate.Exclusive are still both held for this whole sequence, in the
// same order, for the same reasons described where doReset's own doc
// comment used to carry this detail.
func (s *Server) resetLocked(fc *clock.Fake) error {
	s.cfg.LinkCoordinator.Lock()
	defer s.cfg.LinkCoordinator.Unlock()

	release := s.cfg.Gate.Exclusive()
	defer release()

	fc.Set(s.cfg.StartupInstant)
	s.cfg.PhysicsRunner.Reset(s.cfg.NewEngine(), s.cfg.StartupInstant)
	s.cfg.Processor.Reset()
	s.cfg.Store.Restore(s.cfg.StartupSnapshot)
	// linkfault.Apply, not ApplyCoordinated — LinkCoordinator is already
	// held for this whole function (above); calling ApplyCoordinated here
	// would try to re-lock the same, non-reentrant mutex.
	if err := linkfault.Apply(s.cfg.IECServer, s.cfg.ModbusServer, linkfault.ProtocolBoth, linkfault.ModeClear, 0, 0); err != nil {
		return fmt.Errorf("controlapi: reset: clearing link faults: %w", err)
	}
	return nil
}
