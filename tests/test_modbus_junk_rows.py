"""Code-review finding: the 'Parameter Names:' template-artifact rows in the
Modbus workbook (Converter and Master Controller, addr 30001 — see
catalog.parsing._MODBUS_JUNK_ROW_NAMES) were parsed as if they were real
points and only happened to end up unmatched by luck of not colliding with
any real IEC-104 name. They must be filtered explicitly.
"""
from __future__ import annotations

from pathlib import Path

from catalog.parsing import POINT_SHEETS, parse_modbus
from tests.conftest import requires_registermap

REGISTERMAP = Path(__file__).resolve().parent.parent / "m261-registermap"


def test_parameter_names_row_filtered_from_synthetic_sheet(tmp_path):
    import openpyxl

    wb = openpyxl.Workbook()
    ws = wb.active
    ws.title = "Converter"
    for extra in POINT_SHEETS:
        if extra != "Converter":
            wb.create_sheet(extra)
    ws.append(["no:PCS  name:Converter  addr:2", None, None, None, None, None, None, None, None])
    ws.append(["Telemetry Address", "adress", "Name", "Access", "Data Type", "Precision", "Attribute", "Description", "Valid"])
    ws.append([2, 10001, "EPO Fault", "RO", "U8", 1, "alarm", "0: Normal; 1: Fault;", None])
    ws.append([4, 30001, "Parameter Names:", "RO", "I32", "0.1", "realdata", None, None])
    ws.append([4, 30003, "Phase A Voltage (V)", "RO", "I32", "0.1", "realdata", None, None])
    path = tmp_path / "synthetic_modbus.xlsx"
    wb.save(path)

    dropped: list[tuple[str, int, str]] = []
    rows = parse_modbus(path, dropped=dropped)

    assert [r.name_raw for r in rows] == ["EPO Fault", "Phase A Voltage (V)"]
    assert dropped == [("PCS", 30001, "Parameter Names:")]


@requires_registermap
def test_parameter_names_row_filtered_from_real_file():
    dropped: list[tuple[str, int, str]] = []
    rows = parse_modbus(REGISTERMAP / "M261_points_Modbus.xlsx", dropped=dropped)

    assert not any(r.name_raw == "Parameter Names:" for r in rows)
    assert ("PCS", 30001, "Parameter Names:") in dropped
    assert ("BMS", 30001, "Parameter Names:") in dropped
    assert len(dropped) == 2
