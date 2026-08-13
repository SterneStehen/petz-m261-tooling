from pathlib import Path

import pytest

from catalog.build_catalog import apply_overrides, validate_ranges
from catalog.parsing import CatalogParseError


def _sample_records():
    return [
        {"device": "EMS", "slug": "set_active_power_kw", "scale": 1, "dangerous": False, "range": None},
        {"device": "EMS", "slug": "trip", "scale": 1, "dangerous": True, "range": None},
    ]


def test_overrides_can_replace_any_field(tmp_path):
    p = tmp_path / "overrides.yaml"
    p.write_text(
        "points:\n"
        "  EMS:\n"
        "    set_active_power_kw:\n"
        "      scale: 0.1\n",
        encoding="utf-8",
    )
    records = apply_overrides(_sample_records(), p)
    rec = next(r for r in records if r["slug"] == "set_active_power_kw")
    assert rec["scale"] == 0.1


def test_overrides_unknown_point_key_is_an_error(tmp_path):
    p = tmp_path / "overrides.yaml"
    p.write_text(
        "points:\n"
        "  EMS:\n"
        "    this_slug_does_not_exist:\n"
        "      scale: 0.1\n",
        encoding="utf-8",
    )
    with pytest.raises(CatalogParseError):
        apply_overrides(_sample_records(), p)


def test_missing_overrides_file_is_a_noop(tmp_path):
    records = _sample_records()
    result = apply_overrides(records, tmp_path / "does_not_exist.yaml")
    assert result == records


# --------------------------------------------------------------------------
# range propagation through overrides.yaml — AGENT-TASK §6 item 1. Isolated
# temporary overrides files, never the real catalog/overrides.yaml (which
# stays empty — the real map has no confirmed range for any point).
# --------------------------------------------------------------------------


def test_override_sets_min_only_range(tmp_path):
    p = tmp_path / "overrides.yaml"
    p.write_text(
        "points:\n  EMS:\n    set_active_power_kw:\n      range: {min: -130.5, max: null}\n",
        encoding="utf-8",
    )
    records = apply_overrides(_sample_records(), p)
    rec = next(r for r in records if r["slug"] == "set_active_power_kw")
    assert rec["range"] == {"min": -130.5, "max": None}
    validate_ranges(records)  # must not raise


def test_override_sets_max_only_range(tmp_path):
    p = tmp_path / "overrides.yaml"
    p.write_text(
        "points:\n  EMS:\n    set_active_power_kw:\n      range: {min: null, max: 130.5}\n",
        encoding="utf-8",
    )
    records = apply_overrides(_sample_records(), p)
    rec = next(r for r in records if r["slug"] == "set_active_power_kw")
    assert rec["range"] == {"min": None, "max": 130.5}
    validate_ranges(records)  # must not raise


def test_override_sets_min_and_max_range(tmp_path):
    p = tmp_path / "overrides.yaml"
    p.write_text(
        "points:\n  EMS:\n    set_active_power_kw:\n      range: {min: -130.5, max: 130.5}\n",
        encoding="utf-8",
    )
    records = apply_overrides(_sample_records(), p)
    rec = next(r for r in records if r["slug"] == "set_active_power_kw")
    assert rec["range"] == {"min": -130.5, "max": 130.5}
    validate_ranges(records)  # must not raise


@pytest.mark.parametrize(
    "range_yaml",
    [
        "{min: null, max: null}",  # both bounds null
        "{min: 100, max: 0}",  # min > max
        "{min: 0, max: 100, step: 1}",  # unknown key
        "{min: 0}",  # missing max key entirely
        "not_an_object",  # wrong shape
    ],
)
def test_malformed_override_range_rejected_at_build_time(tmp_path, range_yaml):
    p = tmp_path / "overrides.yaml"
    p.write_text(f"points:\n  EMS:\n    set_active_power_kw:\n      range: {range_yaml}\n", encoding="utf-8")
    records = apply_overrides(_sample_records(), p)
    with pytest.raises(CatalogParseError):
        validate_ranges(records)
