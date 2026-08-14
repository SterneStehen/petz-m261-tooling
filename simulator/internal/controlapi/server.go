// Package controlapi implements Task 7 item 3's control HTTP API — the
// one component of the whole simulator with no counterpart in the real
// M261 hardware at all (AGENT-TASK.md, Task 7 item 3): state
// inspection, fault injection, link-fault control, scenario load/start/
// stop, clock fast-forward, and reset.
//
// The clock/scenario endpoints (POST /clock/advance, POST /scenario/*)
// only work when the simulator's single shared Clock is a *clock.Fake —
// checked per-request, not assumed — because advancing or replaying
// against clock.Real has no coherent meaning (AGENT-TASK §1.5: the
// simulator has exactly one injectable clock, business logic never calls
// time.Now() directly, and this package is business logic). Everything
// else (state, faults, link, reset) works the same regardless of clock
// mode.
package controlapi

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/SterneStehen/petz-m261-tooling/gen/go/m261points"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/clock"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/commands"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/faults"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/linkfault"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/physics"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/scenario"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/store"
)

// Config bundles every dependency the control API's handlers need.
// Everything is required except ScenariosDir (POST /scenario/load's
// {"name": ...} form is simply unavailable — 400 — if it's empty; the
// {"yaml": ...} inline form still works).
type Config struct {
	Addr string // e.g. "127.0.0.1:8081" — loopback by default (§1.3), configurable (main.go's -control-addr)

	Store          *store.Store
	Injector       *faults.Injector
	Processor      *commands.Processor
	PhysicsRunner  *physics.Runner
	Clock          clock.Clock // shared with PhysicsRunner/Processor/protocol servers
	StepInterval   time.Duration
	ScenarioRunner *scenario.Runner
	IECServer      linkfault.Target
	ModbusServer   linkfault.Target
	ScenariosDir   string // POST /scenario/load {"name": "<file in scenarios/>"}

	// Reset support (Task 7 item 7) — StartupSnapshot is a
	// store.Store.Snapshot() taken once, right after the simulator
	// finished its own startup initialization; NewEngine rebuilds a fresh
	// physics.Engine with the exact params/initial SoC/RNG seed the
	// simulator started with. StartupInstant is the Clock value at that
	// same moment — only meaningful (and only applied) when Clock is a
	// *clock.Fake; ignored otherwise, since clock.Real can't be rewound.
	StartupSnapshot map[m261points.PointKey]float64
	NewEngine       func() *physics.Engine
	StartupInstant  time.Time
}

// Server is the control API's HTTP server.
type Server struct {
	cfg Config
	ln  net.Listener
	hs  *http.Server
}

func New(cfg Config) *Server {
	s := &Server{cfg: cfg}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /state", s.handleState)
	mux.HandleFunc("POST /faults", s.handleInjectFault)
	mux.HandleFunc("DELETE /faults/{device}/{point}", s.handleClearFault)
	mux.HandleFunc("POST /link", s.handleSetLink)
	mux.HandleFunc("POST /link/clear", s.handleClearLink)
	mux.HandleFunc("POST /scenario/load", s.handleScenarioLoad)
	mux.HandleFunc("POST /scenario/start", s.handleScenarioStart)
	mux.HandleFunc("POST /scenario/stop", s.handleScenarioStop)
	mux.HandleFunc("POST /clock/advance", s.handleClockAdvance)
	mux.HandleFunc("POST /reset", s.handleReset)
	s.hs = &http.Server{Handler: mux}
	return s
}

// Start binds the listener and begins serving in the background.
func (s *Server) Start() error {
	ln, err := net.Listen("tcp", s.cfg.Addr)
	if err != nil {
		return fmt.Errorf("controlapi: listen: %w", err)
	}
	s.ln = ln
	go s.hs.Serve(ln) //nolint:errcheck // Close's ln.Close() is what ends Serve; the resulting ErrServerClosed is expected, not a failure to report
	return nil
}

func (s *Server) Addr() net.Addr { return s.ln.Addr() }

func (s *Server) Close() error {
	return s.hs.Close()
}

// --- JSON helpers -----------------------------------------------------

type errorResponse struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// writeError implements the one error shape every endpoint uses
// (AGENT-TASK.md, Task 7 item 3): {"error": {"code", "message"}}. code
// is a stable, machine-readable string a test or a caller can switch on
// — never log text.
func writeError(w http.ResponseWriter, status int, code string, err error) {
	var body errorResponse
	body.Error.Code = code
	body.Error.Message = err.Error()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body) //nolint:errcheck // response already committed; nothing left to do if this fails
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v) //nolint:errcheck
}

func writeNoContent(w http.ResponseWriter) {
	w.WriteHeader(http.StatusNoContent)
}

func decodeJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}
