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
	s.values[key] = value
	changed := []Change{{Key: key, Value: value}}
	if rbKey, ok := s.readbackOf[key]; ok {
		s.values[rbKey] = value
		changed = append(changed, Change{Key: rbKey, Value: value})
	}
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
	changed := make([]Change, 0, len(snapshot))
	for k, v := range snapshot {
		if cur, ok := s.values[k]; !ok || cur != v {
			s.values[k] = v
			changed = append(changed, Change{Key: k, Value: v})
		}
	}
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
	changed := make([]Change, 0, len(writes)*2)
	for _, kv := range writes {
		s.values[kv.Key] = kv.Value
		changed = append(changed, Change{Key: kv.Key, Value: kv.Value})
		if rbKey, ok := s.readbackOf[kv.Key]; ok {
			s.values[rbKey] = kv.Value
			changed = append(changed, Change{Key: rbKey, Value: kv.Value})
		}
	}
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
