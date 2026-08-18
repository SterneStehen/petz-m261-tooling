package modbustcp

import (
	"sync"
	"time"

	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/linkfault"
)

// linkState holds this Server's currently active link fault (Task 7 item
// 2) — at most one of drop/hang/delay is meaningful at a time in
// practice (they're independent switches, not mutually exclusive, but
// combining them has no documented meaning and callers are expected to
// pick one), plus heartbeat_pause, which is genuinely independent of the
// other three and can be combined with any of them. Guarded by its own
// mutex, separate from Server.connMu, so link-fault control never
// contends with the accept/close bookkeeping it also influences.
type linkState struct {
	mu    sync.Mutex
	drop  bool
	hang  bool
	delay time.Duration

	hbFrozen bool
	hbValue  float64
}

func (l *linkState) dropped() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.drop
}

func (l *linkState) hanging() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.hang
}

func (l *linkState) responseDelay() time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.delay
}

// heartbeatOverride reports whether the heartbeat point's value should be
// overridden and, if so, what with — the caller substitutes this for the
// live Store value only at the one point identified by heartbeatKey.
func (l *linkState) heartbeatOverride() (value float64, ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.hbValue, l.hbFrozen
}

// SetDrop implements linkfault.Target: refuses new connections (checked
// in acceptLoop) and force-closes every connection already open,
// immediately — not lazily on their next request.
func (s *Server) SetDrop() {
	s.link.mu.Lock()
	s.link.drop = true
	s.link.mu.Unlock()

	s.connMu.Lock()
	for c := range s.conns {
		c.Close() //nolint:errcheck // best-effort; handleConn's own defer already tolerates this
	}
	s.connMu.Unlock()
}

// SetHang implements linkfault.Target: connections stay open, but no
// request on them — existing or new — gets a response until cleared.
func (s *Server) SetHang() {
	s.link.mu.Lock()
	s.link.hang = true
	s.link.mu.Unlock()
}

// SetDelay implements linkfault.Target: every response is sent, but only
// after sleeping d first — a real, synchronous time.Sleep at the I/O
// layer, not business logic making a decision based on time (AGENT-TASK
// §1.5 restricts the latter, not "a network handler introducing latency
// on purpose"), the same category as physics.Runner.Run's real ticker.
func (s *Server) SetDelay(d time.Duration) {
	s.link.mu.Lock()
	s.link.delay = d
	s.link.mu.Unlock()
}

// SetHeartbeatPause implements linkfault.Target: freezes this protocol's
// view of the heartbeat point at frozenValue (the live value at the
// instant the caller activated the mode — see linkfault.Apply) until
// cleared; the increment continues underneath in the Store the whole
// time, this protocol's clients just stop seeing it move.
func (s *Server) SetHeartbeatPause(frozenValue float64) {
	s.link.mu.Lock()
	s.link.hbFrozen = true
	s.link.hbValue = frozenValue
	s.link.mu.Unlock()
}

// ClearLinkFaults implements linkfault.Target: removes every active mode
// immediately and deterministically — no timeout, no partial clear, no
// compensation for time spent in heartbeat_pause (the counter simply
// resumes reporting the live, already-advanced Store value).
func (s *Server) ClearLinkFaults() {
	s.link.mu.Lock()
	s.link.drop = false
	s.link.hang = false
	s.link.delay = 0
	s.link.hbFrozen = false
	s.link.hbValue = 0
	s.link.mu.Unlock()
}

func (s *Server) ActiveLinkFaults() []linkfault.Mode {
	s.link.mu.Lock()
	defer s.link.mu.Unlock()
	var modes []linkfault.Mode
	if s.link.drop {
		modes = append(modes, linkfault.ModeDrop)
	}
	if s.link.hang {
		modes = append(modes, linkfault.ModeHang)
	}
	if s.link.delay > 0 {
		modes = append(modes, linkfault.ModeDelay)
	}
	if s.link.hbFrozen {
		modes = append(modes, linkfault.ModeHeartbeatPause)
	}
	return modes
}

// FenceHeartbeat implements linkfault.Target as a no-op — Modbus has no
// outbound push to fence in the first place: every FC03/FC04 response
// resolves heartbeatOverride fresh, at the moment of that specific
// request (see handleReadRegisters), so there is never an
// already-admitted-but-not-yet-sent frame that a concurrent
// SetHeartbeatPause/ClearLinkFaults could race against. See
// linkfault.Target's own doc comment for why iec104.Server, the one
// protocol with a background spontaneous-transmission push, needs a real
// implementation instead.
func (s *Server) FenceHeartbeat() {}
