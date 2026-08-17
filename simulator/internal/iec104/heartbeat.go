package iec104

import (
	"github.com/SterneStehen/petz-m261-tooling/gen/go/m261points"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/linkfault"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/store"
)

// heartbeatQueueSize is generous relative to how often a heartbeat frame
// can actually be admitted (at most once per physics tick, and pauses/
// clears are rare, deliberate admin actions, not a hot path) — this is
// not meant to survive a sustained flood: enqueueOrDisconnect (below)
// disconnects a connection outright once its own queue is this full,
// rather than silently losing anything, so heartbeatQueueSize's only job
// is giving a healthy connection enough slack that a brief scheduling
// delay in its own hbLoop never trips that.
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

// admitHeartbeat is spontaneousLoop's heartbeat-specific admission step
// for one already-published store.Change, c — replacing the previous
// design's "resolve paused?, then broadcast" sequence (fourth review
// round) and, sixth review round, its own successor ("resolve paused?
// and re-read the *current* live value, then broadcast" — see below for
// why that was still wrong).
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
// state (and boundary revision) instead. This is safe to hold linkCoord
// across (unlike waiting for a fence) because every step here is
// memory-only — a channel send into a generously-sized buffer, or a
// disconnect (enqueueOrDisconnect), never a blocking socket write.
//
// Sixth-review-round rewrite: this used to re-read the Store's *current*
// live heartbeat value at admission time instead of trusting c.Value —
// deliberately, to avoid replaying a stale intermediate reading. But
// store.Store.Subscribe's channel is buffered and best-effort, so c can
// easily still be sitting unprocessed when a pause activates *and later
// clears* — and re-reading "current" at that point launders c into a
// brand new, current-value event, exactly the "paused-era events are
// still emitted... after clear" bug the review named: c was generated
// during a now-superseded generation, and no amount of freshening its
// *value* changes that fact. The fix is c.Rev, not the value: c.Rev <=
// s.heartbeatGenBoundaryRev means a *later* pause-or-clear transition
// (bumpHeartbeatGeneration, called from both) already moved the boundary
// past this specific event — permanently stale, discarded here,
// regardless of what "paused" reads right now. Only once that check
// passes does this go back to using c.Value directly (never re-read),
// exactly like every other point's Change already does in
// spontaneousLoop — "do not convert an old queued event into a fresh
// current-value event."
func (s *Server) admitHeartbeat(c store.Change) {
	s.linkCoord.Lock()
	defer s.linkCoord.Unlock()

	if c.Rev <= s.heartbeatGenBoundaryRev {
		return // superseded by a later pause/clear/reset transition -- permanently stale, never delivered
	}
	if _, paused := s.link.heartbeatOverride(); paused {
		return
	}
	meta := m261points.Points[linkfault.HeartbeatKey]
	asdu := monitoredASDU(meta, c.Value, cotSpontaneous)
	if asdu == nil {
		return // unreachable in practice (the heartbeat point is telemetry, always monitored) -- defensive, matching every other monitoredASDU-nil check in this package
	}

	s.connMu.Lock()
	defer s.connMu.Unlock()
	for conn := range s.conns {
		s.enqueueOrDisconnect(conn, hbMsg{asdu: asdu})
	}
}

// enqueueOrDisconnect tries a non-blocking send of msg into conn's
// hbQueue and reports whether it succeeded. Sixth-review-round fix: on
// failure (the queue is full — this connection's own hbLoop isn't
// keeping up), disconnects conn outright instead of silently discarding
// msg. Silently dropping a *barrier* here used to make FenceHeartbeat
// return without ever having waited for this connection at all — the
// fence would report success while an old, unfenced frame could still be
// sitting queued behind the (never delivered) barrier, exactly the
// "silent barrier loss" the review forbids. A dropped plain *value*
// admission would only cost this one connection a single missed update —
// already the accepted trade-off for every other spontaneous point, via
// broadcast's own best-effort send — but this closes it the same way
// anyway, for one uniform rule rather than two different failure modes
// for what's fundamentally the same problem: a connection whose own
// hbQueue is this far behind is not a connection FenceHeartbeat can
// meaningfully wait on, so it's removed instead of silently ignored.
// Must be called while connMu is already held (both callers hold it for
// their whole enqueue loop).
func (s *Server) enqueueOrDisconnect(conn *clientConn, msg hbMsg) bool {
	select {
	case conn.hbQueue <- msg:
		return true
	default:
		conn.nc.Close() //nolint:errcheck // handleConn's own read loop notices and runs its usual cleanup (delete from s.conns, close hbQueue)
		return false
	}
}

// bumpHeartbeatGeneration is SetHeartbeatPause/ClearLinkFaults' own
// fencing step (see linkstate.go): records the current Store revision as
// the new generation boundary (admitHeartbeat's own c.Rev <=
// heartbeatGenBoundaryRev check) and enqueues a fresh barrier into every
// currently open connection's hbQueue, recording its ack channel for a
// later FenceHeartbeat call to wait on. Must be called while linkCoord is
// already held (both callers only ever run from within Apply/
// ApplyCoordinated's own coord.Lock hold) — this is what gives the
// boundary and the barrier the same ordering guarantee: any admitHeartbeat
// call that already started (and is therefore either already fully
// enqueued, or blocked waiting for this same linkCoord) is either fully
// represented ahead of this barrier in every connection's queue, or will
// see the new boundary/paused state once it gets its turn; nothing can
// straddle the two.
func (s *Server) bumpHeartbeatGeneration() {
	s.heartbeatGenBoundaryRev = s.store.CurrentRevision()

	s.connMu.Lock()
	acks := make([]chan struct{}, 0, len(s.conns))
	for conn := range s.conns {
		ack := make(chan struct{})
		if s.enqueueOrDisconnect(conn, hbMsg{isBarrier: true, ack: ack}) {
			acks = append(acks, ack)
		}
		// enqueueOrDisconnect returning false already disconnected conn --
		// nothing to fence for it; it can never receive a stale frame
		// again, so it's simply excluded rather than needing an ack.
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
// this block for as long as that write takes (bounded — see
// clientConn.withWriteDeadline — but still real time), and linkCoord must
// never be held across that (see bumpHeartbeatGeneration's own doc
// comment).
//
// Waits on every ack accumulated since the last call, not just the ones
// from the very latest SetHeartbeatPause/ClearLinkFaults — if two such
// calls landed back to back before FenceHeartbeat was next invoked
// (possible: ApplyCoordinated releases linkCoord before its caller gets
// around to fencing, so a second, unrelated link operation could acquire
// it and run its own bump first), waiting on the newer barrier alone
// would still be correct on its own terms, but draining *all*
// accumulated acks here is simpler to reason about and strictly safe:
// every one of them is guaranteed to close eventually — either hbLoop
// processes it normally, or withWriteDeadline's own timeout disconnects
// the connection outright, whose hbLoop then drains straight through any
// remaining queued items (including barriers) once its own hbQueue is
// closed — never left permanently unclosed.
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
// gone, or FenceHeartbeat/enqueueOrDisconnect's own trigger of it — see
// their own doc comments). Strictly sequential: a barrier can only be
// dequeued (and its ack closed) after every value ahead of it in the
// queue has already been handed to sendIfStarted, which is exactly the
// guarantee FenceHeartbeat's callers rely on. sendIfStarted's own write
// is bounded (clientConn.withWriteDeadline) — a peer that stops reading
// can delay this loop by at most that bound before being disconnected,
// never indefinitely.
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
		c.sendIfStarted(msg.asdu) //nolint:errcheck // a slow/dead peer just misses this update and is disconnected by withWriteDeadline; same convention as broadcast
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
