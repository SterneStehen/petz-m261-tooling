"""End-to-end tests against the real m261-registermap/*.xlsx files — the
authoritative control figures from AGENT-TASK §3.1 and the acceptance
criteria for task 1 (§6).
"""
from __future__ import annotations

import json
from collections import Counter
from pathlib import Path

import pytest

from catalog.build_catalog import ANCHORS, DANGEROUS_NAMES, build_catalog, build_points, check_anchors
from tests.conftest import requires_registermap

REGISTERMAP = Path(__file__).resolve().parent.parent / "m261-registermap"
OVERRIDES = Path(__file__).resolve().parent.parent / "catalog" / "overrides.yaml"

# Every test in this module needs the real files (see the module
# docstring) -- see tests/conftest.py for why this must be a skip, not an
# unconditional failure, when they aren't present.
pytestmark = requires_registermap

# AGENT-TASK §3.1
EXPECTED_COUNTS = {
    ("EMS", "alarm"): 31,
    ("EMS", "telemetry"): 174,
    ("EMS", "setpoint"): 148,
    ("PCS", "alarm"): 88,
    ("PCS", "telemetry"): 91,
    ("BMS", "alarm"): 116,
    ("BMS", "telemetry"): 266,
    ("BMS_CELLS", "telemetry"): 391,
    ("TMS", "alarm"): 49,
    ("TMS", "telemetry"): 102,
    ("PCS_METER", "telemetry"): 31,
    ("DIDO", "telemetry"): 8,
    ("CSJ", "telemetry"): 18,
}
EXPECTED_TOTAL = 1513


@pytest.fixture(scope="module")
def records() -> list[dict]:
    return build_catalog(REGISTERMAP, OVERRIDES)


def test_total_record_count(records):
    assert len(records) == EXPECTED_TOTAL


def test_counts_per_device_and_class_match_3_1(records):
    actual = Counter((r["device"], r["class"]) for r in records)
    assert dict(actual) == EXPECTED_COUNTS


def test_all_148_setpoints_have_both_addresses_and_data_type(records):
    setpoints = [r for r in records if r["class"] == "setpoint"]
    assert len(setpoints) == 148
    for r in setpoints:
        assert r["iec104_addr"] is not None
        assert r["modbus_addr"] is not None, r["name_raw"]
        assert r["data_type"]


def test_no_required_field_is_empty(records):
    for r in records:
        assert r["device"], r
        assert r["slug"], r
        assert r["class"], r
        assert r["access"], r


def test_slug_unique_within_device(records):
    seen: set[tuple[str, str]] = set()
    for r in records:
        key = (r["device"], r["slug"])
        assert key not in seen, f"duplicate slug {key}"
        seen.add(key)


def test_six_anchors_hold(records):
    by_addr = {(r["device"], r["iec104_addr"]): r for r in records}
    for addr, expected_tag in ANCHORS.items():
        assert by_addr[("EMS", addr)]["tag"] == expected_tag


def test_ten_key_write_points_against_4_4_no_tag_bled_from_neighbor():
    """Acceptance criterion: no point receives a neighboring point's tag.
    Eleven setpoints,
    spread across the address range (not clustered, so an off-by-one in the
    join would show up), checked against name + modbus_addr from §4.4."""
    records_ = build_catalog(REGISTERMAP, OVERRIDES)
    by_addr = {r["iec104_addr"]: r for r in records_ if r["device"] == "EMS"}
    expectations = {
        25090: ("Enable Load Tracking", 40003),
        25092: ("Demand Control", 40007),
        25093: ("Set Operating Mode", 40009),
        25098: ("Maximum Charge SOC (%)", 40019),
        25121: ("Set Strategy Mode", 40065),
        25128: ("Energy Storage Meter Power Direction", 40079),
        25160: ("System Maximum Discharge Power", 40143),
        25164: ("Power On/Off", 40151),
        25165: ("Set Active Power (kW)", 40153),
        25168: ("Clear Protection", 40159),
        25171: ("Trip", 40165),
    }
    for iec_addr, (name, modbus_addr) in expectations.items():
        rec = by_addr[iec_addr]
        assert rec["name_raw"] == name, iec_addr
        assert rec["modbus_addr"] == modbus_addr, iec_addr


def test_trip_and_clear_protection_are_dangerous():
    records_ = build_catalog(REGISTERMAP, OVERRIDES)
    by_addr = {r["iec104_addr"]: r for r in records_ if r["device"] == "EMS"}
    assert by_addr[25171]["dangerous"] is True  # Trip
    assert by_addr[25168]["dangerous"] is True  # Clear Protection
    assert by_addr[25093]["dangerous"] is False  # Set Operating Mode, control


def test_all_148_setpoints_keep_range_null(records):
    """AGENT-TASK §6 item 1: the real register map confirms no numeric
    range for any point — catalog/overrides.yaml (the only legitimate
    source for a non-null range) stays empty, so every one of the 148
    setpoints must still be range:null. See tests/test_range_propagation.py
    for proof the mechanism itself works, using isolated temporary
    overrides."""
    setpoints = [r for r in records if r["class"] == "setpoint"]
    assert len(setpoints) == 148
    non_null = [r["slug"] for r in setpoints if r["range"] is not None]
    assert non_null == [], f"invented range(s) found for: {non_null}"


def test_build_is_idempotent(tmp_path):
    out1 = tmp_path / "run1.json"
    out2 = tmp_path / "run2.json"
    for out in (out1, out2):
        recs = build_catalog(REGISTERMAP, OVERRIDES)
        out.write_text(json.dumps(recs, ensure_ascii=False, indent=2), encoding="utf-8")
    assert out1.read_bytes() == out2.read_bytes()


def test_anchor_check_fails_loudly_on_misaligned_tags():
    points, _ = build_points(REGISTERMAP)
    for p in points:
        if p.device == "EMS" and p.iec104_addr == 1:
            p.tag = "DeliberatelyWrong"
    with pytest.raises(Exception):
        check_anchors(points)


def test_known_residual_join_gaps_are_exactly_the_documented_ones(records):
    """These points have no Modbus counterpart by name in the real files —
    confirmed by hand (see the internal task specification and validation
    report for task 2). This test pins the *known* set so any new, unexplained gap
    fails CI instead of silently expanding."""
    known_iec_only = {
        ("PCS", 16451),  # Insulation Detection Value Rx — (kW) in IEC vs (kv) in Modbus
        ("PCS", 16452),  # Insulation Detection Value Ry — same kW/kv mismatch
        ("PCS", 16475),  # Fault Reset Command — no Modbus counterpart at all
        ("BMS", 16467),
        ("BMS", 16468),
        ("BMS", 16469),
        ("BMS", 16471),
        ("BMS", 16559),
        ("BMS", 16560),
        ("BMS", 16627),
        ("BMS", 16628),
        ("BMS", 16650),  # Fan Stop Temperature — no Modbus counterpart at all
    }
    actual_gaps = {
        (r["device"], r["iec104_addr"])
        for r in records
        if r["class"] in ("alarm", "telemetry") and r["modbus_addr"] is None
        # readback-duplicate telemetry rows (16411-16558, EMS) never have a
        # Modbus address by design (§3.2) — excluded, not a gap.
        and not (r["device"] == "EMS" and 16411 <= r["iec104_addr"] <= 16558)
    }
    assert actual_gaps == known_iec_only
