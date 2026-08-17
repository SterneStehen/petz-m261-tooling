package iec104

import (
	"encoding/binary"
	"io"
	"math"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/SterneStehen/petz-m261-tooling/gen/go/m261points"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/linkfault"
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

// tryNextI is nextI's non-fatal counterpart — used by tests that expect
// (and must tolerate) a read timeout as a normal "no more frames right
// now" outcome, not a test failure: more == false means the read failed
// (timeout, EOF, malformed header — the caller's read deadline is what
// actually bounds this, not a distinction this helper makes), isIFrame
// == false with more == true means an S/U-frame was read and skipped,
// keep calling.
func (c *rawClient) tryNextI() (asdu []byte, isIFrame bool, more bool) {
	header := make([]byte, 2)
	if _, err := io.ReadFull(c.nc, header); err != nil {
		return nil, false, false
	}
	if header[0] != 0x68 {
		return nil, false, false
	}
	rest := make([]byte, header[1])
	if _, err := io.ReadFull(c.nc, rest); err != nil {
		return nil, false, false
	}
	if rest[0]&0x01 == 0 { // I-format
		return rest[4:], true, true
	}
	return nil, false, true // S/U frame — not an error, just not an I-frame
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

// --- Code-review finding: only the 148 EMS setpoints may be written via
// an IEC-104 command — every alarm and telemetry point, on any device
// including EMS itself, must reject a command with a negative
// confirmation and leave the store untouched. ---

func TestWriteRejectedForEMSTelemetry(t *testing.T) {
	st := store.New()
	key := m261points.PointKey{Device: "EMS", Slug: "desired_active_power_kw"}
	st.Set(key, 11) // known value, to prove a rejected write doesn't touch it
	_, addr := startServer(t, st)
	c := dialRaw(t, addr)
	c.startDT()

	// EMS Desired Active Power (kW) is telemetry (RO), IOA 16389 — not a
	// command point at all, despite belonging to EMS.
	c.sendI(rawSetpointCommand(emsCommonAddr, 16389, 999))
	ack := c.nextI()
	if ack[2]&cotNegativeFlag == 0 {
		t.Fatalf("expected the P/N (negative) bit set in COT 0x%02x writing to EMS telemetry", ack[2])
	}
	if v, ok := st.Get(key); !ok || v != 11 {
		t.Fatalf("store value after rejected write = %v, %v; want unchanged 11, true", v, ok)
	}
}

func TestWriteRejectedForNonEMSDevice(t *testing.T) {
	st := store.New()
	key := m261points.PointKey{Device: "PCS", Slug: "phase_a_voltage_v"}
	st.Set(key, 231) // known value
	_, addr := startServer(t, st)
	c := dialRaw(t, addr)
	c.startDT()

	const pcsCommonAddr = 2
	// PCS Phase A Voltage (V), IOA 16385 — a real point, but on PCS, not EMS.
	c.sendI(rawSetpointCommand(pcsCommonAddr, 16385, 999))
	ack := c.nextI()
	if ack[2]&cotNegativeFlag == 0 {
		t.Fatalf("expected the P/N (negative) bit set in COT 0x%02x writing to a PCS point", ack[2])
	}
	if v, ok := st.Get(key); !ok || v != 231 {
		t.Fatalf("store value after rejected write = %v, %v; want unchanged 231, true", v, ok)
	}
}

func TestWriteRejectedForEMSAlarm(t *testing.T) {
	st := store.New()
	key := m261points.PointKey{Device: "EMS", Slug: "manual_protection"}
	st.Set(key, 0)
	_, addr := startServer(t, st)
	c := dialRaw(t, addr)
	c.startDT()

	// EMS Manual Protection is an alarm (IOA 1) — real, on EMS, still not writable.
	c.sendI(rawSingleCommand(emsCommonAddr, 1, true))
	ack := c.nextI()
	if ack[2]&cotNegativeFlag == 0 {
		t.Fatalf("expected the P/N (negative) bit set in COT 0x%02x writing to an EMS alarm", ack[2])
	}
	if v, ok := st.Get(key); !ok || v != 0 {
		t.Fatalf("store value after rejected write = %v, %v; want unchanged 0, true", v, ok)
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

// --- Task 7 item 2: link faults --------------------------------------------

// heartbeatIOA is EMS Periodic Heartbeat Indicator's IEC-104 address
// (iec104_addr 16400), verified against the real catalog.
const heartbeatIOA = 16400

// giValue runs a general interrogation on an already-STARTDT'd connection
// and returns the M_ME_NC_1 value for the given IOA, or (0, false) if it
// wasn't present in the response. Always drains all the way through
// activation termination, even after finding ioa — leaving unread
// trailing frames in the socket buffer would corrupt a later call on the
// same connection (its sendI would race the still-pending response of
// this call).
func giValue(c *rawClient, commonAddr, ioa int) (float32, bool) {
	c.sendI(rawGeneralInterrogation(commonAddr))
	value, found := float32(0), false
	for {
		asdu := c.nextI()
		typeID, cot := asdu[0], asdu[2]
		if typeID == 100 && cot == cotActivationTermination {
			return value, found
		}
		if typeID == 100 {
			continue
		}
		gotIOA := int(asdu[6]) | int(asdu[7])<<8 | int(asdu[8])<<16
		if typeID == typeMMENC1 && gotIOA == ioa {
			value, found = decodeFloat32(asdu[9:13]), true
		}
	}
}

func TestIEC104LinkDropClosesExistingAndRefusesNewConnections(t *testing.T) {
	st := store.New()
	srv, addr := startServer(t, st)
	c := dialRaw(t, addr)
	c.startDT()

	srv.SetDrop()

	c.nc.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	if _, err := c.nc.Read(make([]byte, 1)); err == nil {
		t.Error("read on a connection open before SetDrop succeeded, want an error (connection force-closed)")
	}

	nc2, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
	if err == nil {
		defer nc2.Close()
		nc2.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
		if _, rerr := nc2.Read(make([]byte, 1)); rerr == nil {
			t.Error("new connection accepted and answered while drop is active, want refused")
		}
	}

	srv.ClearLinkFaults()
	c2 := dialRaw(t, addr)
	c2.startDT() // must complete normally now — fails the test (via t.Fatalf) otherwise
}

func TestIEC104LinkHangSendsNoResponse(t *testing.T) {
	st := store.New()
	srv, addr := startServer(t, st)
	c := dialRaw(t, addr)
	c.startDT()

	srv.SetHang()
	c.sendI(rawGeneralInterrogation(emsCommonAddr))
	c.nc.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	if _, err := c.nc.Read(make([]byte, 1)); err == nil {
		t.Error("read while hanging returned data, want a timeout (no response of any kind)")
	}

	srv.ClearLinkFaults()
	c2 := dialRaw(t, addr)
	c2.startDT()
	if _, ok := giValue(c2, emsCommonAddr, 16389); !ok {
		// desired_active_power_kw, always present — just proving a fresh
		// connection works normally once hang is cleared.
		t.Error("GI on a fresh connection after ClearLinkFaults saw no telemetry")
	}
}

func TestIEC104LinkDelayDelaysResponse(t *testing.T) {
	st := store.New()
	srv, addr := startServer(t, st)
	c := dialRaw(t, addr)
	c.startDT()

	const delay = 200 * time.Millisecond
	srv.SetDelay(delay)

	start := time.Now()
	c.sendI(rawGeneralInterrogation(emsCommonAddr))
	c.nextI() // the activation confirmation — first frame back
	if elapsed := time.Since(start); elapsed < delay {
		t.Errorf("first response frame arrived after %s, want at least %s (SetDelay)", elapsed, delay)
	}
	// Drain the rest of this GI so it doesn't bleed into other assertions.
	for {
		asdu := c.nextI()
		if asdu[0] == 100 && asdu[2] == cotActivationTermination {
			break
		}
	}

	srv.ClearLinkFaults()
	start = time.Now()
	c.sendI(rawGeneralInterrogation(emsCommonAddr))
	c.nextI()
	if elapsed := time.Since(start); elapsed >= delay {
		t.Errorf("first response frame after ClearLinkFaults took %s, want well under %s (delay cleared)", elapsed, delay)
	}
}

func TestIEC104LinkHeartbeatPauseFreezesGeneralInterrogation(t *testing.T) {
	st := store.New()
	heartbeatKey := m261points.PointKey{Device: "EMS", Slug: "ems_periodic_heartbeat_indicator"}
	st.Set(heartbeatKey, 10)
	srv, addr := startServer(t, st)
	c := dialRaw(t, addr)
	c.startDT()

	srv.SetHeartbeatPause(10)
	st.Set(heartbeatKey, 99) // the live simulator keeps incrementing underneath

	got, ok := giValue(c, emsCommonAddr, heartbeatIOA)
	if !ok {
		t.Fatal("GI response didn't include the heartbeat point")
	}
	if got != 10 {
		t.Errorf("heartbeat via GI while paused = %v, want the frozen value 10 (live Store value is 99)", got)
	}

	srv.ClearLinkFaults()
	got, ok = giValue(c, emsCommonAddr, heartbeatIOA)
	if !ok {
		t.Fatal("GI response after ClearLinkFaults didn't include the heartbeat point")
	}
	if got != 99 {
		t.Errorf("heartbeat via GI after ClearLinkFaults = %v, want the live value 99", got)
	}
}

func TestIEC104LinkHeartbeatPauseSuppressesSpontaneousTransmission(t *testing.T) {
	st := store.New()
	heartbeatKey := m261points.PointKey{Device: "EMS", Slug: "ems_periodic_heartbeat_indicator"}
	srv, addr := startServer(t, st)
	c := dialRaw(t, addr)
	c.startDT()
	time.Sleep(20 * time.Millisecond) // let the STARTDT_CON settle before relying on "started"

	srv.SetHeartbeatPause(0)
	st.Set(heartbeatKey, 1) // would normally trigger a spontaneous M_ME_NC_1

	c.nc.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	if _, err := c.nc.Read(make([]byte, 1)); err == nil {
		t.Error("received a frame after a Store change to a paused heartbeat point, want none (suppressed)")
	}
}

// TestSpontaneousLoopNeverReplaysStaleHeartbeatAfterClear is the fourth
// review round's spontaneousLoop finding: it re-checked
// s.link.heartbeatOverride() fresh at the moment it actually processed
// each queued Change (not the moment the Change was generated), but
// broadcast that Change's own — possibly long-stale, since
// store.Store.Subscribe's channel is buffered and best-effort — c.Value
// whenever paused happened to be false *by then*. A Change generated
// *while paused* (which should stay permanently invisible — the whole
// point of heartbeat_pause) could still be replayed to the client as a
// distinct spontaneous update once spontaneousLoop finally drained it,
// if a concurrent clear landed first.
//
// Deterministically forces this exact interleaving: two Store writes
// happen while paused (queuing two Changes with stale payloads 10 and
// 20), then this test itself holds linkCoord — which spontaneousLoop's
// own per-Change check now also acquires — while ClearLinkFaults runs,
// guaranteeing both already-queued Changes are only actually processed
// after the clear. Every spontaneous heartbeat value the client receives
// afterward must equal the final live value (20) — never the first
// queued Change's stale payload (10) — and, fifth-review-round addition
// (admitHeartbeat's duplicate suppression, heartbeat.go): the two queued
// Changes, once both processed after the clear and both resolving to the
// identical live value 20, must produce at most *one* spontaneous frame,
// not two — "paused-era events are still emitted as duplicates after
// clear" was the review's own wording for exactly this residual gap in
// round four's fix (which only corrected the *value*, not the count).
func TestSpontaneousLoopNeverReplaysStaleHeartbeatAfterClear(t *testing.T) {
	st := store.New()
	heartbeatKey := m261points.PointKey{Device: "EMS", Slug: "ems_periodic_heartbeat_indicator"}
	srv, addr := startServer(t, st)
	coord := linkfault.NewCoordinator()
	srv.SetLinkCoordinator(coord)
	c := dialRaw(t, addr)
	c.startDT()
	time.Sleep(20 * time.Millisecond) // let STARTDT_CON settle before relying on "started"

	srv.SetHeartbeatPause(0)
	st.Set(heartbeatKey, 10) // queued Change #1, generated while paused
	st.Set(heartbeatKey, 20) // queued Change #2, generated while paused -- neither processed by spontaneousLoop yet

	// Block spontaneousLoop's own per-Change check (it now also acquires
	// coord) for the whole clear, forcing both of the Changes queued
	// above to only be processed once paused is already false.
	coord.Lock()
	srv.ClearLinkFaults()
	coord.Unlock()

	var sawStale bool
	var heartbeatFrames int
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		c.nc.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		asdu, isIFrame, more := c.tryNextI()
		if !more {
			break
		}
		if !isIFrame || asdu[0] != typeMMENC1 || asdu[2] != cotSpontaneous {
			continue
		}
		ioa := int(asdu[6]) | int(asdu[7])<<8 | int(asdu[8])<<16
		if ioa != heartbeatIOA {
			continue
		}
		heartbeatFrames++
		if decodeFloat32(asdu[9:13]) == 10 {
			sawStale = true
		}
	}
	if sawStale {
		t.Error("received a spontaneous heartbeat update for the stale, paused-era value 10 after a concurrent clear -- a replay of a reading that should have stayed permanently invisible")
	}
	if heartbeatFrames > 1 {
		t.Errorf("received %d spontaneous heartbeat frames after the clear, want at most 1 -- two paused-era Changes resolving to the same live value must not each produce their own duplicate frame", heartbeatFrames)
	}
}

// TestFenceHeartbeatWaitsForAnAlreadyAdmittedFrameBeforePauseCompletes is
// blocker 2's second required deterministic test (fifth review round):
// "block the outbound send path after it has read 'not paused'; activate
// pause; release the send path; assert no old heartbeat frame is
// delivered [after the pause is reported complete]." Round four's own
// fix (re-resolving "paused?" right before building the ASDU) narrowed
// the window between that check and the actual socket write, but never
// closed it: SetHeartbeatPause's own caller could still report the pause
// active while an already-admitted, pre-pause frame was still sitting
// unsent. This uses testBeforeHeartbeatSend to hold that exact seam open
// — a real frame has been admitted (resolved as "not paused", value 42)
// and dequeued by hbLoop, but not yet handed to sendIfStarted — then
// activates pause and asserts FenceHeartbeat (every real caller's
// required post-Apply step) does not return until that frame has
// actually been sent, never before.
func TestFenceHeartbeatWaitsForAnAlreadyAdmittedFrameBeforePauseCompletes(t *testing.T) {
	st := store.New()
	srv, addr := startServer(t, st)
	coord := linkfault.NewCoordinator()
	srv.SetLinkCoordinator(coord)
	c := dialRaw(t, addr)
	c.startDT()
	time.Sleep(20 * time.Millisecond) // let STARTDT_CON settle before relying on "started"

	blockedInSend := make(chan struct{})
	releaseSend := make(chan struct{})
	release := sync.OnceFunc(func() { close(releaseSend) })
	// Unconditionally released on return, even via t.Fatal (which exits
	// this goroutine via runtime.Goexit without running the rest of this
	// function's own body) -- otherwise a failing assertion below would
	// leave hbLoop permanently parked inside the hook, and t.Cleanup's
	// srv.Close (which waits for every server goroutine, hbLoop included)
	// would hang for the rest of the test binary's own timeout instead of
	// this test failing fast and cleanly.
	defer release()
	testBeforeHeartbeatSend = func() {
		close(blockedInSend)
		<-releaseSend
	}
	defer func() { testBeforeHeartbeatSend = nil }()

	st.Set(linkfault.HeartbeatKey, 42) // admitted while not paused -- queued, then parked at the hook
	<-blockedInSend                    // hbLoop has dequeued it and is about to call sendIfStarted

	srv.SetHeartbeatPause(999) // activates pause while the admitted frame is still unsent

	fenceDone := make(chan struct{})
	go func() {
		defer close(fenceDone)
		srv.FenceHeartbeat()
	}()

	select {
	case <-fenceDone:
		t.Fatal("FenceHeartbeat returned before the already-admitted heartbeat frame (value 42) was actually sent -- the pause must not be reported complete while an earlier frame is still in flight")
	case <-time.After(100 * time.Millisecond):
		// Expected: FenceHeartbeat is still blocked on the barrier's ack.
	}

	release() // let hbLoop actually send the frame and reach the barrier behind it

	select {
	case <-fenceDone:
	case <-time.After(2 * time.Second):
		t.Fatal("FenceHeartbeat did not return within 2s after releasing the blocked send -- deadlocked, or lost the barrier ack")
	}

	c.nc.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	asdu, isIFrame, more := c.tryNextI()
	if !more || !isIFrame || asdu[0] != typeMMENC1 {
		t.Fatalf("expected a spontaneous heartbeat I-frame once the send was released, got isIFrame=%v more=%v", isIFrame, more)
	}
	ioa := int(asdu[6]) | int(asdu[7])<<8 | int(asdu[8])<<16
	if ioa != heartbeatIOA {
		t.Fatalf("IOA = %d, want the heartbeat point's IOA %d", ioa, heartbeatIOA)
	}
	if got := decodeFloat32(asdu[9:13]); got != 42 {
		t.Errorf("delivered heartbeat value = %v, want 42 (the pre-pause admitted value) -- it must be sent, not silently discarded, just strictly before the fence returns", got)
	}
}

// TestFenceHeartbeatWaitsForAnAlreadyAdmittedFrameBeforeClearCompletes is
// blocker 2's third required deterministic test — "repeat the cutoff
// guarantee for POST /reset" — applied to ClearLinkFaults, the exact
// call controlapi.Server.doReset makes (via linkfault.Apply) as its own
// step 7, immediately before calling FenceHeartbeat (doReset's step 8).
// ClearLinkFaults calls the identical bumpHeartbeatGeneration fencing
// step SetHeartbeatPause does (see linkstate.go), so this is the same
// scenario as TestFenceHeartbeatWaitsForAnAlreadyAdmittedFrameBefore
// PauseCompletes with the pause/clear roles matched to what a real reset
// actually calls, proving reset gets the identical cutoff guarantee
// rather than a merely-assumed-identical one.
func TestFenceHeartbeatWaitsForAnAlreadyAdmittedFrameBeforeClearCompletes(t *testing.T) {
	st := store.New()
	srv, addr := startServer(t, st)
	coord := linkfault.NewCoordinator()
	srv.SetLinkCoordinator(coord)
	c := dialRaw(t, addr)
	c.startDT()
	time.Sleep(20 * time.Millisecond) // let STARTDT_CON settle before relying on "started"

	blockedInSend := make(chan struct{})
	releaseSend := make(chan struct{})
	release := sync.OnceFunc(func() { close(releaseSend) })
	defer release() // see the pause variant's identical comment on why this must be unconditional
	testBeforeHeartbeatSend = func() {
		close(blockedInSend)
		<-releaseSend
	}
	defer func() { testBeforeHeartbeatSend = nil }()

	st.Set(linkfault.HeartbeatKey, 77) // admitted while not paused -- queued, then parked at the hook
	<-blockedInSend                    // hbLoop has dequeued it and is about to call sendIfStarted

	srv.ClearLinkFaults() // mirrors doReset's own step 7 -- a reset with nothing active to clear is still a real, bump-worthy call

	fenceDone := make(chan struct{})
	go func() {
		defer close(fenceDone)
		srv.FenceHeartbeat() // mirrors doReset's own step 8
	}()

	select {
	case <-fenceDone:
		t.Fatal("FenceHeartbeat returned before the already-admitted heartbeat frame (value 77) was actually sent -- a reset must not be reported complete while an earlier frame is still in flight")
	case <-time.After(100 * time.Millisecond):
		// Expected: FenceHeartbeat is still blocked on the barrier's ack.
	}

	release() // let hbLoop actually send the frame and reach the barrier behind it

	select {
	case <-fenceDone:
	case <-time.After(2 * time.Second):
		t.Fatal("FenceHeartbeat did not return within 2s after releasing the blocked send -- deadlocked, or lost the barrier ack")
	}

	c.nc.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	asdu, isIFrame, more := c.tryNextI()
	if !more || !isIFrame || asdu[0] != typeMMENC1 {
		t.Fatalf("expected a spontaneous heartbeat I-frame once the send was released, got isIFrame=%v more=%v", isIFrame, more)
	}
	ioa := int(asdu[6]) | int(asdu[7])<<8 | int(asdu[8])<<16
	if ioa != heartbeatIOA {
		t.Fatalf("IOA = %d, want the heartbeat point's IOA %d", ioa, heartbeatIOA)
	}
	if got := decodeFloat32(asdu[9:13]); got != 77 {
		t.Errorf("delivered heartbeat value = %v, want 77 (the pre-reset admitted value) -- it must be sent, not silently discarded, just strictly before the fence returns", got)
	}
}
