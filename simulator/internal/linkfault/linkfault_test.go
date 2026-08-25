package linkfault_test

import (
	"testing"
	"time"

	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/linkfault"
)

// fakeTarget records which method was called and with what argument —
// enough to prove Apply calls the right one(s) on the right target(s)
// without needing a real protocol server.
type fakeTarget struct {
	drop, hang, cleared bool
	delay               time.Duration
	hbValue             float64
	hbSet               bool
}

func (f *fakeTarget) SetDrop()                           { f.drop = true }
func (f *fakeTarget) SetHang()                           { f.hang = true }
func (f *fakeTarget) SetDelay(d time.Duration)           { f.delay = d }
func (f *fakeTarget) SetHeartbeatPause(v float64)        { f.hbValue, f.hbSet = v, true }
func (f *fakeTarget) ClearLinkFaults()                   { *f = fakeTarget{cleared: true} }
func (f *fakeTarget) FenceHeartbeat()                    {}
func (f *fakeTarget) ActiveLinkFaults() []linkfault.Mode { return nil }

func TestApplyTargetsIEC104Only(t *testing.T) {
	iec, mb := &fakeTarget{}, &fakeTarget{}
	if err := linkfault.Apply(iec, mb, linkfault.ProtocolIEC104, linkfault.ModeDrop, 0, 0); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !iec.drop {
		t.Error("iec.drop = false, want true")
	}
	if mb.drop {
		t.Error("mb.drop = true, want untouched (protocol was iec104 only)")
	}
}

func TestApplyTargetsModbusOnly(t *testing.T) {
	iec, mb := &fakeTarget{}, &fakeTarget{}
	if err := linkfault.Apply(iec, mb, linkfault.ProtocolModbus, linkfault.ModeHang, 0, 0); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !mb.hang {
		t.Error("mb.hang = false, want true")
	}
	if iec.hang {
		t.Error("iec.hang = true, want untouched (protocol was modbus only)")
	}
}

func TestApplyTargetsBoth(t *testing.T) {
	iec, mb := &fakeTarget{}, &fakeTarget{}
	if err := linkfault.Apply(iec, mb, linkfault.ProtocolBoth, linkfault.ModeDelay, 250*time.Millisecond, 0); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if iec.delay != 250*time.Millisecond || mb.delay != 250*time.Millisecond {
		t.Errorf("delays = %v, %v, want both 250ms", iec.delay, mb.delay)
	}
}

func TestApplyHeartbeatPausePassesFrozenValue(t *testing.T) {
	iec, mb := &fakeTarget{}, &fakeTarget{}
	if err := linkfault.Apply(iec, mb, linkfault.ProtocolBoth, linkfault.ModeHeartbeatPause, 0, 42); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !iec.hbSet || iec.hbValue != 42 || !mb.hbSet || mb.hbValue != 42 {
		t.Errorf("heartbeat freeze not applied to both targets with value 42: iec=%+v mb=%+v", iec, mb)
	}
}

func TestApplyClearCallsClearLinkFaults(t *testing.T) {
	iec, mb := &fakeTarget{drop: true}, &fakeTarget{hang: true}
	if err := linkfault.Apply(iec, mb, linkfault.ProtocolBoth, linkfault.ModeClear, 0, 0); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !iec.cleared || !mb.cleared {
		t.Errorf("ClearLinkFaults not called on both: iec=%+v mb=%+v", iec, mb)
	}
}

func TestApplyRejectsUnknownProtocol(t *testing.T) {
	iec, mb := &fakeTarget{}, &fakeTarget{}
	if err := linkfault.Apply(iec, mb, linkfault.Protocol("bogus"), linkfault.ModeDrop, 0, 0); err == nil {
		t.Error("Apply with an unknown protocol succeeded, want an error")
	}
}

func TestApplyRejectsUnknownMode(t *testing.T) {
	iec, mb := &fakeTarget{}, &fakeTarget{}
	if err := linkfault.Apply(iec, mb, linkfault.ProtocolBoth, linkfault.Mode("bogus"), 0, 0); err == nil {
		t.Error("Apply with an unknown mode succeeded, want an error")
	}
}
