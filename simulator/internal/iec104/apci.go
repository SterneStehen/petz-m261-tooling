// Package iec104 implements a minimal IEC 60870-5-104 server (Task 4):
// APCI framing (I/S/U formats with sequence counters), ASDU types
// M_SP_NA_1, M_ME_NC_1, C_SC_NA_1, C_SE_NC_1, plus C_IC_NA_1 for general
// interrogation (not in AGENT-TASK's literal 4-type list, but general
// interrogation — itself explicitly required — is unimplementable without
// a command type to carry the interrogation request, so it's added as a
// necessary extension of the same "minimal subset" clause).
//
// No suitable ready-made Go SERVER library was found (checked
// thinkgos/go-iecp5: archived since 2023, LGPL-3.0 copyleft, despite
// having server support; pascaldekloe/part5: permissive CC0 license, but
// its own README flags incomplete command submission ("once implemented")
// and a "help wanted" banner, and its tooling is oriented at the
// controlling-station/client role, not the secondary/RTU role this
// simulator needs to play; Yobol/go-iec104 and viduq/vv104: low adoption,
// stale or explicitly WIP) — implemented directly per Task 4's own
// fallback clause for exactly this situation.
package iec104

import (
	"fmt"
	"io"
)

const startByte = 0x68

type frameFormat int

const (
	formatI frameFormat = iota
	formatS
	formatU
)

type uType byte

const (
	uStartDTAct uType = 0x07
	uStartDTCon uType = 0x0B
	uStopDTAct  uType = 0x13
	uStopDTCon  uType = 0x17
	uTestFRAct  uType = 0x43
	uTestFRCon  uType = 0x83
)

type frame struct {
	format  frameFormat
	sendSeq uint16 // I-format only
	recvSeq uint16 // I-format and S-format
	uType   uType  // U-format only
	asdu    []byte // I-format only
}

func decodeSeq(lo, hi byte) uint16 {
	return (uint16(hi)<<8 | uint16(lo)) >> 1
}

func encodeSeq(seq uint16) (lo, hi byte) {
	v := seq << 1
	return byte(v), byte(v >> 8)
}

// readFrame reads one APCI frame (and its ASDU, for I-format) from r.
func readFrame(r io.Reader) (frame, error) {
	header := make([]byte, 2)
	if _, err := io.ReadFull(r, header); err != nil {
		return frame{}, err
	}
	if header[0] != startByte {
		return frame{}, fmt.Errorf("iec104: bad start byte 0x%02x", header[0])
	}
	length := header[1]
	if length < 4 {
		return frame{}, fmt.Errorf("iec104: apci length %d too short for a control field", length)
	}
	rest := make([]byte, length)
	if _, err := io.ReadFull(r, rest); err != nil {
		return frame{}, err
	}
	control := rest[0:4]
	body := rest[4:]

	switch {
	case control[0]&0x01 == 0:
		return frame{
			format:  formatI,
			sendSeq: decodeSeq(control[0], control[1]),
			recvSeq: decodeSeq(control[2], control[3]),
			asdu:    body,
		}, nil
	case control[0]&0x03 == 0x01:
		return frame{format: formatS, recvSeq: decodeSeq(control[2], control[3])}, nil
	case control[0]&0x03 == 0x03:
		return frame{format: formatU, uType: uType(control[0])}, nil
	default:
		return frame{}, fmt.Errorf("iec104: unrecognized control field 0x%02x", control[0])
	}
}

func writeRawFrame(w io.Writer, control []byte, asdu []byte) error {
	length := len(control) + len(asdu)
	if length > 253 {
		return fmt.Errorf("iec104: frame too long (%d bytes)", length)
	}
	buf := make([]byte, 2+length)
	buf[0] = startByte
	buf[1] = byte(length)
	copy(buf[2:], control)
	copy(buf[2+len(control):], asdu)
	_, err := w.Write(buf)
	return err
}

func writeIFrame(w io.Writer, sendSeq, recvSeq uint16, asdu []byte) error {
	lo1, hi1 := encodeSeq(sendSeq)
	lo2, hi2 := encodeSeq(recvSeq)
	return writeRawFrame(w, []byte{lo1, hi1, lo2, hi2}, asdu)
}

func writeSFrame(w io.Writer, recvSeq uint16) error {
	lo2, hi2 := encodeSeq(recvSeq)
	return writeRawFrame(w, []byte{0x01, 0x00, lo2, hi2}, nil)
}

func writeUFrame(w io.Writer, u uType) error {
	return writeRawFrame(w, []byte{byte(u), 0x00, 0x00, 0x00}, nil)
}
