package iec104

import (
	"github.com/SterneStehen/petz-m261-tooling/gen/go/m261points"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/linkfault"
)

// heartbeatQueueSize is generous relative to how often a heartbeat frame
// can actually be admitted (at most once per physics tick, and pauses/
// clears are rare, deliberate admin actions, not a hot path) — this is
// not meant to survive a sustained flood, only to give
// bumpHeartbeatGeneration's own connMu-held enqueue loop (below) enough
// slack that it practically never blocks on a healthy connection's own
// queue filling up.
const heartbeatQueueSize = 256

// hbMsg is one item in a connection's own heartbeat outbound queue —
// this package's fifth-review-round fix for Task 7 item 2's
// heartbeat_pause. Either a value already resolved for delivery (asdu,
// pre-built so every connection sends byte-identical frames without
// redoing the encoding per connection), or a barrier: a marker that,
// once dequeued, closes ack — see bumpHeartbeatGeneration/FenceHeartbeat.
type hbMsg struct {
	asdu      []byte
	isBarrier bool
	ack       chan struct{}
}

// admitHeartbeat is spontaneousLoop's heartbeat-specific admission step,
// replacing the previous design's "resolve paused?, then broadcast"
// sequence (fourth review round) — the gap between resolving and the
// actual socket write in broadcast was exactly the residual race the
// fifth review round's blocker 2 named: "a frame can also be classified
// as allowed, then pause can finish, and that old frame can be sent
// afterward."
//
// Holds linkCoord for the whole admission decision *and* the enqueue
// into every connection's own hbQueue (not just the decision) —
// deliberately, so a concurrent SetHeartbeatPause/ClearLinkFaults (which
// also runs under linkCoord, from within Apply/ApplyCoordinated) can
// never observe this admission as only half-done: either this value is
// fully enqueued into every connection ahead of that call's own barrier
// (bumpHeartbeatGeneration, called from within SetHeartbeatPause/
// ClearLinkFaults itself, under the same linkCoord hold), or this whole
// admission hasn't started yet and will correctly see the new, paused
// state instead. This is safe to hold linkCoord across (unlike waiting
// for a fence) because every step here is memory-only — a channel send
// into a generously-sized buffer, never a socket write.
//
// A paused heartbeat Change is discarded permanently right here, before
// ever reaching any connection's queue — Task 7 item 2's own model:
// events generated during a paused generation don't exist as far as any
// client is concerned, clear or no clear.
//
// Also suppresses admitting a value identical to the last one actually
// admitted (lastHeartbeatAdmitted) — closing the specific "paused-era
// events are still emitted as duplicates after clear" gap the review
// named: several store.Change events queued back-to-back while paused
// (each with a different, now-superseded stale payload) all resolve to
// the *same* current live value once finally processed after a clear —
// correct in what they'd report (round four's fix), but each one was
// still a separate, redundant admission of a value the client had
// already been sent. The real heartbeat point only ever increments, so a
// duplicate here can only be an artifact of paused-era backlog draining,
// never a legitimate repeated live reading being dropped.
func (s *Server) admitHeartbeat() {
	s.linkCoord.Lock()
	defer s.linkCoord.Unlock()

	_, paused := s.link.heartbeatOverride()
	if paused {
		return
	}
	live, _ := s.store.Get(linkfault.HeartbeatKey)
	if s.haveLastHeartbeatAdmitted && s.lastHeartbeatAdmitted == live {
		return
	}
	meta := m261points.Points[linkfault.HeartbeatKey]
	asdu := monitoredASDU(meta, live, cotSpontaneous)
	if asdu == nil {
		return // unreachable in practice (the heartbeat point is telemetry, always monitored) -- defensive, matching every other monitoredASDU-nil check in this package
	}
	s.lastHeartbeatAdmitted, s.haveLastHeartbeatAdmitted = live, true

	s.connMu.Lock()
	defer s.connMu.Unlock()
	for c := range s.conns {
		select {
		case c.hbQueue <- hbMsg{asdu: asdu}:
		default:
			// The queue is full -- heartbeatQueueSize's own doc comment:
			// this connection isn't keeping up (a very slow/stuck peer).
			// Dropping this one admission rather than blocking connMu (and
			// therefore every other connection's own accept/close/link
			// bookkeeping) behind it is the same trade-off broadcast's own
			// design already makes for every other spontaneous point.
		}
	}
}

// bumpHeartbeatGeneration is SetHeartbeatPause/ClearLinkFaults' own
// fencing step (see linkstate.go) — enqueues a fresh barrier into every
// currently open connection's hbQueue and records its ack channel for a
// later FenceHeartbeat call to wait on. Must be called while linkCoord is
// already held (both callers only ever run from within Apply/
// ApplyCoordinated's own coord.Lock hold) — this is what gives the
// barrier its ordering guarantee: any admitHeartbeat call that already
// finished enqueueing before this one started is fully represented by
// items sitting ahead of this barrier in every connection's queue (FIFO),
// and any admitHeartbeat call that hasn't started yet will see the new
// paused/cleared state once it does. Doesn't itself wait for anything —
// see FenceHeartbeat for why that must happen outside this hold.
func (s *Server) bumpHeartbeatGeneration() {
	s.connMu.Lock()
	acks := make([]chan struct{}, 0, len(s.conns))
	for c := range s.conns {
		ack := make(chan struct{})
		select {
		case c.hbQueue <- hbMsg{isBarrier: true, ack: ack}:
			acks = append(acks, ack)
		default:
			// Queue full (see admitHeartbeat) -- this connection's own
			// backlog is already being dropped, not delivered in order
			// FenceHeartbeat could meaningfully wait on; nothing to fence.
		}
	}
	s.connMu.Unlock()

	s.fenceMu.Lock()
	s.pendingFenceAcks = append(s.pendingFenceAcks, acks...)
	s.fenceMu.Unlock()
}

// FenceHeartbeat implements linkfault.Target — see its own doc comment
// for the contract every caller (controlapi, scenario.Runner) must
// follow: call this only *after* releasing linkCoord, never while still
// holding it, since a slow connection's own transport write can make
// this block for as long as that write takes, and linkCoord must never
// be held across that (see bumpHeartbeatGeneration's own doc comment).
//
// Waits on every ack accumulated since the last call, not just the ones
// from the very latest SetHeartbeatPause/ClearLinkFaults — if two such
// calls landed back to back before FenceHeartbeat was next invoked
// (possible: ApplyCoordinated releases linkCoord before its caller gets
// around to fencing, so a second, unrelated link operation could acquire
// it and run its own bump first), waiting on the newer barrier alone
// would still be correct on its own terms, but draining *all*
// accumulated acks here is simpler to reason about and strictly safe:
// every one of them is guaranteed to close eventually (hbLoop always
// processes a barrier it successfully dequeued), and waiting on a few
// extra, already-satisfied ones costs nothing once they're closed.
func (s *Server) FenceHeartbeat() {
	s.fenceMu.Lock()
	acks := s.pendingFenceAcks
	s.pendingFenceAcks = nil
	s.fenceMu.Unlock()

	for _, ack := range acks {
		<-ack
	}
}

// hbLoop is this connection's own outbound heartbeat actor — the "per-
// connection outbound actor/queue with generation-tagged frames and a
// barrier/ack command" the fifth review round asked for. Runs until
// hbQueue is closed (handleConn's own cleanup, once this connection is
// gone). Strictly sequential: a barrier can only be dequeued (and its
// ack closed) after every value ahead of it in the queue has already
// been handed to sendIfStarted, which is exactly the guarantee
// FenceHeartbeat's callers rely on.
func (c *clientConn) hbLoop() {
	defer c.srv.wg.Done()
	for msg := range c.hbQueue {
		if msg.isBarrier {
			close(msg.ack)
			continue
		}
		if testBeforeHeartbeatSend != nil {
			testBeforeHeartbeatSend()
		}
		c.sendIfStarted(msg.asdu) //nolint:errcheck // a slow/dead peer just misses this update, same as broadcast's own convention
	}
}

// testBeforeHeartbeatSend, when non-nil, runs from within hbLoop right
// before an already-dequeued, already-admitted heartbeat value is handed
// to sendIfStarted — the exact seam a test needs to hold open to prove
// FenceHeartbeat genuinely waits for an in-flight frame rather than
// racing it (see server_test.go's
// TestFenceHeartbeatWaitsForAnAlreadyAdmittedFrameBeforePauseCompletes).
// Always nil in production; never set outside a test.
var testBeforeHeartbeatSend func()
