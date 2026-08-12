#!/usr/bin/env python3
"""Validate catalog/point_catalog.json and write catalog/validation_report.md.

See the internal task specification, task 2, for the full checklist. Exits
non-zero if any CRITICAL check fails; warnings never affect the exit code
but are always listed by name in the report.

Usage:
    python3 -m catalog.validate_catalog
    python3 -m catalog.validate_catalog --catalog catalog/point_catalog.json \
        --registermap m261-registermap --out catalog/validation_report.md
"""
from __future__ import annotations

import argparse
import json
import re
import sys
from collections import Counter, defaultdict
from dataclasses import dataclass, field
from pathlib import Path

from catalog.build_catalog import build_points
from catalog.parsing import parse_iec104, parse_modbus, parse_tag, split_subtables
from catalog.xlsx_reader import read_sheet_rows

REPO_ROOT = Path(__file__).resolve().parent.parent
DEFAULT_CATALOG = REPO_ROOT / "catalog" / "point_catalog.json"
DEFAULT_REGISTERMAP = REPO_ROOT / "m261-registermap"
DEFAULT_OUT = REPO_ROOT / "catalog" / "validation_report.md"

CRITICAL = "critical"
WARNING = "warning"

# EMS setpoints (25089-25236, WO) each have a mirror read-back point in this
# IEC-104 range (16411-16558, RO/telemetry) — see §3.2/§4.3. They have no
# Modbus or TAG entry of their own by design, not by omission.
EMS_READBACK_RANGE = range(16411, 16559)


@dataclass
class CheckResult:
    name: str
    level: str  # CRITICAL | WARNING
    passed: bool
    summary: str
    details: list[str] = field(default_factory=list)
    note: str | None = None  # extra prose, e.g. explaining an unexpected number
    # Whether this warning's note belongs in "Questions for the manufacturer".
    # False for checks whose note is an internal calibration remark (e.g.
    # "AGENT-TASK expected 20, real files have 0") rather than something to
    # actually ask the vendor.
    manufacturer_question: bool = True


def load_catalog(path: Path) -> list[dict]:
    return json.loads(path.read_text(encoding="utf-8"))


def _has_cjk(s: str) -> bool:
    return any("一" <= ch <= "鿿" for ch in s)


# --------------------------------------------------------------------------
# Checks working purely off point_catalog.json
# --------------------------------------------------------------------------

# AGENT-TASK §3.1
_EXPECTED_3_1 = {
    ("EMS", "alarm"): 31, ("EMS", "telemetry"): 174, ("EMS", "setpoint"): 148,
    ("PCS", "alarm"): 88, ("PCS", "telemetry"): 91,
    ("BMS", "alarm"): 116, ("BMS", "telemetry"): 266,
    ("BMS_CELLS", "telemetry"): 391,
    ("TMS", "alarm"): 49, ("TMS", "telemetry"): 102,
    ("PCS_METER", "telemetry"): 31,
    ("DIDO", "telemetry"): 8,
    ("CSJ", "telemetry"): 18,
}
_EXPECTED_TOTAL_3_1 = 1513
_EXPECTED_TOTAL_3_2 = 1365  # §3.2: Modbus/TAG representation, no readback duplicates
_EXPECTED_EMS_3_2 = 205


def check_control_figures(records: list[dict], registermap_dir: Path) -> CheckResult:
    # §3.1: catalog vs. its own IEC-104-anchored bookkeeping.
    counts = Counter((r["device"], r["class"]) for r in records)
    failures = [
        f"{dev}/{cls}: expected {expected}, got {counts.get((dev, cls), 0)}"
        for (dev, cls), expected in _EXPECTED_3_1.items()
        if counts.get((dev, cls), 0) != expected
    ]
    total = len(records)
    if total != _EXPECTED_TOTAL_3_1:
        failures.append(f"total (§3.1): expected {_EXPECTED_TOTAL_3_1}, got {total}")

    ems_readback = sum(
        1 for r in records
        if r["device"] == "EMS" and r["class"] == "telemetry" and r["iec104_addr"] in EMS_READBACK_RANGE
    )
    if ems_readback != 148:
        failures.append(f"EMS readback-duplicate range 16411-16558: expected 148 points, got {ems_readback}")

    # §3.2: code-review finding — the check above only ever re-derives its
    # own "1365" from the catalog's own total (1513 - 148), which passes
    # unconditionally regardless of what the Modbus/TAG files actually
    # contain. §3.2 is a claim about the SOURCE FILES, so verify it against
    # them directly: parse Modbus and TAG independently of the catalog/join
    # pipeline and count their real rows.
    dropped_modbus_junk: list[tuple[str, int, str]] = []
    modbus_rows = parse_modbus(registermap_dir / "M261_points_Modbus.xlsx", dropped=dropped_modbus_junk)
    tag_rows = parse_tag(registermap_dir / "M261_points_TAG.xlsx")

    # parse_modbus already filters the 2 known "Parameter Names:" template
    # artifacts (not real points, see Task 1) — add them back in here so
    # this check counts what §3.2's "1365 content rows" actually counted:
    # every row shaped like a point, artifacts included.
    modbus_raw_total = len(modbus_rows) + len(dropped_modbus_junk)
    tag_raw_total = len(tag_rows)
    if modbus_raw_total != _EXPECTED_TOTAL_3_2:
        failures.append(
            f"Modbus file raw row count (§3.2): expected {_EXPECTED_TOTAL_3_2}, got {modbus_raw_total} "
            f"({len(modbus_rows)} genuine + {len(dropped_modbus_junk)} known template artifact(s))"
        )
    if tag_raw_total != _EXPECTED_TOTAL_3_2:
        failures.append(f"TAG file raw row count (§3.2): expected {_EXPECTED_TOTAL_3_2}, got {tag_raw_total}")

    modbus_ems = sum(1 for r in modbus_rows if r.device == "EMS") + sum(1 for d, _, _ in dropped_modbus_junk if d == "EMS")
    tag_ems = sum(1 for r in tag_rows if r.device == "EMS")
    if modbus_ems != _EXPECTED_EMS_3_2:
        failures.append(f"Modbus EMS raw row count (§3.2): expected {_EXPECTED_EMS_3_2}, got {modbus_ems}")
    if tag_ems != _EXPECTED_EMS_3_2:
        failures.append(f"TAG EMS raw row count (§3.2): expected {_EXPECTED_EMS_3_2}, got {tag_ems}")

    # Not a failure, just making the arithmetic legible: catalog total minus
    # readback duplicates (1513-148=1365) equals the RAW §3.2 figures above,
    # but the catalog's own Modbus-sourced point count is 1363, not 1365 —
    # 2 fewer, exactly the 2 template artifacts, which correctly never
    # became catalog points (see "Points not found in all three files").
    detail_note = (
        f"catalog total {total} - {ems_readback} EMS readback duplicates = {total - ems_readback} "
        f"(matches the raw §3.2 figures above); Modbus file raw rows = {modbus_raw_total} "
        f"({len(dropped_modbus_junk)} of which are template artifacts, not real points, so only "
        f"{len(modbus_rows)} genuine Modbus points actually feed the catalog); TAG file raw rows = {tag_raw_total}."
    )

    return CheckResult(
        "Control figures §3.1/§3.2", CRITICAL, not failures,
        f"{total} catalog records; {modbus_raw_total} raw Modbus rows; {tag_raw_total} raw TAG rows" if not failures else f"{len(failures)} mismatch(es)",
        failures, detail_note,
        manufacturer_question=False,
    )


_ANCHORS = {
    1: "SysStoped", 16410: "Online", 25089: "AirCondStartStopCtrl",
    25093: "ControlMode", 25098: "SOCmax", 25236: "SDistCool",
}


def check_anchors(records: list[dict]) -> CheckResult:
    by_addr = {r["iec104_addr"]: r for r in records if r["device"] == "EMS"}
    failures = []
    for addr, expected_tag in _ANCHORS.items():
        rec = by_addr.get(addr)
        if rec is None:
            failures.append(f"addr {addr}: point missing entirely")
        elif rec["tag"] != expected_tag:
            failures.append(f"addr {addr}: expected tag {expected_tag!r}, got {rec['tag']!r}")
    return CheckResult("§4.9 anchors (6)", CRITICAL, not failures, "all 6 present" if not failures else f"{len(failures)} broken", failures)


def check_setpoint_formula(records: list[dict]) -> CheckResult:
    setpoints = [r for r in records if r["class"] == "setpoint"]
    failures = []
    if len(setpoints) != 148:
        failures.append(f"expected 148 setpoints, got {len(setpoints)}")
    for r in setpoints:
        expected_modbus = 40001 + (r["iec104_addr"] - 25089) * 2
        expected_readback = 16411 + (r["iec104_addr"] - 25089)
        if r["modbus_addr"] != expected_modbus:
            failures.append(f"{r['device']}/{r['slug']} iec={r['iec104_addr']}: modbus_addr={r['modbus_addr']}, formula requires {expected_modbus}")
        if r["readback_iec104_addr"] != expected_readback:
            failures.append(f"{r['device']}/{r['slug']} iec={r['iec104_addr']}: readback_iec104_addr={r['readback_iec104_addr']}, formula requires {expected_readback}")
    return CheckResult(
        "§4.3 formula for all 148 setpoints", CRITICAL, not failures,
        "148/148 hold" if not failures else f"{len(failures)} violation(s)", failures,
    )


def check_tag_count_matches_ro_count(records: list[dict]) -> CheckResult:
    by_device: dict[str, dict[str, int]] = defaultdict(lambda: {"tags": 0, "ro": 0})
    for r in records:
        d = by_device[r["device"]]
        if r["tag"] is not None:
            d["tags"] += 1
        if r["access"] == "RO":
            d["ro"] += 1
    failures = [
        f"{dev}: {v['tags']} non-empty tags != {v['ro']} RO points"
        for dev, v in sorted(by_device.items())
        if v["tags"] != v["ro"]
    ]
    return CheckResult("Tag count == RO count, per device", CRITICAL, not failures, "matches for all devices" if not failures else f"{len(failures)} device(s) off", failures)


# AGENT-TASK §4.4 (20 individually-addressed setpoints; Strategy Period is
# checked separately, §4.6/check 13) + §4.7 (9 safety-loop telemetry points).
_EXPECTED_KEY_POINTS = [
    # (name_raw, iec104_addr, modbus_addr)
    ("Power On/Off", 25164, 40151),
    ("Set Operating Mode", 25093, 40009),
    ("Set Strategy Mode", 25121, 40065),
    ("Set Active Power (kW)", 25165, 40153),
    ("Set Reactive Power (kvar)", 25166, 40155),
    ("Set Grid-connected/Off-grid Mode", 25167, 40157),
    ("Maximum Charge SOC (%)", 25098, 40019),
    ("Minimum Discharge SOC (%)", 25099, 40021),
    ("System Maximum Charge Power", 25161, 40145),
    ("System Maximum Discharge Power", 25160, 40143),
    ("Start Charge Power (kW)", 25173, 40169),
    ("Start Discharge Power (kW)", 25174, 40171),
    ("Demand Control", 25092, 40007),
    ("Enable Load Tracking", 25090, 40003),
    ("Enable Reverse Power Protection", 25091, 40005),
    ("Anti-reverse Power Margin (kW)", 25100, 40023),
    ("Adjustment Interval (seconds)", 25101, 40025),
    ("Clear Protection", 25168, 40159),
    ("Trip", 25171, 40165),
    ("Energy Storage Meter Power Direction", 25128, 40079),
    # §4.7
    ("EMS Periodic Heartbeat Indicator", 16400, 30031),
    ("Online Status", 16410, 30051),
    ("System Operating Status Word", 16403, 30037),
    ("Maximum Chargeable Power (kW)", 16397, 30025),
    ("Maximum Dischargeable Power (kW)", 16398, 30027),
    ("Charge Prohibition Protection", 16394, 30019),
    ("Discharge Prohibition Protection", 16395, 30021),
    ("Desired Active Power (kW)", 16389, 30009),
    ("Last Charge/Discharge Power (kW)", 16396, 30023),
]


def check_key_points_present(records: list[dict]) -> CheckResult:
    by_addr = {r["iec104_addr"]: r for r in records if r["device"] == "EMS"}
    failures = []
    for name, iec_addr, modbus_addr in _EXPECTED_KEY_POINTS:
        rec = by_addr.get(iec_addr)
        if rec is None:
            failures.append(f"iec={iec_addr} ({name!r}): missing entirely")
            continue
        if rec["name_raw"] != name:
            failures.append(f"iec={iec_addr}: expected name {name!r}, got {rec['name_raw']!r}")
        if rec["modbus_addr"] != modbus_addr:
            failures.append(f"iec={iec_addr} ({name!r}): expected modbus_addr {modbus_addr}, got {rec['modbus_addr']}")
    return CheckResult(
        "Key points §4.4/§4.7 present with documented addresses", CRITICAL, not failures,
        f"{len(_EXPECTED_KEY_POINTS)}/{len(_EXPECTED_KEY_POINTS)} present" if not failures else f"{len(failures)} problem(s)",
        failures,
    )


def check_no_address_collisions(records: list[dict]) -> CheckResult:
    failures = []
    by_device_iec: dict[str, list[int]] = defaultdict(list)
    by_device_modbus: dict[str, list[int]] = defaultdict(list)
    for r in records:
        by_device_iec[r["device"]].append(r["iec104_addr"])
        if r["modbus_addr"] is not None:
            by_device_modbus[r["device"]].append(r["modbus_addr"])
    for dev, addrs in sorted(by_device_iec.items()):
        dupes = sorted(a for a, c in Counter(addrs).items() if c > 1)
        if dupes:
            failures.append(f"{dev}: duplicate iec104_addr {dupes}")
    for dev, addrs in sorted(by_device_modbus.items()):
        dupes = sorted(a for a, c in Counter(addrs).items() if c > 1)
        if dupes:
            failures.append(f"{dev}: duplicate modbus_addr {dupes}")
    return CheckResult("No address collisions within a device", CRITICAL, not failures, "none found" if not failures else f"{len(failures)} collision(s)", failures)


_STRATEGY_RE = re.compile(r"^Strategy Period (\d+) (.+)$")
_STRATEGY_FIELDS = {"Start Hour", "Start Minute", "End Hour", "End Minute", "Execution Power (-Charge +Discharge)"}


def check_strategy_periods(records: list[dict]) -> CheckResult:
    groups: dict[int, set[str]] = defaultdict(set)
    for r in records:
        if r["device"] != "EMS" or r["class"] != "setpoint":
            continue
        m = _STRATEGY_RE.match(r["name_raw"])
        if m:
            groups[int(m.group(1))].add(m.group(2))

    failures = []
    if set(groups) != set(range(1, 11)):
        failures.append(f"expected periods 1..10, found {sorted(groups)}")
    for n in range(1, 11):
        fields = groups.get(n, set())
        missing = _STRATEGY_FIELDS - fields
        extra = fields - _STRATEGY_FIELDS
        if missing:
            failures.append(f"period {n}: missing field(s) {sorted(missing)}")
        if extra:
            failures.append(f"period {n}: unexpected field(s) {sorted(extra)}")
    return CheckResult(
        "Strategy Period rows form 10 complete groups of 5", CRITICAL, not failures,
        "10/10 complete" if not failures else f"{len(failures)} problem(s)", failures,
    )


def check_chinese_names_have_slug(records: list[dict]) -> CheckResult:
    failures = [
        f"{r['device']} iec={r['iec104_addr']} {r['name_raw']!r}: empty slug"
        for r in records
        if _has_cjk(r["name_raw"]) and not r["slug"]
    ]
    n_chinese = sum(1 for r in records if _has_cjk(r["name_raw"]))
    return CheckResult(
        "Chinese-named points have a non-empty slug", WARNING, not failures,
        f"{n_chinese} Chinese-named point(s), all with a slug" if not failures else f"{len(failures)} missing slug(s)",
        failures,
    )


def check_sources_completeness(records: list[dict]) -> CheckResult:
    unexplained = []
    for r in records:
        if len(r["sources"]) == 3:
            continue
        is_expected_readback_dup = (
            r["device"] == "EMS" and r["class"] == "telemetry" and r["iec104_addr"] in EMS_READBACK_RANGE
        )
        if is_expected_readback_dup:
            continue
        unexplained.append(f"{r['device']} iec={r['iec104_addr']} {r['name_raw']!r}: sources={r['sources']}")
    note = (
        f"(148 EMS telemetry points in the 16411-16558 range are excluded from this "
        f"list by design — §3.2: they mirror a setpoint's write register and have no "
        f"separate Modbus/TAG entry, see Task 1.) The {len(unexplained)} listed above "
        f"are genuine gaps: please confirm whether each exists under a different name "
        f"in Modbus/TAG, or doesn't exist there at all (2 look like a kW/kV unit typo, "
        f"most look like real omissions)."
    )
    return CheckResult(
        "Points not found in all three files", WARNING, True,
        f"{len(unexplained)} point(s) found in only 1-2 files (excl. expected EMS readback duplicates)",
        unexplained, note,
    )


def check_cell_coverage(records: list[dict]) -> CheckResult:
    voltages = sum(1 for r in records if r["device"] == "BMS_CELLS" and r["name_raw"].startswith("Cell Voltage"))
    note = None
    if voltages != 260:
        note = (
            f"The cabinet is wired as 1P260S (260 cells, §4.8) but device BMS_CELLS "
            f"only exposes {voltages} cell-voltage points. Whether addresses beyond "
            f"the mapped range cover the remaining {260 - voltages} cells is an open "
            f"question for the manufacturer (§3.3)."
        )
    return CheckResult("260-cell coverage (240 voltage points)", WARNING, True, f"{voltages}/260 cell-voltage points mapped", [], note)


# --------------------------------------------------------------------------
# Checks that need the raw xlsx (per-file data the catalog doesn't retain)
# --------------------------------------------------------------------------


def check_formula_only_matches(registermap_dir: Path) -> CheckResult:
    _, warnings = build_points(registermap_dir)
    entries = [
        f"{dev} iec={addr}: iec name {iname!r} vs modbus name {mname!r}"
        for dev, addr, iname, mname in warnings.setpoints_joined_by_formula
    ]
    note = (
        f"AGENT-TASK §2.3.8 expects ~20 setpoints to need the formula fallback. "
        f"On the actual files this is {len(entries)}: every one of the 148 setpoint "
        f"names matches its Modbus counterpart exactly after normalization (see "
        f"Task 1 findings — the 22 wording differences that DO exist are between "
        f"a setpoint's own WO name and its own IEC-104 readback name, never against "
        f"Modbus, and are paired by address formula rather than by name)."
    )
    return CheckResult(
        "Setpoints matched by formula, not by name (§2.3.8 expects ~20)", WARNING, True,
        f"{len(entries)} (see note)", entries, note,
        manufacturer_question=False,  # calibration remark against AGENT-TASK, not a vendor question
    )


# Modbus encodes every point as a fixed 2-register I32 or F32 (§2.2: "step
# between points is 2 registers"), regardless of IEC-104's native, narrower
# type — so IEC I16/U8 showing up as Modbus I32, and IEC F32 showing up as
# Modbus I32 (scaled by Precision instead of stored as a native float), are
# both SYSTEMATIC and expected, not per-point anomalies. U8->U8 and F32->F32
# (type preserved) are the non-widened case. Anything outside these four
# pairs is a genuine, itemized anomaly.
_EXPECTED_TYPE_PAIRS = {("I16", "I32"), ("U8", "I32"), ("F32", "I32"), ("U8", "U8"), ("F32", "F32")}


def check_data_types_consistent(registermap_dir: Path, records: list[dict]) -> CheckResult:
    iec_rows = parse_iec104(registermap_dir / "M261_points_IEC104.xlsx")
    modbus_rows = parse_modbus(registermap_dir / "M261_points_Modbus.xlsx")
    iec_by = {(r.device, r.address): r for r in iec_rows}
    modbus_by = {(r.device, r.address): r for r in modbus_rows}

    expected_pattern_counts: Counter = Counter()
    anomalies = []
    for rec in records:
        if rec["modbus_addr"] is None:
            continue
        iec_row = iec_by.get((rec["device"], rec["iec104_addr"]))
        modbus_row = modbus_by.get((rec["device"], rec["modbus_addr"]))
        if iec_row is None or modbus_row is None:
            continue
        pair = (iec_row.data_type, modbus_row.data_type)
        if pair[0] == pair[1] or pair in _EXPECTED_TYPE_PAIRS:
            if pair[0] != pair[1]:
                expected_pattern_counts[pair] += 1
            continue
        anomalies.append(
            f"{rec['device']}/{rec['slug']} iec={rec['iec104_addr']} ({rec['name_raw']!r}): "
            f"IEC-104 {iec_row.data_type!r} vs Modbus {modbus_row.data_type!r}"
        )

    background = "Expected, systematic width promotion (not itemized): " + "; ".join(
        f"{a}->{b}: {n} points" for (a, b), n in sorted(expected_pattern_counts.items())
    )
    if anomalies:
        note = (
            f"{background}. Outside that pattern: " + "; ".join(anomalies) +
            " — please confirm the correct type/range for this point; an I16 range "
            "(±32767) is an implausibly small ceiling for cumulative energy in kWh."
        )
    else:
        note = background
    return CheckResult(
        "Data types consistent between IEC-104 and Modbus", WARNING, True,
        f"{len(anomalies)} genuine anomaly/anomalies (of {sum(expected_pattern_counts.values())} widened as expected)",
        anomalies, note,
        manufacturer_question=bool(anomalies),  # only the genuine anomaly is worth asking about
    )


def check_precision_discrepancy(registermap_dir: Path, records: list[dict]) -> CheckResult:
    iec_rows = parse_iec104(registermap_dir / "M261_points_IEC104.xlsx")
    modbus_rows = parse_modbus(registermap_dir / "M261_points_Modbus.xlsx")
    iec_by = {(r.device, r.address): r for r in iec_rows}
    modbus_by = {(r.device, r.address): r for r in modbus_rows}

    mismatches = []
    pattern_counts: Counter = Counter()
    for rec in records:
        if rec["modbus_addr"] is None:
            continue
        iec_row = iec_by.get((rec["device"], rec["iec104_addr"]))
        modbus_row = modbus_by.get((rec["device"], rec["modbus_addr"]))
        if iec_row is None or modbus_row is None:
            continue
        if iec_row.precision != modbus_row.precision:
            pattern_counts[(iec_row.precision, modbus_row.precision)] += 1
            mismatches.append(
                f"{rec['device']}/{rec['slug']} iec={rec['iec104_addr']}: "
                f"IEC-104 precision {iec_row.precision!r} vs Modbus {modbus_row.precision!r}"
            )
    # This one is deliberately NOT compressed to a summary (unlike the data-type
    # check above): §7 leaves the actual scale factor as `scaling.source`,
    # unconfirmed by design, and this report is the input document for the §0
    # on-site confirmation — the full, per-point list is the point.
    note = (
        "Pattern breakdown: " + "; ".join(f"IEC-104 {a!r} vs Modbus {b!r}: {n} points" for (a, b), n in sorted(pattern_counts.items(), key=lambda kv: -kv[1]))
        + ". These are exactly the points `scaling.source`/`scale` in the catalog "
        "and simulator config are unconfirmed for (§7) — confirm the real scale "
        "against the EMS display on-site (Stage 0) before trusting either file."
    )
    return CheckResult(
        "Precision discrepancy (IEC-104 vs Modbus)", WARNING, True,
        f"{len(mismatches)} point(s) differ", mismatches, note,
    )


def check_dry_contact_address(registermap_dir: Path) -> CheckResult:
    from catalog.parsing import DEVICE_ADDR

    canonical = DEVICE_ADDR["DIDO"]
    raw_headers: set[int] = set()
    for fname in ("M261_points_IEC104.xlsx", "M261_points_Modbus.xlsx"):
        rows = read_sheet_rows(registermap_dir / fname, "other")
        for st in split_subtables("other", rows):
            if st.device_code == "DIDO" and st.header_addr is not None:
                raw_headers.add(st.header_addr)

    note = None
    if raw_headers - {canonical}:
        note = (
            f"The 'other' sheet's own subtable header for Dry Contact reads "
            f"commonAddr:{sorted(raw_headers - {canonical})} in both files, while "
            f"Address Code Allocation / §4.1 (used for device_addr in the catalog) "
            f"says {canonical}. Confirm the correct address with the manufacturer "
            f"before commissioning (§2.3.4)."
        )
    return CheckResult(
        "Dry Contact address 168 vs 172", WARNING, True,
        f"canonical={canonical}, in-sheet header(s)={sorted(raw_headers) or 'none found'}",
        [], note,
    )


# --------------------------------------------------------------------------
# Orchestration
# --------------------------------------------------------------------------


def run_checks(catalog_path: Path, registermap_dir: Path) -> list[CheckResult]:
    records = load_catalog(catalog_path)
    return [
        check_control_figures(records, registermap_dir),
        check_anchors(records),
        check_setpoint_formula(records),
        check_tag_count_matches_ro_count(records),
        check_formula_only_matches(registermap_dir),
        check_key_points_present(records),
        check_no_address_collisions(records),
        check_data_types_consistent(registermap_dir, records),
        check_precision_discrepancy(registermap_dir, records),
        check_sources_completeness(records),
        check_dry_contact_address(registermap_dir),
        check_cell_coverage(records),
        check_strategy_periods(records),
        check_chinese_names_have_slug(records),
    ]


def render_report(results: list[CheckResult], catalog_path: Path) -> str:
    lines = ["# M261 catalog validation report", ""]
    lines.append(f"Input: `{catalog_path.name}`. Generated by `catalog/validate_catalog.py`.")
    lines.append("")

    critical = [r for r in results if r.level == CRITICAL]
    warnings = [r for r in results if r.level == WARNING]
    failed_critical = [r for r in critical if not r.passed]

    lines.append("## Summary")
    lines.append("")
    lines.append(f"- Critical checks: {len(critical) - len(failed_critical)}/{len(critical)} passed")
    lines.append(f"- Warnings raised: {sum(1 for r in warnings if r.details or r.note)}/{len(warnings)}")
    lines.append(f"- **Overall: {'FAIL' if failed_critical else 'PASS'}**")
    lines.append("")

    lines.append("## Critical checks")
    lines.append("")
    for r in critical:
        status = "✅ PASS" if r.passed else "❌ FAIL"
        lines.append(f"### {status} — {r.name}")
        lines.append("")
        lines.append(r.summary)
        if r.note:
            lines.append("")
            lines.append(r.note)
        if r.details:
            lines.append("")
            for d in r.details:
                lines.append(f"- {d}")
        lines.append("")

    lines.append("## Warnings")
    lines.append("")
    for r in warnings:
        marker = "⚠️" if (r.details or r.note) else "—"
        lines.append(f"### {marker} {r.name}")
        lines.append("")
        lines.append(r.summary)
        if r.note:
            lines.append("")
            lines.append(r.note)
        if r.details:
            lines.append("")
            for d in r.details:
                lines.append(f"- {d}")
        lines.append("")

    lines.append("## Questions for the manufacturer")
    lines.append("")
    lines.append(
        "Auto-collected from warnings that raised something AND are actually "
        "addressed to the manufacturer (not internal remarks about how this "
        "catalog compares to AGENT-TASK's own expectations — see those under "
        "Warnings above)."
    )
    lines.append("")
    questions = [r for r in warnings if r.manufacturer_question and (r.details or r.note)]
    if not questions:
        lines.append("None — no warning raised anything actionable.")
    else:
        for r in questions:
            lines.append(f"- **{r.name}**: {r.note or r.summary}")
    lines.append("")

    return "\n".join(lines)


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--catalog", type=Path, default=DEFAULT_CATALOG)
    ap.add_argument("--registermap", type=Path, default=DEFAULT_REGISTERMAP)
    ap.add_argument("--out", type=Path, default=DEFAULT_OUT)
    args = ap.parse_args(argv)

    results = run_checks(args.catalog, args.registermap)
    report = render_report(results, args.catalog)

    args.out.parent.mkdir(parents=True, exist_ok=True)
    args.out.write_text(report, encoding="utf-8")

    failed_critical = [r for r in results if r.level == CRITICAL and not r.passed]
    for r in failed_critical:
        print(f"CRITICAL FAIL: {r.name}", file=sys.stderr)
    print(f"wrote {args.out}", file=sys.stderr)
    return 1 if failed_critical else 0


if __name__ == "__main__":
    raise SystemExit(main())
