// Package clock provides the only source of "now" business logic is
// allowed to use (AGENT-TASK §1.5: "the simulator uses an injectable
// clock; tests use a fake clock with manual fast-forward; no time.Now()
// in business logic"). Production wiring uses Real; tests use Fake so
// time-dependent behavior (Strategy Period scheduling in Task 6, watchdog
// timeouts, scenario playback in Task 7) is deterministic and doesn't
// need a real wall-clock sleep to exercise.
package clock

import (
	"sync"
	"time"
)

// Clock provides the current time.
type Clock interface {
	Now() time.Time
}

// Real is a Clock backed by the actual wall clock — used only at the
// outermost wiring layer (main.go, a Runner's ticker), never inside the
// packages that make decisions based on time.
type Real struct{}

func (Real) Now() time.Time { return time.Now() }

// Fake is a manually-advanced Clock for deterministic tests.
type Fake struct {
	mu  sync.Mutex
	now time.Time
}

// NewFake returns a Fake starting at start.
func NewFake(start time.Time) *Fake {
	return &Fake{now: start}
}

func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

// Advance moves the fake clock forward by d (d must be >= 0).
func (f *Fake) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
}

// Set moves the fake clock to an absolute time (backwards allowed, for
// tests that need to construct a specific moment directly).
func (f *Fake) Set(t time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = t
}
