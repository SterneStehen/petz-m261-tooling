"""Tests for catalog.validate_catalog: real-data end-to-end (all critical
checks pass on the real catalog) plus one synthetic negative test per
critical check, so a regression in any single rule fails CI on its own
rather than only showing up as a diff in validation_report.md.
"""
from __future__ import annotations

from pathlib import Path

import pytest

from catalog.build_catalog import build_catalog
from catalog.validate_catalog import (
    CheckResult,
    check_anchors,
    check_control_figures,
    check_key_points_present,
    check_no_address_collisions,
    check_setpoint_formula,
    check_strategy_periods,
    check_tag_count_matches_ro_count,
    main,
    render_report,
    run_checks,
)

REGISTERMAP = Path(__file__).resolve().parent.parent / "m261-registermap"
OVERRIDES = Path(__file__).resolve().parent.parent / "catalog" / "overrides.yaml"


def _rec(**overrides) -> dict:
    r = {
        "device": "EMS", "device_addr": 1, "tag": None, "name_raw": "X", "slug": "x",
        "class": "alarm", "access": "RO", "iec104_addr": 1, "modbus_addr": None,
        "modbus_class": None, "modbus_function": None, "data_type": "U8", "scale": 1,
        "unit": None, "enum": None, "description": None, "dangerous": False,
        "readback_iec104_addr": None, "sources": ["IEC104"],
    }
    r.update(overrides)
    return r


# --------------------------------------------------------------------------
# Real-data end-to-end
# --------------------------------------------------------------------------


@pytest.fixture(scope="module")
def catalog_path(tmp_path_factory) -> Path:
    import json

    records = build_catalog(REGISTERMAP, OVERRIDES)
    path = tmp_path_factory.mktemp("validate") / "point_catalog.json"
    path.write_text(json.dumps(records, ensure_ascii=False, indent=2), encoding="utf-8")
    return path


def test_all_critical_checks_pass_on_real_catalog(catalog_path):
    results = run_checks(catalog_path, REGISTERMAP)
    failed = [r for r in results if r.level == "critical" and not r.passed]
    assert failed == [], [(r.name, r.details) for r in failed]


def test_main_exits_zero_on_real_catalog(catalog_path, tmp_path):
    out = tmp_path / "report.md"
    code = main(["--catalog", str(catalog_path), "--registermap", str(REGISTERMAP), "--out", str(out)])
    assert code == 0
    assert out.exists()


def test_main_exits_nonzero_on_catalog_with_broken_anchor(catalog_path, tmp_path):
    """Task 2 acceptance: 'non-zero exit code on critical discrepancies' —
    exercised through main(), not just the check function, so a wiring bug
    (e.g. exit code not actually reflecting failures) would be caught too."""
    import json

    records = json.loads(catalog_path.read_text(encoding="utf-8"))
    for r in records:
        if r["device"] == "EMS" and r["iec104_addr"] == 1:
            r["tag"] = "Corrupted"
    broken = tmp_path / "broken_catalog.json"
    broken.write_text(json.dumps(records, ensure_ascii=False), encoding="utf-8")

    out = tmp_path / "report.md"
    code = main(["--catalog", str(broken), "--registermap", str(REGISTERMAP), "--out", str(out)])
    assert code == 1
    assert "❌ FAIL" in out.read_text(encoding="utf-8")


def test_report_has_required_sections(catalog_path, tmp_path):
    out = tmp_path / "report.md"
    main(["--catalog", str(catalog_path), "--registermap", str(REGISTERMAP), "--out", str(out)])
    text = out.read_text(encoding="utf-8")
    assert "## Critical checks" in text
    assert "## Warnings" in text
    assert "## Questions for the manufacturer" in text


def test_known_warning_counts_on_real_catalog(catalog_path):
    """Pins the known, hand-verified warning counts so a real regression (not
    just a report diff) fails CI. See Task 1 findings for provenance."""
    results = run_checks(catalog_path, REGISTERMAP)
    by_name = {r.name: r for r in results}
    assert len(by_name["Points not found in all three files"].details) == 12
    assert "240/260" in by_name["260-cell coverage (240 voltage points)"].summary
    dry_contact = by_name["Dry Contact address 168 vs 172"]
    assert "172" in dry_contact.summary and "canonical=168" in dry_contact.summary


# --------------------------------------------------------------------------
# Report rendering
# --------------------------------------------------------------------------


def test_manufacturer_questions_exclude_internal_calibration_remarks(tmp_path):
    internal = CheckResult(
        "Internal remark", "warning", True, "0 (see note)", ["detail"], "an internal note",
        manufacturer_question=False,
    )
    vendor_facing = CheckResult(
        "Vendor question", "warning", True, "1 thing", ["detail"], "please confirm X",
    )
    report = render_report([internal, vendor_facing], Path("point_catalog.json"))
    section = report.split("## Questions for the manufacturer")[1]
    assert "Vendor question" in section
    assert "Internal remark" not in section


# --------------------------------------------------------------------------
# One synthetic negative test per critical check
# --------------------------------------------------------------------------


def test_control_figures_fails_on_wrong_count():
    records = [_rec(device="EMS", **{"class": "alarm"}, iec104_addr=i) for i in range(1, 31)]  # 30, not 31
    result = check_control_figures(records)
    assert not result.passed
    assert any("EMS/alarm" in d for d in result.details)


def test_anchors_fails_on_wrong_tag():
    records = [_rec(device="EMS", tag="WrongTag", iec104_addr=1)]
    result = check_anchors(records)
    assert not result.passed
    assert any("addr 1" in d for d in result.details)


def test_setpoint_formula_fails_on_wrong_modbus_addr():
    records = [_rec(
        device="EMS", **{"class": "setpoint"}, access="WO", iec104_addr=25089,
        modbus_addr=49999, readback_iec104_addr=16411,
    )]
    result = check_setpoint_formula(records)
    assert not result.passed
    assert any("49999" in d for d in result.details)


def test_tag_count_mismatch_fails():
    records = [
        _rec(device="EMS", access="RO", tag=None, iec104_addr=1),  # RO but no tag
    ]
    result = check_tag_count_matches_ro_count(records)
    assert not result.passed
    assert "EMS" in result.details[0]


def test_key_points_present_fails_when_trip_missing():
    records = [_rec(device="EMS", **{"class": "setpoint"}, access="WO", name_raw="Not Trip", iec104_addr=25171, modbus_addr=40165)]
    result = check_key_points_present(records)
    assert not result.passed
    assert any("Trip" in d for d in result.details)


def test_no_address_collisions_fails_on_duplicate_iec_addr():
    records = [
        _rec(device="EMS", iec104_addr=1, slug="a"),
        _rec(device="EMS", iec104_addr=1, slug="b"),
    ]
    result = check_no_address_collisions(records)
    assert not result.passed
    assert "duplicate iec104_addr" in result.details[0]


def test_strategy_periods_fails_on_missing_field():
    records = []
    for n in range(1, 11):
        fields = ["Start Hour", "Start Minute", "End Hour", "End Minute", "Execution Power (-Charge +Discharge)"]
        if n == 3:
            fields = fields[:-1]  # drop Execution Power for period 3
        for f in fields:
            records.append(_rec(device="EMS", **{"class": "setpoint"}, name_raw=f"Strategy Period {n} {f}", iec104_addr=n * 100))
    result = check_strategy_periods(records)
    assert not result.passed
    assert any("period 3" in d for d in result.details)
