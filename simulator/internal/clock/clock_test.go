package clock

import (
	"testing"
	"time"
)

func TestRealReturnsCurrentTime(t *testing.T) {
	before := time.Now()
	got := Real{}.Now()
	after := time.Now()
	if got.Before(before) || got.After(after) {
		t.Errorf("Real{}.Now() = %v, not within [%v, %v]", got, before, after)
	}
}

func TestFakeStartsAtGivenTime(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	f := NewFake(start)
	if got := f.Now(); !got.Equal(start) {
		t.Errorf("NewFake(start).Now() = %v, want %v", got, start)
	}
}

func TestFakeAdvanceIsManualNotWallClock(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	f := NewFake(start)
	f.Advance(24 * time.Hour)
	want := start.Add(24 * time.Hour)
	if got := f.Now(); !got.Equal(want) {
		t.Errorf("after Advance(24h), Now() = %v, want %v", got, want)
	}
	// A second Advance call must move relative to the last value, not reset.
	f.Advance(time.Hour)
	want = want.Add(time.Hour)
	if got := f.Now(); !got.Equal(want) {
		t.Errorf("after a second Advance(1h), Now() = %v, want %v", got, want)
	}
}

func TestFakeSetJumpsToAbsoluteTime(t *testing.T) {
	f := NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	target := time.Date(2020, 6, 15, 12, 30, 0, 0, time.UTC)
	f.Set(target)
	if got := f.Now(); !got.Equal(target) {
		t.Errorf("after Set(target), Now() = %v, want %v", got, target)
	}
}

func TestFakeSatisfiesClockInterface(t *testing.T) {
	var _ Clock = (*Fake)(nil)
	var _ Clock = Real{}
}
