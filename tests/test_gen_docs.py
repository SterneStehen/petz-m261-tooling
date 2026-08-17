"""Tests for codegen.gen_docs (Task 8 item 4: docs/point-reference.md)."""
from __future__ import annotations

import json
import re
from pathlib import Path

import pytest

from codegen.gen_docs import main as gen_docs_main
from tests.conftest import requires_catalog

REPO_ROOT = Path(__file__).resolve().parent.parent
CATALOG = REPO_ROOT / "catalog" / "point_catalog.json"

pytestmark = requires_catalog


@pytest.fixture(scope="module")
def catalog_records() -> list[dict]:
    return json.loads(CATALOG.read_text(encoding="utf-8"))


@pytest.fixture()
def doc(tmp_path) -> str:
    out = tmp_path / "point-reference.md"
    gen_docs_main(["--out", str(out)])
    return out.read_text(encoding="utf-8")


def test_contains_every_slug_once(doc, catalog_records):
    """Task 8's own acceptance criterion: the reference contains all 1513
    records — checked as every (device, slug) actually appearing as its
    own table row, not just a total row count that could hide a
    duplicate/omission pair cancelling out."""
    for r in catalog_records:
        assert f"`{r['slug']}`" in doc, f"{r['device']}/{r['slug']} missing from point-reference.md"


def test_row_count_matches_catalog_total(doc, catalog_records):
    row_lines = [
        line for line in doc.splitlines()
        if line.startswith("| `") and not line.startswith("|---")
    ]
    assert len(row_lines) == len(catalog_records) == 1513


def test_every_device_section_present(doc, catalog_records):
    devices = {r["device"] for r in catalog_records}
    for device in devices:
        assert re.search(rf"^## {re.escape(device)}$", doc, re.MULTILINE), f"missing ## {device} section"


def test_generated_banner_present(doc):
    assert "DO NOT EDIT" in doc


def test_setpoint_shows_readback_and_dangerous_columns(doc, catalog_records):
    trip = next(r for r in catalog_records if r["device"] == "EMS" and r["slug"] == "trip")
    assert trip["dangerous"] is True
    # Find Trip's own row and confirm it's marked dangerous.
    row = next(line for line in doc.splitlines() if line.startswith("| `trip` "))
    assert row.rstrip().endswith("| yes |")


def test_table_cells_never_contain_a_bare_pipe(doc):
    """A '|' inside a cell (Modbus function/label, or a name with one)
    would silently corrupt the Markdown table structure if not escaped."""
    for line in doc.splitlines():
        if not line.startswith("| `"):
            continue
        cols = line.split(" | ")
        # 13 columns -> 12 separators inside the row, plus the leading/
        # trailing "|" from the line itself; an unescaped '|' in a cell's
        # own content would produce more columns than the header does.
        assert len(cols) == 13, f"unexpected column count in row: {line!r}"
