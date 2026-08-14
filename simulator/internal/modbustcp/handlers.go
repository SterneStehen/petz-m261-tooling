package modbustcp

import (
	"encoding/binary"
	"math"

	"github.com/SterneStehen/petz-m261-tooling/gen/go/m261points"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/linkfault"
	"github.com/SterneStehen/petz-m261-tooling/simulator/internal/store"
)

// regSlotKey/registerSlot map one wire-level (unit, class, 0-based
// register address) to the point that register belongs to and which half
// of that point's 2-register slot it is (§2.2: every non-alarm point
// occupies exactly 2 registers). Built once at server construction from
// the static m261points.Points table.
type regSlotKey struct {
	unit  int
	class int
	addr  uint16
}

type registerSlot struct {
	key   m261points.PointKey
	which int // 0 = first register of the pair, 1 = second
}

// classBase is the Modicon-style documentation base for each register
// class — subtracting it from a catalog modbus_addr gives the 0-based
// wire address actually sent on the wire (e.g. 40009 -> 8).
var classBase = map[int]int{2: 10001, 3: 40001, 4: 30001}

func wireAddress(class int, docAddr int) uint16 {
	return uint16(docAddr - classBase[class])
}

func buildRegisterSlots() map[regSlotKey]registerSlot {
	slots := make(map[regSlotKey]registerSlot, len(m261points.Points)*2)
	for key, meta := range m261points.Points {
		if meta.ModbusAddr == nil || meta.ModbusClass == nil || *meta.ModbusClass == 2 {
			continue // discrete inputs (alarms) are bit-addressed, handled in handleReadBits
		}
		class := *meta.ModbusClass
		base := wireAddress(class, *meta.ModbusAddr)
		slots[regSlotKey{unit: meta.DeviceAddr, class: class, addr: base}] = registerSlot{key: key, which: 0}
		slots[regSlotKey{unit: meta.DeviceAddr, class: class, addr: base + 1}] = registerSlot{key: key, which: 1}
	}
	return slots
}

// encodeRegisterPair and decodeRegisterPair apply scale identically for
// F32 and non-F32 points — raw = engineering / scale on encode,
// engineering = raw * scale on decode — only the wire width/format
// differs (native float32 vs. an I16 catalog value widened to a 4-byte
// I32 slot, §2.2). Every point in the real catalog has scale=1 today, so
// this was previously indistinguishable from applying no scale at all
// for F32 specifically — a real gap once a non-1 F32 scale exists (via
// catalog/overrides.yaml, §7 scaling.source), since commands.Processor's
// own validateValue already computes raw = value/scale for F32 too, and
// this function silently skipping that division for F32 would mean
// Modbus reads/writes stop agreeing with what Processor validated.
func encodeRegisterPair(value float64, meta m261points.PointMeta, order m261points.ByteOrder) []byte {
	raw := value
	if meta.Scale != 0 {
		raw = value / meta.Scale
	}
	if meta.DataType == m261points.DataTypeF32 {
		return m261points.EncodeF32(float32(raw), order)
	}
	return m261points.EncodeI32(int32(math.Round(raw)), order)
}

func decodeRegisterPair(b []byte, meta m261points.PointMeta, order m261points.ByteOrder) float64 {
	if meta.DataType == m261points.DataTypeF32 {
		return float64(m261points.DecodeF32(b, order)) * meta.Scale
	}
	return float64(m261points.DecodeI32(b, order)) * meta.Scale
}

// --- function 02: read discrete inputs (alarms) -------------------------

func (s *Server) handleReadBits(unitID byte, pdu []byte) []byte {
	if len(pdu) != 5 {
		return exceptionPDU(pdu[0], excIllegalDataValue)
	}
	start := binary.BigEndian.Uint16(pdu[1:3])
	qty := binary.BigEndian.Uint16(pdu[3:5])
	if qty == 0 || qty > 2000 {
		return exceptionPDU(pdu[0], excIllegalDataValue)
	}
	byteCount := (qty + 7) / 8
	bits := make([]byte, byteCount)
	for i := 0; i < int(qty); i++ {
		docAddr := int(start+uint16(i)) + classBase[2]
		_, val, ok := s.store.GetByModbus(store.ModbusAddr{UnitID: int(unitID), Class: 2, Address: docAddr})
		if ok && val != 0 {
			bits[i/8] |= 1 << uint(i%8)
		}
	}
	resp := make([]byte, 0, 2+len(bits))
	resp = append(resp, pdu[0], byte(byteCount))
	return append(resp, bits...)
}

// --- functions 03/04: read holding/input registers -----------------------

func (s *Server) handleReadRegisters(unitID byte, pdu []byte, class int) []byte {
	if len(pdu) != 5 {
		return exceptionPDU(pdu[0], excIllegalDataValue)
	}
	start := binary.BigEndian.Uint16(pdu[1:3])
	qty := binary.BigEndian.Uint16(pdu[3:5])
	if qty == 0 || qty > 125 {
		return exceptionPDU(pdu[0], excIllegalDataValue)
	}

	buf := make([]byte, int(qty)*2)
	cache := make(map[m261points.PointKey][]byte)
	for i := 0; i < int(qty); i++ {
		slot, ok := s.slots[regSlotKey{unit: int(unitID), class: class, addr: start + uint16(i)}]
		if !ok {
			continue // unmapped register / gap between points — reads as 0x0000
		}
		enc, ok := cache[slot.key]
		if !ok {
			meta := m261points.Points[slot.key]
			val, _ := s.store.Get(slot.key)
			if slot.key == linkfault.HeartbeatKey {
				// Task 7 item 2's heartbeat_pause: this protocol's
				// clients stop seeing the counter move, even though it
				// keeps incrementing underneath in the Store — see
				// linkState.heartbeatOverride.
				if frozen, paused := s.link.heartbeatOverride(); paused {
					val = frozen
				}
			}
			enc = encodeRegisterPair(val, meta, s.cfg.ByteOrder)
			cache[slot.key] = enc
		}
		buf[i*2], buf[i*2+1] = enc[slot.which*2], enc[slot.which*2+1]
	}

	resp := make([]byte, 0, 2+len(buf))
	resp = append(resp, pdu[0], byte(len(buf)))
	return append(resp, buf...)
}

// --- functions 06/16: write holding registers -----------------------------
//
// Both funnel through applyRegisterWrites, which does a read-modify-write
// on whichever points the addressed registers belong to — this handles a
// single-register FC06 write to one half of a 2-register point (a
// read-modify-write of that half) as well as an aligned 2-register FC16
// write (a full new value) uniformly. Every touched point is validated
// through Task 6's commands.Processor (range/enum, dangerous-gating,
// mode-arbitration side effects) before anything is committed to the
// store — see applyRegisterWrites.

func (s *Server) handleWriteSingleRegister(unitID byte, pdu []byte) []byte {
	if len(pdu) != 5 {
		return exceptionPDU(pdu[0], excIllegalDataValue)
	}
	addr := binary.BigEndian.Uint16(pdu[1:3])
	if exc := s.applyRegisterWrites(int(unitID), addr, [][]byte{pdu[3:5]}); exc != 0 {
		return exceptionPDU(pdu[0], exc)
	}
	echo := make([]byte, len(pdu))
	copy(echo, pdu) // FC06 ack echoes the request verbatim
	return echo
}

func (s *Server) handleWriteMultipleRegisters(unitID byte, pdu []byte) []byte {
	if len(pdu) < 6 {
		return exceptionPDU(pdu[0], excIllegalDataValue)
	}
	addr := binary.BigEndian.Uint16(pdu[1:3])
	qty := binary.BigEndian.Uint16(pdu[3:5])
	byteCount := pdu[5]
	if qty == 0 || qty > 123 || int(byteCount) != int(qty)*2 || len(pdu) != 6+int(byteCount) {
		return exceptionPDU(pdu[0], excIllegalDataValue)
	}
	regs := make([][]byte, qty)
	for i := 0; i < int(qty); i++ {
		regs[i] = pdu[6+i*2 : 8+i*2]
	}
	if exc := s.applyRegisterWrites(int(unitID), addr, regs); exc != 0 {
		return exceptionPDU(pdu[0], exc)
	}
	resp := make([]byte, 5)
	resp[0] = pdu[0]
	binary.BigEndian.PutUint16(resp[1:3], addr)
	binary.BigEndian.PutUint16(resp[3:5], qty)
	return resp
}

// applyRegisterWrites returns 0 on success or a Modbus exception code.
//
// Every point an FC06/FC16 request touches is decoded and validated
// (s.cfg.Commands.Validate) before any of them are committed — a
// multi-register FC16 write spanning more than one catalog point must not
// partially apply if a later point in the request fails validation, so
// commit only happens once every touched point has already passed.
func (s *Server) applyRegisterWrites(unit int, start uint16, regs [][]byte) byte {
	touched := make(map[m261points.PointKey][]byte)
	order := make([]m261points.PointKey, 0, len(regs)/2+1)
	for i, val2 := range regs {
		addr := start + uint16(i)
		slot, ok := s.slots[regSlotKey{unit: unit, class: 3, addr: addr}]
		if !ok {
			return excIllegalDataAddress
		}
		buf, ok := touched[slot.key]
		if !ok {
			meta := m261points.Points[slot.key]
			cur, _ := s.store.Get(slot.key)
			buf = append([]byte(nil), encodeRegisterPair(cur, meta, s.cfg.ByteOrder)...)
			touched[slot.key] = buf
			order = append(order, slot.key)
		}
		buf[slot.which*2], buf[slot.which*2+1] = val2[0], val2[1]
	}

	decoded := make(map[m261points.PointKey]float64, len(order))
	for _, key := range order {
		meta := m261points.Points[key]
		val := decodeRegisterPair(touched[key], meta, s.cfg.ByteOrder)
		if s.cfg.Commands != nil {
			if err := s.cfg.Commands.Validate(key, val); err != nil {
				return excIllegalDataValue
			}
		}
		decoded[key] = val
	}
	for _, key := range order {
		if s.cfg.Commands != nil {
			s.cfg.Commands.Write(key, decoded[key]) //nolint:errcheck // already validated above; Write can only fail Validate's own check
		} else {
			s.store.Set(key, decoded[key])
		}
	}
	return 0
}
