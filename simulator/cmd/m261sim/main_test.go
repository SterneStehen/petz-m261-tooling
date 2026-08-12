package main

import (
	"encoding/binary"
	"io"
	"math"
	"net"
	"testing"
	"time"

	gomodbus "github.com/goburrow/modbus"

	"github.com/SterneStehen/petz-m261-tooling/gen/go/m261points"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/config"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/iec104"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/modbustcp"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/store"
)

func TestResolveByteOrderDefaultsToConfigValue(t *testing.T) {
	// No CLI override: whatever the loaded config says wins — this is the
	// default/no-override half of the behavior the config file exists for.
	order, resolved, err := resolveByteOrder(config.Default(), "")
	if err != nil || order != m261points.BigEndian {
		t.Fatalf("resolveByteOrder(Default(), \"\") = %v, %v; want BigEndian, nil", order, err)
	}
	if resolved.Modbus.ByteOrder.Value != "big" {
		t.Errorf("resolved config value = %q, want %q", resolved.Modbus.ByteOrder.Value, "big")
	}
}

func TestResolveByteOrderCLIOverridesConfig(t *testing.T) {
	order, resolved, err := resolveByteOrder(config.Default(), "little_word_swap")
	if err != nil || order != m261points.LittleEndianWordSwap {
		t.Fatalf("resolveByteOrder(Default(), \"little_word_swap\") = %v, %v; want LittleEndianWordSwap, nil", order, err)
	}
	if resolved.Modbus.ByteOrder.Value != "little_word_swap" {
		t.Errorf("resolved config value = %q, want %q", resolved.Modbus.ByteOrder.Value, "little_word_swap")
	}
}

func TestResolveByteOrderRejectsInvalidOverride(t *testing.T) {
	if _, _, err := resolveByteOrder(config.Default(), "nonsense"); err == nil {
		t.Error("resolveByteOrder with an out-of-enum override returned nil error")
	}
}

// TestBothServersShareOneStore is the full Task 4 wiring test: one store,
// both servers, exactly as main() constructs them — a Modbus write is
// read back via a real IEC-104 client, and an IEC-104 write is read back
// via a real Modbus client. Each direction is also covered within
// modbustcp's and iec104's own package tests against a bare store; this
// proves the two servers are actually wired to the SAME store together,
// not just individually correct in isolation.
func TestBothServersShareOneStore(t *testing.T) {
	st := store.New()

	mb := modbustcp.New(st, modbustcp.Config{Addr: "127.0.0.1:0", ByteOrder: m261points.BigEndian})
	if err := mb.Start(); err != nil {
		t.Fatalf("modbustcp Start: %v", err)
	}
	t.Cleanup(func() { mb.Close() })

	iec := iec104.New(st, iec104.Config{Addr: "127.0.0.1:0"})
	if err := iec.Start(); err != nil {
		t.Fatalf("iec104 Start: %v", err)
	}
	t.Cleanup(func() { iec.Close() })

	t.Run("modbus write visible via IEC-104", func(t *testing.T) {
		handler := gomodbus.NewTCPClientHandler(mb.Addr().String())
		handler.SlaveId = 1 // EMS
		handler.Timeout = 2 * time.Second
		if err := handler.Connect(); err != nil {
			t.Fatalf("modbus Connect: %v", err)
		}
		defer handler.Close()
		client := gomodbus.NewClient(handler)

		buf := make([]byte, 4)
		binary.BigEndian.PutUint32(buf, math.Float32bits(-55))
		// EMS Set Active Power: modbus_addr 40153 -> wire address 40153-40001=152
		if _, err := client.WriteMultipleRegisters(152, 2, buf); err != nil {
			t.Fatalf("WriteMultipleRegisters: %v", err)
		}

		// EMS Set Active Power's readback is IEC-104 address 16487.
		c := dialRawIEC(t, iec.Addr().String())
		c.startDT()
		c.sendGeneralInterrogation(1)
		got, ok := c.waitForFloat(16487)
		if !ok || got != -55 {
			t.Fatalf("IEC-104 readback = %v, %v; want -55, true", got, ok)
		}
	})

	t.Run("IEC-104 write visible via Modbus", func(t *testing.T) {
		c := dialRawIEC(t, iec.Addr().String())
		c.startDT()
		c.sendSetpointCommand(1, 25165, 63.5) // EMS Set Active Power
		c.expectActivationConfirmation()

		handler := gomodbus.NewTCPClientHandler(mb.Addr().String())
		handler.SlaveId = 1
		handler.Timeout = 2 * time.Second
		if err := handler.Connect(); err != nil {
			t.Fatalf("modbus Connect: %v", err)
		}
		defer handler.Close()
		client := gomodbus.NewClient(handler)

		regs, err := client.ReadHoldingRegisters(152, 2)
		if err != nil {
			t.Fatalf("ReadHoldingRegisters: %v", err)
		}
		if got := math.Float32frombits(binary.BigEndian.Uint32(regs)); got != 63.5 {
			t.Fatalf("Modbus readback = %v, want 63.5", got)
		}
	})
}

// --- minimal, self-contained raw IEC-104 client for this integration test ---

type rawIEC struct {
	t       *testing.T
	nc      net.Conn
	sendSeq uint16
	recvSeq uint16
}

func dialRawIEC(t *testing.T, addr string) *rawIEC {
	t.Helper()
	nc, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	nc.SetDeadline(time.Now().Add(5 * time.Second))
	t.Cleanup(func() { nc.Close() })
	return &rawIEC{t: t, nc: nc}
}

func (c *rawIEC) writeRaw(control, asdu []byte) {
	c.t.Helper()
	buf := make([]byte, 2+len(control)+len(asdu))
	buf[0], buf[1] = 0x68, byte(len(control)+len(asdu))
	copy(buf[2:], control)
	copy(buf[2+len(control):], asdu)
	if _, err := c.nc.Write(buf); err != nil {
		c.t.Fatalf("write: %v", err)
	}
}

func (c *rawIEC) readFrame() (control, asdu []byte) {
	c.t.Helper()
	header := make([]byte, 2)
	if _, err := io.ReadFull(c.nc, header); err != nil {
		c.t.Fatalf("read header: %v", err)
	}
	rest := make([]byte, header[1])
	if _, err := io.ReadFull(c.nc, rest); err != nil {
		c.t.Fatalf("read body: %v", err)
	}
	return rest[0:4], rest[4:]
}

func (c *rawIEC) startDT() {
	c.writeRaw([]byte{0x07, 0x00, 0x00, 0x00}, nil)
	control, _ := c.readFrame()
	if control[0] != 0x0B {
		c.t.Fatalf("expected STARTDT_CON, got control 0x%02x", control[0])
	}
}

func (c *rawIEC) sendI(asdu []byte) {
	sLo, sHi := byte(c.sendSeq<<1), byte(c.sendSeq>>7)
	rLo, rHi := byte(c.recvSeq<<1), byte(c.recvSeq>>7)
	c.writeRaw([]byte{sLo, sHi, rLo, rHi}, asdu)
	c.sendSeq++
}

func (c *rawIEC) nextI() []byte {
	c.t.Helper()
	for {
		control, asdu := c.readFrame()
		if control[0]&0x01 == 0 {
			c.recvSeq = ((uint16(control[1])<<8 | uint16(control[0])) >> 1) + 1
			return asdu
		}
	}
}

func (c *rawIEC) sendGeneralInterrogation(commonAddr int) {
	asdu := make([]byte, 10)
	asdu[0], asdu[1], asdu[2] = 100, 1, 6
	binary.LittleEndian.PutUint16(asdu[4:6], uint16(commonAddr))
	asdu[9] = 20
	c.sendI(asdu)
}

func (c *rawIEC) sendSetpointCommand(commonAddr, ioa int, value float32) {
	asdu := make([]byte, 14)
	asdu[0], asdu[1], asdu[2] = 50, 1, 6
	binary.LittleEndian.PutUint16(asdu[4:6], uint16(commonAddr))
	asdu[6], asdu[7], asdu[8] = byte(ioa), byte(ioa>>8), byte(ioa>>16)
	binary.LittleEndian.PutUint32(asdu[9:13], math.Float32bits(value))
	c.sendI(asdu)
}

// expectActivationConfirmation scans past anything else (a spontaneous
// transmission from the readback mirror can legitimately race with the
// command's own confirmation on this same connection — a real client
// can't assume its ack is literally the next frame) to find the C_SE_NC_1
// activation confirmation.
func (c *rawIEC) expectActivationConfirmation() {
	c.t.Helper()
	for i := 0; i < 50; i++ {
		asdu := c.nextI()
		if asdu[0] == 50 { // C_SE_NC_1
			if asdu[2] != 7 { // COT activation confirmation
				c.t.Fatalf("expected C_SE_NC_1 activation confirmation, got cot=%d", asdu[2])
			}
			return
		}
	}
	c.t.Fatal("did not see a C_SE_NC_1 confirmation after 50 frames")
}

// waitForFloat drains I-frames (general interrogation response) until it
// finds an M_ME_NC_1 for the given IOA, or the interrogation ends.
func (c *rawIEC) waitForFloat(ioa int) (float32, bool) {
	c.t.Helper()
	for {
		asdu := c.nextI()
		if asdu[0] == 100 {
			if asdu[2] == 10 { // activation termination
				return 0, false
			}
			continue
		}
		gotIOA := int(asdu[6]) | int(asdu[7])<<8 | int(asdu[8])<<16
		if asdu[0] == 13 && gotIOA == ioa { // M_ME_NC_1
			return math.Float32frombits(binary.LittleEndian.Uint32(asdu[9:13])), true
		}
	}
}
