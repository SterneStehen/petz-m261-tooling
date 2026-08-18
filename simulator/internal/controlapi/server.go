// Package controlapi implements Task 7 item 3's control HTTP API — the
// one component of the whole simulator with no counterpart in the real
// M261 hardware at all (AGENT-TASK.md, Task 7 item 3): state
// inspection, fault injection, link-fault control, scenario load/start/
// stop, clock fast-forward, and reset.
package controlapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/SterneStehen/petz-m261-tooling/gen/go/m261points"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/appgate"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/clock"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/commands"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/faults"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/linkfault"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/physics"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/scenario"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/store"
	"github.com/SterneStehen/petz-m261-tooling/webui"
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
	Clock          clock.Clock // always a *clock.Fake — see main.go's single-clock wiring
	StepInterval   time.Duration
	ScenarioRunner *scenario.Runner
	IECServer      linkfault.Target
	ModbusServer   linkfault.Target
	ScenariosDir   string // POST /scenario/load {"name": "<file in scenarios/>"}

	// Gate is the process-wide reset-atomicity lock (package appgate) —
	// doReset acquires it exclusively for its whole sequence; every
	// ordinary write elsewhere (commands.Processor.Write,
	// faults.Injector.Inject/Clear, physics.Runner.Tick/TickOnce) takes
	// its shared side. Required — a nil Gate makes doReset's atomicity
	// guarantee (AGENT-TASK.md, Task 7 item 7) silently vacuous.
	Gate *appgate.Gate

	// LinkCoordinator serializes every link-fault operation (POST /link,
	// POST /link/clear, a scenario's link: step, and doReset's own clear)
	// against each other and against a heartbeat_pause's own heartbeat
	// capture — see linkfault.Coordinator's doc comment for the races
	// this closes. Shared with scenario.Runner and both protocol servers
	// (main.go wires the same instance everywhere).
	LinkCoordinator *linkfault.Coordinator

	// Reset support (Task 7 item 7) — StartupSnapshot is a
	// store.Store.Snapshot() taken once, before either protocol listener
	// opens (so no client-visible write can land in it — see main.go);
	// NewEngine rebuilds a fresh physics.Engine with the exact params/
	// initial SoC/RNG seed the simulator started with. StartupInstant is
	// the Clock value at that same moment.
	StartupSnapshot map[m261points.PointKey]float64
	NewEngine       func() *physics.Engine
	StartupInstant  time.Time

	// PublicConfig is the already-resolved subset of configuration the web
	// console may display. Each entry retains its source unconfirmed flag.
	PublicConfig map[string]any
	// Ready reports whether all simulator listeners are available.
	Ready func() bool
}

// Server is the control API's HTTP server.
type Server struct {
	cfg          Config
	validDevices map[string]bool
	ln           net.Listener
	hs           *http.Server
	events       *eventHub
	listening    atomic.Bool
}

func New(cfg Config) *Server {
	s := &Server{cfg: cfg, validDevices: make(map[string]bool), events: newEventHub(256)}
	for key := range m261points.Points {
		s.validDevices[key.Device] = true
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/state", s.handleState)
	mux.HandleFunc("/faults", s.handleFaults)
	mux.HandleFunc("/faults/{device}/{point}", s.handleFaultByPath)
	mux.HandleFunc("/link", s.handleLink)
	mux.HandleFunc("/link/clear", s.handleLinkClear)
	mux.HandleFunc("/scenario/load", s.handleScenarioLoad)
	mux.HandleFunc("/scenario/start", s.handleScenarioStart)
	mux.HandleFunc("/scenario/stop", s.handleScenarioStop)
	mux.HandleFunc("/clock/advance", s.handleClockAdvance)
	mux.HandleFunc("/reset", s.handleReset)
	mux.HandleFunc("/api/v1/catalog", s.handleV1Catalog)
	mux.HandleFunc("/api/v1/status", s.handleV1Status)
	mux.HandleFunc("/api/v1/state", s.handleV1State)
	mux.HandleFunc("/api/v1/commands", s.handleV1Commands)
	mux.HandleFunc("/api/v1/scenarios", s.handleV1Scenarios)
	mux.HandleFunc("/api/v1/scenario/status", s.handleV1ScenarioStatus)
	mux.HandleFunc("/api/v1/diagnostics", s.handleV1Diagnostics)
	mux.HandleFunc("/api/v1/events", s.handleV1Events)
	mux.HandleFunc("/api/v1/demo/prepare", s.handleV1DemoPrepare)
	mux.HandleFunc("/api/v1/health/live", s.handleV1Live)
	mux.HandleFunc("/api/v1/health/ready", s.handleV1Ready)
	mux.HandleFunc("/api/", s.handleNotFound) // API typos must never fall through to the SPA shell.
	mux.HandleFunc("/", s.handleWebUI)

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
	s.listening.Store(true)
	go s.hs.Serve(ln) //nolint:errcheck // Close's ln.Close() is what ends Serve; the resulting ErrServerClosed is expected, not a failure to report
	return nil
}

func (s *Server) Addr() net.Addr { return s.ln.Addr() }

func (s *Server) Close() error {
	s.listening.Store(false)
	return s.hs.Close()
}

func (s *Server) listeningReady() bool { return s.listening.Load() }

func (s *Server) handleNotFound(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotFound, "not_found", fmt.Errorf("no such endpoint: %s %s", r.Method, r.URL.Path))
}

// handleWebUI serves Task 10's embedded production bundle. Known static
// assets are returned as files; all other browser routes receive index.html
// so navigation remains client-side. API routes are registered before this
// catch-all and /api/ has an explicit JSON 404 above.
func (s *Server) handleWebUI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", fmt.Errorf("%s not allowed on %s", r.Method, r.URL.Path))
		return
	}
	bundle, err := fs.Sub(webui.Assets, "dist")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "webui_unavailable", err)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/")
	if path == "" {
		path = "index.html"
	}
	if _, err := fs.Stat(bundle, path); err != nil {
		path = "index.html"
	}
	if path == "index.html" {
		data, err := fs.ReadFile(bundle, path)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "webui_unavailable", err)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		http.ServeContent(w, r, path, time.Time{}, bytes.NewReader(data))
		return
	}
	served := r.Clone(r.Context())
	served.URL.Path = "/" + path
	http.FileServer(http.FS(bundle)).ServeHTTP(w, served)
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

// requireMethod writes a JSON 405 (with an Allow header, matching
// net/http's own convention for the automatic 405 this replaces) and
// returns false if r wasn't sent with method want — every handler in
// this package registers its path without a method restriction (see
// New) specifically so it can produce this JSON shape instead of net/
// http's plain-text default for a mismatched method on a known path.
func requireMethod(w http.ResponseWriter, r *http.Request, want string) bool {
	if r.Method == want {
		return true
	}
	w.Header().Set("Allow", want)
	writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", fmt.Errorf("%s not allowed on %s, want %s", r.Method, r.URL.Path, want))
	return false
}

// decodeJSON decodes exactly one JSON value from r.Body and rejects
// anything else in the body after it (a second JSON value, or trailing
// non-whitespace garbage) — without this, {"a":1}{"b":2} silently
// decodes only the first object and ignores the second rather than
// being rejected as a malformed request.
func decodeJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	var trailing json.RawMessage
	if err := dec.Decode(&trailing); err == nil {
		return fmt.Errorf("body contains more than one JSON value")
	} else if err != io.EOF {
		return err
	}
	return nil
}

// knownDevice reports whether device is a real catalog device (EMS/BMS/
// PCS/TMS/BMS_CELLS/PCS_METER) — used so a typo'd ?device= query gets a
// clear 400 instead of silently matching nothing.
func (s *Server) knownDevice(device string) bool {
	return s.validDevices[device]
}
