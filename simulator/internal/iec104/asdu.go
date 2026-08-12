package iec104

import (
	"encoding/binary"
	"fmt"
	"math"
)

// ASDU type IDs actually implemented. Multi-byte numeric fields (common
// address, information object address, IEEE-754 float) are little-endian
// per the IEC 60870-5-104 wire format itself — a fixed protocol rule, not
// the "unconfirmed" modbus.byte_order config (AGENT-TASK §7), which is
// specific to Modbus.
const (
	typeMSPNA1 = 1   // single-point information — alarms
	typeMMENC1 = 13  // measured value, short float — telemetry
	typeCSCNA1 = 45  // single command — binary (2-value enum) setpoints
	typeCSENC1 = 50  // set-point command, short float — every other setpoint
	typeCICNA1 = 100 // interrogation command — needed to carry general interrogation
)

// Cause of transmission (COT), low 6 bits; bit 6 is the P/N flag (0
// positive, 1 negative confirmation), bit 7 is T (test), unused here.
const (
	cotPeriodic              = 1
	cotSpontaneous           = 3
	cotActivation            = 6
	cotActivationCon         = 7
	cotDeactivation          = 8
	cotDeactivationCon       = 9
	cotActivationTermination = 10
	cotInterrogatedByStation = 20
	cotUnknownCommonAddr     = 46
	cotUnknownIOA            = 47
	cotNegativeFlag          = 0x40
)

const qoiStationInterrogation = 20

func buildASDUHeader(typeID byte, numObjs int, cot byte, commonAddr int) []byte {
	buf := make([]byte, 6)
	buf[0] = typeID
	buf[1] = byte(numObjs) // SQ=0: numObjs individually-addressed objects, not a sequence
	buf[2] = cot
	buf[3] = 0 // originator address, unused
	binary.LittleEndian.PutUint16(buf[4:6], uint16(commonAddr))
	return buf
}

func putIOA(buf []byte, ioa int) {
	buf[0] = byte(ioa)
	buf[1] = byte(ioa >> 8)
	buf[2] = byte(ioa >> 16)
}

func buildMSPNA1(commonAddr, ioa int, value bool, cot byte) []byte {
	hdr := buildASDUHeader(typeMSPNA1, 1, cot, commonAddr)
	body := make([]byte, 4) // IOA(3) + SIQ(1)
	putIOA(body, ioa)
	if value {
		body[3] = 0x01
	}
	return append(hdr, body...)
}

func buildMMENC1(commonAddr, ioa int, value float32, cot byte) []byte {
	hdr := buildASDUHeader(typeMMENC1, 1, cot, commonAddr)
	body := make([]byte, 8) // IOA(3) + float32(4) + QDS(1)
	putIOA(body, ioa)
	binary.LittleEndian.PutUint32(body[3:7], math.Float32bits(value))
	body[7] = 0 // QDS: good, no flags
	return append(hdr, body...)
}

func buildCSCNA1(commonAddr, ioa int, value bool, cot byte) []byte {
	hdr := buildASDUHeader(typeCSCNA1, 1, cot, commonAddr)
	body := make([]byte, 4)
	putIOA(body, ioa)
	if value {
		body[3] = 0x01
	}
	return append(hdr, body...)
}

func buildCSENC1(commonAddr, ioa int, value float32, cot byte) []byte {
	hdr := buildASDUHeader(typeCSENC1, 1, cot, commonAddr)
	body := make([]byte, 8)
	putIOA(body, ioa)
	binary.LittleEndian.PutUint32(body[3:7], math.Float32bits(value))
	body[7] = 0 // QOS: good, no flags
	return append(hdr, body...)
}

func decodeFloat32(b []byte) float32 {
	return math.Float32frombits(binary.LittleEndian.Uint32(b))
}

func buildCICNA1(commonAddr int, cot byte, qoi byte) []byte {
	hdr := buildASDUHeader(typeCICNA1, 1, cot, commonAddr)
	body := make([]byte, 4) // IOA(3, always 0 for this type) + QOI(1)
	body[3] = qoi
	return append(hdr, body...)
}

type asduHeader struct {
	TypeID     byte
	COT        byte
	CommonAddr int
}

type infoObject struct {
	IOA  int
	Data []byte
}

func infoObjectSize(typeID byte) (int, error) {
	switch typeID {
	case typeMSPNA1, typeCSCNA1:
		return 4, nil // IOA(3) + 1 byte
	case typeMMENC1, typeCSENC1:
		return 8, nil // IOA(3) + float32(4) + 1 byte
	case typeCICNA1:
		return 4, nil // IOA(3) + QOI(1)
	default:
		return 0, fmt.Errorf("iec104: unsupported ASDU type %d", typeID)
	}
}

// parseASDU only handles SQ=0 (individually addressed objects) — the only
// form this server itself ever sends, and the only form the 148 setpoint
// commands and general interrogation request need.
func parseASDU(b []byte) (asduHeader, []infoObject, error) {
	if len(b) < 6 {
		return asduHeader{}, nil, fmt.Errorf("iec104: asdu too short (%d bytes)", len(b))
	}
	typeID := b[0]
	vsq := b[1]
	if vsq&0x80 != 0 {
		return asduHeader{}, nil, fmt.Errorf("iec104: sequence (SQ=1) ASDUs not supported")
	}
	numObjs := int(vsq)
	cot := b[2]
	commonAddr := int(binary.LittleEndian.Uint16(b[4:6]))
	rest := b[6:]

	objSize, err := infoObjectSize(typeID)
	if err != nil {
		return asduHeader{}, nil, err
	}
	objs := make([]infoObject, 0, numObjs)
	for i := 0; i < numObjs; i++ {
		if len(rest) < objSize {
			return asduHeader{}, nil, fmt.Errorf("iec104: truncated ASDU, expected %d objects", numObjs)
		}
		ioa := int(rest[0]) | int(rest[1])<<8 | int(rest[2])<<16
		objs = append(objs, infoObject{IOA: ioa, Data: rest[3:objSize]})
		rest = rest[objSize:]
	}
	return asduHeader{TypeID: typeID, COT: cot, CommonAddr: commonAddr}, objs, nil
}
