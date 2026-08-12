"""Task 3 acceptance: 'make generate regenerates everything from scratch,
git diff is empty'. Exercised directly here (byte-for-byte, two separate
output directories) rather than relying only on a manual `git diff` check.
"""
from __future__ import annotations

import filecmp
from pathlib import Path

from codegen.gen_go import main as gen_go_main
from codegen.gen_python import main as gen_python_main
from codegen.gen_seeds import main as gen_seeds_main


def _assert_dirs_identical(a: Path, b: Path) -> None:
    cmp = filecmp.dircmp(a, b)
    assert not cmp.left_only, f"only in {a}: {cmp.left_only}"
    assert not cmp.right_only, f"only in {b}: {cmp.right_only}"
    _, mismatch, errors = filecmp.cmpfiles(a, b, cmp.common_files, shallow=False)
    assert not mismatch, f"differ: {mismatch}"
    assert not errors, f"couldn't compare: {errors}"


def test_gen_go_is_idempotent(tmp_path):
    out1, out2 = tmp_path / "run1", tmp_path / "run2"
    gen_go_main(["--out", str(out1)])
    gen_go_main(["--out", str(out2)])
    _assert_dirs_identical(out1, out2)


def test_gen_python_is_idempotent(tmp_path):
    out1, out2 = tmp_path / "run1", tmp_path / "run2"
    gen_python_main(["--out", str(out1)])
    gen_python_main(["--out", str(out2)])
    _assert_dirs_identical(out1, out2)


def test_gen_seeds_is_idempotent(tmp_path):
    out1, out2 = tmp_path / "run1", tmp_path / "run2"
    gen_seeds_main(["--out", str(out1)])
    gen_seeds_main(["--out", str(out2)])
    _assert_dirs_identical(out1, out2)
