#!/usr/bin/env python3
"""Generate gen/seeds/{alarms,metrics,setpoints}.json from
catalog/point_catalog.json.

JSON has no comment syntax, so the "DO NOT EDIT" banner (identical in
spirit to the Go/Python generated-file header) lives in a `_generated`
top-level field instead of a leading comment line.

Usage:
    python3 -m codegen.gen_seeds
"""
from __future__ import annotations

import argparse
import json
from pathlib import Path

from codegen.common import GENERATED_BANNER_JSON_NOTE, REPO_ROOT, load_catalog

DEFAULT_OUT = REPO_ROOT / "gen" / "seeds"


def alarm_record(r: dict) -> dict:
    return {
        "device": r["device"],
        "device_addr": r["device_addr"],
        "iec104_addr": r["iec104_addr"],
        "modbus_addr": r["modbus_addr"],
        "tag": r["tag"],
        "slug": r["slug"],
        "name": r["name_raw"],
        "description": r["description"],
        "severity": None,  # not in the register map — filled in by hand later
    }


def metric_record(r: dict) -> dict:
    return {
        "device": r["device"],
        "device_addr": r["device_addr"],
        "iec104_addr": r["iec104_addr"],
        "modbus_addr": r["modbus_addr"],
        "tag": r["tag"],
        "slug": r["slug"],
        "name": r["name_raw"],
        "unit": r["unit"],
        "data_type": r["data_type"],
        "scale": r["scale"],
    }


def setpoint_record(r: dict) -> dict:
    return {
        "device": r["device"],
        "device_addr": r["device_addr"],
        "iec104_addr": r["iec104_addr"],
        "modbus_addr": r["modbus_addr"],
        "readback_iec104_addr": r["readback_iec104_addr"],
        "tag": r["tag"],
        "slug": r["slug"],
        "name": r["name_raw"],
        "data_type": r["data_type"],
        "scale": r["scale"],
        "enum": r["enum"],
        # AGENT-TASK §6 item 1: null, or {"min": <number|null>, "max":
        # <number|null>} with at least one bound set — sourced from
        # catalog/point_catalog.json's own "range" field (itself always
        # null for the real map; only catalog/overrides.yaml can set one,
        # once Stage 0 confirms it). Read through, not reinvented here —
        # this used to be hardcoded None regardless of the catalog record.
        "range": r["range"],
        "dangerous": r["dangerous"],
    }


def write_seed(path: Path, records: list[dict]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    payload = {"_generated": GENERATED_BANNER_JSON_NOTE, "count": len(records), "points": records}
    path.write_text(json.dumps(payload, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--catalog", type=Path, default=None)
    ap.add_argument("--out", type=Path, default=DEFAULT_OUT)
    args = ap.parse_args(argv)

    records = load_catalog(args.catalog) if args.catalog else load_catalog()

    alarms = [alarm_record(r) for r in records if r["class"] == "alarm"]
    metrics = [metric_record(r) for r in records if r["class"] == "telemetry"]
    setpoints = [setpoint_record(r) for r in records if r["class"] == "setpoint"]

    write_seed(args.out / "alarms.json", alarms)
    write_seed(args.out / "metrics.json", metrics)
    write_seed(args.out / "setpoints.json", setpoints)

    print(f"wrote {len(alarms)} alarms, {len(metrics)} metrics, {len(setpoints)} setpoints to {args.out}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
