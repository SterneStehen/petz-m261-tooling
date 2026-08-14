// Package modbustcp implements a minimal Modbus TCP server (Task 4) over
// simulator/internal/store: function codes 02 (read discrete inputs), 03
// (read holding registers), 04 (read input registers), 06 (write single
// register), 16 (write multiple registers), routed by Unit ID == device
// address (§4.1).
//
// No suitable ready-made Go server library was found for this (checked
// goburrow/modbus: BSD-3, 1000+ stars, but client-only, no server code at
// all; rinzlerlabs/gomodbus: MIT, has a real TCP server, but the
// maintainer's own README says "APIs are not yet stable" and it has zero
// external adoption; simpleiot/simpleiot's modbus package: Apache-2.0,
// actively maintained, but its Server explicitly documents "currently
// only supports Modbus RTU" — no TCP server at all) — implemented
// directly per Task 4's IEC-104 fallback clause, extended to Modbus for
// the same reasons.
package modbustcp

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/SterneStehen/petz-m261-tooling/gen/go/m261points"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/commands"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/store"
)

// Config holds the parameters AGENT-TASK §7 marks unconfirmed by the
// manufacturer. ByteOrder defaults to m261points.BigEndian if left zero.
type Config struct {
	Addr      string // e.g. "127.0.0.1:502" or ":502"
	ByteOrder m261points.ByteOrder

	// Commands, if non-nil, routes every setpoint write through Task 6's
	// validation/mode-arbitration/dangerous-gating layer instead of
	// writing the store directly. nil falls back to the pre-Task-6
	// behavior (write straight through) — used by protocol-level tests in
	// this package that predate Task 6. main.go always wires a real
	// *commands.Processor.
	Commands *commands.Processor
}

// Server is a Modbus TCP server backed by a shared store.Store. Multiple
// Servers (and an iec104.Server) can share one Store — a write via one
// protocol is a write to the same underlying value the other protocol
// reads.
type Server struct {
	cfg   Config
	store *store.Store
	slots map[regSlotKey]registerSlot

	ln     net.Listener
	wg     sync.WaitGroup
	quit   chan struct{}
	connMu sync.Mutex
	conns  map[net.Conn]struct{}

	link linkState // Task 7 item 2 — see linkstate.go
}

func New(st *store.Store, cfg Config) *Server {
	return &Server{
		cfg:   cfg,
		store: st,
		slots: buildRegisterSlots(),
		quit:  make(chan struct{}),
		conns: make(map[net.Conn]struct{}),
	}
}

// Start binds the listener and begins serving in the background. Safe to
// call once; Addr() is only valid after Start returns nil.
func (s *Server) Start() error {
	ln, err := net.Listen("tcp", s.cfg.Addr)
	if err != nil {
		return fmt.Errorf("modbustcp: listen: %w", err)
	}
	s.ln = ln
	s.wg.Add(1)
	go s.acceptLoop()
	return nil
}

// Addr returns the listener's actual address (useful with Addr: "127.0.0.1:0" in tests).
func (s *Server) Addr() net.Addr { return s.ln.Addr() }

// Close stops accepting new connections, closes every open connection,
// and waits for their handler goroutines to exit.
func (s *Server) Close() error {
	close(s.quit)
	err := s.ln.Close()
	s.connMu.Lock()
	for c := range s.conns {
		c.Close()
	}
	s.connMu.Unlock()
	s.wg.Wait()
	return err
}

func (s *Server) acceptLoop() {
	defer s.wg.Done()
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return // listener closed by Close(), or a fatal accept error either way
		}
		// The dropped() check and the registration below must happen as
		// one critical section under connMu — the same lock SetDrop's own
		// close-everything-currently-registered loop takes. Checking
		// dropped() first and registering separately (the original
		// version) left a window: SetDrop could run its close loop
		// between the two, missing this connection entirely because it
		// wasn't registered yet, and leaving it to be registered and
		// served normally right after — a connection that should have
		// been refused instead survives drop being activated. Serializing
		// both under connMu with SetDrop closes that window: whichever of
		// "this registers" or "SetDrop's close loop runs" happens first,
		// the other is guaranteed to observe it (drop already true, or
		// the connection already in conns to be closed).
		s.connMu.Lock()
		if s.link.dropped() {
			s.connMu.Unlock()
			conn.Close() //nolint:errcheck
			continue
		}
		s.conns[conn] = struct{}{}
		s.connMu.Unlock()
		s.wg.Add(1)
		go s.handleConn(conn)
	}
}

func (s *Server) handleConn(conn net.Conn) {
	defer s.wg.Done()
	defer func() {
		s.connMu.Lock()
		delete(s.conns, conn)
		s.connMu.Unlock()
		conn.Close()
	}()

	for {
		req, err := readMBAPFrame(conn)
		if err != nil {
			return // client disconnected, or malformed frame — either way, drop this connection
		}
		if s.link.hanging() {
			// Task 7 item 2's hang mode: connection stays open (no
			// Close, unlike drop), but this request — and every other
			// one received while still hanging — never gets a response.
			continue
		}
		if d := s.link.responseDelay(); d > 0 {
			time.Sleep(d) // I/O-layer latency simulation, not a business-logic time decision
		}
		resp := s.handleRequest(req)
		if _, err := conn.Write(resp); err != nil {
			return
		}
	}
}

type mbapRequest struct {
	TransactionID uint16
	UnitID        byte
	PDU           []byte
}

// readMBAPFrame reads one MBAP header (7 bytes: transaction id, protocol
// id, length, unit id) plus its PDU.
func readMBAPFrame(r io.Reader) (mbapRequest, error) {
	header := make([]byte, 7)
	if _, err := io.ReadFull(r, header); err != nil {
		return mbapRequest{}, err
	}
	transactionID := binary.BigEndian.Uint16(header[0:2])
	protocolID := binary.BigEndian.Uint16(header[2:4])
	length := binary.BigEndian.Uint16(header[4:6])
	unitID := header[6]
	if protocolID != 0 {
		return mbapRequest{}, fmt.Errorf("modbustcp: unexpected protocol id %d", protocolID)
	}
	if length < 1 || length > 253 {
		return mbapRequest{}, fmt.Errorf("modbustcp: invalid MBAP length %d", length)
	}
	pdu := make([]byte, length-1) // length counts unit id (already read) + PDU
	if _, err := io.ReadFull(r, pdu); err != nil {
		return mbapRequest{}, err
	}
	return mbapRequest{TransactionID: transactionID, UnitID: unitID, PDU: pdu}, nil
}

func buildMBAPResponse(transactionID uint16, unitID byte, pdu []byte) []byte {
	out := make([]byte, 7+len(pdu))
	binary.BigEndian.PutUint16(out[0:2], transactionID)
	binary.BigEndian.PutUint16(out[2:4], 0) // protocol id
	binary.BigEndian.PutUint16(out[4:6], uint16(1+len(pdu)))
	out[6] = unitID
	copy(out[7:], pdu)
	return out
}

func (s *Server) handleRequest(req mbapRequest) []byte {
	var respPDU []byte
	if len(req.PDU) == 0 {
		respPDU = exceptionPDU(0, excIllegalFunction)
	} else {
		switch fc := req.PDU[0]; fc {
		case 0x02:
			respPDU = s.handleReadBits(req.UnitID, req.PDU)
		case 0x03:
			respPDU = s.handleReadRegisters(req.UnitID, req.PDU, 3)
		case 0x04:
			respPDU = s.handleReadRegisters(req.UnitID, req.PDU, 4)
		case 0x06:
			respPDU = s.handleWriteSingleRegister(req.UnitID, req.PDU)
		case 0x10:
			respPDU = s.handleWriteMultipleRegisters(req.UnitID, req.PDU)
		default:
			respPDU = exceptionPDU(fc, excIllegalFunction)
		}
	}
	return buildMBAPResponse(req.TransactionID, req.UnitID, respPDU)
}

const (
	excIllegalFunction    = 0x01
	excIllegalDataAddress = 0x02
	excIllegalDataValue   = 0x03
)

func exceptionPDU(fc byte, code byte) []byte {
	return []byte{fc | 0x80, code}
}
