#!/usr/bin/env python3
"""Generate the Python package gen/python/m261_points from
catalog/point_catalog.json: dataclass models, a metadata dict, the same
address constants as the Go package, and IntEnum types for enum-bearing
points.

Usage:
    python3 -m codegen.gen_python
"""
from __future__ import annotations

import argparse
import json
import re
from pathlib import Path

from codegen.common import (
    DEVICE_ORDER,
    DEVICE_PASCAL,
    GENERATED_BANNER_PY,
    REPO_ROOT,
    enum_items,
    group_by_device,
    load_catalog,
    point_identifier,
)

DEFAULT_OUT = REPO_ROOT / "gen" / "python" / "m261_points"

_WORD_RE = re.compile(r"[a-zA-Z0-9]+")


def py_const_name(device: str, slug: str) -> str:
    return f"{device}_{slug}".upper()


def py_enum_member_name(label: str) -> str:
    parts = _WORD_RE.findall(label)
    name = "_".join(p.upper() for p in parts) or "VALUE"
    if name[0].isdigit():
        name = "N" + name
    return name


def py_str(s: str | None) -> str:
    return repr(s or "")


def gen_constants(records: list[dict]) -> str:
    lines = [GENERATED_BANNER_PY, '"""Address constants — IEC-104 always, Modbus where present."""', ""]
    for r in records:
        name = py_const_name(r["device"], r["slug"])
        lines.append(f"{name}: int = {r['iec104_addr']}  # {r['name_raw']}")
        if r["modbus_addr"] is not None:
            lines.append(f"{name}_MODBUS: int = {r['modbus_addr']}")
    lines.append("")
    return "\n".join(lines)


def gen_enums(records: list[dict]) -> tuple[str, dict[tuple[str, str], str]]:
    lines = [GENERATED_BANNER_PY, "", "from __future__ import annotations", "", "from enum import IntEnum", ""]
    enum_types: dict[tuple[str, str], str] = {}
    for r in records:
        items = enum_items(r["enum"])
        if not items:
            continue
        ident = point_identifier(r["device"], r["slug"])
        enum_type = ident + "Enum"
        enum_types[(r["device"], r["slug"])] = enum_type
        lines.append("")
        lines.append(f"class {enum_type}(IntEnum):")
        lines.append(f'    """{r["name_raw"]}: {r["description"]}"""')
        seen: set[str] = set()
        for code, label in items:
            base = py_enum_member_name(label)
            name = base
            n = 2
            while name in seen:
                name = f"{base}_{n}"
                n += 1
            seen.add(name)
            lines.append(f"    {name} = {code}")
        lines.append("")
    lines.append("")
    return "\n".join(lines), enum_types


def gen_models(
    records: list[dict], records_by_device: dict[str, list[dict]], enum_types: dict[tuple[str, str], str]
) -> str:
    lines = [
        GENERATED_BANNER_PY,
        "",
        "from __future__ import annotations",
        "",
        "from dataclasses import dataclass",
        "from typing import NamedTuple, Optional",
        "",
        "from .enums import *  # noqa: F401,F403 — enum types referenced by state dataclasses below",
        "",
    ]

    lines.append("class PointKey(NamedTuple):")
    lines.append('    """(device, slug) — the same key catalog/overrides.yaml uses."""')
    lines.append("    device: str")
    lines.append("    slug: str")
    lines.append("")
    lines.append("")
    lines.append("@dataclass(frozen=True)")
    lines.append("class PointMeta:")
    lines.append('    """One point\'s full catalog record."""')
    lines.append("    device: str")
    lines.append("    device_addr: int")
    lines.append("    tag: Optional[str]")
    lines.append("    name_raw: str")
    lines.append("    slug: str")
    lines.append("    point_class: str  # 'alarm' | 'telemetry' | 'setpoint' — 'class' is a Python keyword")
    lines.append("    access: str  # 'RO' | 'WO'")
    lines.append("    iec104_addr: int")
    lines.append("    modbus_addr: Optional[int]")
    lines.append("    modbus_class: Optional[int]")
    lines.append("    modbus_function: Optional[list[int]]")
    lines.append("    data_type: str  # 'U8' | 'I16' | 'F32'")
    lines.append("    scale: float")
    lines.append("    unit: Optional[str]")
    lines.append("    enum: Optional[dict[int, str]]")
    lines.append("    description: Optional[str]")
    lines.append("    dangerous: bool")
    lines.append("    readback_iec104_addr: Optional[int]")
    lines.append("    sources: list[str]")
    lines.append("")
    lines.append("")

    for device in DEVICE_ORDER:
        recs = records_by_device.get(device, [])
        if not recs:
            continue
        struct_name = DEVICE_PASCAL[device] + "State"
        lines.append("@dataclass")
        lines.append(f"class {struct_name}:")
        lines.append(f'    """Current value of every {device} point."""')
        for r in sorted(recs, key=lambda r: r["iec104_addr"]):
            field_name = r["slug"]
            if field_name[0].isdigit():
                field_name = "n_" + field_name  # Python identifiers can't start with a digit
            enum_type = enum_types.get((r["device"], r["slug"]))
            if enum_type:
                # Default to the lowest defined code, not a hardcoded 0 — a
                # handful of real enums (e.g. PCS fault_reset_command,
                # {32768: "Reset"}) don't have a 0 member, and IntEnum(0)
                # would raise ValueError at import time.
                default_code = enum_items(r["enum"])[0][0]
                lines.append(f"    {field_name}: {enum_type} = {enum_type}({default_code})  # {r['name_raw']}")
            else:
                lines.append(f"    {field_name}: float = 0.0  # {r['name_raw']}")
        lines.append("")
        lines.append("")

    return "\n".join(lines)


def gen_points(records: list[dict]) -> str:
    lines = [
        GENERATED_BANNER_PY,
        "",
        "from __future__ import annotations",
        "",
        "from .models import PointKey, PointMeta",
        "",
        f"POINTS: dict[PointKey, PointMeta] = {{",
    ]
    for r in records:
        key = f'PointKey({r["device"]!r}, {r["slug"]!r})'
        enum_py = "None" if not r["enum"] else "{" + ", ".join(f"{int(k)}: {v!r}" for k, v in r["enum"].items()) + "}"
        modbus_function = "None" if not r["modbus_function"] else str(list(r["modbus_function"]))
        lines.append(
            f"    {key}: PointMeta("
            f"device={r['device']!r}, "
            f"device_addr={r['device_addr']}, "
            f"tag={r['tag']!r}, "
            f"name_raw={r['name_raw']!r}, "
            f"slug={r['slug']!r}, "
            f"point_class={r['class']!r}, "
            f"access={r['access']!r}, "
            f"iec104_addr={r['iec104_addr']}, "
            f"modbus_addr={r['modbus_addr']!r}, "
            f"modbus_class={r['modbus_class']!r}, "
            f"modbus_function={modbus_function}, "
            f"data_type={r['data_type']!r}, "
            f"scale={float(r['scale'])}, "
            f"unit={r['unit']!r}, "
            f"enum={enum_py}, "
            f"description={r['description']!r}, "
            f"dangerous={r['dangerous']!r}, "
            f"readback_iec104_addr={r['readback_iec104_addr']!r}, "
            f"sources={list(r['sources'])!r}),"
        )
    lines.append("}")
    lines.append("")
    return "\n".join(lines)


def gen_init(records: list[dict]) -> str:
    return f'''{GENERATED_BANNER_PY}
"""M261 point catalog — generated Python models.

{len(records)} points. See catalog/point_catalog.json for the source of
truth and catalog/overrides.yaml for manual corrections.
"""
from __future__ import annotations

from .models import PointKey, PointMeta
from .points import POINTS

__all__ = ["PointKey", "PointMeta", "POINTS"]
'''


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

    enums_src, enum_types = gen_enums(records)
    write(args.out / "enums.py", enums_src)
    write(args.out / "constants.py", gen_constants(records))
    write(args.out / "models.py", gen_models(records, by_device, enum_types))
    write(args.out / "points.py", gen_points(records))
    write(args.out / "__init__.py", gen_init(records))
    write(args.out / "py.typed", "")

    print(f"wrote {len(records)} points to Python package at {args.out}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
