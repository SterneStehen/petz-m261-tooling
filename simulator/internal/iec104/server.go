package iec104

import (
	"errors"
	"fmt"
	"net"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/SterneStehen/petz-m261-tooling/gen/go/m261points"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/commands"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/store"
)

type Config struct {
	Addr string // e.g. "127.0.0.1:2404" or ":2404"

	// Commands, if non-nil, routes every setpoint write through Task 6's
	// validation/mode-arbitration/dangerous-gating layer instead of
	// writing the store directly. nil falls back to the pre-Task-6
	// behavior (write straight through, scoped only to EMS setpoints) —
	// used by protocol-level tests in this package that predate Task 6
	// and aren't exercising its concerns. main.go always wires a real
	// *commands.Processor; production writes are never validation-free.
	Commands *commands.Processor
}

// Server is an IEC-104 server backed by a shared store.Store. Any point
// write here is visible to a Modbus TCP server (Task 4) sharing the same
// Store, and vice versa.
type Server struct {
	cfg   Config
	store *store.Store

	ln          net.Listener
	wg          sync.WaitGroup
	quit        chan struct{}
	unsubscribe func()

	connMu sync.Mutex
	conns  map[*clientConn]struct{}
}

func New(st *store.Store, cfg Config) *Server {
	return &Server{
		cfg:   cfg,
		store: st,
		quit:  make(chan struct{}),
		conns: make(map[*clientConn]struct{}),
	}
}

func (s *Server) Start() error {
	ln, err := net.Listen("tcp", s.cfg.Addr)
	if err != nil {
		return fmt.Errorf("iec104: listen: %w", err)
	}
	s.ln = ln

	changes, unsubscribe := s.store.Subscribe()
	s.unsubscribe = unsubscribe
	s.wg.Add(2)
	go s.acceptLoop()
	go s.spontaneousLoop(changes)
	return nil
}

func (s *Server) Addr() net.Addr { return s.ln.Addr() }

func (s *Server) Close() error {
	close(s.quit)
	err := s.ln.Close()
	s.unsubscribe()
	s.connMu.Lock()
	for c := range s.conns {
		c.nc.Close()
	}
	s.connMu.Unlock()
	s.wg.Wait()
	return err
}

func (s *Server) acceptLoop() {
	defer s.wg.Done()
	for {
		nc, err := s.ln.Accept()
		if err != nil {
			return
		}
		c := &clientConn{srv: s, nc: nc}
		s.connMu.Lock()
		s.conns[c] = struct{}{}
		s.connMu.Unlock()
		s.wg.Add(1)
		go s.handleConn(c)
	}
}

// spontaneousLoop pushes every store Change to every "started" (STARTDT
// completed) connection as an unsolicited M_SP_NA_1/M_ME_NC_1 ASDU
// (cotSpontaneous) — general interrogation is the catch-up mechanism for
// anything missed (e.g. a connection that wasn't started yet).
func (s *Server) spontaneousLoop(changes <-chan store.Change) {
	defer s.wg.Done()
	for c := range changes {
		meta, ok := m261points.Points[c.Key]
		if !ok {
			continue
		}
		asdu := monitoredASDU(meta, c.Value, cotSpontaneous)
		if asdu == nil {
			continue // setpoints (WO) aren't monitored/reported spontaneously
		}
		s.broadcast(meta.DeviceAddr, asdu)
	}
}

func (s *Server) broadcast(commonAddr int, asdu []byte) {
	s.connMu.Lock()
	defer s.connMu.Unlock()
	for c := range s.conns {
		c.sendIfStarted(asdu) //nolint:errcheck // a slow/dead peer just misses this update; TCP handles the rest
	}
}

// monitoredASDU builds the single-object ASDU representing one point's
// current value for monitoring (general interrogation or spontaneous
// transmission). Returns nil for setpoints — those are commands, not
// monitored data.
func monitoredASDU(meta m261points.PointMeta, value float64, cot byte) []byte {
	switch meta.Class {
	case m261points.ClassAlarm:
		return buildMSPNA1(meta.DeviceAddr, meta.IEC104Addr, value != 0, cot)
	case m261points.ClassTelemetry:
		return buildMMENC1(meta.DeviceAddr, meta.IEC104Addr, float32(value), cot)
	default:
		return nil
	}
}

type clientConn struct {
	srv     *Server
	nc      net.Conn
	writeMu sync.Mutex
	sendSeq uint16
	recvSeq uint16
	// started is read by sendIfStarted and written by startDataTransfer/
	// stopDataTransfer — atomic. It used to be checked and written
	// independently of writeMu, which -race caught as a plain data race;
	// fixing that (checking under writeMu, same as now) surfaced a second,
	// deeper bug: writing STARTDT_CON and setting started=true were two
	// separate critical sections, so a broadcast between them could
	// observe started still false even though the client had already
	// physically received the confirmation on the wire — a lost update,
	// not merely a race the detector flags. See startDataTransfer.
	started atomic.Bool
}

// setRecvSeq updates the connection's receive sequence number under the
// same lock writeIFrame/writeSFrame read it with — recvSeq is written
// from handleConn's goroutine but read from whichever goroutine sends the
// next frame (which, for spontaneous transmission, is spontaneousLoop's,
// not handleConn's), so both sides need the same lock, not just the
// send-side bookkeeping.
func (c *clientConn) setRecvSeq(seq uint16) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	c.recvSeq = seq
}

func (c *clientConn) writeIFrame(asdu []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.writeIFrameLocked(asdu)
}

// writeIFrameLocked is writeIFrame's body, factored out for callers that
// already hold writeMu (sendIfStarted, startDataTransfer's kin) and must
// not re-lock it.
func (c *clientConn) writeIFrameLocked(asdu []byte) error {
	if err := writeIFrame(c.nc, c.sendSeq, c.recvSeq, asdu); err != nil {
		return err
	}
	c.sendSeq++
	return nil
}

// sendIfStarted writes asdu as a spontaneous I-frame only if the
// connection has completed STARTDT — checked under the same writeMu that
// startDataTransfer/stopDataTransfer hold across their own write. Checking
// started here without that lock (the original design, and the immediate
// -race fix that just made the load/store atomic without changing where
// they were checked) still left a window: a client could receive
// STARTDT_CON and, believing it is now subscribed, trigger or observe a
// store change before this connection's started flag was actually set,
// and broadcast would then skip it — a permanently lost update, not
// retried or caught up on until the next unrelated change or a general
// interrogation. Sharing the lock makes "the client can have observed
// CON" and "started is visible as true to every future lock holder" the
// same instant, so that window no longer exists.
func (c *clientConn) sendIfStarted(asdu []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if !c.started.Load() {
		return nil
	}
	return c.writeIFrameLocked(asdu)
}

func (c *clientConn) writeSFrame() error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return writeSFrame(c.nc, c.recvSeq)
}

func (c *clientConn) writeUFrame(u uType) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return writeUFrame(c.nc, u)
}

// startDataTransfer sends STARTDT_CON and marks the connection started as
// one atomic critical section (see the comment on sendIfStarted for why
// that atomicity is required, not just convenient).
func (c *clientConn) startDataTransfer() error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if err := writeUFrame(c.nc, uStartDTCon); err != nil {
		return err
	}
	c.started.Store(true)
	return nil
}

// stopDataTransfer is startDataTransfer's mirror: clearing started and
// sending STOPDT_CON as one critical section means a concurrent broadcast
// either completes fully before this (the peer may get one last frame,
// which is fine — it hasn't confirmed the stop yet) or fully after (sees
// started already false), never something in between.
func (c *clientConn) stopDataTransfer() error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	c.started.Store(false)
	return writeUFrame(c.nc, uStopDTCon)
}

func (s *Server) handleConn(c *clientConn) {
	defer s.wg.Done()
	defer func() {
		s.connMu.Lock()
		delete(s.conns, c)
		s.connMu.Unlock()
		c.nc.Close()
	}()

	for {
		f, err := readFrame(c.nc)
		if err != nil {
			return
		}
		switch f.format {
		case formatU:
			switch f.uType {
			case uStartDTAct:
				c.startDataTransfer() //nolint:errcheck
			case uStopDTAct:
				c.stopDataTransfer() //nolint:errcheck
			case uTestFRAct:
				c.writeUFrame(uTestFRCon) //nolint:errcheck
			}
		case formatS:
			// pure acknowledgement — nothing to retransmit, we don't buffer unacked I-frames
		case formatI:
			c.setRecvSeq(f.sendSeq + 1)
			c.writeSFrame() //nolint:errcheck // explicit ack every time, simpler than piggybacking correctly
			s.handleASDU(c, f.asdu)
		}
	}
}

func (s *Server) handleASDU(c *clientConn, asdu []byte) {
	hdr, objs, err := parseASDU(asdu)
	if err != nil {
		return // malformed ASDU from the client — drop it, connection stays open
	}
	switch hdr.TypeID {
	case typeCICNA1:
		s.handleGeneralInterrogation(c, hdr, objs)
	case typeCSCNA1:
		s.handleSingleCommand(c, hdr, objs)
	case typeCSENC1:
		s.handleSetpointCommand(c, hdr, objs)
	}
}

// handleGeneralInterrogation implements station interrogation (Task 4's
// "general interrogation" requirement), scoped to hdr.CommonAddr — one device's points only,
// matching the "correctly handle multiple ASDU common addresses"
// requirement (each device gets its own interrogation, answered from its
// own slice of the store).
func (s *Server) handleGeneralInterrogation(c *clientConn, hdr asduHeader, objs []infoObject) {
	if len(objs) != 1 {
		return
	}
	qoi := objs[0].Data[0]
	if qoi != qoiStationInterrogation {
		return // only global (station) interrogation is implemented
	}

	c.writeIFrame(buildCICNA1(hdr.CommonAddr, cotActivationCon, qoi)) //nolint:errcheck

	snap := s.store.SnapshotDevice(hdr.CommonAddr)
	keys := make([]m261points.PointKey, 0, len(snap))
	for k := range snap {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		return m261points.Points[keys[i]].IEC104Addr < m261points.Points[keys[j]].IEC104Addr
	})
	for _, k := range keys {
		meta := m261points.Points[k]
		if asdu := monitoredASDU(meta, snap[k], cotInterrogatedByStation); asdu != nil {
			c.writeIFrame(asdu) //nolint:errcheck
		}
	}

	c.writeIFrame(buildCICNA1(hdr.CommonAddr, cotActivationTermination, qoi)) //nolint:errcheck
}

// handleSingleCommand and handleSetpointCommand both accept a command for
// any setpoint IOA regardless of which of the two command ASDU types the
// client used — the register map doesn't say which of the 148 setpoints
// "should" use C_SC_NA_1 vs C_SE_NC_1 (only that both types exist, Task 4's
// minimal-subset list), so guessing a per-point assignment would
// be inventing a rule the manufacturer never documented. Accepting both
// uniformly, by IOA, sidesteps the guess entirely.
//
// Both funnel through writePoint, which is the ONLY path a command may
// reach the store through: it must resolve to an EMS setpoint specifically.
// Every alarm and telemetry point (any device, including EMS's own) is
// monitoring-only and must never be mutated by an inbound command — a
// client is not entitled to overwrite simulated readings just because it
// knows (or guesses) the IOA.

// errUnknownIOA is writePoint's sentinel for "no legitimately writable
// point at this address" (unresolvable IOA, wrong device, or wrong
// class) — distinct from a commands.Processor rejection (a real point,
// but an invalid/dangerous value), so the caller can pick the
// standard-appropriate cause of transmission (cotUnknownIOA vs a negative
// activation confirmation).
var errUnknownIOA = errors.New("iec104: no writable EMS setpoint at this address")

// writePoint resolves (commonAddr, ioa) to a point, restricted to EMS
// setpoints — the only 148 points a real IEC-104 command may ever
// legitimately target — and forwards to Task 6's commands.Processor for
// validation/mode-arbitration/dangerous-gating and the actual store write.
func (s *Server) writePoint(commonAddr, ioa int, value float64) error {
	addr := store.IECAddr{CommonAddr: commonAddr, ObjAddr: ioa}
	key, _, ok := s.store.GetByIEC(addr)
	if !ok {
		return errUnknownIOA
	}
	meta, ok := m261points.Points[key]
	if !ok || meta.Device != "EMS" || meta.Class != m261points.ClassSetpoint {
		return errUnknownIOA
	}
	if s.cfg.Commands != nil {
		return s.cfg.Commands.Write(key, value)
	}
	if _, ok := s.store.SetByIEC(addr, value); !ok {
		return errUnknownIOA
	}
	return nil
}

// negativeCOT maps a writePoint error to the standard-appropriate cause of
// transmission for a negative activation confirmation: an address that
// resolves to nothing writable is "unknown IOA" (cause 47); a real,
// resolvable point rejected by the commands layer (bad enum/range value,
// or a dangerous command without allow_dangerous) is a negative activation
// confirmation on the same cause the successful case would use (cause 7),
// per the standard's own convention for "known point, rejected value".
func negativeCOT(err error) byte {
	if errors.Is(err, errUnknownIOA) {
		return cotUnknownIOA | cotNegativeFlag
	}
	return cotActivationCon | cotNegativeFlag
}

func (s *Server) handleSingleCommand(c *clientConn, hdr asduHeader, objs []infoObject) {
	if len(objs) != 1 {
		return
	}
	value := 0.0
	if objs[0].Data[0]&0x01 != 0 {
		value = 1
	}
	cot := byte(cotActivationCon)
	if err := s.writePoint(hdr.CommonAddr, objs[0].IOA, value); err != nil {
		cot = negativeCOT(err)
	}
	c.writeIFrame(buildCSCNA1(hdr.CommonAddr, objs[0].IOA, value != 0, cot)) //nolint:errcheck
}

func (s *Server) handleSetpointCommand(c *clientConn, hdr asduHeader, objs []infoObject) {
	if len(objs) != 1 {
		return
	}
	value := decodeFloat32(objs[0].Data[0:4])
	cot := byte(cotActivationCon)
	if err := s.writePoint(hdr.CommonAddr, objs[0].IOA, float64(value)); err != nil {
		cot = negativeCOT(err)
	}
	c.writeIFrame(buildCSENC1(hdr.CommonAddr, objs[0].IOA, value, cot)) //nolint:errcheck
}
