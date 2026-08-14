package iec104

import (
	"sync"
	"time"
)

// linkState mirrors modbustcp's — see that package's linkstate.go for the
// full rationale, identical here except for what "answer a request"
// means at the IEC-104 framing layer (handleConn, below) instead of
// Modbus's request/response PDU pairing.
type linkState struct {
	mu    sync.Mutex
	drop  bool
	hang  bool
	delay time.Duration

	hbFrozen bool
	hbValue  float64
}

func (l *linkState) dropped() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.drop
}

func (l *linkState) hanging() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.hang
}

func (l *linkState) responseDelay() time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.delay
}

func (l *linkState) heartbeatOverride() (value float64, ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.hbValue, l.hbFrozen
}

// SetDrop implements linkfault.Target.
func (s *Server) SetDrop() {
	s.link.mu.Lock()
	s.link.drop = true
	s.link.mu.Unlock()

	s.connMu.Lock()
	for c := range s.conns {
		c.nc.Close() //nolint:errcheck
	}
	s.connMu.Unlock()
}

// SetHang implements linkfault.Target.
func (s *Server) SetHang() {
	s.link.mu.Lock()
	s.link.hang = true
	s.link.mu.Unlock()
}

// SetDelay implements linkfault.Target — see modbustcp's SetDelay for why
// a real time.Sleep here isn't the business-logic time.Now() AGENT-TASK
// §1.5 forbids.
func (s *Server) SetDelay(d time.Duration) {
	s.link.mu.Lock()
	s.link.delay = d
	s.link.mu.Unlock()
}

// SetHeartbeatPause implements linkfault.Target.
func (s *Server) SetHeartbeatPause(frozenValue float64) {
	s.link.mu.Lock()
	s.link.hbFrozen = true
	s.link.hbValue = frozenValue
	s.link.mu.Unlock()
}

// ClearLinkFaults implements linkfault.Target.
func (s *Server) ClearLinkFaults() {
	s.link.mu.Lock()
	s.link.drop = false
	s.link.hang = false
	s.link.delay = 0
	s.link.hbFrozen = false
	s.link.hbValue = 0
	s.link.mu.Unlock()
}
