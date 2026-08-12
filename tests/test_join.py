"""Tests for catalog.join against synthetic rows, covering the trickiest
rules in AGENT-TASK §4.9: TAG's Name-column offset trap (§2.3.7), the
setpoint<->readback tag redirect, and the setpoint name-join formula
fallback (§4.3).
"""
from __future__ import annotations

import pytest

from catalog.join import JoinWarnings, join_device
from catalog.parsing import IecRow, ModbusRow, TagRow


def iec(addr, name, access="RO", attribute="alarm", dtype="U8"):
    return IecRow(
        device="EMS", device_addr=1, address=addr, name_raw=name, access=access,
        data_type=dtype, precision="1", attribute=attribute, description=None,
    )


def mb(reg_class, addr, name, access="RO", dtype="U8"):
    return ModbusRow(
        device="EMS", device_addr=1, reg_class=reg_class, address=addr, name_raw=name,
        access=access, data_type=dtype, precision="1", attribute="alarm" if reg_class == 2 else "realdata",
        description=None,
    )


def tag(t, name="IGNORED — deliberately wrong, see §2.3.7", access="RO"):
    return TagRow(
        device="EMS", device_addr=1, tag=t, name_raw=name, access=access,
        data_type="I32", attribute="alarm", description=None,
    )


def test_tag_positional_join_ignores_tag_files_own_name_column():
    """§2.3.7: TAG's Name column is offset/unreliable. The join must attach
    tags purely by position against IEC-104's RO rows, never by matching
    TAG's own (wrong) Name column."""
    iec_rows = [iec(1, "Manual Protection"), iec(2, "Electricity Meter Fault")]
    tag_rows = [tag("SysStoped"), tag("MeterError")]
    points = join_device("EMS", iec_rows, [], tag_rows, JoinWarnings())
    by_addr = {p.iec104_addr: p for p in points}
    assert by_addr[1].tag == "SysStoped"
    assert by_addr[2].tag == "MeterError"


def test_setpoint_gets_tag_from_its_readback_position_not_its_own():
    """A setpoint (WO, 25089+k) has no entry of its own among IEC-104's RO
    rows, so its tag must come from the TAG row positioned at the matching
    readback address (16411+k), per §4.3/§4.9."""
    iec_rows = [
        iec(1, "Manual Protection"),
        iec(16411, "Air Conditioner Control", attribute="realdata"),  # readback of 25089
        iec(25089, "Air Conditioner Control", access="WO", attribute="realdata"),
    ]
    modbus_rows = [mb(3, 40001, "Air Conditioner Control", access="RW")]  # required, §4.3
    tag_rows = [tag("SysStoped"), tag("AirCondStartStopCtrl")]  # 2 RO rows -> 2 tags
    points = join_device("EMS", iec_rows, modbus_rows, tag_rows, JoinWarnings())
    by_addr = {p.iec104_addr: p for p in points}
    assert by_addr[25089].tag == "AirCondStartStopCtrl"
    assert by_addr[25089].readback_iec104_addr == 16411
    assert by_addr[16411].tag is None  # readback row itself carries no tag


def test_tag_row_count_mismatch_raises():
    """A TAG/RO length mismatch breaks positional alignment and must stop the build."""
    iec_rows = [iec(1, "Manual Protection"), iec(2, "Electricity Meter Fault")]
    tag_rows = [tag("SysStoped")]  # one short
    with pytest.raises(ValueError):
        join_device("EMS", iec_rows, [], tag_rows, JoinWarnings())


def test_name_matched_setpoint_with_wrong_address_is_rejected():
    """Code-review finding: a name-based match must still be checked against
    the §4.3 formula. Reproduces the reported case verbatim — IEC 25089
    ('Air Conditioner Control') name-matches a Modbus row of the same name,
    but at addr=49999 instead of the mandatory 40001. This must not be
    silently accepted into the catalog."""
    iec_rows = [iec(25089, "Air Conditioner Control", access="WO", attribute="realdata")]
    modbus_rows = [mb(3, 49999, "Air Conditioner Control", access="RW")]
    with pytest.raises(Exception, match="formula"):
        join_device("EMS", iec_rows, modbus_rows, [], JoinWarnings())


def test_setpoint_with_no_modbus_counterpart_at_all_is_rejected():
    """Code-review finding: when neither the name-join nor the §4.3 formula
    finds a Modbus row, the setpoint must not be silently emitted with
    modbus_addr: null — Task 1 requires all 148 setpoints to have both
    addresses. Reproduces the reported case verbatim: IEC 25089 with no
    Modbus row present at all."""
    iec_rows = [iec(25089, "Air Conditioner Control", access="WO", attribute="realdata")]
    with pytest.raises(Exception, match="no Modbus counterpart"):
        join_device("EMS", iec_rows, [], [], JoinWarnings())


def test_setpoint_name_mismatch_falls_back_to_address_formula():
    """§4.3/§4.9: when a setpoint's name doesn't match any Modbus row, fall
    back to modbus_addr = 40001 + (iec_addr - 25089) * 2 and record it as a
    controlled fallback, not a silent success."""
    iec_rows = [iec(25089, "Air Conditioner Control", access="WO", attribute="realdata")]
    modbus_rows = [mb(3, 40001, "AC Control (renamed on the Modbus side)", access="RW")]
    warnings = JoinWarnings()
    points = join_device("EMS", iec_rows, modbus_rows, [], warnings)
    assert points[0].modbus_addr == 40001
    assert len(warnings.setpoints_joined_by_formula) == 1
    assert warnings.setpoints_joined_by_formula[0][0] == "EMS"


def test_unmatched_alarm_leaves_modbus_fields_none_and_warns():
    iec_rows = [iec(1, "Only In IEC104")]
    warnings = JoinWarnings()
    points = join_device("EMS", iec_rows, [], [tag("SomeTag")], warnings)
    assert points[0].modbus_addr is None
    assert points[0].sources == ["IEC104", "TAG"]
    assert len(warnings.unmatched_iec_alarms_telemetry) == 1


def test_leftover_modbus_row_is_recorded_not_dropped_silently():
    iec_rows: list[IecRow] = []
    modbus_rows = [mb(2, 10001, "Only In Modbus")]
    warnings = JoinWarnings()
    join_device("EMS", iec_rows, modbus_rows, [], warnings)
    assert warnings.unmatched_modbus == [("EMS", 10001, "Only In Modbus")]
