// Package store holds the current value of every M261 point behind a
// single thread-safe map, addressable by either protocol's native
// addressing scheme. Both the IEC-104 and Modbus TCP servers (Task 4)
// read and write through the same Store, so a write via one protocol is
// visible via the other — that's the whole point of a shared store rather
// than one per server.
package store

import (
	"sync"

	"github.com/SterneStehen/petz-m261-tooling/gen/go/m261points"
)

// Change describes one point's value changing, delivered to subscribers
// for spontaneous transmission (IEC-104) or any other change-driven use.
type Change struct {
	Key   m261points.PointKey
	Value float64
	// Rev is the Store's own monotonic revision as of the mutation that
	// produced this Change — sixth-review-round addition. Every Set/
	// SetBatch/Restore call increments one global counter exactly once
	// and stamps that single value onto every Change it publishes (a
	// setpoint's own value plus its readback twin, or every point in one
	// batch/restore) — a strictly later call always produces a strictly
	// larger Rev, comparable across every key, not just within one.
	//
	// This is what lets a subscriber establish "was this specific event
	// generated before or after some later cutoff", something checking
	// current state at *processing* time cannot do on its own: a Change
	// can sit in a buffered subscriber channel for a while before it's
	// actually processed, and by then "current state" may already
	// reflect a transition that happened *after* this Change was
	// generated but *before* it was processed — re-deriving a fresh
	// "current" value at that point silently launders a stale,
	// superseded event into what looks like a brand new one. See
	// iec104.Server.admitHeartbeat for the concrete case this closes: a
	// heartbeat event still queued when heartbeat_pause activates or
	// clears must be identifiable as belonging to that now-superseded
	// generation even once a subscriber finally processes it afterward,
	// not reinterpreted as a fresh, current-value event.
	Rev uint64
}

// ChangeBatch is one indivisible Store mutation. A consumer either receives
// every change produced by Set/SetBatch/Restore or receives none of them and
// can detect that fact from Revision.
type ChangeBatch struct {
	Revision uint64
	Changes  []Change
}

// IECAddr is a point's IEC-104 address: ASDU common address (== device
// address, §4.1) plus information object address.
type IECAddr struct {
	CommonAddr int
	ObjAddr    int
}

// ModbusAddr is a point's Modbus address: Unit ID (== device address)
// plus register class (2 discrete input / 3 holding / 4 input, §2.2) plus
// register address. Class is part of the key rather than assumed unique
// across classes — this map's ranges never actually collide (10001+ /
// 30001+ / 40001+), but the store shouldn't rely on that by accident.
type ModbusAddr struct {
	UnitID  int
	Class   int
	Address int
}

const subscriberBufferSize = 64

// Store is a thread-safe holder of the current value of every catalog
// point. Values are always float64 — the logical, scale-applied
// engineering value produced by gen/go/m261points' codec (Task 3), not
// raw register bytes.
type Store struct {
	mu     sync.RWMutex
	values map[m261points.PointKey]float64
	// rev is the current Store revision — see Change.Rev's own doc
	// comment. Guarded by mu, incremented exactly once per Set/SetBatch/
	// Restore call (not once per Change — every Change one of those calls
	// publishes shares that call's single new value).
	rev uint64

	iecIndex    map[IECAddr]m261points.PointKey
	modbusIndex map[ModbusAddr]m261points.PointKey

	// readbackOf[setpoint] = its readback twin. Writing a setpoint also
	// mirrors the raw value onto this point (§3.2: IEC-104 exposes the same
	// register twice, once as a WO command point and once as its own RO
	// readback point; Task 4's acceptance needs "a Modbus write visible via
	// an IEC-104 read", which for a setpoint means the readback point since
	// WO points aren't polled in real IEC-104). This is a purely mechanical
	// mirror — range/enum validation, watchdog, and mode arbitration are
	// Task 6's job, layered on top of Store, not duplicated here.
	readbackOf map[m261points.PointKey]m261points.PointKey

	subMu     sync.Mutex
	subs      map[int]chan Change
	nextSubID int

	batchSubMu     sync.Mutex
	batchSubs      map[int]chan ChangeBatch
	nextBatchSubID int
}

// New builds a Store pre-populated (at zero) with every point in
// gen/go/m261points.Points, and both address indices.
func New() *Store {
	s := &Store{
		values:      make(map[m261points.PointKey]float64, len(m261points.Points)),
		iecIndex:    make(map[IECAddr]m261points.PointKey, len(m261points.Points)),
		modbusIndex: make(map[ModbusAddr]m261points.PointKey, len(m261points.Points)),
		readbackOf:  make(map[m261points.PointKey]m261points.PointKey),
		subs:        make(map[int]chan Change),
		batchSubs:   make(map[int]chan ChangeBatch),
	}

	byIECAddr := make(map[IECAddr]m261points.PointKey, len(m261points.Points))
	for key, meta := range m261points.Points {
		s.values[key] = 0
		iecAddr := IECAddr{CommonAddr: meta.DeviceAddr, ObjAddr: meta.IEC104Addr}
		s.iecIndex[iecAddr] = key
		byIECAddr[iecAddr] = key
		if meta.ModbusAddr != nil && meta.ModbusClass != nil {
			s.modbusIndex[ModbusAddr{UnitID: meta.DeviceAddr, Class: *meta.ModbusClass, Address: *meta.ModbusAddr}] = key
		}
	}
	for key, meta := range m261points.Points {
		if meta.ReadbackIEC104Addr == nil {
			continue
		}
		if rbKey, ok := byIECAddr[IECAddr{CommonAddr: meta.DeviceAddr, ObjAddr: *meta.ReadbackIEC104Addr}]; ok {
			s.readbackOf[key] = rbKey
		}
	}
	return s
}

// Get returns a point's current value by (device, slug) key.
func (s *Store) Get(key m261points.PointKey) (float64, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.GetLocked(key)
}

// RLock/RUnlock let a caller hold this Store's read lock across more
// than one operation — a multi-register Modbus response
// (modbustcp.handleReadBits/handleReadRegisters), or one physics tick's
// dispatch decision (commands.Processor.ResolvePower, which reads a
// dozen-plus separate setpoints across several of its own helper
// methods) — so the *whole* sequence is atomic against a concurrent
// SetBatch/Restore, not just each individual point read on its own.
// Fourth-review-round fix: SetBatch already makes a multi-point *write*
// atomic (one Store-mutex hold for the whole batch), but a *reader* that
// still called Get/GetByIEC/GetByModbus once per point (each taking and
// releasing its own brief RLock) could still observe a batch half-
// applied — this protected only the writer's own side of that race, not
// the reader's.
//
// Never call Get/GetByIEC/GetByModbus/Set/etc. while holding RLock — each
// of those takes its own RLock/Lock internally, and Go's sync.RWMutex is
// not safe for a nested RLock on the same goroutine if a writer is queued
// in between (a real deadlock risk, not hypothetical — the same class of
// bug an earlier review flagged for appgate.Gate). Use the Locked
// variants (GetLocked, GetByIECLocked, GetByModbusLocked) instead, which
// assume the caller already holds RLock and never acquire it themselves.
func (s *Store) RLock()   { s.mu.RLock() }
func (s *Store) RUnlock() { s.mu.RUnlock() }

// CurrentRevision returns the Store's current revision — see Change.Rev's
// own doc comment. Any Change published by a Set/SetBatch/Restore call
// that *starts* after this method returns is guaranteed to carry a Rev
// strictly greater than the value returned here (mu serializes the two);
// this is what lets a caller establish "any Change with Rev <= this
// snapshot was, or should be treated as if it was, already accounted for
// as of this instant" — see iec104.Server.bumpHeartbeatGeneration.
func (s *Store) CurrentRevision() uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.rev
}

// WithCurrentRevision holds the Store's own *write* lock for the
// duration of fn and returns the revision fn observed — sixth-review-
// round follow-up: CurrentRevision alone is only atomic with respect to
// Set/SetBatch/Restore *by itself*; a caller that needs some other state
// transition of its own (not a Store mutation) to be linearized against
// the revision counter — "everything before this instant is Rev <= N,
// everything after is Rev > N" — cannot get that by calling
// CurrentRevision() as a *separate* step after its own transition,
// because a Set can complete entirely in the gap between the two,
// landing at the wrong side of the boundary the caller captures a moment
// later. Running fn while still holding the write lock closes that gap:
// no Set/SetBatch/Restore can even begin (they need this same lock)
// until fn returns, so every one of them is provably either fully
// complete before fn started (Rev <= the value returned here) or entirely
// after (Rev > it) — never straddling fn's own transition.
//
// fn must never call back into the Store (Get/Set/etc. all need the same
// non-reentrant mu). See iec104.Server.applyHeartbeatTransition for the
// motivating case: a heartbeat pause/clear transition needs exactly this
// atomicity against a concurrent physics tick's own heartbeat Set, or a
// tick landing in the old two-step gap could be permanently misclassified
// as having happened on the wrong side of the transition.
func (s *Store) WithCurrentRevision(fn func(currentRev uint64)) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(s.rev)
	return s.rev
}

// GetLocked is Get's variant for a caller that already holds RLock (via
// Store.RLock) for a larger read transaction spanning more than one
// point — does not itself acquire s.mu.
func (s *Store) GetLocked(key m261points.PointKey) (float64, bool) {
	v, ok := s.values[key]
	return v, ok
}

// Set writes a point's value by (device, slug) key, mirrors it onto the
// point's readback twin if it has one, and notifies subscribers. Returns
// false if the key isn't a real point.
func (s *Store) Set(key m261points.PointKey, value float64) bool {
	s.mu.Lock()
	if _, ok := s.values[key]; !ok {
		s.mu.Unlock()
		return false
	}
	s.rev++
	rev := s.rev
	s.values[key] = value
	changed := []Change{{Key: key, Value: value, Rev: rev}}
	if rbKey, ok := s.readbackOf[key]; ok {
		s.values[rbKey] = value
		changed = append(changed, Change{Key: rbKey, Value: value, Rev: rev})
	}
	s.publishBatchLocked(ChangeBatch{Revision: rev, Changes: changed})
	s.mu.Unlock()

	for _, c := range changed {
		s.publish(c)
	}
	return true
}

// GetByIEC reads a point by its IEC-104 address.
func (s *Store) GetByIEC(addr IECAddr) (m261points.PointKey, float64, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.GetByIECLocked(addr)
}

// GetByIECLocked is GetByIEC's variant for a caller that already holds
// RLock — see GetLocked's doc comment.
func (s *Store) GetByIECLocked(addr IECAddr) (m261points.PointKey, float64, bool) {
	key, ok := s.iecIndex[addr]
	if !ok {
		return m261points.PointKey{}, 0, false
	}
	return key, s.values[key], true
}

// SetByIEC writes a point by its IEC-104 address.
func (s *Store) SetByIEC(addr IECAddr, value float64) (m261points.PointKey, bool) {
	s.mu.RLock()
	key, ok := s.iecIndex[addr]
	s.mu.RUnlock()
	if !ok {
		return m261points.PointKey{}, false
	}
	return key, s.Set(key, value)
}

// GetByModbus reads a point by its Modbus (unit, class, address).
func (s *Store) GetByModbus(addr ModbusAddr) (m261points.PointKey, float64, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.GetByModbusLocked(addr)
}

// GetByModbusLocked is GetByModbus's variant for a caller that already
// holds RLock — see GetLocked's doc comment.
func (s *Store) GetByModbusLocked(addr ModbusAddr) (m261points.PointKey, float64, bool) {
	key, ok := s.modbusIndex[addr]
	if !ok {
		return m261points.PointKey{}, 0, false
	}
	return key, s.values[key], true
}

// SetByModbus writes a point by its Modbus (unit, class, address).
func (s *Store) SetByModbus(addr ModbusAddr, value float64) (m261points.PointKey, bool) {
	s.mu.RLock()
	key, ok := s.modbusIndex[addr]
	s.mu.RUnlock()
	if !ok {
		return m261points.PointKey{}, false
	}
	return key, s.Set(key, value)
}

// Snapshot returns every point's current value — used by IEC-104 general
// interrogation and anything else that needs the full picture at once.
func (s *Store) Snapshot() map[m261points.PointKey]float64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[m261points.PointKey]float64, len(s.values))
	for k, v := range s.values {
		out[k] = v
	}
	return out
}

// SnapshotWithRevision captures values and the Store revision under one read
// lock. Consumers must use this rather than Snapshot followed by
// CurrentRevision when establishing a UI synchronisation point.
func (s *Store) SnapshotWithRevision() (map[m261points.PointKey]float64, uint64) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.snapshotLocked(), s.rev
}

func (s *Store) snapshotLocked() map[m261points.PointKey]float64 {
	out := make(map[m261points.PointKey]float64, len(s.values))
	for k, v := range s.values {
		out[k] = v
	}
	return out
}

// SnapshotDevice returns every point's current value for one device (one
// IEC-104 common address / Modbus Unit ID) — general interrogation is
// scoped per common address (Task 4: "correctly handle multiple ASDU
// common addresses").
func (s *Store) SnapshotDevice(deviceAddr int) map[m261points.PointKey]float64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[m261points.PointKey]float64)
	for k, v := range s.values {
		if meta, ok := m261points.Points[k]; ok && meta.DeviceAddr == deviceAddr {
			out[k] = v
		}
	}
	return out
}

// Restore replaces every point's value with snapshot's as one atomic
// operation with respect to concurrent Get/Set/GetByIEC/etc — no caller
// ever observes a partially restored store (Task 7 item 7: reset must be
// atomic relative to ticks and other API/protocol actions). snapshot is
// normally a copy of Snapshot() taken once, right after the simulator
// finished its own startup initialization (commands.Processor's sensible
// defaults and physics.Runner's initial publish already applied) — this
// is the single source of truth for "the state right after process
// start", rather than re-deriving defaults through logic a future change
// could drift out of sync with. Only publishes a Change for points whose
// value actually differs from before the restore, so IEC-104 spontaneous
// transmission reflects the reset like any other write without spamming
// unchanged points. Keys present in the live store but absent from
// snapshot are left untouched — callers are expected to pass a full
// Snapshot(), which by construction has one entry per point already.
func (s *Store) Restore(snapshot map[m261points.PointKey]float64) {
	s.mu.Lock()
	s.rev++
	rev := s.rev
	changed := make([]Change, 0, len(snapshot))
	for k, v := range snapshot {
		if cur, ok := s.values[k]; !ok || cur != v {
			s.values[k] = v
			changed = append(changed, Change{Key: k, Value: v, Rev: rev})
		}
	}
	s.publishBatchLocked(ChangeBatch{Revision: rev, Changes: changed})
	s.mu.Unlock()

	for _, c := range changed {
		s.publish(c)
	}
}

// KeyValue is one (point, value) pair for SetBatch.
type KeyValue struct {
	Key   m261points.PointKey
	Value float64
}

// SetBatch writes every pair in writes (and mirrors each onto its
// readback twin, same as Set) as one atomic operation with respect to
// concurrent Get/Set/Snapshot/etc — no concurrent reader ever observes
// only some of a multi-point batch applied, mirroring Restore's own
// atomicity for the same reason: a multi-register Modbus FC16 request
// spanning more than one catalog point must commit as one indivisible
// step, not several independently-locked Set calls (third review round:
// a concurrent Snapshot — even one already excluded from racing a
// reset's own Store.Restore by appgate.Gate — could still observe a
// batch partially applied, since gate.Op is a *shared* lock and doesn't
// serialize two concurrent Op holders against each other; only Store's
// own mutex, held for the whole batch, closes that). Returns false
// without applying any of the batch if any key isn't a real point —
// every production caller (commands.Processor.WriteBatch) already
// validates each one beforehand, so this is a caller-bug guard, not a
// partial-application path.
func (s *Store) SetBatch(writes []KeyValue) bool {
	s.mu.Lock()
	for _, kv := range writes {
		if _, ok := s.values[kv.Key]; !ok {
			s.mu.Unlock()
			return false
		}
	}
	s.rev++
	rev := s.rev
	changed := make([]Change, 0, len(writes)*2)
	for _, kv := range writes {
		s.values[kv.Key] = kv.Value
		changed = append(changed, Change{Key: kv.Key, Value: kv.Value, Rev: rev})
		if rbKey, ok := s.readbackOf[kv.Key]; ok {
			s.values[rbKey] = kv.Value
			changed = append(changed, Change{Key: rbKey, Value: kv.Value, Rev: rev})
		}
	}
	s.publishBatchLocked(ChangeBatch{Revision: rev, Changes: changed})
	s.mu.Unlock()

	for _, c := range changed {
		s.publish(c)
	}
	return true
}

// Subscribe returns a channel of every future Change plus an unsubscribe
// function. The channel is buffered and best-effort: a subscriber that
// falls behind has changes dropped rather than blocking writers — general
// interrogation is the catch-up mechanism for a slow/reconnecting client.
func (s *Store) Subscribe() (<-chan Change, func()) {
	s.subMu.Lock()
	id := s.nextSubID
	s.nextSubID++
	ch := make(chan Change, subscriberBufferSize)
	s.subs[id] = ch
	s.subMu.Unlock()

	unsubscribe := func() {
		s.subMu.Lock()
		if ch, ok := s.subs[id]; ok {
			delete(s.subs, id)
			close(ch)
		}
		s.subMu.Unlock()
	}
	return ch, unsubscribe
}

// SubscribeBatches returns future mutations as atomic batches. Its bounded,
// best-effort queue drops a whole batch when full; it never exposes a subset
// of one mutation. This is deliberately independent from Subscribe(), whose
// per-point contract is retained for IEC-104 spontaneous transmission.
func (s *Store) SubscribeBatches() (<-chan ChangeBatch, func()) {
	s.batchSubMu.Lock()
	defer s.batchSubMu.Unlock()
	return s.subscribeBatchesLocked()
}

// SubscribeBatchesWithSnapshot atomically registers the batch subscriber and
// captures a snapshot/revision. A mutation is therefore either represented in
// the returned snapshot or queued after it, never lost between the two.
func (s *Store) SubscribeBatchesWithSnapshot() (<-chan ChangeBatch, map[m261points.PointKey]float64, uint64, func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.batchSubMu.Lock()
	defer s.batchSubMu.Unlock()
	var ch <-chan ChangeBatch
	var unsubscribe func()
	ch, unsubscribe = s.subscribeBatchesLocked()
	return ch, s.snapshotLocked(), s.rev, unsubscribe
}

// subscribeBatchesLocked is called with batchSubMu held. The Store mutex is
// optionally held too by SubscribeBatchesWithSnapshot (in that order).
func (s *Store) subscribeBatchesLocked() (<-chan ChangeBatch, func()) {
	id := s.nextBatchSubID
	s.nextBatchSubID++
	ch := make(chan ChangeBatch, subscriberBufferSize)
	s.batchSubs[id] = ch
	return ch, func() {
		s.batchSubMu.Lock()
		if existing, ok := s.batchSubs[id]; ok {
			delete(s.batchSubs, id)
			close(existing)
		}
		s.batchSubMu.Unlock()
	}
}

func (s *Store) publish(c Change) {
	s.subMu.Lock()
	defer s.subMu.Unlock()
	for _, ch := range s.subs {
		select {
		case ch <- c:
		default:
		}
	}
}

// publishBatchLocked queues the complete batch while the mutation still owns
// s.mu. This is the ordering primitive used by the SSE bootstrap.
func (s *Store) publishBatchLocked(batch ChangeBatch) {
	s.batchSubMu.Lock()
	defer s.batchSubMu.Unlock()
	for _, ch := range s.batchSubs {
		copyChanges := append([]Change(nil), batch.Changes...)
		select {
		case ch <- ChangeBatch{Revision: batch.Revision, Changes: copyChanges}:
		default:
		}
	}
}
