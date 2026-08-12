"""Parse the three M261 point-map xlsx files into flat per-file row lists.

Handles the parsing traps documented in AGENT-TASK §2.3:
  1. multiple subtables per sheet (`other` has DIDO + CSJ)
  2. a subtable's "no:" header line may be absent -> continues previous device
  3. blank/service rows between subtables are skipped
  4. Dry Contact's in-sheet header says commonAddr:172 while §4.1 says 168 —
     we deliberately IGNORE the in-sheet address and use the canonical §4.1
     table for `device_addr`; the raw value is kept on RawSubtable.header_addr
     for the validation report (task 2) to flag.
  5/6. names are taken as-is; only the *join key* gets normalized (normalize.py)
  7. the TAG file's Name column is offset from TAG/Access/... by ~26 rows on
     the EMS sheet — we never read TAG's own Name column at all (see join.py)

This module produces one dataclass per source row; no cross-file join
happens here (see join.py).
"""
from __future__ import annotations

import re
from dataclasses import dataclass
from pathlib import Path

from catalog.xlsx_reader import read_sheet_rows

# Canonical device address table, AGENT-TASK §4.1. Authoritative — not
# re-derived from the in-sheet "no:...commonAddr:N" headers (see trap 4/§2.3.4:
# the DIDO subtable header in `other` literally says commonAddr:172, which is
# wrong; Address Code Allocation / §4.1 says 168).
DEVICE_ADDR: dict[str, int] = {
    "EMS": 1,
    "PCS": 2,
    "BMS": 34,
    "BMS_CELLS": 98,
    "PCS_METER": 163,
    "DIDO": 168,
    "TMS": 170,
    "CSJ": 172,
}

# Sheets that hold points. "Address Code Allocation" is the device-address
# table already captured verbatim in DEVICE_ADDR and is not itself a point
# source.
POINT_SHEETS: list[str] = [
    "Energy Storage Meter",
    "Converter",
    "Battery Cell",
    "Master Controller",
    "Liquid Cooler",
    "other",
    "EMS",
]

ALL_SHEETS: list[str] = ["Address Code Allocation", *POINT_SHEETS]

_NO_LINE_RE = re.compile(
    r"^no:\s*(?P<code>\S+)\s+name:\s*(?P<name>.+?)"
    r"(?:\s+(?:commonAddr|addr):\s*(?P<addr>\d+))?$"
)

# Header markers that identify a (possibly repeated, possibly bilingual)
# header row rather than a data row. Matched against the *first* cell only.
_HEADER_MARKERS = {
    "Telemetry Address",
    "TAG",
    "遥控地址",
    "遥测地址",
    "遥信地址",
    "遥调地址",
    "功能码",  # Modbus file's bilingual header ("function code"), other/CSJ
}


class CatalogParseError(ValueError):
    """Raised when a source file doesn't match the documented structure."""


@dataclass
class RawSubtable:
    sheet: str
    device_code: str
    device_name: str
    header_addr: int | None  # commonAddr/addr as literally written; validation only
    rows: list[list[object]]


def split_subtables(sheet_name: str, rows: list[list[object]]) -> list[RawSubtable]:
    """Split one sheet's raw rows into per-device subtables (AGENT-TASK §2.3.1-3)."""
    subtables: list[RawSubtable] = []
    current: RawSubtable | None = None

    for row in rows:
        first = row[0] if row else None

        if first is None or (isinstance(first, str) and not first.strip()):
            continue  # blank / service row

        if isinstance(first, str):
            stripped = first.strip()
            m = _NO_LINE_RE.match(stripped)
            if m:
                addr = m.group("addr")
                current = RawSubtable(
                    sheet=sheet_name,
                    device_code=m.group("code"),
                    device_name=m.group("name").strip(),
                    header_addr=int(addr) if addr is not None else None,
                    rows=[],
                )
                subtables.append(current)
                continue
            if stripped in _HEADER_MARKERS:
                continue  # header row, English or Chinese, possibly repeated

        if current is None:
            raise CatalogParseError(
                f"data row before any 'no:' line on sheet {sheet_name!r}: {row!r}"
            )
        current.rows.append(row)

    return subtables


def resolve_device(code: str, sheet: str) -> tuple[str, int]:
    try:
        return code, DEVICE_ADDR[code]
    except KeyError:
        raise CatalogParseError(
            f"unknown device code {code!r} on sheet {sheet!r} "
            f"(not in AGENT-TASK §4.1 table)"
        ) from None


def _cell_str(row: list[object], idx: int) -> str | None:
    if idx >= len(row):
        return None
    v = row[idx]
    if v is None:
        return None
    s = str(v).strip()
    return s or None


def _all_subtables(path: Path) -> list[RawSubtable]:
    out: list[RawSubtable] = []
    for sheet in POINT_SHEETS:
        rows = read_sheet_rows(path, sheet)
        out.extend(split_subtables(sheet, rows))
    return out


# --------------------------------------------------------------------------
# IEC-104
# --------------------------------------------------------------------------


@dataclass
class IecRow:
    device: str
    device_addr: int
    address: int
    name_raw: str
    access: str
    data_type: str
    precision: str | None
    attribute: str | None  # "alarm" | "realdata"
    description: str | None


def parse_iec104(path: Path) -> list[IecRow]:
    out: list[IecRow] = []
    for st in _all_subtables(path):
        device, addr = resolve_device(st.device_code, st.sheet)
        for row in st.rows:
            if not isinstance(row[0], int):
                raise CatalogParseError(
                    f"IEC104 {st.sheet}/{device}: non-integer address {row[0]!r}"
                )
            out.append(
                IecRow(
                    device=device,
                    device_addr=addr,
                    address=row[0],
                    name_raw=_cell_str(row, 1) or "",
                    access=_cell_str(row, 2) or "",
                    data_type=_cell_str(row, 3) or "",
                    precision=_cell_str(row, 4),
                    attribute=_cell_str(row, 5),
                    description=_cell_str(row, 6),
                )
            )
    return out


# --------------------------------------------------------------------------
# Modbus
# --------------------------------------------------------------------------

_MODBUS_CLASS_FUNCTION = {2: [2], 3: [3, 6, 16], 4: [4]}

# Not documented in AGENT-TASK §2.3, confirmed by hand against the real
# file: the Modbus workbook has a stray row shaped exactly like a real
# telemetry point — reg_class 4, a real-looking address (30001), RO/I32/
# realdata — but its name is a leftover template label, not a point. Seen
# on both Converter (addr 30001) and Master Controller (addr 30001), right
# at the alarm->telemetry section boundary where IEC-104 has a (skipped)
# repeated header row instead. Filtered explicitly rather than left to
# become a spurious "found only in Modbus" entry in the validation report.
_MODBUS_JUNK_ROW_NAMES = {"Parameter Names:"}


@dataclass
class ModbusRow:
    device: str
    device_addr: int
    reg_class: int  # 2 discrete input / 3 holding register / 4 input register
    address: int
    name_raw: str
    access: str
    data_type: str
    precision: str | None
    attribute: str | None
    description: str | None


def parse_modbus(path: Path, dropped: list[tuple[str, int, str]] | None = None) -> list[ModbusRow]:
    """Parse the Modbus workbook. `dropped`, if given, is appended with
    (device, address, name) for every junk row filtered out (see
    _MODBUS_JUNK_ROW_NAMES) so callers can report it instead of losing it
    silently."""
    out: list[ModbusRow] = []
    for st in _all_subtables(path):
        device, addr = resolve_device(st.device_code, st.sheet)
        for row in st.rows:
            if not isinstance(row[0], int) or not isinstance(row[1], int):
                raise CatalogParseError(
                    f"Modbus {st.sheet}/{device}: bad class/address {row[0]!r}/{row[1]!r}"
                )
            reg_class = row[0]
            if reg_class not in _MODBUS_CLASS_FUNCTION:
                raise CatalogParseError(
                    f"Modbus {st.sheet}/{device}: unknown register class {reg_class!r}"
                )
            name = _cell_str(row, 2) or ""
            if name in _MODBUS_JUNK_ROW_NAMES:
                if dropped is not None:
                    dropped.append((device, row[1], name))
                continue
            out.append(
                ModbusRow(
                    device=device,
                    device_addr=addr,
                    reg_class=reg_class,
                    address=row[1],
                    name_raw=name,
                    access=_cell_str(row, 3) or "",
                    data_type=_cell_str(row, 4) or "",
                    precision=_cell_str(row, 5),
                    attribute=_cell_str(row, 6),
                    description=_cell_str(row, 7),
                )
            )
    return out


# --------------------------------------------------------------------------
# TAG
# --------------------------------------------------------------------------


@dataclass
class TagRow:
    device: str
    device_addr: int
    tag: str
    name_raw: str  # UNRELIABLE on the EMS sheet (offset ~26 rows, §2.3.7) — never
    #                used for joining, kept only for debugging/inspection.
    access: str
    data_type: str
    attribute: str | None  # from the mislabeled "Precision" column, §2.2
    description: str | None


def parse_tag(path: Path) -> list[TagRow]:
    out: list[TagRow] = []
    for st in _all_subtables(path):
        device, addr = resolve_device(st.device_code, st.sheet)
        for row in st.rows:
            tag = _cell_str(row, 0)
            if not tag:
                raise CatalogParseError(
                    f"TAG {st.sheet}/{device}: empty tag in row {row!r}"
                )
            out.append(
                TagRow(
                    device=device,
                    device_addr=addr,
                    tag=tag,
                    name_raw=_cell_str(row, 1) or "",
                    access=_cell_str(row, 2) or "",
                    data_type=_cell_str(row, 3) or "",
                    attribute=_cell_str(row, 4),
                    description=_cell_str(row, 5),
                )
            )
    return out
