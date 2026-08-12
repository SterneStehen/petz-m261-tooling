from pathlib import Path

import pytest

from catalog.build_catalog import apply_overrides
from catalog.parsing import CatalogParseError


def _sample_records():
    return [
        {"device": "EMS", "slug": "set_active_power", "scale": 1, "dangerous": False},
        {"device": "EMS", "slug": "trip", "scale": 1, "dangerous": True},
    ]


def test_overrides_can_replace_any_field(tmp_path):
    p = tmp_path / "overrides.yaml"
    p.write_text(
        "points:\n"
        "  EMS:\n"
        "    set_active_power:\n"
        "      scale: 0.1\n",
        encoding="utf-8",
    )
    records = apply_overrides(_sample_records(), p)
    rec = next(r for r in records if r["slug"] == "set_active_power")
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
