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
// review round's spontaneousLoop finding, since strengthened twice more:
//
//   - Fourth review round: spontaneousLoop re-checked
//     s.link.heartbeatOverride() fresh at the moment it actually processed
//     each queued Change (not the moment the Change was generated), but
//     broadcast that Change's own — possibly long-stale, since
//     store.Store.Subscribe's channel is buffered and best-effort —
//     c.Value whenever paused happened to be false *by then*.
//   - Fifth review round: admitHeartbeat started re-reading the *current*
//     live Store value instead of trusting c.Value, and suppressing a
//     duplicate of the last value actually admitted — closing the wrong-
//     *value* replay, but still delivering at least one frame for the
//     paused-era backlog once it finally drained after the clear.
//   - Sixth review round (this fix): a heartbeat event generated during a
//     paused generation must be discarded permanently, full stop — zero
//     frames, not "at most one correct one" — even once processed after a
//     clear. admitHeartbeat now compares each Change's own Store revision
//     (c.Rev) against the revision boundary bumpHeartbeatGeneration
//     records at every pause/clear transition, and uses c.Value directly
//     (never a fresh re-read) once a Change passes that check — see
//     heartbeat.go.
//
// Deterministically forces this exact interleaving: two Store writes
// happen while paused (queuing two Changes with stale payloads 10 and
// 20), then this test itself holds linkCoord — which spontaneousLoop's
// own per-Change check now also acquires — while ClearLinkFaults runs,
// guaranteeing both already-queued Changes are only actually processed
// after the clear. Zero spontaneous heartbeat frames may arrive for
// either of them. Only a genuinely new Change after the clear (the "next
// real heartbeat tick") must be delivered — checked explicitly, so this
// test can't be trivially satisfied by a fix that discards *everything*
// forever.
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

	readHeartbeatFrames := func(window time.Duration) (values []float32) {
		t.Helper()
		deadline := time.Now().Add(window)
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
			values = append(values, decodeFloat32(asdu[9:13]))
		}
		return values
	}

	if got := readHeartbeatFrames(500 * time.Millisecond); len(got) != 0 {
		t.Errorf("received %d spontaneous heartbeat frame(s) after the clear (%v), want zero -- both queued Changes were generated during the paused generation and must be discarded permanently, not replayed (even at the correct value) once finally processed after the clear", len(got), got)
	}

	// The "next real heartbeat tick": a genuinely new Change, generated
	// *after* the clear, must still be delivered -- proves the fix
	// discards specifically the paused-era backlog, not every future
	// heartbeat frame.
	st.Set(heartbeatKey, 30)
	got := readHeartbeatFrames(2 * time.Second)
	if len(got) != 1 || got[0] != 30 {
		t.Errorf("spontaneous heartbeat frames after a genuinely new post-clear Change = %v, want exactly [30]", got)
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

// TestFenceHeartbeatDisconnectsRatherThanSilentlyLosingABarrier is
// blocker 2's second required deterministic test (sixth review round):
// "fill the per-connection heartbeat queue, activate pause/clear/reset,
// and prove no pre-fence frame is sent after the API operation returns."
//
// Before this fix, bumpHeartbeatGeneration's own barrier enqueue used the
// same best-effort "drop silently if full" convention as an ordinary
// value admission (select/default) — meaning FenceHeartbeat could return
// having *never actually waited* for a connection whose queue happened to
// be full at exactly the wrong moment: the barrier vanished, no ack was
// ever recorded for it, and FenceHeartbeat had nothing to wait on for
// that connection at all. Silent barrier loss, forbidden by design.
//
// This fills one connection's hbQueue to capacity — first admitting one
// value (dequeued by hbLoop, which then blocks in the testBeforeHeartbeat
// -Send hook so nothing drains further), then admitting exactly
// heartbeatQueueSize more (filling the buffer completely) — then
// activates pause. The barrier enqueue must fail (queue full): asserts
// the connection is disconnected outright (never silently ignored) and
// that FenceHeartbeat still returns promptly, proving the fence was
// resolved rather than defeated.
func TestFenceHeartbeatDisconnectsRatherThanSilentlyLosingABarrier(t *testing.T) {
	st := store.New()
	heartbeatKey := m261points.PointKey{Device: "EMS", Slug: "ems_periodic_heartbeat_indicator"}
	srv, addr := startServer(t, st)
	coord := linkfault.NewCoordinator()
	srv.SetLinkCoordinator(coord)
	c := dialRaw(t, addr)
	c.startDT()
	time.Sleep(20 * time.Millisecond) // let STARTDT_CON settle before relying on "started"

	blockedInSend := make(chan struct{})
	releaseSend := make(chan struct{})
	release := sync.OnceFunc(func() { close(releaseSend) })
	defer release() // unconditional, even on a failing assertion -- see the pause-fence test's identical comment on why
	testBeforeHeartbeatSend = func() {
		close(blockedInSend)
		<-releaseSend
	}
	defer func() { testBeforeHeartbeatSend = nil }()

	// Calls admitHeartbeat directly (bypassing store.Store.Subscribe's own
	// pipeline) rather than 257 rapid st.Set calls: store.Store.publish is
	// itself best-effort against a *64*-deep subscriber channel (well
	// under heartbeatQueueSize), so a tight loop of that many Sets, faster
	// than spontaneousLoop's own goroutine could ever drain them, would
	// mostly get silently dropped *before* ever reaching admitHeartbeat --
	// never actually filling hbQueue, and defeating this test's own
	// premise. Calling the exact function spontaneousLoop itself calls
	// keeps this a faithful test of admitHeartbeat/enqueueOrDisconnect
	// while making the fill fully deterministic.
	srv.admitHeartbeat(store.Change{Key: heartbeatKey, Value: 0, Rev: 1}) // dequeued by hbLoop, which then blocks in the hook -- hbQueue itself is now empty (0 pending)
	<-blockedInSend

	for i := 1; i <= heartbeatQueueSize; i++ {
		srv.admitHeartbeat(store.Change{Key: heartbeatKey, Value: float64(i), Rev: uint64(i + 1)}) // fills hbQueue to exactly its capacity (heartbeatQueueSize pending)
	}

	srv.SetHeartbeatPause(999) // bumpHeartbeatGeneration's own barrier enqueue must now find the queue full

	fenceDone := make(chan struct{})
	go func() {
		defer close(fenceDone)
		srv.FenceHeartbeat()
	}()

	select {
	case <-fenceDone:
		// Expected: nothing to wait on for the now-disconnected connection.
	case <-time.After(2 * time.Second):
		t.Fatal("FenceHeartbeat did not return within 2s -- a silently lost barrier must not make the fence hang, and a correctly-disconnected connection has nothing left to wait on")
	}

	// A plain read *timeout* is not evidence of a disconnect -- it just
	// means nothing arrived before the deadline, which is equally true of
	// a connection that's still open but silent. Only a definite error
	// other than a timeout (EOF, connection reset, "use of closed network
	// connection") confirms the server actually closed its side.
	c.nc.SetReadDeadline(time.Now().Add(1 * time.Second))
	_, err := c.nc.Read(make([]byte, 1))
	if err == nil {
		t.Error("connection was not disconnected after its hbQueue filled up during a pause activation -- a lost barrier must result in a definite disconnect, not a connection silently left behind")
	} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Errorf("read timed out instead of erroring (%v) -- the connection was not actually disconnected after its hbQueue filled up during a pause activation, a lost barrier must result in a definite disconnect", err)
	}
}

// TestSlowWriteGetsDisconnectedInsteadOfHangingIndefinitely is blocker
// 2's third required deterministic test (sixth review round): "block an
// in-flight write and prove the operation completes by bounded
// disconnect/timeout, not indefinite waiting."
//
// Builds a clientConn directly around one side of a net.Pipe() — a
// synchronous, in-memory net.Conn implementation, so a Write on it blocks
// for exactly as long as nothing reads the other side, deterministically
// (unlike trying to fill a real TCP socket's kernel buffers, which is
// both slow and unreliable to force) — and registers it in the server's
// own connection set directly, bypassing the real TCP listener/
// acceptLoop/handleConn entirely (this connection's read side is never
// serviced, since nothing here ever calls readFrame on it — cleanup is
// therefore this test's own responsibility, done manually to mirror
// exactly what handleConn's cleanup defer would otherwise do). Shrinks
// writeDeadline first so this test doesn't have to wait out the real 5s
// production value.
//
// Never reads from the client side, so sendIfStarted's write blocks
// until withWriteDeadline's own bound trips it — asserts the call
// actually blocked for approximately writeDeadline (not less — proving
// this test's own premise, that the write really was blocking, holds),
// returns with a timeout error rather than hanging indefinitely, and
// that the connection is subsequently, actually closed.
func TestSlowWriteGetsDisconnectedInsteadOfHangingIndefinitely(t *testing.T) {
	old := writeDeadline
	writeDeadline = 300 * time.Millisecond
	t.Cleanup(func() { writeDeadline = old })

	st := store.New()
	srv, _ := startServer(t, st)

	serverSide, clientSide := net.Pipe()
	t.Cleanup(func() { clientSide.Close() })

	conn := &clientConn{srv: srv, nc: serverSide, hbQueue: make(chan hbMsg, heartbeatQueueSize)}
	conn.started.Store(true)
	srv.connMu.Lock()
	srv.conns[conn] = struct{}{}
	srv.connMu.Unlock()
	srv.wg.Add(1)
	go conn.hbLoop()
	t.Cleanup(func() {
		// Mirrors handleConn's own cleanup defer exactly (server.go) --
		// this connection never went through acceptLoop/handleConn, so
		// nothing else will ever do this for it.
		srv.connMu.Lock()
		delete(srv.conns, conn)
		close(conn.hbQueue)
		srv.connMu.Unlock()
	})

	started := time.Now()
	done := make(chan error, 1)
	go func() {
		done <- conn.sendIfStarted([]byte{0x01, 0x02, 0x03}) // arbitrary ASDU bytes -- content is irrelevant, only that it's a real write attempt
	}()

	select {
	case err := <-done:
		elapsed := time.Since(started)
		if elapsed < writeDeadline/2 {
			t.Fatalf("sendIfStarted returned after only %s, well before writeDeadline (%s) could plausibly have elapsed -- the write did not actually block on the unread pipe as this test assumes", elapsed, writeDeadline)
		}
		if err == nil {
			t.Fatal("sendIfStarted returned a nil error for a write that should have hit its deadline -- the peer (clientSide) was never read from")
		}
	case <-time.After(writeDeadline + 3*time.Second):
		t.Fatalf("sendIfStarted did not return within writeDeadline+3s (%s) -- the write is hanging indefinitely instead of being bounded", writeDeadline+3*time.Second)
	}

	// Confirm the connection was actually closed as a result of the
	// timeout, not just this one call failing on its own — a second write
	// attempt on an already-closed net.Pipe side errors immediately.
	if _, err := serverSide.Write([]byte{0x00}); err == nil {
		t.Error("server-side connection was not closed after a write hit its deadline -- a timed-out write must result in disconnecting the slow peer, not just failing this one call")
	}
}

// TestClearAtomicWithHeartbeatRevisionBoundary is the sixth review
// round's own follow-up finding: ClearLinkFaults used to flip hbFrozen to
// false, release the lock protecting it, and only *afterward* call
// store.Store.CurrentRevision() as a separate step to capture the
// generation boundary. A physics tick's own heartbeat Set could complete
// entirely in that gap — a heartbeat genuinely written *after* the clear
// already took effect, whose own Rev would then be <= the boundary
// captured a moment later, so admitHeartbeat would wrongly discard it as
// stale. The first normal tick after a clear could be silently lost.
//
// Deterministically forces exactly this window open (rather than hoping
// a stress loop finds it) via testDuringHeartbeatTransition, a hook
// applyHeartbeatTransition calls while still holding the Store's own
// write lock — after ClearLinkFaults' own state flip, before the write
// lock (and therefore the ability to compute the *next* revision) is
// released. Asserts a concurrent store.Store.Set genuinely blocks for as
// long as the transition is held open there (not just "happens to
// resolve in the right order") and, once released, that heartbeat is
// delivered exactly once — proving the fix's own precise rule: every
// Change with Rev <= boundary happened before the transition, every
// Change with Rev > boundary happened after it, with no gap either can
// fall into.
func TestClearAtomicWithHeartbeatRevisionBoundary(t *testing.T) {
	st := store.New()
	heartbeatKey := m261points.PointKey{Device: "EMS", Slug: "ems_periodic_heartbeat_indicator"}
	srv, addr := startServer(t, st)
	coord := linkfault.NewCoordinator()
	srv.SetLinkCoordinator(coord)
	c := dialRaw(t, addr)
	c.startDT()
	time.Sleep(20 * time.Millisecond) // let STARTDT_CON settle before relying on "started"

	srv.SetHeartbeatPause(0) // pause first, so ClearLinkFaults below has a real transition (paused -> unpaused) to apply

	blockedInTransition := make(chan struct{})
	releaseTransition := make(chan struct{})
	release := sync.OnceFunc(func() { close(releaseTransition) })
	defer release() // unconditional, even on a failing assertion -- see the earlier fence tests' identical comment on why
	testDuringHeartbeatTransition = func() {
		close(blockedInTransition)
		<-releaseTransition
	}
	defer func() { testDuringHeartbeatTransition = nil }()

	clearDone := make(chan struct{})
	go func() {
		defer close(clearDone)
		// Holds coord itself, matching how every real caller reaches
		// ClearLinkFaults (via linkfault.Apply/ApplyCoordinated, which
		// holds the same coordinator for the whole call) -- without this,
		// a concurrent admitHeartbeat (triggered by the Set below) could
		// acquire linkCoord and race bumpHeartbeatGeneration's own write
		// to heartbeatGenBoundaryRev, which assumes (like production)
		// that nothing else is running concurrently under linkCoord.
		coord.Lock()
		defer coord.Unlock()
		srv.ClearLinkFaults()
	}()
	<-blockedInTransition // ClearLinkFaults has flipped hbFrozen and is now parked mid-transition, still holding the Store's write lock

	// A concurrent physics-tick-style heartbeat write, attempted while the
	// transition is still open, must not be able to commit yet -- it needs
	// the same Store write lock applyHeartbeatTransition is holding.
	setDone := make(chan struct{})
	go func() {
		defer close(setDone)
		st.Set(heartbeatKey, 42)
	}()

	select {
	case <-setDone:
		t.Fatal("Store.Set completed while ClearLinkFaults' own transition was still open -- the Store write must wait for the atomic flip-plus-boundary-capture to finish")
	case <-time.After(100 * time.Millisecond):
		// Expected: Set is blocked on the Store's own write lock.
	}

	release() // let the transition finish (capturing the boundary), which also unblocks the waiting Set
	<-clearDone
	<-setDone

	// The heartbeat (42) could only have committed strictly *after* the
	// clear's boundary was captured (it was blocked until then) -- it is a
	// genuine post-clear event and must be delivered exactly once.
	var got []float32
	deadline := time.Now().Add(2 * time.Second)
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
		got = append(got, decodeFloat32(asdu[9:13]))
	}
	if len(got) != 1 || got[0] != 42 {
		t.Errorf("post-clear heartbeat frames = %v, want exactly [42] -- a heartbeat that committed during the clear's own atomic transition must be delivered exactly once, never lost", got)
	}
}
