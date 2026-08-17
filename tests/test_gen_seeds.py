"""Tests for codegen.gen_seeds against the real catalog."""
from __future__ import annotations

import json
from pathlib import Path

import pytest

from codegen.gen_seeds import main as gen_seeds_main
from tests.conftest import requires_catalog

REPO_ROOT = Path(__file__).resolve().parent.parent
CATALOG = REPO_ROOT / "catalog" / "point_catalog.json"
SEEDS = REPO_ROOT / "gen" / "seeds"

pytestmark = requires_catalog


@pytest.fixture(scope="module", autouse=True)
def generated():
    gen_seeds_main([])


def _load(name: str) -> dict:
    return json.loads((SEEDS / name).read_text(encoding="utf-8"))


def test_alarms_seed_has_284_entries_with_severity_null():
    data = _load("alarms.json")
    assert data["count"] == 284
    assert len(data["points"]) == 284
    assert all(p["severity"] is None for p in data["points"])
    assert all(p["device"] and p["iec104_addr"] and p["name"] for p in data["points"])


def test_metrics_seed_has_1081_entries():
    data = _load("metrics.json")
    assert data["count"] == 1081
    assert len(data["points"]) == 1081
    assert all("slug" in p and "unit" in p and "data_type" in p for p in data["points"])


def test_setpoints_seed_has_148_entries_with_range_null():
    data = _load("setpoints.json")
    assert data["count"] == 148
    assert len(data["points"]) == 148
    assert all(p["range"] is None for p in data["points"])
    # ~40% of setpoints have an enum (390/1513 catalog-wide, mostly setpoints)
    assert sum(1 for p in data["points"] if p["enum"]) > 0


def test_seed_totals_match_catalog_class_counts():
    records = json.loads(CATALOG.read_text(encoding="utf-8"))
    by_class = {}
    for r in records:
        by_class[r["class"]] = by_class.get(r["class"], 0) + 1
    assert _load("alarms.json")["count"] == by_class["alarm"]
    assert _load("metrics.json")["count"] == by_class["telemetry"]
    assert _load("setpoints.json")["count"] == by_class["setpoint"]


def test_seeds_have_generated_banner():
    for name in ("alarms.json", "metrics.json", "setpoints.json"):
        data = _load(name)
        assert "DO NOT EDIT" in data["_generated"]
