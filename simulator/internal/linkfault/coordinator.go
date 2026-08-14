package linkfault

import (
	"sync"
	"time"

	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/store"
)

// Coordinator serializes every operation that reads or mutates link-fault
// state together with the heartbeat override it interacts with — POST
// /link, POST /link/clear, a scenario's link: step, POST /reset's own
// clear, and any read (IEC-104 general interrogation, a Modbus register
// read) that must resolve the heartbeat point's live-vs-frozen value
// consistently with the rest of what it reports. One shared instance is
// wired into controlapi, scenario.Runner, iec104.Server, and
// modbustcp.Server (see main.go) — this is deliberately a *different*
// lock from package appgate's Gate: Gate's Op is a shared (RLock)
// operation, so two concurrent Op holders never exclude each other,
// which is exactly why concurrent link operations could still interleave
// even after they were each wrapped in a single gate.Op (third review
// round finding). Coordinator is a plain mutex instead — every
// participant here benefits from full serialization far more than from
// any two of them running truly concurrently.
//
// Reviewed regressions this closes:
//   - protocol: both used to mutate iec104's and modbus's target state as
//     two independent, uncoordinated calls, so two concurrent link
//     operations (or POST /reset's own two ClearLinkFaults calls) could
//     interleave mid-pair, leaving (for example) IEC-104 cleared while
//     Modbus stayed dropped.
//   - heartbeat_pause used to read the Store's current heartbeat value
//     before acquiring any lock at all — a POST /reset landing between
//     that read and the eventual Apply could produce a pause holding a
//     stale pre-reset value that survives the reset (the Store's own
//     heartbeat point gets reset by Store.Restore, but the *link
//     server's* frozen copy is a separate field Restore never touches).
//   - IEC-104 general interrogation (and Modbus's equivalent per-register
//     read) used to capture the Store snapshot under one lock, release
//     it, and only afterward read the heartbeat override under a
//     *different* lock — a reset or link mutation landing in between
//     could combine pre-reset Store data with post-reset link state, or
//     vice versa.
//   - within a single SetDrop call, "set the drop flag" and "close every
//     open connection" used to be two separate steps with no lock held
//     across both — a concurrent Clear() landing between them let an
//     already-superseded drop still close connections after the clear
//     had logically already taken effect. Serializing every Apply/Clear
//     call against every other one closes this as a side effect: the two
//     steps inside one target's SetDrop implementation can no longer be
//     interrupted by another coordinator-mediated call, since that call
//     can't even start until this whole one returns.
type Coordinator struct {
	mu sync.Mutex
}

func NewCoordinator() *Coordinator { return &Coordinator{} }

// Lock/Unlock are exposed directly, not only via a Do(func()) wrapper,
// because some callers (doReset, iec104/modbustcp's own read handlers)
// need to interleave the critical section with other package-local logic
// in a way a single closure would make awkward. Safe to call on a nil
// *Coordinator (no coordination — matches every other gated type's
// SetGate-never-called convention in this codebase, for tests that don't
// care about cross-component link atomicity).
func (c *Coordinator) Lock() {
	if c == nil {
		return
	}
	c.mu.Lock()
}

func (c *Coordinator) Unlock() {
	if c == nil {
		return
	}
	c.mu.Unlock()
}

// ApplyCoordinated is Apply's coordinated counterpart, and the only
// correct way to apply a link fault outside this package: it holds coord
// for the whole operation, reads the live heartbeat value from st itself
// (only relevant for mode == ModeHeartbeatPause) *inside* that hold, and
// calls Apply — capture and apply as one atomic step, never two separate
// ones a reset or another link operation could land between. Every
// caller (controlapi's POST /link, scenario's link: step, POST /reset's
// own clear) funnels through this rather than duplicating the "read
// heartbeat, then Apply" sequence uncoordinated at each call site.
func ApplyCoordinated(coord *Coordinator, st *store.Store, iecTarget, modbusTarget Target, protocol Protocol, mode Mode, delay time.Duration) error {
	coord.Lock()
	defer coord.Unlock()
	var heartbeatValue float64
	if mode == ModeHeartbeatPause {
		heartbeatValue, _ = st.Get(HeartbeatKey)
	}
	return Apply(iecTarget, modbusTarget, protocol, mode, delay, heartbeatValue)
}
