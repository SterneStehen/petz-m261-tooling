"""End-to-end test for AGENT-TASK §6 item 1's range metadata chain:

    overrides.yaml
      -> catalog/point_catalog.json
      -> Go PointMeta (gen/go/m261points)
      -> Python PointMeta (gen/python/m261_points)
      -> gen/seeds/setpoints.json

Uses an isolated, temporary overrides.yaml — never catalog/overrides.yaml
itself, which stays empty (the real map confirms no range for any point;
see test_build_catalog.py's test_all_148_setpoints_keep_range_null for the
pin on that fact).
"""
from __future__ import annotations

import json
import re
from pathlib import Path

from catalog.build_catalog import build_catalog
from codegen.gen_go import main as gen_go_main
from codegen.gen_python import main as gen_python_main
from codegen.gen_seeds import main as gen_seeds_main

REGISTERMAP = Path(__file__).resolve().parent.parent / "m261-registermap"


def _build_with_override(tmp_path: Path, range_yaml: str) -> Path:
    """Builds a full catalog with one temporary override applied to
    EMS/set_active_power_kw, writes it to tmp_path/point_catalog.json, returns
    the path."""
    overrides = tmp_path / "overrides.yaml"
    overrides.write_text(
        f"points:\n  EMS:\n    set_active_power_kw:\n      range: {range_yaml}\n",
        encoding="utf-8",
    )
    records = build_catalog(REGISTERMAP, overrides)
    catalog_path = tmp_path / "point_catalog.json"
    catalog_path.write_text(json.dumps(records, ensure_ascii=False, indent=2), encoding="utf-8")
    return catalog_path


def _propagate(tmp_path: Path, catalog_path: Path) -> tuple[str, str, dict]:
    """Runs all three codegen scripts against catalog_path, returns
    (go_points_source, python_points_source, seeds_setpoints_json)."""
    go_out = tmp_path / "go"
    py_out = tmp_path / "py"
    seeds_out = tmp_path / "seeds"
    gen_go_main(["--catalog", str(catalog_path), "--out", str(go_out)])
    gen_python_main(["--catalog", str(catalog_path), "--out", str(py_out)])
    gen_seeds_main(["--catalog", str(catalog_path), "--out", str(seeds_out)])

    go_src = (go_out / "points.go").read_text(encoding="utf-8")
    py_src = (py_out / "points.py").read_text(encoding="utf-8")
    seeds = json.loads((seeds_out / "setpoints.json").read_text(encoding="utf-8"))
    return go_src, py_src, seeds


def _setpoint_range_go(go_src: str) -> str:
    """Extracts the Range: ... fragment for the EMS/set_active_power_kw line."""
    m = re.search(r'Slug: "set_active_power_kw",.*?Range: (&Range\{[^}]*\}|nil),', go_src)
    assert m, "EMS/set_active_power_kw not found in generated Go source"
    return m.group(1)


def _setpoint_range_py(py_src: str) -> str:
    m = re.search(r"slug='set_active_power_kw',.*?range=(Range\([^)]*\)|None),", py_src)
    assert m, "EMS/set_active_power_kw not found in generated Python source"
    return m.group(1)


def _setpoint_range_seed(seeds: dict) -> dict | None:
    rec = next(p for p in seeds["points"] if p["slug"] == "set_active_power_kw")
    return rec["range"]


def test_min_only_range_propagates_through_full_chain(tmp_path):
    catalog_path = _build_with_override(tmp_path, "{min: -130.5, max: null}")
    go_src, py_src, seeds = _propagate(tmp_path, catalog_path)

    assert _setpoint_range_go(go_src) == "&Range{Min: float64Ptr(-130.5), Max: nil}"
    assert _setpoint_range_py(py_src) == "Range(min=-130.5, max=None)"
    assert _setpoint_range_seed(seeds) == {"min": -130.5, "max": None}


def test_max_only_range_propagates_through_full_chain(tmp_path):
    catalog_path = _build_with_override(tmp_path, "{min: null, max: 130.5}")
    go_src, py_src, seeds = _propagate(tmp_path, catalog_path)

    assert _setpoint_range_go(go_src) == "&Range{Min: nil, Max: float64Ptr(130.5)}"
    assert _setpoint_range_py(py_src) == "Range(min=None, max=130.5)"
    assert _setpoint_range_seed(seeds) == {"min": None, "max": 130.5}


def test_min_and_max_range_propagates_through_full_chain(tmp_path):
    catalog_path = _build_with_override(tmp_path, "{min: -130.5, max: 130.5}")
    go_src, py_src, seeds = _propagate(tmp_path, catalog_path)

    assert _setpoint_range_go(go_src) == "&Range{Min: float64Ptr(-130.5), Max: float64Ptr(130.5)}"
    assert _setpoint_range_py(py_src) == "Range(min=-130.5, max=130.5)"
    assert _setpoint_range_seed(seeds) == {"min": -130.5, "max": 130.5}


def test_generation_stays_idempotent_with_a_real_range_present(tmp_path):
    """Task 3's own acceptance criterion ('make generate is idempotent')
    must still hold once a point actually carries a non-null range, not
    just in the all-null common case."""
    catalog_path = _build_with_override(tmp_path, "{min: -130.5, max: 130.5}")
    out1 = tmp_path / "run1"
    out2 = tmp_path / "run2"
    gen_go_main(["--catalog", str(catalog_path), "--out", str(out1)])
    gen_go_main(["--catalog", str(catalog_path), "--out", str(out2)])
    assert (out1 / "points.go").read_bytes() == (out2 / "points.go").read_bytes()
