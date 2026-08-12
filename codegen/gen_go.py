#!/usr/bin/env python3
"""Generate the Go package gen/go/m261points from catalog/point_catalog.json.

See docs (internal task specification, task 3) for the target shape:
address constants, a PointMeta metadata table, typed per-device state
structs, enum types, and scale-aware encode/decode functions.

Usage:
    python3 -m codegen.gen_go
"""
from __future__ import annotations

import argparse
import json
import subprocess
from pathlib import Path

from codegen.common import (
    DEVICE_ORDER,
    DEVICE_PASCAL,
    GENERATED_BANNER_GO,
    REPO_ROOT,
    enum_items,
    group_by_device,
    load_catalog,
    point_identifier,
    slug_to_pascal,
)

DEFAULT_OUT = REPO_ROOT / "gen" / "go" / "m261points"

_DATA_TYPES = ["U8", "I16", "F32"]


def go_string(s: str | None) -> str:
    """A valid Go double-quoted string literal. JSON string escaping is a
    valid subset of Go's (both escape \\, \", and control chars the same
    way), so json.dumps does the job."""
    return json.dumps(s or "", ensure_ascii=False)


class NameRegistry:
    """Collects every top-level identifier we emit and fails loudly on a
    collision instead of silently overwriting a Go declaration."""

    def __init__(self) -> None:
        self._seen: dict[str, str] = {}

    def claim(self, name: str, context: str) -> str:
        if name in self._seen:
            raise ValueError(
                f"identifier collision: {name!r} wanted by {context!r}, "
                f"already claimed by {self._seen[name]!r}"
            )
        self._seen[name] = context
        return name


def gen_codec() -> str:
    return f'''{GENERATED_BANNER_GO}

package m261points

import "math"

// ByteOrder selects how multi-byte values are packed on the Modbus wire.
// Config: modbus.byte_order — unconfirmed by the manufacturer as of
// writing (see AGENT-TASK §7); Big is the default, all four are
// implemented and mutually consistent (Decode(Encode(x)) == x for every
// mode), so switching the config value needs no code change once the
// real value is confirmed on site.
type ByteOrder int

const (
	BigEndian ByteOrder = iota
	LittleEndian
	BigEndianWordSwap
	LittleEndianWordSwap
)

// DataType is the on-wire register encoding of a point, taken verbatim
// from the IEC-104 representation of the M261 point map (§2.2).
type DataType string

const (
	DataTypeU8  DataType = "U8"
	DataTypeI16 DataType = "I16"
	DataTypeF32 DataType = "F32"
)

// Width returns the native encoded width in bytes for the type: 1 for
// U8, 2 for I16, 4 for F32.
func (dt DataType) Width() int {{
	switch dt {{
	case DataTypeU8:
		return 1
	case DataTypeI16:
		return 2
	case DataTypeF32:
		return 4
	default:
		return 0
	}}
}}

// reorder applies (or, called a second time, undoes — it is its own
// inverse) the configured byte/word order to a native big-endian byte
// slice. Word swap only has an effect on 4-byte (F32) values; U8 (1
// byte) is order-independent by construction.
func reorder(b []byte, order ByteOrder) []byte {{
	out := append([]byte(nil), b...)
	switch order {{
	case BigEndian:
		return out
	case LittleEndian:
		reverseInPlace(out)
		return out
	case BigEndianWordSwap:
		swapWordsInPlace(out)
		return out
	case LittleEndianWordSwap:
		swapWordsInPlace(out)
		reverseInPlace(out)
		return out
	default:
		return out
	}}
}}

func reverseInPlace(b []byte) {{
	for i, j := 0, len(b)-1; i < j; i, j = i+1, j-1 {{
		b[i], b[j] = b[j], b[i]
	}}
}}

// swapWordsInPlace exchanges the two 16-bit halves of a 4-byte slice; a
// no-op for widths other than 4 (U8/I16 have only one "word").
func swapWordsInPlace(b []byte) {{
	if len(b) != 4 {{
		return
	}}
	b[0], b[1], b[2], b[3] = b[2], b[3], b[0], b[1]
}}

// --- native, unscaled codecs -------------------------------------------

func DecodeU8(b []byte, order ByteOrder) uint8 {{
	r := reorder(b, order)
	return r[0]
}}

func EncodeU8(v uint8, order ByteOrder) []byte {{
	return reorder([]byte{{v}}, order)
}}

func DecodeI16(b []byte, order ByteOrder) int16 {{
	r := reorder(b, order)
	return int16(uint16(r[0])<<8 | uint16(r[1]))
}}

func EncodeI16(v int16, order ByteOrder) []byte {{
	u := uint16(v)
	return reorder([]byte{{byte(u >> 8), byte(u)}}, order)
}}

func DecodeF32(b []byte, order ByteOrder) float32 {{
	r := reorder(b, order)
	bits := uint32(r[0])<<24 | uint32(r[1])<<16 | uint32(r[2])<<8 | uint32(r[3])
	return math.Float32frombits(bits)
}}

func EncodeF32(v float32, order ByteOrder) []byte {{
	bits := math.Float32bits(v)
	return reorder([]byte{{byte(bits >> 24), byte(bits >> 16), byte(bits >> 8), byte(bits)}}, order)
}}

// DecodeI32/EncodeI32: not a catalog DataType (no point's own data_type is
// ever "I32" — only U8/I16/F32 appear, see catalog/point_catalog.json).
// This is a wire-level primitive for Modbus specifically: per §2.2, Modbus
// packs every non-alarm point into a fixed 2-register (4-byte) slot, which
// widens an IEC-104-native I16 point up to I32 on the wire (confirmed
// empirically against the real register map, Task 2 validation). Kept
// here alongside the other native-width codecs rather than in
// simulator/internal/modbustcp so every protocol implementation shares
// one reorder()-backed definition of what each byte order means.
func DecodeI32(b []byte, order ByteOrder) int32 {{
	r := reorder(b, order)
	return int32(uint32(r[0])<<24 | uint32(r[1])<<16 | uint32(r[2])<<8 | uint32(r[3]))
}}

func EncodeI32(v int32, order ByteOrder) []byte {{
	u := uint32(v)
	return reorder([]byte{{byte(u >> 24), byte(u >> 16), byte(u >> 8), byte(u)}}, order)
}}

// --- scale-aware generic codec ------------------------------------------
//
// scale is applied as: logical = raw * scale (decode) / raw = logical /
// scale (encode, rounded to the nearest integer for U8/I16). Every point
// in the catalog currently has scale=1 (§7: scaling.source defaults to
// "override", unconfirmed) — these functions are exercised at scale=1
// today and are ready for a non-1 override without further code changes.

func DecodeValue(b []byte, dt DataType, scale float64, order ByteOrder) float64 {{
	switch dt {{
	case DataTypeU8:
		return float64(DecodeU8(b, order)) * scale
	case DataTypeI16:
		return float64(DecodeI16(b, order)) * scale
	case DataTypeF32:
		return float64(DecodeF32(b, order)) * scale
	default:
		return 0
	}}
}}

func EncodeValue(v float64, dt DataType, scale float64, order ByteOrder) []byte {{
	raw := v
	if scale != 0 {{
		raw = v / scale
	}}
	switch dt {{
	case DataTypeU8:
		return EncodeU8(uint8(math.Round(raw)), order)
	case DataTypeI16:
		return EncodeI16(int16(math.Round(raw)), order)
	case DataTypeF32:
		return EncodeF32(float32(raw), order)
	default:
		return nil
	}}
}}
'''


def gen_codec_test() -> str:
    return f'''{GENERATED_BANNER_GO}

package m261points

import "testing"

var allByteOrders = []ByteOrder{{BigEndian, LittleEndian, BigEndianWordSwap, LittleEndianWordSwap}}

// Task 3 acceptance: "round-trip test — decode and encode of every data
// type returns the original value", for every byte order — Task 4 will
// pick one via config, but the codec itself must be correct for all four.
func TestRoundTripU8(t *testing.T) {{
	for _, order := range allByteOrders {{
		for _, v := range []uint8{{0, 1, 42, 255}} {{
			got := DecodeU8(EncodeU8(v, order), order)
			if got != v {{
				t.Errorf("order=%v: EncodeU8/DecodeU8(%d) = %d, want %d", order, v, got, v)
			}}
		}}
	}}
}}

func TestRoundTripI16(t *testing.T) {{
	for _, order := range allByteOrders {{
		for _, v := range []int16{{0, 1, -1, 32767, -32768, 1234}} {{
			got := DecodeI16(EncodeI16(v, order), order)
			if got != v {{
				t.Errorf("order=%v: EncodeI16/DecodeI16(%d) = %d, want %d", order, v, got, v)
			}}
		}}
	}}
}}

func TestRoundTripF32(t *testing.T) {{
	for _, order := range allByteOrders {{
		for _, v := range []float32{{0, 1, -1, 3.14159, -273.15, 1e10, 1e-10}} {{
			got := DecodeF32(EncodeF32(v, order), order)
			if got != v {{
				t.Errorf("order=%v: EncodeF32/DecodeF32(%v) = %v, want %v", order, v, got, v)
			}}
		}}
	}}
}}

func TestRoundTripI32(t *testing.T) {{
	for _, order := range allByteOrders {{
		for _, v := range []int32{{0, 1, -1, 2147483647, -2147483648, 40001, -40001}} {{
			got := DecodeI32(EncodeI32(v, order), order)
			if got != v {{
				t.Errorf("order=%v: EncodeI32/DecodeI32(%d) = %d, want %d", order, v, got, v)
			}}
		}}
	}}
}}

func TestRoundTripValueWithScale(t *testing.T) {{
	cases := []struct {{
		dt    DataType
		scale float64
		v     float64
	}}{{
		{{DataTypeU8, 1, 1}},
		{{DataTypeI16, 1, -1234}},
		{{DataTypeF32, 1, 105.5}},
		{{DataTypeI16, 0.1, 105.5}},  // scale not yet used by the real catalog (all scale=1
		{{DataTypeI16, 0.01, 3.14}},  // today), but must already round-trip once overrides set one
	}}
	for _, order := range allByteOrders {{
		for _, c := range cases {{
			encoded := EncodeValue(c.v, c.dt, c.scale, order)
			got := DecodeValue(encoded, c.dt, c.scale, order)
			// Integer-backed types lose sub-integer-raw-unit precision when
			// scale != 1 (real == raw*scale, raw is rounded to an int) —
			// tolerate rounding to the nearest scale step instead of exact
			// equality for those; F32 stays exact.
			tolerance := c.scale / 2
			diff := got - c.v
			if diff < 0 {{
				diff = -diff
			}}
			if diff > tolerance+1e-9 {{
				t.Errorf("order=%v dt=%v scale=%v: EncodeValue/DecodeValue(%v) = %v, want ~%v", order, c.dt, c.scale, c.v, got, c.v)
			}}
		}}
	}}
}}

func TestDataTypeWidth(t *testing.T) {{
	cases := map[DataType]int{{DataTypeU8: 1, DataTypeI16: 2, DataTypeF32: 4}}
	for dt, want := range cases {{
		if got := dt.Width(); got != want {{
			t.Errorf("%v.Width() = %d, want %d", dt, got, want)
		}}
	}}
}}
'''


def gen_points_go(records: list[dict]) -> str:
    lines = [GENERATED_BANNER_GO, "", "package m261points", ""]
    lines.append("// PointClass mirrors the catalog's `class` field.")
    lines.append("type PointClass string")
    lines.append("")
    lines.append("const (")
    lines.append('\tClassAlarm     PointClass = "alarm"')
    lines.append('\tClassTelemetry PointClass = "telemetry"')
    lines.append('\tClassSetpoint  PointClass = "setpoint"')
    lines.append(")")
    lines.append("")
    lines.append("// Access mirrors the catalog's `access` field.")
    lines.append("type Access string")
    lines.append("")
    lines.append("const (")
    lines.append('\tAccessRO Access = "RO"')
    lines.append('\tAccessWO Access = "WO"')
    lines.append(")")
    lines.append("")
    lines.append("// PointKey identifies one point by (device, slug) — the same key catalog")
    lines.append("// overrides.yaml uses.")
    lines.append("type PointKey struct {")
    lines.append("\tDevice string")
    lines.append("\tSlug   string")
    lines.append("}")
    lines.append("")
    lines.append("// PointMeta is one point's full catalog record.")
    lines.append("type PointMeta struct {")
    lines.append("\tDevice             string")
    lines.append("\tDeviceAddr         int")
    lines.append("\tTag                string // \"\" if the point has none")
    lines.append("\tNameRaw            string")
    lines.append("\tSlug               string")
    lines.append("\tClass              PointClass")
    lines.append("\tAccess             Access")
    lines.append("\tIEC104Addr         int")
    lines.append("\tModbusAddr         *int // nil if the point has no Modbus representation")
    lines.append("\tModbusClass        *int")
    lines.append("\tModbusFunction     []int")
    lines.append("\tDataType           DataType")
    lines.append("\tScale              float64")
    lines.append("\tUnit               string // \"\" if none")
    lines.append("\tEnum               map[int]string // nil if none")
    lines.append("\tDescription        string // \"\" if none")
    lines.append("\tDangerous          bool")
    lines.append("\tReadbackIEC104Addr *int")
    lines.append("\tSources            []string")
    lines.append("}")
    lines.append("")
    lines.append(f"// Points holds all {len(records)} points from the catalog, keyed by (device, slug).")
    lines.append("var Points = map[PointKey]PointMeta{")
    for r in records:
        key = f'{{Device: {go_string(r["device"])}, Slug: {go_string(r["slug"])}}}'
        modbus_addr = "nil" if r["modbus_addr"] is None else f'intPtr({r["modbus_addr"]})'
        modbus_class = "nil" if r["modbus_class"] is None else f'intPtr({r["modbus_class"]})'
        modbus_function = (
            "nil" if not r["modbus_function"] else "[]int{" + ", ".join(str(x) for x in r["modbus_function"]) + "}"
        )
        readback = "nil" if r["readback_iec104_addr"] is None else f'intPtr({r["readback_iec104_addr"]})'
        enum_go = "nil"
        if r["enum"]:
            items = ", ".join(f"{int(k)}: {go_string(v)}" for k, v in r["enum"].items())
            enum_go = "map[int]string{" + items + "}"
        sources = "[]string{" + ", ".join(go_string(s) for s in r["sources"]) + "}"
        lines.append(
            f"\tPointKey{key}: {{"
            f"Device: {go_string(r['device'])}, "
            f"DeviceAddr: {r['device_addr']}, "
            f"Tag: {go_string(r['tag'])}, "
            f"NameRaw: {go_string(r['name_raw'])}, "
            f"Slug: {go_string(r['slug'])}, "
            f"Class: {go_string(r['class'])}, "
            f"Access: {go_string(r['access'])}, "
            f"IEC104Addr: {r['iec104_addr']}, "
            f"ModbusAddr: {modbus_addr}, "
            f"ModbusClass: {modbus_class}, "
            f"ModbusFunction: {modbus_function}, "
            f"DataType: {go_string(r['data_type'])}, "
            f"Scale: {float(r['scale'])}, "
            f"Unit: {go_string(r['unit'])}, "
            f"Enum: {enum_go}, "
            f"Description: {go_string(r['description'])}, "
            f"Dangerous: {'true' if r['dangerous'] else 'false'}, "
            f"ReadbackIEC104Addr: {readback}, "
            f"Sources: {sources}"
            f"}},"
        )
    lines.append("}")
    lines.append("")
    lines.append("func intPtr(v int) *int { return &v }")
    lines.append("")
    return "\n".join(lines)


def gen_constants_go(records: list[dict], registry: NameRegistry) -> str:
    lines = [GENERATED_BANNER_GO, "", "package m261points", ""]
    lines.append("// Address constants, one pair (IEC-104 always, Modbus where the point has")
    lines.append("// a Modbus representation) per catalog point.")
    lines.append("const (")
    for r in records:
        ident = registry.claim(point_identifier(r["device"], r["slug"]), f"{r['device']}/{r['slug']} iec104_addr const")
        lines.append(f"\t{ident} = {r['iec104_addr']} // {r['name_raw']}")
        if r["modbus_addr"] is not None:
            modbus_ident = registry.claim(ident + "Modbus", f"{r['device']}/{r['slug']} modbus_addr const")
            lines.append(f"\t{modbus_ident} = {r['modbus_addr']}")
    lines.append(")")
    lines.append("")
    return "\n".join(lines)


def gen_enums_go(records: list[dict], registry: NameRegistry) -> str:
    lines = [GENERATED_BANNER_GO, "", "package m261points", "", "import \"fmt\"", ""]
    for r in records:
        items = enum_items(r["enum"])
        if not items:
            continue
        ident = point_identifier(r["device"], r["slug"])
        enum_type = registry.claim(ident + "Enum", f"{r['device']}/{r['slug']} enum type")
        lines.append(f"// {enum_type}: {r['name_raw']} ({r['description']})")
        # int32, not int16: PCS/fault_reset_command's real enum value is
        # 32768 (register is I16, but the map's own description literally
        # says "32768: Reset" — reinterpreting that as -32768 two's-
        # complement would be guessing at intent, not reading the map, so
        # every enum type here is int32-backed to fit the value as written.
        lines.append(f"type {enum_type} int32")
        lines.append("")
        lines.append("const (")
        seen_value_names: set[str] = set()
        value_names: dict[int, str] = {}
        for code, label in items:
            base = enum_type + slug_to_pascal(label)
            name = base
            n = 2
            while name in seen_value_names:
                name = f"{base}{n}"
                n += 1
            seen_value_names.add(name)
            value_names[code] = name
            registry.claim(name, f"{r['device']}/{r['slug']} enum value {code}")
            lines.append(f"\t{name} {enum_type} = {code}")
        lines.append(")")
        lines.append("")
        lines.append(f"func (v {enum_type}) String() string {{")
        lines.append("\tswitch v {")
        for code, label in items:
            lines.append(f"\tcase {value_names[code]}:")
            lines.append(f"\t\treturn {go_string(label)}")
        lines.append("\tdefault:")
        lines.append(f'\t\treturn fmt.Sprintf("{enum_type}(%d)", int32(v))')
        lines.append("\t}")
        lines.append("}")
        lines.append("")
    return "\n".join(lines)


def gen_state_go(records_by_device: dict[str, list[dict]], enum_types: dict[tuple[str, str], str]) -> str:
    lines = [GENERATED_BANNER_GO, "", "package m261points", ""]
    for device in DEVICE_ORDER:
        recs = records_by_device.get(device, [])
        if not recs:
            continue
        struct_name = DEVICE_PASCAL[device] + "State"
        lines.append(f"// {struct_name} holds the current value of every {device} point.")
        lines.append(f"type {struct_name} struct {{")
        for r in sorted(recs, key=lambda r: r["iec104_addr"]):
            field = slug_to_pascal(r["slug"])
            enum_type = enum_types.get((r["device"], r["slug"]))
            field_type = enum_type if enum_type else "float64"
            lines.append(f"\t{field} {field_type} // {r['name_raw']}")
        lines.append("}")
        lines.append("")
    return "\n".join(lines)


def gen_go_mod() -> str:
    return "module github.com/SterneStehen/petz-m261-tooling\n\ngo 1.22\n"


def write(path: Path, content: str) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    if not content.endswith("\n"):
        content += "\n"
    path.write_text(content, encoding="utf-8")


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--catalog", type=Path, default=None)
    ap.add_argument("--out", type=Path, default=DEFAULT_OUT)
    args = ap.parse_args(argv)

    records = load_catalog(args.catalog) if args.catalog else load_catalog()
    by_device = group_by_device(records)
    registry = NameRegistry()

    write(args.out / "codec.go", gen_codec())
    write(args.out / "codec_test.go", gen_codec_test())
    write(args.out / "constants.go", gen_constants_go(records, registry))
    write(args.out / "enums.go", gen_enums_go(records, registry))

    enum_types = {
        (r["device"], r["slug"]): point_identifier(r["device"], r["slug"]) + "Enum"
        for r in records
        if r["enum"]
    }
    write(args.out / "state.go", gen_state_go(by_device, enum_types))
    write(args.out / "points.go", gen_points_go(records))

    go_mod = REPO_ROOT / "go.mod"
    if not go_mod.exists():
        write(go_mod, gen_go_mod())

    subprocess.run(["gofmt", "-w", str(args.out)], check=True)

    print(f"wrote {len(records)} points to Go package at {args.out}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
