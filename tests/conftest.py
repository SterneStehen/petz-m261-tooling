"""Shared pytest fixtures/markers for the tests/ suite.

REGISTERMAP points at m261-registermap/ (the real manufacturer XLSX
files) -- private project material, gitignored, and not available in a
plain CI checkout (see /.gitignore's own "private project materials"
note). Several end-to-end tests genuinely need those real files (not a
synthetic fixture) to be meaningful at all; requires_registermap and
skip_if_no_registermap let them skip cleanly, with a clear reason, rather
than failing with a raw FileNotFoundError whenever the maps aren't
present -- Task 8's own "CI green" criterion has to hold in an
environment that was never given access to the private inputs.
"""
from __future__ import annotations

from pathlib import Path

import pytest

REPO_ROOT = Path(__file__).resolve().parent.parent
REGISTERMAP = REPO_ROOT / "m261-registermap"
CATALOG = REPO_ROOT / "catalog" / "point_catalog.json"

registermap_available = REGISTERMAP.exists()
# catalog/point_catalog.json is itself derived from the register maps
# (catalog.build_catalog) and gitignored — a plain CI checkout has
# neither. Checked separately from registermap_available: a caller with
# only the *already-built* catalog JSON locally (no raw XLSX) can still
# run everything that only reads it (codegen), so this isn't simply
# "registermap_available or worse".
catalog_available = CATALOG.exists()

_REGISTERMAP_SKIP_REASON = (
    "m261-registermap/ (private, local-only manufacturer register maps) "
    "is not present -- this test needs the real files, not a synthetic "
    "fixture, and cannot run without them"
)
_CATALOG_SKIP_REASON = (
    "catalog/point_catalog.json (private, local-only, generated from the "
    "manufacturer register maps) is not present -- this test needs the "
    "real generated catalog, not a synthetic fixture, and cannot run "
    "without it"
)

# Decorator form, for a single test function or (assigned to `pytestmark`)
# an entire module.
requires_registermap = pytest.mark.skipif(not registermap_available, reason=_REGISTERMAP_SKIP_REASON)
requires_catalog = pytest.mark.skipif(not catalog_available, reason=_CATALOG_SKIP_REASON)


def skip_if_no_registermap() -> None:
    """Call form, for use inside a fixture -- a bare @pytest.mark.skipif
    on a fixture function does not skip the tests that request it, but
    pytest.skip() raised from within the fixture itself does."""
    if not registermap_available:
        pytest.skip(_REGISTERMAP_SKIP_REASON)


def skip_if_no_catalog() -> None:
    if not catalog_available:
        pytest.skip(_CATALOG_SKIP_REASON)
