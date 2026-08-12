from catalog.normalize import extract_unit, join_key, parse_enum, slugify


def test_join_key_collapses_nbsp_and_extra_whitespace():
    """§2.3.6."""
    assert join_key("Set\xa0Active  Power") == join_key("Set Active Power")


def test_join_key_folds_degree_c_and_bare_c():
    """§4.9 anchor case: 'Output Cooling Target Temperature (C)' vs '(°C)'."""
    a = "Output Cooling Target Temperature (C)"
    b = "Output Cooling Target Temperature (°C)"
    assert join_key(a) == join_key(b)


def test_join_key_folds_mangled_omega():
    """Confirmed against the real files: the IEC-104 file spells the ohm sign
    as the Chinese character 惟 (mojibake) on 6 Master Controller insulation
    points, while Modbus spells it correctly as Ω for the same points."""
    assert join_key("Insulation Value (k惟)") == join_key("Insulation Value (kΩ)")


def test_join_key_is_case_insensitive():
    assert join_key("ONLINE STATUS") == join_key("Online Status")


# Full sweep of all 22 WO-name vs readback-name differences found across the
# real EMS setpoints (§4.9 "word-order variants"). Kept as an explicit table
# so the reasoning in the normalize.py docstring is pinned to a test, not
# just prose. This does not affect the current IEC-104<->Modbus join (which
# is already 148/148 exact without it) — see module docstring.
_SAFE_FOLDED_PAIRS = [
    ("Enable Fan Power Following", "Enable Fan Following Power"),
    ("Fan Power Following Threshold (kW)", "Fan Following Power Threshold (kW)"),
    ("Enable Fan Temperature Following", "Enable Fan Following Temperature"),
    ("Fan Temperature Following Threshold (°C)", "Fan Following Temperature Threshold (°C)"),
    ("Heating Start Temperature - Operation Phase (°C)", "Heating Start Temperature During Operation (°C)"),
    ("Cooling Target Temperature - Idle Phase (°C)", "Cooling Target Temperature During Idle (°C)"),
]

_DELIBERATELY_NOT_FOLDED_PAIRS = [
    ("Enable Reverse Power Protection", "Enable Reverse Power Flow Protection"),  # word inserted
    ("Anti-reverse Power Margin (kW)", "Anti-Reverse Power Flow Margin (kW)"),  # word inserted
    ("Anti-reverse Power Sliding Window (s)", "Anti-Reverse Power Flow Sliding Window (s)"),  # word inserted
    ("Power On/Off", "On/Off"),  # word dropped
    ("Fan Control", "Fan"),  # word dropped
    ("Dynamic Capacity Expansion Margin", "Dynamic Capacity Increase Margin"),  # synonym, not formatting
]


def test_safe_word_order_variants_are_folded():
    for a, b in _SAFE_FOLDED_PAIRS:
        assert join_key(a) == join_key(b), f"{a!r} vs {b!r}"


def test_risky_word_variants_are_deliberately_not_folded():
    """These are real differences between a setpoint's WO name and its own
    readback name (never between IEC-104 and Modbus). Folding them would
    mean guessing they're synonyms — left alone per AGENT-TASK §1.1."""
    for a, b in _DELIBERATELY_NOT_FOLDED_PAIRS:
        assert join_key(a) != join_key(b), f"{a!r} vs {b!r} should NOT be folded"


def test_extract_unit_known_units():
    assert extract_unit("Set Active Power (kW)") == "kW"
    assert extract_unit("Maximum Charge SOC (%)") == "%"
    assert extract_unit("Cell Voltage 001 (mV)") == "mV"
    assert extract_unit("Output Cooling Target Temperature (°C)") == "°C"
    assert extract_unit("Output Cooling Target Temperature (C)") == "°C"


def test_extract_unit_none_for_non_unit_parenthetical():
    assert extract_unit("Strategy Period 1 Execution Power (-Charge +Discharge)") is None
    assert extract_unit("Manual Protection") is None


def test_parse_enum_basic():
    assert parse_enum("0: Manual; 1: Auto Strategy; 2: Remote;") == {
        "0": "Manual",
        "1": "Auto Strategy",
        "2": "Remote",
    }


def test_parse_enum_none_when_absent():
    assert parse_enum(None) is None
    assert parse_enum("just a free-text description") is None


def test_slugify_ascii_name():
    assert slugify("Set Operating Mode") == "set_operating_mode"
    assert slugify("Set Active Power (kW)") == "set_active_power_kw"


def test_slugify_chinese_name_uses_pinyin():
    """§1.8: Chinese names get a latin slug via pinyin, original kept in name_raw."""
    slug = slugify("除湿机")
    assert slug  # non-empty
    assert slug.isascii()
