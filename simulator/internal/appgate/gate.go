// Package appgate provides the single cross-component lock Task 7's
// reset (AGENT-TASK.md, Task 7 item 7) needs to be atomic at the
// application level, not just within each individual component.
//
// Each component with its own internal mutex (store.Store,
// commands.Processor, physics.Runner) is already safe against concurrent
// calls to itself. That's not the same guarantee reset needs: reset
// touches four independent components (physics, commands, store, link
// state) in sequence, and without something serializing "a reset in
// progress" against "a protocol write / physics tick / fault injection
// in progress", a write or tick can land between two of those steps and
// produce a result that's neither the pre-reset nor the post-reset
// state — reviewed and confirmed as a real gap in the first version of
// Task 7's reset.
//
// Gate is a single process-wide RWMutex used inversely from the usual
// reader/writer meaning: every ordinary mutating operation (a protocol
// write, a fault injection, a physics tick) takes the *shared* side
// (Op) — any number of them may run concurrently with each other, same
// as before Gate existed — while Reset takes the *exclusive* side
// (Exclusive), which blocks until every in-flight Op has finished and
// then guarantees nothing else starts one until Reset's own critical
// section returns.
package appgate

import "sync"

type Gate struct {
	mu sync.RWMutex
}

func New() *Gate { return &Gate{} }

// Op marks the start of one ordinary mutating operation (a protocol
// write, a fault injection, a physics tick) and returns a function that
// must be called when that operation is done — normally via defer,
// immediately after obtaining it. Safe to call on a nil *Gate (no
// gating, matching the individual components' own "SetGate never
// called" no-op behavior) — callers that always hold a real Gate (every
// production path, wired in main.go) never rely on this, but it keeps a
// test harness that doesn't care about reset atomicity from needing to
// construct one just to avoid a nil pointer dereference.
func (g *Gate) Op() func() {
	if g == nil {
		return func() {}
	}
	g.mu.RLock()
	return g.mu.RUnlock
}

// Exclusive marks the start of a reset — blocks until every in-flight Op
// has finished, and blocks every Op call started after it until the
// returned function is called. Safe to call on a nil *Gate — see Op's
// doc comment; a nil Gate makes Exclusive a no-op, which for
// controlapi.Server.doReset specifically means reset is no longer
// guaranteed atomic against concurrent writes, exactly like never
// setting a Gate anywhere production-wise never happens (main.go always
// does).
func (g *Gate) Exclusive() func() {
	if g == nil {
		return func() {}
	}
	g.mu.Lock()
	return g.mu.Unlock
}
