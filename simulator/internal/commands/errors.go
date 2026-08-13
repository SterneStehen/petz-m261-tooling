package commands

import "errors"

// Sentinel errors Validate/Write return, distinguishable via errors.Is —
// protocol servers map these to different wire-level negative responses
// (iec104: ErrNotWritable behaves like an unknown IOA; the other two are a
// legitimate point with a rejected value. modbustcp: all three become
// exception 0x03, illegal data value, since Modbus has no equivalent
// "unknown address vs known-but-rejected" distinction at this layer).
var (
	// ErrNotWritable means key isn't one of the 148 EMS setpoints a
	// command may ever legitimately target — wrong device, wrong class,
	// or not a real point at all.
	ErrNotWritable = errors.New("commands: not a writable EMS setpoint")

	// ErrInvalidValue means the point is real and writable, but value
	// fails its enum membership or native-range check (Task 6 item 1).
	ErrInvalidValue = errors.New("commands: value fails validation")

	// ErrDangerous means the point is real, writable, and the value would
	// otherwise be valid, but the catalog flags it Dangerous and
	// commands.allow_dangerous is false (Task 6 item 7).
	ErrDangerous = errors.New("commands: dangerous command rejected (allow_dangerous is false)")
)
