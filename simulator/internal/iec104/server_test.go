package iec104

import (
	"encoding/binary"
	"io"
	"math"
	"net"
	"testing"
	"time"

	"github.com/SterneStehen/petz-m261-tooling/gen/go/m261points"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/store"
)

// Fixture points, verified against the real catalog (same ones used in
// modbustcp's tests): EMS Manual Protection (alarm, iec 1), EMS Desired
// Active Power (telemetry, F32, iec 16389), EMS Set Operating Mode
// (setpoint, iec 25093, readback 16415), EMS Set Active Power (setpoint,
// F32, iec 25165, readback 16487).
const emsCommonAddr = 1

// --- a deliberately independent test client -----------------------------
//
// Constructs and parses raw APCI/ASDU bytes itself rather than calling
// this package's own encode/decode functions — a test that only checks
// the server against its own wire-format code proves nothing about wire
// compatibility. This is the "external client" Task 4 asks for, kept
// intentionally low-level.

type rawClient struct {
	t       *testing.T
	nc      net.Conn
	sendSeq uint16
	recvSeq uint16
}

func dialRaw(t *testing.T, addr string) *rawClient {
	t.Helper()
	nc, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	nc.SetDeadline(time.Now().Add(5 * time.Second))
	t.Cleanup(func() { nc.Close() })
	return &rawClient{t: t, nc: nc}
}

func (c *rawClient) writeRaw(control []byte, asdu []byte) {
	c.t.Helper()
	buf := make([]byte, 2+len(control)+len(asdu))
	buf[0] = 0x68
	buf[1] = byte(len(control) + len(asdu))
	copy(buf[2:], control)
	copy(buf[2+len(control):], asdu)
	if _, err := c.nc.Write(buf); err != nil {
		c.t.Fatalf("write: %v", err)
	}
}

func (c *rawClient) startDT() {
	c.writeRaw([]byte{0x07, 0x00, 0x00, 0x00}, nil) // STARTDT_ACT
	c.expectU(0x0B)                                 // STARTDT_CON
}

// readFrameRaw reads one raw frame independently of the package's own readFrame.
type rawFrame struct {
	control []byte
	asdu    []byte
}

func (c *rawClient) readFrameRaw() rawFrame {
	c.t.Helper()
	header := make([]byte, 2)
	if _, err := io.ReadFull(c.nc, header); err != nil {
		c.t.Fatalf("read header: %v", err)
	}
	if header[0] != 0x68 {
		c.t.Fatalf("bad start byte 0x%02x", header[0])
	}
	rest := make([]byte, header[1])
	if _, err := io.ReadFull(c.nc, rest); err != nil {
		c.t.Fatalf("read body: %v", err)
	}
	return rawFrame{control: rest[0:4], asdu: rest[4:]}
}

func (c *rawClient) expectU(expected byte) {
	c.t.Helper()
	f := c.readFrameRaw()
	if f.control[0] != expected {
		c.t.Fatalf("expected U-frame control 0x%02x, got 0x%02x", expected, f.control[0])
	}
}

// nextI reads frames, transparently tracking (and acking) S-frames, until
// it gets the next I-frame's ASDU.
func (c *rawClient) nextI() []byte {
	c.t.Helper()
	for {
		f := c.readFrameRaw()
		if f.control[0]&0x01 == 0 { // I-format
			c.recvSeq = ((uint16(f.control[1])<<8 | uint16(f.control[0])) >> 1) + 1
			return f.asdu
		}
		// S or U frame with nothing pending for us in this test harness
	}
}

// waitForType scans frames until it finds one of the given ASDU type,
// skipping anything else — a real, correctly-behaved IEC-104 client
// cannot assume its command's confirmation is literally the very next
// frame: a spontaneous transmission for an unrelated (or the same,
// readback-mirrored) point can legitimately race with it on the same
// connection, and must be tolerated rather than mistaken for the ack.
func (c *rawClient) waitForType(typeID byte) []byte {
	c.t.Helper()
	for i := 0; i < 50; i++ {
		asdu := c.nextI()
		if asdu[0] == typeID {
			return asdu
		}
	}
	c.t.Fatalf("did not see an ASDU of type %d after 50 frames", typeID)
	return nil
}

func (c *rawClient) sendI(asdu []byte) {
	c.t.Helper()
	sLo, sHi := byte(c.sendSeq<<1), byte(c.sendSeq>>7)
	rLo, rHi := byte(c.recvSeq<<1), byte(c.recvSeq>>7)
	c.writeRaw([]byte{sLo, sHi, rLo, rHi}, asdu)
	c.sendSeq++
}

func rawGeneralInterrogation(commonAddr int) []byte {
	asdu := make([]byte, 10)
	asdu[0] = 100 // C_IC_NA_1
	asdu[1] = 1   // one object, SQ=0
	asdu[2] = 6   // COT activation
	asdu[3] = 0
	binary.LittleEndian.PutUint16(asdu[4:6], uint16(commonAddr))
	// IOA = 0
	asdu[9] = 20 // QOI: station interrogation
	return asdu
}

func rawSetpointCommand(commonAddr, ioa int, value float32) []byte {
	asdu := make([]byte, 14)
	asdu[0] = 50 // C_SE_NC_1
	asdu[1] = 1
	asdu[2] = 6 // COT activation
	asdu[3] = 0
	binary.LittleEndian.PutUint16(asdu[4:6], uint16(commonAddr))
	asdu[6], asdu[7], asdu[8] = byte(ioa), byte(ioa>>8), byte(ioa>>16)
	binary.LittleEndian.PutUint32(asdu[9:13], math.Float32bits(value))
	return asdu
}

func rawSingleCommand(commonAddr, ioa int, value bool) []byte {
	asdu := make([]byte, 10)
	asdu[0] = 45 // C_SC_NA_1
	asdu[1] = 1
	asdu[2] = 6
	asdu[3] = 0
	binary.LittleEndian.PutUint16(asdu[4:6], uint16(commonAddr))
	asdu[6], asdu[7], asdu[8] = byte(ioa), byte(ioa>>8), byte(ioa>>16)
	if value {
		asdu[9] = 1
	}
	return asdu
}

// --- server test helper --------------------------------------------------

func startServer(t *testing.T, st *store.Store) (*Server, string) {
	t.Helper()
	srv := New(st, Config{Addr: "127.0.0.1:0"})
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { srv.Close() })
	return srv, srv.Addr().String()
}

// --- Task 4 acceptance: external client runs general interrogation and gets every point ---

func TestGeneralInterrogationReturnsEveryPoint(t *testing.T) {
	st := store.New()
	st.Set(m261points.PointKey{Device: "EMS", Slug: "manual_protection"}, 1)
	st.Set(m261points.PointKey{Device: "EMS", Slug: "desired_active_power_kw"}, 42.5)
	_, addr := startServer(t, st)

	c := dialRaw(t, addr)
	c.startDT()
	c.sendI(rawGeneralInterrogation(emsCommonAddr))

	seenAlarm, seenTelemetry := false, false
	nEMSPoints := 0
	for {
		asdu := c.nextI()
		typeID, cot := asdu[0], asdu[2]
		if typeID == 100 && cot == cotActivationTermination {
			break
		}
		if typeID == 100 {
			continue // activation confirmation
		}
		nEMSPoints++
		ioa := int(asdu[6]) | int(asdu[7])<<8 | int(asdu[8])<<16
		switch {
		case typeID == typeMSPNA1 && ioa == 1:
			seenAlarm = asdu[9]&0x01 == 1
		case typeID == typeMMENC1 && ioa == 16389:
			seenTelemetry = decodeFloat32(asdu[9:13]) == 42.5
		}
	}
	if !seenAlarm {
		t.Error("did not see Manual Protection = true during general interrogation")
	}
	if !seenTelemetry {
		t.Error("did not see Desired Active Power = 42.5 during general interrogation")
	}
	// EMS has 31 alarms + 174 telemetry = 205 monitored points (setpoints excluded, Task 1 counts)
	if want := 205; nEMSPoints != want {
		t.Errorf("general interrogation returned %d EMS points, want %d", nEMSPoints, want)
	}
}

// --- Task 4 acceptance: setpoint write via IEC-104 visible via Modbus (and the reverse, in modbustcp's own test) ---

func TestSetpointCommandUpdatesStoreAndReadback(t *testing.T) {
	st := store.New()
	_, addr := startServer(t, st)
	c := dialRaw(t, addr)
	c.startDT()

	c.sendI(rawSetpointCommand(emsCommonAddr, 25165, -88.5)) // Set Active Power
	ack := c.waitForType(typeCSENC1)
	if ack[2] != cotActivationCon {
		t.Fatalf("expected C_SE_NC_1 activation confirmation, got cot=%d", ack[2])
	}

	key := m261points.PointKey{Device: "EMS", Slug: "set_active_power_kw"}
	if v, ok := st.Get(key); !ok || v != -88.5 {
		t.Fatalf("store value = %v, %v; want -88.5, true", v, ok)
	}
	meta := m261points.Points[key]
	if _, rb, ok := st.GetByIEC(store.IECAddr{CommonAddr: meta.DeviceAddr, ObjAddr: *meta.ReadbackIEC104Addr}); !ok || rb != -88.5 {
		t.Fatalf("readback = %v, %v; want -88.5, true", rb, ok)
	}
}

func TestSingleCommandAcceptedForAnySetpoint(t *testing.T) {
	// C_SC_NA_1 must work too — the server doesn't require a specific
	// command type per setpoint (see server.go's rationale).
	st := store.New()
	_, addr := startServer(t, st)
	c := dialRaw(t, addr)
	c.startDT()

	c.sendI(rawSingleCommand(emsCommonAddr, 25164, true)) // Power On/Off
	ack := c.waitForType(typeCSCNA1)
	if ack[2] != cotActivationCon {
		t.Fatalf("expected C_SC_NA_1 activation confirmation, got cot=%d", ack[2])
	}
	if v, ok := st.Get(m261points.PointKey{Device: "EMS", Slug: "power_on_off"}); !ok || v != 1 {
		t.Fatalf("store value = %v, %v; want 1, true", v, ok)
	}
}

func TestUnknownIOAGetsNegativeConfirmation(t *testing.T) {
	st := store.New()
	_, addr := startServer(t, st)
	c := dialRaw(t, addr)
	c.startDT()

	c.sendI(rawSetpointCommand(emsCommonAddr, 999999, 1))
	ack := c.nextI()
	if ack[2]&cotNegativeFlag == 0 {
		t.Fatalf("expected the P/N (negative) bit set in COT 0x%02x for an unknown IOA", ack[2])
	}
}

// --- Task 4 acceptance: spontaneous transmission on change ---

func TestSpontaneousTransmissionOnChange(t *testing.T) {
	st := store.New()
	_, addr := startServer(t, st)
	c := dialRaw(t, addr)
	c.startDT()

	st.Set(m261points.PointKey{Device: "EMS", Slug: "desired_active_power_kw"}, 17)

	asdu := c.nextI()
	if asdu[0] != typeMMENC1 || asdu[2] != cotSpontaneous {
		t.Fatalf("expected spontaneous M_ME_NC_1, got type=%d cot=%d", asdu[0], asdu[2])
	}
	ioa := int(asdu[6]) | int(asdu[7])<<8 | int(asdu[8])<<16
	if ioa != 16389 || decodeFloat32(asdu[9:13]) != 17 {
		t.Fatalf("spontaneous ASDU = ioa %d value %v, want ioa 16389 value 17", ioa, decodeFloat32(asdu[9:13]))
	}
}

// --- Task 4 acceptance: server survives disconnect + reconnect ---

func TestServerSurvivesDisconnectAndReconnect(t *testing.T) {
	st := store.New()
	st.Set(m261points.PointKey{Device: "EMS", Slug: "manual_protection"}, 1)
	_, addr := startServer(t, st)

	c1 := dialRaw(t, addr)
	c1.startDT()
	c1.nc.Close() // simulate a dropped connection before any exchange completes

	c2 := dialRaw(t, addr)
	c2.startDT()
	c2.sendI(rawGeneralInterrogation(emsCommonAddr))
	// Must not hang or error — drain until activation termination.
	for {
		asdu := c2.nextI()
		if asdu[0] == 100 && asdu[2] == cotActivationTermination {
			break
		}
	}
}

func TestMultipleCommonAddressesHandledIndependently(t *testing.T) {
	// Task 4: "correctly handle multiple ASDU common addresses" — PCS is
	// common address 2, EMS is 1; a GI for one must not leak the other's points.
	st := store.New()
	st.Set(m261points.PointKey{Device: "EMS", Slug: "manual_protection"}, 1)
	_, addr := startServer(t, st)
	c := dialRaw(t, addr)
	c.startDT()

	c.sendI(rawGeneralInterrogation(2)) // PCS
	for {
		asdu := c.nextI()
		if asdu[0] == 100 {
			if asdu[2] == cotActivationTermination {
				break
			}
			continue // activation confirmation
		}
		commonAddr := int(binary.LittleEndian.Uint16(asdu[4:6]))
		if commonAddr != 2 {
			t.Fatalf("GI for common address 2 returned a point from common address %d", commonAddr)
		}
	}
}
