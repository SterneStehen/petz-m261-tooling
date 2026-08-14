// Package linkfault defines the shared vocabulary Task 7's control API
// and scenario engine use to apply link faults (AGENT-TASK.md, Task 7,
// item 2) to whichever protocol server(s) they target, without either of
// modbustcp or iec104 needing to import the other, and without
// controlapi/scenario needing separate code paths per protocol.
//
// The state itself (which mode, if any, is active, and any per-mode
// data — a delay duration, a frozen heartbeat value) lives inside each
// protocol server, not here — Target is a structural interface both
// *modbustcp.Server and *iec104.Server satisfy simply by implementing
// these five methods, the same way any other Go interface is satisfied,
// with no explicit "implements" declaration and no import of this
// package by either.
package linkfault

import (
	"fmt"
	"time"

	"github.com/SterneStehen/petz-m261-tooling/gen/go/m261points"
)

// HeartbeatKey is the one point heartbeat_pause (AGENT-TASK.md, Task 7,
// item 2) freezes — verified against the real catalog, not guessed from
// AGENT-TASK's prose name for it. Defined once here, the one dependency
// modbustcp/iec104/scenario/controlapi all take on this package (beyond
// Target/Protocol/Mode/Apply), rather than four separate copies of the
// same PointKey literal.
var HeartbeatKey = m261points.PointKey{Device: "EMS", Slug: "ems_periodic_heartbeat_indicator"}

// Target is one protocol server's link-fault control surface.
type Target interface {
	// SetDrop refuses new connections and force-closes every connection
	// already open, immediately.
	SetDrop()
	// SetHang keeps connections open but stops answering any request on
	// them (existing or new).
	SetHang()
	// SetDelay(d) answers every request, but only after sleeping d first.
	// d == 0 has the same effect as ClearLinkFaults for the delay mode
	// specifically (see each server's own doc comment).
	SetDelay(d time.Duration)
	// SetHeartbeatPause freezes this protocol's view of the EMS Periodic
	// Heartbeat Indicator at frozenValue — the caller (Apply, below) is
	// responsible for having read the live value at the instant the mode
	// was activated; the server itself has no opinion on which sample.
	SetHeartbeatPause(frozenValue float64)
	// ClearLinkFaults immediately and deterministically removes every
	// active mode set above — no timeout, no partial clear.
	ClearLinkFaults()
}

// Protocol is a link fault's target: one of the two protocol servers, or
// both.
type Protocol string

const (
	ProtocolIEC104 Protocol = "iec104"
	ProtocolModbus Protocol = "modbus"
	ProtocolBoth   Protocol = "both"
)

// Mode is one of the four independent link-fault actions, plus the
// pseudo-mode Clear (AGENT-TASK.md, Task 7 item 2) that removes
// whichever of the four (if any) is currently active.
type Mode string

const (
	ModeDrop           Mode = "drop"
	ModeHang           Mode = "hang"
	ModeDelay          Mode = "delay"
	ModeHeartbeatPause Mode = "heartbeat_pause"
	ModeClear          Mode = "clear"
)

// Apply resolves protocol to one or both concrete targets and applies
// mode to each — the single function controlapi's POST /link and the
// scenario engine's link: step both call, so "targeting one protocol
// applies only there, both applies to both" is defined exactly once.
// delay is only consulted for ModeDelay; heartbeatValue only for
// ModeHeartbeatPause.
func Apply(iecTarget, modbusTarget Target, protocol Protocol, mode Mode, delay time.Duration, heartbeatValue float64) error {
	targets, err := resolve(iecTarget, modbusTarget, protocol)
	if err != nil {
		return err
	}
	for _, t := range targets {
		switch mode {
		case ModeDrop:
			t.SetDrop()
		case ModeHang:
			t.SetHang()
		case ModeDelay:
			t.SetDelay(delay)
		case ModeHeartbeatPause:
			t.SetHeartbeatPause(heartbeatValue)
		case ModeClear:
			t.ClearLinkFaults()
		default:
			return fmt.Errorf("linkfault: mode %q is not one of drop/hang/delay/heartbeat_pause/clear", mode)
		}
	}
	return nil
}

func resolve(iecTarget, modbusTarget Target, protocol Protocol) ([]Target, error) {
	switch protocol {
	case ProtocolIEC104:
		return []Target{iecTarget}, nil
	case ProtocolModbus:
		return []Target{modbusTarget}, nil
	case ProtocolBoth:
		return []Target{iecTarget, modbusTarget}, nil
	default:
		return nil, fmt.Errorf("linkfault: protocol %q is not one of iec104/modbus/both", protocol)
	}
}
