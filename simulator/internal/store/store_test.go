package store

import (
	"sync"
	"testing"

	"github.com/SterneStehen/petz-m261-tooling/gen/go/m261points"
)

func TestNewPopulatesEveryPoint(t *testing.T) {
	s := New()
	snap := s.Snapshot()
	if len(snap) != len(m261points.Points) {
		t.Fatalf("Snapshot() has %d points, want %d", len(snap), len(m261points.Points))
	}
	for k, v := range snap {
		if v != 0 {
			t.Fatalf("point %v not zero-initialized: %v", k, v)
		}
	}
}

func TestGetSetByKey(t *testing.T) {
	s := New()
	key := m261points.PointKey{Device: "EMS", Slug: "set_active_power_kw"}
	if !s.Set(key, -55.5) {
		t.Fatal("Set on a real key returned false")
	}
	v, ok := s.Get(key)
	if !ok || v != -55.5 {
		t.Fatalf("Get() = %v, %v; want -55.5, true", v, ok)
	}
}

func TestSetUnknownKeyFails(t *testing.T) {
	s := New()
	if s.Set(m261points.PointKey{Device: "NOPE", Slug: "nope"}, 1) {
		t.Fatal("Set on an unknown key returned true")
	}
}

func TestIECAndModbusIndicesAgreeWithCatalog(t *testing.T) {
	s := New()
	for key, meta := range m261points.Points {
		gotKey, _, ok := s.GetByIEC(IECAddr{CommonAddr: meta.DeviceAddr, ObjAddr: meta.IEC104Addr})
		if !ok || gotKey != key {
			t.Fatalf("GetByIEC(%d,%d) = %v, %v; want %v, true", meta.DeviceAddr, meta.IEC104Addr, gotKey, ok, key)
		}
		if meta.ModbusAddr != nil && meta.ModbusClass != nil {
			addr := ModbusAddr{UnitID: meta.DeviceAddr, Class: *meta.ModbusClass, Address: *meta.ModbusAddr}
			gotKey, _, ok := s.GetByModbus(addr)
			if !ok || gotKey != key {
				t.Fatalf("GetByModbus(%+v) = %v, %v; want %v, true", addr, gotKey, ok, key)
			}
		}
	}
}

func TestWriteViaModbusVisibleViaIEC(t *testing.T) {
	// Task 4 acceptance: "a setpoint write via Modbus is visible reading via
	// IEC-104 and vice versa." A setpoint's WO address isn't itself polled
	// in real IEC-104 — the readback point is what a client actually reads.
	s := New()
	meta, ok := m261points.Points[m261points.PointKey{Device: "EMS", Slug: "set_active_power_kw"}]
	if !ok {
		t.Fatal("fixture point missing from catalog")
	}
	if meta.ModbusAddr == nil || meta.ModbusClass == nil || meta.ReadbackIEC104Addr == nil {
		t.Fatal("fixture point unexpectedly missing modbus/readback addressing")
	}

	mbAddr := ModbusAddr{UnitID: meta.DeviceAddr, Class: *meta.ModbusClass, Address: *meta.ModbusAddr}
	if _, ok := s.SetByModbus(mbAddr, -42); !ok {
		t.Fatal("SetByModbus failed")
	}

	rbAddr := IECAddr{CommonAddr: meta.DeviceAddr, ObjAddr: *meta.ReadbackIEC104Addr}
	_, v, ok := s.GetByIEC(rbAddr)
	if !ok || v != -42 {
		t.Fatalf("readback via IEC-104 = %v, %v; want -42, true", v, ok)
	}

	// And the reverse direction: write via IEC-104 (its own WO address),
	// read back via Modbus.
	iecAddr := IECAddr{CommonAddr: meta.DeviceAddr, ObjAddr: meta.IEC104Addr}
	if _, ok := s.SetByIEC(iecAddr, 17); !ok {
		t.Fatal("SetByIEC failed")
	}
	_, v, ok = s.GetByModbus(mbAddr)
	if !ok || v != 17 {
		t.Fatalf("readback via Modbus = %v, %v; want 17, true", v, ok)
	}
}

func TestSubscribeReceivesChanges(t *testing.T) {
	s := New()
	ch, unsubscribe := s.Subscribe()
	defer unsubscribe()

	key := m261points.PointKey{Device: "EMS", Slug: "set_active_power_kw"}
	s.Set(key, 99)

	select {
	case c := <-ch:
		if c.Key != key || c.Value != 99 {
			t.Fatalf("got Change %+v, want {%v 99}", c, key)
		}
	default:
		t.Fatal("expected a Change on the subscription channel, got none")
	}
}

func TestUnsubscribeStopsDelivery(t *testing.T) {
	s := New()
	ch, unsubscribe := s.Subscribe()
	unsubscribe()

	key := m261points.PointKey{Device: "EMS", Slug: "set_active_power_kw"}
	s.Set(key, 1) // must not panic or block despite the channel being closed/removed

	if _, ok := <-ch; ok {
		t.Fatal("expected the channel to be closed after unsubscribe")
	}
}

func TestSlowSubscriberIsDroppedNotBlocked(t *testing.T) {
	s := New()
	ch, unsubscribe := s.Subscribe()
	defer unsubscribe()

	key := m261points.PointKey{Device: "EMS", Slug: "set_active_power_kw"}
	// Fill the subscriber's buffer without ever reading it.
	for i := 0; i < subscriberBufferSize+10; i++ {
		s.Set(key, float64(i)) // must never block, even though nothing drains ch
	}
	if len(ch) != subscriberBufferSize {
		t.Fatalf("channel buffered len = %d, want %d (full but not blocked)", len(ch), subscriberBufferSize)
	}
}

func TestConcurrentReadWriteIsRaceFree(t *testing.T) {
	s := New()
	key := m261points.PointKey{Device: "EMS", Slug: "set_active_power_kw"}
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			s.Set(key, float64(i))
		}(i)
		go func() {
			defer wg.Done()
			s.Get(key)
			s.Snapshot()
		}()
	}
	wg.Wait()
}

func TestSnapshotDeviceScopesToOneDevice(t *testing.T) {
	s := New()
	snap := s.SnapshotDevice(1) // EMS
	if len(snap) == 0 {
		t.Fatal("SnapshotDevice(1) is empty")
	}
	for k := range snap {
		if k.Device != "EMS" {
			t.Fatalf("SnapshotDevice(1) included non-EMS point %v", k)
		}
	}
	if len(snap) != countDevice("EMS") {
		t.Fatalf("SnapshotDevice(1) has %d points, want %d", len(snap), countDevice("EMS"))
	}
}

func countDevice(device string) int {
	n := 0
	for k := range m261points.Points {
		if k.Device == device {
			n++
		}
	}
	return n
}

// TestRestoreReturnsToSnapshot is Task 7 item 7's core building block:
// take a snapshot, dirty the store, Restore, and the store must be
// byte-for-byte (well, float-for-float) back to the snapshot — including
// points nothing else touched, not just the one deliberately dirtied.
func TestRestoreReturnsToSnapshot(t *testing.T) {
	s := New()
	key := m261points.PointKey{Device: "EMS", Slug: "set_active_power_kw"}
	baseline := s.Snapshot()

	s.Set(key, -42)
	if v, _ := s.Get(key); v != -42 {
		t.Fatalf("Set didn't take effect before Restore, got %v", v)
	}

	s.Restore(baseline)

	got := s.Snapshot()
	if len(got) != len(baseline) {
		t.Fatalf("Snapshot after Restore has %d points, want %d", len(got), len(baseline))
	}
	for k, want := range baseline {
		if got[k] != want {
			t.Errorf("after Restore, %v = %v, want %v", k, got[k], want)
		}
	}
}

// TestRestorePublishesOnlyChangedPoints proves Restore doesn't spam every
// subscriber with len(snapshot) Changes when only one point actually
// differed — it would defeat IEC-104 spontaneous transmission's whole
// point (only telling clients what actually moved) if reset looked like
// every one of 1513 points changing at once.
func TestRestorePublishesOnlyChangedPoints(t *testing.T) {
	s := New()
	// A telemetry point, deliberately — a setpoint's Set also mirrors
	// onto its readback twin (a second point), which would make this
	// test about readback mirroring instead of about Restore's own
	// change-filtering.
	key := m261points.PointKey{Device: "BMS", Slug: "soc"}
	baseline := s.Snapshot()
	s.Set(key, -42)

	ch, unsubscribe := s.Subscribe()
	defer unsubscribe()

	s.Restore(baseline)

	select {
	case c := <-ch:
		if c.Key != key || c.Value != 0 {
			t.Fatalf("got Change %+v, want {%v 0}", c, key)
		}
	default:
		t.Fatal("expected exactly one Change (the point Restore actually changed), got none")
	}
	select {
	case c := <-ch:
		t.Fatalf("got an unexpected second Change %+v — Restore must only publish points that actually changed", c)
	default:
	}
}

// TestRestoreIsAtomicUnderConcurrentAccess is Task 7 item 7's atomicity
// requirement: a concurrent reader must never observe a store where some
// points already reflect the restored snapshot and others still hold
// their pre-Restore values — every point here starts at 0 and the
// snapshot sets every point to the same sentinel, so a single Snapshot()
// call that contains both 0 and sentinel proves a torn (non-atomic)
// Restore. A per-key Get loop can't detect this on its own — each
// individual key is always internally consistent (0 or sentinel), only
// the aggregate across keys can be torn — so this repeatedly takes a
// full Snapshot() concurrently with Restore instead.
func TestRestoreIsAtomicUnderConcurrentAccess(t *testing.T) {
	s := New()
	const sentinel = 12345.0
	snapshot := s.Snapshot()
	for k := range snapshot {
		snapshot[k] = sentinel
	}

	torn := make(chan struct{}, 1)
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			snap := s.Snapshot()
			sawZero, sawSentinel := false, false
			for _, v := range snap {
				switch v {
				case 0:
					sawZero = true
				case sentinel:
					sawSentinel = true
				}
			}
			if sawZero && sawSentinel {
				select {
				case torn <- struct{}{}:
				default:
				}
				return
			}
		}
	}()

	s.Restore(snapshot)
	close(stop)

	select {
	case <-torn:
		t.Fatal("observed a torn Restore: a single Snapshot() contained both pre- and post-Restore values")
	default:
	}
}

func TestSubscribeBatchesDeliversWholeMutationAndSnapshotBoundary(t *testing.T) {
	s := New()
	ch, snap, rev, unsubscribe := s.SubscribeBatchesWithSnapshot()
	defer unsubscribe()
	if rev != s.CurrentRevision() || len(snap) != len(m261points.Points) {
		t.Fatalf("bootstrap snapshot/revision is not self-consistent: rev=%d current=%d points=%d", rev, s.CurrentRevision(), len(snap))
	}
	writes := []KeyValue{{Key: m261points.PointKey{Device: "EMS", Slug: "desired_active_power_kw"}, Value: 11}, {Key: m261points.PointKey{Device: "BMS", Slug: "soc"}, Value: 22}}
	if !s.SetBatch(writes) {
		t.Fatal("SetBatch failed")
	}
	batch := <-ch
	if batch.Revision != rev+1 {
		t.Fatalf("revision = %d, want %d", batch.Revision, rev+1)
	}
	if len(batch.Changes) != len(writes) {
		t.Fatalf("changes = %d, want %d", len(batch.Changes), len(writes))
	}
	for _, change := range batch.Changes {
		if change.Rev != batch.Revision {
			t.Fatalf("change has rev %d, batch has %d", change.Rev, batch.Revision)
		}
	}
}

// TestBatchSubscriberDetectsOneForcedDrop fills the bounded queue before
// publishing exactly one more mutation. A later batch then has a revision
// gap; no queued batch can contain a partial mutation because publication is
// one channel send per ChangeBatch.
func TestBatchSubscriberDetectsOneForcedDrop(t *testing.T) {
	s := New()
	ch, unsubscribe := s.SubscribeBatches()
	defer unsubscribe()
	keyA := m261points.PointKey{Device: "EMS", Slug: "desired_active_power_kw"}
	keyB := m261points.PointKey{Device: "BMS", Slug: "soc"}

	for i := 0; i < subscriberBufferSize; i++ {
		if !s.SetBatch([]KeyValue{{Key: keyA, Value: float64(i)}, {Key: keyB, Value: float64(i)}}) {
			t.Fatal("SetBatch failed while filling subscriber queue")
		}
	}
	if !s.SetBatch([]KeyValue{{Key: keyA, Value: 1000}, {Key: keyB, Value: 1000}}) {
		t.Fatal("SetBatch failed for forced drop")
	}

	var last uint64
	for i := 0; i < subscriberBufferSize; i++ {
		batch := <-ch
		if len(batch.Changes) != 2 {
			t.Fatalf("batch %d exposed %d of 2 changes", batch.Revision, len(batch.Changes))
		}
		if i > 0 && batch.Revision != last+1 {
			t.Fatalf("unexpected gap before forced drop: got %d after %d", batch.Revision, last)
		}
		last = batch.Revision
	}
	if !s.Set(keyA, 2000) {
		t.Fatal("Set failed after draining queue")
	}
	afterDrop := <-ch
	if afterDrop.Revision != last+2 {
		t.Fatalf("revision after forced drop = %d after %d, want exactly one missing batch", afterDrop.Revision, last)
	}
}
