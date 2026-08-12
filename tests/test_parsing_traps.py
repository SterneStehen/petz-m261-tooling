"""Unit tests for catalog.parsing.split_subtables against every parsing trap
listed in AGENT-TASK §2.3, using small synthetic inputs (not the real xlsx —
see test_build_catalog.py for end-to-end checks against the real files).
"""
from __future__ import annotations

import pytest

from catalog.parsing import CatalogParseError, resolve_device, split_subtables

IEC_HEADER = ["Telemetry Address", "Name", "Access", "Data Type", "Precision", "Attribute", "Description", "Valid"]


def row(addr, name, access="RO", dtype="U8", precision=1, attribute="alarm", desc=None):
    return [addr, name, access, dtype, precision, attribute, desc, None]


def test_trap1_multiple_subtables_per_sheet():
    """§2.3.1: one sheet (`other`) holds two independent device subtables."""
    rows = [
        ["no:DIDO  name:Dry Contact  commonAddr:172", None, None, None, None, None, None, None],
        IEC_HEADER,
        row(16385, "Fire Protection Level 1 Alarm Feedback Signal", attribute="realdata"),
        ["no:CSJ  name:除湿机  commonAddr:172", None, None, None, None, None, None, None],
        IEC_HEADER,
        row(16685, "温度(℃)", attribute="realdata"),
    ]
    subtables = split_subtables("other", rows)
    assert [st.device_code for st in subtables] == ["DIDO", "CSJ"]
    assert len(subtables[0].rows) == 1
    assert len(subtables[1].rows) == 1


def test_trap2_missing_no_line_continues_previous_device():
    """§2.3.2: a repeated 'Telemetry Address' header with no preceding 'no:'
    line continues the current device rather than starting a new one."""
    rows = [
        ["no:EMS  name:EMS  commonAddr:1", None, None, None, None, None, None, None],
        IEC_HEADER,
        row(1, "Manual Protection"),
        IEC_HEADER,  # repeated header, no 'no:' line before it
        row(2, "Electricity Meter Fault"),
    ]
    subtables = split_subtables("EMS", rows)
    assert len(subtables) == 1
    assert len(subtables[0].rows) == 2


def test_trap3_blank_and_service_rows_are_skipped():
    """§2.3.3: blank rows between subtables must not break parsing."""
    rows = [
        ["no:EMS  name:EMS  commonAddr:1", None, None, None, None, None, None, None],
        IEC_HEADER,
        row(1, "Manual Protection"),
        [None, None, None, None, None, None, None, None],
        [None, None, None, None, None, None, None, None],
        row(2, "Electricity Meter Fault"),
    ]
    subtables = split_subtables("EMS", rows)
    assert len(subtables) == 1
    assert len(subtables[0].rows) == 2


def test_trap4_dry_contact_header_address_disagrees_with_canonical_table():
    """§2.3.4: the in-sheet header says commonAddr:172 for DIDO, but §4.1 says
    168. We must record the raw header value for the validation report AND
    use the canonical §4.1 address for device_addr — never the in-sheet one.
    """
    rows = [
        ["no:DIDO  name:Dry Contact  commonAddr:172", None, None, None, None, None, None, None],
        IEC_HEADER,
        row(16385, "Fire Protection Level 1 Alarm Feedback Signal", attribute="realdata"),
    ]
    subtables = split_subtables("other", rows)
    assert subtables[0].header_addr == 172  # raw value, preserved for validation
    device, addr = resolve_device(subtables[0].device_code, "other")
    assert (device, addr) == ("DIDO", 168)  # canonical §4.1 value used for the catalog


def test_trap5_truncated_names_preserved_as_is():
    """§2.3.5: truncated manufacturer names are kept verbatim, not 'fixed'."""
    rows = [
        ["no:PCS  name:Converter  commonAddr:2", None, None, None, None, None, None, None],
        IEC_HEADER,
        row(1, "Power Module Cycle-by-Cycle Current Limi", attribute="realdata"),
    ]
    subtables = split_subtables("Converter", rows)
    assert subtables[0].rows[0][1] == "Power Module Cycle-by-Cycle Current Limi"


def test_trap_data_row_without_device_header_is_an_error():
    """A data row before any 'no:' line means our assumptions about the sheet
    structure are wrong — fail loudly rather than silently attribute it to
    the wrong device."""
    rows = [IEC_HEADER, row(1, "Manual Protection")]
    with pytest.raises(CatalogParseError):
        split_subtables("EMS", rows)


def test_unknown_device_code_is_an_error():
    rows = [
        ["no:NOPE  name:Something", None, None, None, None, None, None, None],
        IEC_HEADER,
        row(1, "x"),
    ]
    subtables = split_subtables("EMS", rows)
    with pytest.raises(CatalogParseError):
        resolve_device(subtables[0].device_code, "EMS")
