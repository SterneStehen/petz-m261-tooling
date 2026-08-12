"""Three-way join of IEC-104 / Modbus / TAG rows into one record per point.

Implements AGENT-TASK §4.9 exactly:

  - IEC-104 <-> Modbus: by normalized name (catalog.normalize.join_key),
    with the §4.3 address formula as a fallback ONLY for setpoints, and
    only ever used as a controlled fallback/cross-check, never as the
    primary join method.
  - TAG: purely positional against the IEC-104 file's RO rows (alarms then
    telemetry, in file order) — TAG's own Name column is never read for
    joining (§2.3.7 offset trap).
  - A setpoint's TAG comes from the position of its own readback point
    (§4.3: readback_iec104_addr = 16411 + (iec_addr - 25089)), not from the
    position of the setpoint's own WO row. The readback row itself then
    carries no tag of its own (the tag belongs to the setpoint/readback
    pair as a whole, of which the setpoint is the addressable "primary").
"""
from __future__ import annotations

from collections import defaultdict, deque
from dataclasses import dataclass, field

from catalog.normalize import join_key
from catalog.parsing import CatalogParseError, IecRow, ModbusRow, TagRow

MODBUS_FUNCTION = {2: [2], 3: [3, 6, 16], 4: [4]}

READBACK_BASE_IEC = 16411
SETPOINT_BASE_IEC = 25089
SETPOINT_MAX_IEC = 25236


@dataclass
class JoinedPoint:
    device: str
    device_addr: int
    tag: str | None
    name_raw: str
    point_class: str  # alarm | telemetry | setpoint
    access: str
    iec104_addr: int
    modbus_addr: int | None
    modbus_class: int | None
    modbus_function: list[int] | None
    data_type: str
    precision_iec: str | None
    precision_modbus: str | None
    description: str | None
    readback_iec104_addr: int | None
    sources: list[str]


@dataclass
class JoinWarnings:
    unmatched_modbus: list[tuple[str, int, str]] = field(default_factory=list)
    unmatched_iec_alarms_telemetry: list[tuple[str, int, str]] = field(default_factory=list)
    setpoints_joined_by_formula: list[tuple[str, int, str, str]] = field(default_factory=list)
    setpoints_without_readback: list[tuple[str, int]] = field(default_factory=list)
    dropped_modbus_junk_rows: list[tuple[str, int, str]] = field(default_factory=list)


def _point_class(row: IecRow) -> str:
    if row.attribute == "alarm":
        return "alarm"
    return "setpoint" if row.access == "WO" else "telemetry"


def _modbus_pool(rows: list[ModbusRow], reg_class: int) -> dict[str, deque[ModbusRow]]:
    pool: dict[str, deque[ModbusRow]] = defaultdict(deque)
    for r in rows:
        if r.reg_class == reg_class:
            pool[join_key(r.name_raw)].append(r)
    return pool


def join_device(
    device: str,
    iec_rows: list[IecRow],
    modbus_rows: list[ModbusRow],
    tag_rows: list[TagRow],
    warnings: JoinWarnings,
) -> list[JoinedPoint]:
    alarm_pool = _modbus_pool(modbus_rows, 2)
    telemetry_pool = _modbus_pool(modbus_rows, 4)
    setpoint_pool = _modbus_pool(modbus_rows, 3)
    # index for the formula fallback: exact modbus address -> row (setpoints only)
    setpoint_by_addr = {r.address: r for r in modbus_rows if r.reg_class == 3}

    points: dict[int, JoinedPoint] = {}  # keyed by iec104_addr, one per source row
    readback_claimed: dict[int, int] = {}  # readback iec addr -> owning setpoint iec addr

    # Pass 1: setpoints first, so we know which telemetry rows are readback
    # duplicates before we try to name-join telemetry against Modbus.
    for row in iec_rows:
        if _point_class(row) != "setpoint":
            continue
        expected_modbus_addr = 40001 + (row.address - SETPOINT_BASE_IEC) * 2
        key = join_key(row.name_raw)
        mrow = setpoint_pool[key].popleft() if setpoint_pool[key] else None
        if mrow is None:
            mrow = setpoint_by_addr.get(expected_modbus_addr)
            if mrow is not None:
                # pull it out of the name pool too, if it's still sitting there
                nk = join_key(mrow.name_raw)
                if mrow in setpoint_pool[nk]:
                    setpoint_pool[nk].remove(mrow)
                warnings.setpoints_joined_by_formula.append(
                    (device, row.address, row.name_raw, mrow.name_raw)
                )
            else:
                # Code-review finding: neither the name-join nor the §4.3
                # formula found a Modbus counterpart at all. Unlike alarms
                # and telemetry (where a genuine gap is a legitimate,
                # reportable data-quality issue — see
                # test_known_residual_join_gaps_are_exactly_the_documented_ones),
                # a setpoint is REQUIRED to have both addresses (Task 1
                # acceptance: "all 148 write points have both addresses").
                # Manufacturer's map always provides both for every one of
                # the 148 real setpoints, so a missing Modbus row here means
                # something is broken upstream (bad file, bad override, a
                # future revision that dropped a register) — surface it as
                # a hard failure, not a catalog entry with modbus_addr: null.
                raise CatalogParseError(
                    f"{device} setpoint iec104_addr={row.address} ({row.name_raw!r}) "
                    f"has no Modbus counterpart by name AND none at the §4.3 "
                    f"formula address {expected_modbus_addr} — a setpoint must "
                    f"have both addresses, refusing to emit modbus_addr: null"
                )
        elif mrow.address != expected_modbus_addr:
            # §4.3: the formula is a mandatory CONTROL CHECK after the
            # name-based join, not merely a fallback for when it fails. A
            # name match that lands on the wrong address means the join
            # picked up the wrong row (duplicate/colliding name, corrupted
            # pool state, ...) — accepting it would silently corrupt the
            # catalog, so this is a hard failure, same as an anchor miss.
            raise CatalogParseError(
                f"{device} setpoint iec104_addr={row.address} ({row.name_raw!r}) "
                f"matched Modbus addr={mrow.address} by name, but §4.3 formula "
                f"requires {expected_modbus_addr} — refusing to accept a "
                f"name-matched address that fails its own control check"
            )

        readback_addr: int | None = None
        if SETPOINT_BASE_IEC <= row.address <= SETPOINT_MAX_IEC:
            candidate = READBACK_BASE_IEC + (row.address - SETPOINT_BASE_IEC)
            readback_addr = candidate
            readback_claimed[candidate] = row.address
        else:
            warnings.setpoints_without_readback.append((device, row.address))

        points[row.address] = JoinedPoint(
            device=device,
            device_addr=row.device_addr,
            tag=None,  # filled in by TAG positional pass, below
            name_raw=row.name_raw,
            point_class="setpoint",
            access=row.access,
            iec104_addr=row.address,
            modbus_addr=mrow.address if mrow else None,
            modbus_class=mrow.reg_class if mrow else None,
            modbus_function=MODBUS_FUNCTION[mrow.reg_class] if mrow else None,
            data_type=row.data_type,
            precision_iec=row.precision,
            precision_modbus=mrow.precision if mrow else None,
            description=row.description,
            readback_iec104_addr=readback_addr,
            sources=["IEC104"] + (["Modbus"] if mrow else []),
        )

    # Pass 2: alarms and telemetry (skipping readback-duplicate addresses for
    # the Modbus name-join, since Modbus has no separate entry for them).
    for row in iec_rows:
        cls = _point_class(row)
        if cls == "setpoint":
            continue

        is_readback_dup = row.address in readback_claimed
        mrow = None
        if cls == "alarm":
            key = join_key(row.name_raw)
            if alarm_pool[key]:
                mrow = alarm_pool[key].popleft()
            else:
                warnings.unmatched_iec_alarms_telemetry.append((device, row.address, row.name_raw))
        elif not is_readback_dup:
            key = join_key(row.name_raw)
            if telemetry_pool[key]:
                mrow = telemetry_pool[key].popleft()
            else:
                warnings.unmatched_iec_alarms_telemetry.append((device, row.address, row.name_raw))

        points[row.address] = JoinedPoint(
            device=device,
            device_addr=row.device_addr,
            tag=None,
            name_raw=row.name_raw,
            point_class=cls,
            access=row.access,
            iec104_addr=row.address,
            modbus_addr=mrow.address if mrow else None,
            modbus_class=mrow.reg_class if mrow else None,
            modbus_function=MODBUS_FUNCTION[mrow.reg_class] if mrow else None,
            data_type=row.data_type,
            precision_iec=row.precision,
            precision_modbus=mrow.precision if mrow else None,
            description=row.description,
            readback_iec104_addr=None,
            sources=["IEC104"] + (["Modbus"] if mrow else []),
        )

    for pool, label in ((alarm_pool, "alarm"), (telemetry_pool, "telemetry"), (setpoint_pool, "setpoint")):
        for dq in pool.values():
            for leftover in dq:
                warnings.unmatched_modbus.append((device, leftover.address, leftover.name_raw))

    # Pass 3: TAG, purely positional against IEC-104's RO rows (§4.9).
    iec_ro_rows = [r for r in iec_rows if r.access == "RO"]
    if len(tag_rows) != len(iec_ro_rows):
        raise ValueError(
            f"{device}: TAG row count ({len(tag_rows)}) != IEC-104 RO row count "
            f"({len(iec_ro_rows)}) — positional join (§4.9) is broken, refusing to guess"
        )

    tag_by_ro_addr = {r.address: t.tag for r, t in zip(iec_ro_rows, tag_rows)}

    # A setpoint's tag is the tag positioned at its readback address, not at
    # the setpoint's own (WO) address, which never appears in iec_ro_rows.
    for addr, point in points.items():
        if point.point_class == "setpoint" and point.readback_iec104_addr in tag_by_ro_addr:
            point.tag = tag_by_ro_addr.pop(point.readback_iec104_addr)

    for addr, tag in tag_by_ro_addr.items():
        points[addr].tag = tag

    for point in points.values():
        if point.tag is not None:
            point.sources.append("TAG")

    return [points[addr] for addr in sorted(points)]
