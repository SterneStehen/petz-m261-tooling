package modbustcp

import (
	"encoding/binary"
	"math"
	"testing"
	"time"

	gomodbus "github.com/goburrow/modbus"

	"github.com/SterneStehen/petz-m261-tooling/gen/go/m261points"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/store"
)

// Fixture points, verified against the real catalog (see commit message /
// PR description): EMS Manual Protection (alarm, U8, modbus 10001 class
// 2), EMS Desired Active Power (telemetry, F32, modbus 30009 class 4),
// EMS Set Operating Mode (setpoint, I16, modbus 40009 class 3), EMS Set
// Active Power (setpoint, F32, modbus 40153 class 3).
const emsUnit = 1

func startServer(t *testing.T, st *store.Store, order m261points.ByteOrder) (*Server, string) {
	t.Helper()
	srv := New(st, Config{Addr: "127.0.0.1:0", ByteOrder: order})
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { srv.Close() })
	return srv, srv.Addr().String()
}

func newClient(t *testing.T, addr string, unit byte) gomodbus.Client {
	t.Helper()
	handler := gomodbus.NewTCPClientHandler(addr)
	handler.SlaveId = unit
	handler.Timeout = 2 * time.Second
	if err := handler.Connect(); err != nil {
		t.Fatalf("client Connect: %v", err)
	}
	t.Cleanup(func() { handler.Close() })
	return gomodbus.NewClient(handler)
}

// --- Task 4 acceptance: external Modbus client reads telemetry from every device ---

func TestExternalClientReadsTelemetry(t *testing.T) {
	st := store.New()
	st.Set(m261points.PointKey{Device: "EMS", Slug: "desired_active_power_kw"}, 42.5)
	_, addr := startServer(t, st, m261points.BigEndian)
	client := newClient(t, addr, emsUnit)

	// wire address = doc address 30009 - 30001 = 8
	regs, err := client.ReadInputRegisters(8, 2)
	if err != nil {
		t.Fatalf("ReadInputRegisters: %v", err)
	}
	got := math.Float32frombits(binary.BigEndian.Uint32(regs))
	if got != 42.5 {
		t.Fatalf("ReadInputRegisters = %v, want 42.5", got)
	}
}

// TestExternalClientReadsTelemetryFromEveryDevice covers the "every
// device" half of the acceptance criterion above — one telemetry point
// per device, each addressed by its own Unit ID.
func TestExternalClientReadsTelemetryFromEveryDevice(t *testing.T) {
	cases := []struct {
		device   string
		slug     string
		unit     byte
		wireAddr uint16
		dataType m261points.DataType
		wantF32  float32
		wantI32  int32
	}{
		{"PCS", "phase_a_voltage_v", 2, 30003 - 30001, m261points.DataTypeF32, 231.5, 0},
		{"BMS", "soc", 34, 30003 - 30001, m261points.DataTypeF32, 87.5, 0},
		{"BMS_CELLS", "cell_voltage_001_mv", 98, 30001 - 30001, m261points.DataTypeI16, 0, 3312},
		{"TMS", "unit_operation_status", 170, 30001 - 30001, m261points.DataTypeI16, 0, 2},
		{"PCS_METER", "phase_a_voltage_v", 163, 30001 - 30001, m261points.DataTypeF32, 230.1, 0},
		{"DIDO", "fire_protection_level_1_alarm_feedback_signal", 168, 30001 - 30001, m261points.DataTypeI16, 0, 1},
		{"CSJ", "wen_du", 172, 30501 - 30001, m261points.DataTypeF32, 24.5, 0},
	}
	for _, tc := range cases {
		t.Run(tc.device, func(t *testing.T) {
			st := store.New()
			var want float64
			if tc.dataType == m261points.DataTypeF32 {
				want = float64(tc.wantF32)
			} else {
				want = float64(tc.wantI32)
			}
			st.Set(m261points.PointKey{Device: tc.device, Slug: tc.slug}, want)
			_, addr := startServer(t, st, m261points.BigEndian)
			client := newClient(t, addr, tc.unit)

			regs, err := client.ReadInputRegisters(tc.wireAddr, 2)
			if err != nil {
				t.Fatalf("ReadInputRegisters: %v", err)
			}
			var got float64
			if tc.dataType == m261points.DataTypeF32 {
				got = float64(math.Float32frombits(binary.BigEndian.Uint32(regs)))
			} else {
				got = float64(int32(binary.BigEndian.Uint32(regs)))
			}
			if got != want {
				t.Fatalf("%s/%s: ReadInputRegisters = %v, want %v", tc.device, tc.slug, got, want)
			}
		})
	}
}

func TestExternalClientReadsAlarmsAsDiscreteInputs(t *testing.T) {
	st := store.New()
	st.Set(m261points.PointKey{Device: "EMS", Slug: "manual_protection"}, 1)
	_, addr := startServer(t, st, m261points.BigEndian)
	client := newClient(t, addr, emsUnit)

	// wire address = doc address 10001 - 10001 = 0
	bits, err := client.ReadDiscreteInputs(0, 1)
	if err != nil {
		t.Fatalf("ReadDiscreteInputs: %v", err)
	}
	if bits[0]&1 != 1 {
		t.Fatalf("ReadDiscreteInputs = %08b, want bit 0 set", bits[0])
	}
}

// --- Task 4 acceptance: setpoint write via Modbus visible via IEC-104 (readback), and vice versa ---

func TestWriteSetpointViaModbusMirrorsToReadback(t *testing.T) {
	st := store.New()
	_, addr := startServer(t, st, m261points.BigEndian)
	client := newClient(t, addr, emsUnit)

	buf := make([]byte, 4)
	binary.BigEndian.PutUint32(buf, math.Float32bits(-99.5))
	// wire address = doc address 40153 - 40001 = 152
	if _, err := client.WriteMultipleRegisters(152, 2, buf); err != nil {
		t.Fatalf("WriteMultipleRegisters: %v", err)
	}

	key := m261points.PointKey{Device: "EMS", Slug: "set_active_power_kw"}
	v, ok := st.Get(key)
	if !ok || v != -99.5 {
		t.Fatalf("store value after Modbus write = %v, %v; want -99.5, true", v, ok)
	}
	meta := m261points.Points[key]
	rbKey, rbVal, ok := st.GetByIEC(store.IECAddr{CommonAddr: meta.DeviceAddr, ObjAddr: *meta.ReadbackIEC104Addr})
	if !ok || rbVal != -99.5 {
		t.Fatalf("readback point %v = %v, %v; want -99.5, true", rbKey, rbVal, ok)
	}
}

func TestWriteSingleRegisterIsReadModifyWrite(t *testing.T) {
	// FC06 writes one register at a time; Set Operating Mode is I16-native
	// (widened to I32 on the wire, §2.2), so only its low-order register
	// actually carries a meaningful value with BigEndian ordering — this
	// confirms a single-register write doesn't corrupt the other half.
	st := store.New()
	_, addr := startServer(t, st, m261points.BigEndian)
	client := newClient(t, addr, emsUnit)

	// wire address = doc address 40009 - 40001 = 8; write the low register (8+1=9) to 2 ("Remote")
	if _, err := client.WriteSingleRegister(9, 2); err != nil {
		t.Fatalf("WriteSingleRegister: %v", err)
	}
	v, ok := st.Get(m261points.PointKey{Device: "EMS", Slug: "set_operating_mode"})
	if !ok || v != 2 {
		t.Fatalf("store value = %v, %v; want 2, true", v, ok)
	}
}

// TestF32ScaleAppliedOnReadAndWrite is the review-required proof that
// Modbus applies scale to F32 points exactly the same way it already
// does for I16-native ones (encode: raw = engineering/scale; decode:
// engineering = raw*scale) — every real F32 point in the catalog has
// scale=1 today, which made the F32 path indistinguishable from applying
// no scale at all until this was fixed. m261points.Points is a shared,
// exported package-level map; EMS/Set Active Power's Scale is temporarily
// overwritten for this test only and restored on cleanup — never
// touching the real generated catalog — since no real point can exercise
// a non-1 F32 scale on its own.
func TestF32ScaleAppliedOnReadAndWrite(t *testing.T) {
	key := m261points.PointKey{Device: "EMS", Slug: "set_active_power_kw"}
	original := m261points.Points[key]
	modified := original
	modified.Scale = 10
	m261points.Points[key] = modified
	t.Cleanup(func() { m261points.Points[key] = original })

	st := store.New()
	_, addr := startServer(t, st, m261points.BigEndian)
	client := newClient(t, addr, emsUnit)

	// Write raw=42.5 over the wire (wire address = doc 40153 - 40001 =
	// 152) — decodeRegisterPair must apply engineering = raw*scale.
	buf := make([]byte, 4)
	binary.BigEndian.PutUint32(buf, math.Float32bits(42.5))
	if _, err := client.WriteMultipleRegisters(152, 2, buf); err != nil {
		t.Fatalf("WriteMultipleRegisters: %v", err)
	}
	if got, ok := st.Get(key); !ok || got != 425 {
		t.Errorf("stored engineering value = %v, %v; want 425, true (raw 42.5 * scale 10)", got, ok)
	}

	// The other direction: set the store to a known engineering value
	// directly, confirm the register read back is raw = engineering /
	// scale, not the engineering value's own float32 bits.
	st.Set(key, 425)
	regs, err := client.ReadHoldingRegisters(152, 2)
	if err != nil {
		t.Fatalf("ReadHoldingRegisters: %v", err)
	}
	if got := math.Float32frombits(binary.BigEndian.Uint32(regs)); got != 42.5 {
		t.Errorf("register read back = %v, want 42.5 (engineering 425 / scale 10)", got)
	}
}

// --- Task 4 acceptance: all four byte order variants ---

func TestAllFourByteOrders(t *testing.T) {
	orders := []m261points.ByteOrder{
		m261points.BigEndian, m261points.LittleEndian,
		m261points.BigEndianWordSwap, m261points.LittleEndianWordSwap,
	}
	for _, order := range orders {
		st := store.New()
		_, addr := startServer(t, st, order)
		client := newClient(t, addr, emsUnit)

		want := float32(-273.15)
		encoded := m261points.EncodeF32(want, order)
		if _, err := client.WriteMultipleRegisters(152, 2, encoded); err != nil {
			t.Fatalf("order=%v: WriteMultipleRegisters: %v", order, err)
		}
		regs, err := client.ReadHoldingRegisters(152, 2)
		if err != nil {
			t.Fatalf("order=%v: ReadHoldingRegisters: %v", order, err)
		}
		got := m261points.DecodeF32(regs, order)
		if got != want {
			t.Errorf("order=%v: round trip via wire = %v, want %v", order, got, want)
		}
	}
}

// --- Task 4 acceptance: server survives a client disconnect + reconnect ---

func TestServerSurvivesDisconnectAndReconnect(t *testing.T) {
	st := store.New()
	st.Set(m261points.PointKey{Device: "EMS", Slug: "desired_active_power_kw"}, 7)
	_, addr := startServer(t, st, m261points.BigEndian)

	handler1 := gomodbus.NewTCPClientHandler(addr)
	handler1.SlaveId = emsUnit
	if err := handler1.Connect(); err != nil {
		t.Fatalf("first Connect: %v", err)
	}
	client1 := gomodbus.NewClient(handler1)
	if _, err := client1.ReadInputRegisters(8, 2); err != nil {
		t.Fatalf("first read: %v", err)
	}
	handler1.Close() // simulate a dropped connection

	client2 := newClient(t, addr, emsUnit)
	regs, err := client2.ReadInputRegisters(8, 2)
	if err != nil {
		t.Fatalf("read after reconnect: %v", err)
	}
	if got := math.Float32frombits(binary.BigEndian.Uint32(regs)); got != 7 {
		t.Fatalf("read after reconnect = %v, want 7", got)
	}
}

func TestUnmappedRegisterReadsZeroNotError(t *testing.T) {
	st := store.New()
	_, addr := startServer(t, st, m261points.BigEndian)
	client := newClient(t, addr, emsUnit)

	// Address far beyond any real EMS holding register.
	regs, err := client.ReadHoldingRegisters(60000, 2)
	if err != nil {
		t.Fatalf("ReadHoldingRegisters on an unmapped range: %v", err)
	}
	if regs[0] != 0 || regs[1] != 0 || regs[2] != 0 || regs[3] != 0 {
		t.Fatalf("unmapped registers = %v, want all zero", regs)
	}
}

func TestWriteToUnmappedAddressIsRejected(t *testing.T) {
	st := store.New()
	_, addr := startServer(t, st, m261points.BigEndian)
	client := newClient(t, addr, emsUnit)

	_, err := client.WriteSingleRegister(60000, 1)
	if err == nil {
		t.Fatal("expected a Modbus exception writing to an unmapped address, got nil error")
	}
}

func TestUnitIDRouting(t *testing.T) {
	// Unit 2 is PCS, not EMS — reading an EMS-only address under unit 2
	// must not resolve to the EMS point.
	st := store.New()
	st.Set(m261points.PointKey{Device: "EMS", Slug: "desired_active_power_kw"}, 42)
	_, addr := startServer(t, st, m261points.BigEndian)
	client := newClient(t, addr, 2)

	regs, err := client.ReadInputRegisters(8, 2)
	if err != nil {
		t.Fatalf("ReadInputRegisters under unit 2: %v", err)
	}
	if regs[0] != 0 || regs[1] != 0 || regs[2] != 0 || regs[3] != 0 {
		t.Fatalf("unit 2 read returned EMS's value: %v", regs)
	}
}

// --- Task 7 item 2: link faults --------------------------------------------
//
// heartbeatWireAddr is EMS Periodic Heartbeat Indicator's wire address
// (modbus_addr 30031, class 4 — 30031-30001=30), verified against the
// real catalog.
const heartbeatWireAddr = 30

func newClientWithTimeout(t *testing.T, addr string, unit byte, timeout time.Duration) gomodbus.Client {
	t.Helper()
	handler := gomodbus.NewTCPClientHandler(addr)
	handler.SlaveId = unit
	handler.Timeout = timeout
	if err := handler.Connect(); err != nil {
		t.Fatalf("client Connect: %v", err)
	}
	t.Cleanup(func() { handler.Close() })
	return gomodbus.NewClient(handler)
}

func TestLinkDropClosesExistingAndRefusesNewConnections(t *testing.T) {
	st := store.New()
	srv, addr := startServer(t, st, m261points.BigEndian)
	client := newClient(t, addr, emsUnit)
	if _, err := client.ReadInputRegisters(8, 2); err != nil {
		t.Fatalf("read before drop: %v", err)
	}

	srv.SetDrop()

	// The already-open connection must be force-closed, not merely left
	// unanswered — the very next request on it fails outright.
	if _, err := client.ReadInputRegisters(8, 2); err == nil {
		t.Error("read on a connection open before SetDrop succeeded, want an error (connection force-closed)")
	}

	// A brand new connection attempt must also be refused while drop is
	// active.
	handler := gomodbus.NewTCPClientHandler(addr)
	handler.SlaveId = emsUnit
	handler.Timeout = 500 * time.Millisecond
	if err := handler.Connect(); err == nil {
		defer handler.Close()
		if _, rerr := gomodbus.NewClient(handler).ReadInputRegisters(8, 2); rerr == nil {
			t.Error("new connection accepted and answered while drop is active, want refused")
		}
	}

	srv.ClearLinkFaults()
	client2 := newClient(t, addr, emsUnit)
	if _, err := client2.ReadInputRegisters(8, 2); err != nil {
		t.Errorf("read after ClearLinkFaults: %v, want a normal successful read", err)
	}
}

func TestLinkHangSendsNoResponse(t *testing.T) {
	st := store.New()
	srv, addr := startServer(t, st, m261points.BigEndian)
	client := newClientWithTimeout(t, addr, emsUnit, 300*time.Millisecond)

	srv.SetHang()
	if _, err := client.ReadInputRegisters(8, 2); err == nil {
		t.Error("read while hanging succeeded, want a client-side timeout (no response at all)")
	}

	srv.ClearLinkFaults()
	client2 := newClient(t, addr, emsUnit)
	if _, err := client2.ReadInputRegisters(8, 2); err != nil {
		t.Errorf("read after ClearLinkFaults: %v, want a normal successful read", err)
	}
}

func TestLinkDelayDelaysResponse(t *testing.T) {
	st := store.New()
	srv, addr := startServer(t, st, m261points.BigEndian)
	client := newClientWithTimeout(t, addr, emsUnit, 2*time.Second)

	const delay = 200 * time.Millisecond
	srv.SetDelay(delay)

	start := time.Now()
	if _, err := client.ReadInputRegisters(8, 2); err != nil {
		t.Fatalf("read while delayed: %v", err)
	}
	if elapsed := time.Since(start); elapsed < delay {
		t.Errorf("read returned after %s, want at least %s (SetDelay)", elapsed, delay)
	}

	srv.ClearLinkFaults()
	start = time.Now()
	if _, err := client.ReadInputRegisters(8, 2); err != nil {
		t.Fatalf("read after ClearLinkFaults: %v", err)
	}
	if elapsed := time.Since(start); elapsed >= delay {
		t.Errorf("read after ClearLinkFaults took %s, want well under %s (delay cleared)", elapsed, delay)
	}
}

func TestLinkHeartbeatPauseFreezesReadValue(t *testing.T) {
	st := store.New()
	heartbeatKey := m261points.PointKey{Device: "EMS", Slug: "ems_periodic_heartbeat_indicator"}
	st.Set(heartbeatKey, 10)
	srv, addr := startServer(t, st, m261points.BigEndian)
	client := newClient(t, addr, emsUnit)

	srv.SetHeartbeatPause(10)
	st.Set(heartbeatKey, 99) // the "live" simulator keeps incrementing underneath

	regs, err := client.ReadInputRegisters(heartbeatWireAddr, 2)
	if err != nil {
		t.Fatalf("ReadInputRegisters(heartbeat): %v", err)
	}
	if got := int32(binary.BigEndian.Uint32(regs)); got != 10 {
		t.Errorf("heartbeat read while paused = %v, want the frozen value 10 (live Store value is 99)", got)
	}

	srv.ClearLinkFaults()
	regs, err = client.ReadInputRegisters(heartbeatWireAddr, 2)
	if err != nil {
		t.Fatalf("ReadInputRegisters(heartbeat) after clear: %v", err)
	}
	if got := int32(binary.BigEndian.Uint32(regs)); got != 99 {
		t.Errorf("heartbeat read after ClearLinkFaults = %v, want the live value 99", got)
	}
}

func TestLinkFaultModesAreIndependent(t *testing.T) {
	st := store.New()
	srv, _ := startServer(t, st, m261points.BigEndian)

	srv.SetDelay(50 * time.Millisecond)
	if srv.link.dropped() || srv.link.hanging() {
		t.Error("SetDelay alone unexpectedly also set drop or hang")
	}
	if d := srv.link.responseDelay(); d != 50*time.Millisecond {
		t.Errorf("responseDelay() = %v, want 50ms", d)
	}
}
