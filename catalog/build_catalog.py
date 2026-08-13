#!/usr/bin/env python3
"""Build catalog/point_catalog.json from the three M261 register-map xlsx
files. See the internal task specification, task 1.

Usage:
    python3 -m catalog.build_catalog
    python3 -m catalog.build_catalog --registermap m261-registermap --out catalog/point_catalog.json
"""
from __future__ import annotations

import argparse
import json
import sys
from collections import defaultdict
from pathlib import Path

import yaml

from catalog.join import JoinedPoint, JoinWarnings, join_device
from catalog.normalize import extract_unit, parse_enum, slugify, validate_range
from catalog.parsing import (
    CatalogParseError,
    DEVICE_ADDR,
    IecRow,
    ModbusRow,
    TagRow,
    parse_iec104,
    parse_modbus,
    parse_tag,
)

REPO_ROOT = Path(__file__).resolve().parent.parent
DEFAULT_REGISTERMAP = REPO_ROOT / "m261-registermap"
DEFAULT_OUT = REPO_ROOT / "catalog" / "point_catalog.json"
DEFAULT_OVERRIDES = REPO_ROOT / "catalog" / "overrides.yaml"

# Internal specification §4.9 control anchors — hard-fail the build if any of these
# don't hold. All six are EMS points, keyed by their canonical IEC-104 address.
ANCHORS: dict[int, str] = {
    1: "SysStoped",  # Manual Protection
    16410: "Online",  # Online Status
    25089: "AirCondStartStopCtrl",  # Air Conditioner Control (first setpoint)
    25093: "ControlMode",  # Set Operating Mode
    25098: "SOCmax",  # Maximum Charge SOC
    25236: "SDistCool",  # Cooling Target Temperature During Idle (last setpoint)
}

# Task 1.8 requires these points to be flagged dangerous, matched by
# (device, normalized name) rather than address so overrides.yaml stays the
# only place addresses are hand-maintained.
DANGEROUS_NAMES = {("EMS", "trip"), ("EMS", "clear protection")}


def _group_by_device(rows: list) -> dict[str, list]:
    out: dict[str, list] = defaultdict(list)
    for r in rows:
        out[r.device].append(r)
    return out


def build_points(
    registermap_dir: Path, warnings: JoinWarnings | None = None
) -> tuple[list[JoinedPoint], JoinWarnings]:
    if warnings is None:
        warnings = JoinWarnings()
    iec_rows = parse_iec104(registermap_dir / "M261_points_IEC104.xlsx")
    modbus_rows = parse_modbus(
        registermap_dir / "M261_points_Modbus.xlsx", dropped=warnings.dropped_modbus_junk_rows
    )
    tag_rows = parse_tag(registermap_dir / "M261_points_TAG.xlsx")

    iec_by_device = _group_by_device(iec_rows)
    modbus_by_device = _group_by_device(modbus_rows)
    tag_by_device = _group_by_device(tag_rows)

    all_points: list[JoinedPoint] = []
    for device in DEVICE_ADDR:
        if device not in iec_by_device:
            continue
        all_points.extend(
            join_device(
                device,
                iec_by_device[device],
                modbus_by_device.get(device, []),
                tag_by_device.get(device, []),
                warnings,
            )
        )
    return all_points, warnings


def check_anchors(points: list[JoinedPoint]) -> None:
    by_addr = {p.iec104_addr: p for p in points if p.device == "EMS"}
    failures = []
    for addr, expected_tag in ANCHORS.items():
        actual = by_addr.get(addr)
        if actual is None:
            failures.append(f"addr {addr}: point missing entirely")
        elif actual.tag != expected_tag:
            failures.append(f"addr {addr}: expected tag {expected_tag!r}, got {actual.tag!r}")
    if failures:
        raise CatalogParseError(
            "§4.9 anchor check failed — TAG positional alignment is broken:\n  "
            + "\n  ".join(failures)
        )


def _slug_for(device: str, name_raw: str, used: set[str]) -> str:
    base = slugify(name_raw) or "point"
    slug = base
    n = 2
    while slug in used:
        slug = f"{base}_{n}"
        n += 1
    used.add(slug)
    return slug


def to_record(point: JoinedPoint, slug: str) -> dict:
    scale = 1
    if point.precision_iec is not None:
        try:
            scale = int(float(point.precision_iec))
        except ValueError:
            scale = 1
    is_dangerous = (point.device, slugify(point.name_raw).replace("_", " ")) in DANGEROUS_NAMES
    return {
        "device": point.device,
        "device_addr": point.device_addr,
        "tag": point.tag,
        "name_raw": point.name_raw,
        "slug": slug,
        "class": point.point_class,
        "access": point.access,
        "iec104_addr": point.iec104_addr,
        "modbus_addr": point.modbus_addr,
        "modbus_class": point.modbus_class,
        "modbus_function": point.modbus_function,
        "data_type": point.data_type,
        "scale": scale,
        "unit": extract_unit(point.name_raw),
        "enum": parse_enum(point.description),
        "description": point.description,
        "dangerous": is_dangerous,
        "readback_iec104_addr": point.readback_iec104_addr,
        # No numeric range is present anywhere in the register map (only
        # enum constraints, handled above) — null rather than an invented
        # number, same "unconfirmed stays null/config" rule the map follows
        # elsewhere. AGENT-TASK §6 item 1: the only legitimate source for a
        # non-null range is catalog/overrides.yaml, once Stage 0 confirms
        # one — see apply_overrides/validate_ranges below.
        "range": None,
        "sources": point.sources,
    }


def apply_overrides(records: list[dict], overrides_path: Path) -> list[dict]:
    if not overrides_path.exists():
        return records
    data = yaml.safe_load(overrides_path.read_text(encoding="utf-8")) or {}
    points_cfg = data.get("points", {})
    if not points_cfg:
        return records
    by_key = {(r["device"], r["slug"]): r for r in records}
    for device, slugs in points_cfg.items():
        for slug, fields in (slugs or {}).items():
            rec = by_key.get((device, slug))
            if rec is None:
                raise CatalogParseError(
                    f"overrides.yaml references unknown point ({device}, {slug})"
                )
            rec.update(fields)
    return records


def validate_ranges(records: list[dict]) -> None:
    """AGENT-TASK §6 item 1: a point's `range` is either null or a
    well-formed {"min", "max"} object (catalog/normalize.validate_range) —
    checked here, after overrides.yaml has been applied, so a malformed
    override fails the build loudly (matching apply_overrides' own
    unknown-point-key failure) rather than silently producing a catalog
    Task 6's commands.Processor would choke on later. Collects every
    failure before raising, same style as check_anchors.
    """
    failures = []
    for r in records:
        reason = validate_range(r.get("range"))
        if reason is not None:
            failures.append(f"{r['device']}/{r['slug']}: {reason}")
    if failures:
        raise CatalogParseError("invalid `range` field(s):\n  " + "\n  ".join(failures))


def build_catalog(
    registermap_dir: Path, overrides_path: Path, warnings: JoinWarnings | None = None
) -> list[dict]:
    points, warnings = build_points(registermap_dir, warnings)
    check_anchors(points)

    # Slugs are assigned in a priority order — alarms/setpoints before
    # telemetry — so that a setpoint claims the clean slug (e.g.
    # "set_operating_mode") and its readback-duplicate telemetry twin
    # (same name_raw, by construction — §3.1/§4.3) gets the "_2" suffix,
    # never the other way around. Output order is still by iec104_addr.
    _SLUG_PRIORITY = {"alarm": 0, "setpoint": 0, "telemetry": 1}
    used_slugs_by_device: dict[str, set[str]] = defaultdict(set)
    slug_by_point_id: dict[int, str] = {}
    for p in sorted(points, key=lambda p: (p.device, _SLUG_PRIORITY[p.point_class], p.iec104_addr)):
        slug_by_point_id[id(p)] = _slug_for(p.device, p.name_raw, used_slugs_by_device[p.device])

    points.sort(key=lambda p: (p.device, p.iec104_addr))
    records = [to_record(p, slug_by_point_id[id(p)]) for p in points]

    records = apply_overrides(records, overrides_path)
    validate_ranges(records)
    return records


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--registermap", type=Path, default=DEFAULT_REGISTERMAP)
    ap.add_argument("--out", type=Path, default=DEFAULT_OUT)
    ap.add_argument("--overrides", type=Path, default=DEFAULT_OVERRIDES)
    args = ap.parse_args(argv)

    warnings = JoinWarnings()
    try:
        records = build_catalog(args.registermap, args.overrides, warnings)
    except CatalogParseError as e:
        print(f"error: {e}", file=sys.stderr)
        return 1

    args.out.parent.mkdir(parents=True, exist_ok=True)
    with args.out.open("w", encoding="utf-8") as f:
        json.dump(records, f, ensure_ascii=False, indent=2, sort_keys=False)
        f.write("\n")

    for device, addr, name in warnings.dropped_modbus_junk_rows:
        print(
            f"note: dropped junk Modbus row {device} addr={addr} name={name!r} "
            f"(template artifact, not a real point)",
            file=sys.stderr,
        )
    print(f"wrote {len(records)} points to {args.out}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
