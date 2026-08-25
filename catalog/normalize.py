"""Name normalization, unit/enum extraction, and slug generation.

`join_key()` is used ONLY to decide whether two names refer to the same
point across IEC-104 and Modbus (AGENT-TASK §4.9, §2.3.6). It intentionally
folds two known source-file artifacts, both confirmed by inspecting the
actual xlsx (not hypothetical):

  - "(°C)" vs "(C)"                         — §2.3.8, §4.9 anchor case
  - "惟" vs "Ω" in insulation-resistance units on Master Controller —
    "惟" is not a real unit, it is mangled/mis-encoded "Ω" in the IEC-104
    file only (confirmed against the Modbus file, which spells it "Ω"
    consistently). Folding it is a join-key normalization, not a data fix:
    the stored name_raw is never altered.

§4.9 additionally requires normalization to fold "word-order variants". A
full sweep of all 148 setpoints (IEC-104 WO name vs its own RO-readback
name — the only place such variants were actually found; IEC-104's WO
names already match Modbus's names exactly, 148/148, without any of this)
turned up 22 differences, of three different kinds:

  1. Pure reordering, e.g. "Enable Fan Power Following" vs "Enable Fan
     Following Power" (4 pairs) — folded below, safe: same words, no
     information lost.
  2. Phrase substitution, e.g. "Heating Start Temperature - Operation
     Phase (°C)" vs "...During Operation (°C)" (12 pairs, Operation/Idle) —
     folded below, safe: mechanical rewording of the same phrase.
  3. Word insertion/omission or outright synonyms, e.g. "Anti-reverse Power
     Margin" vs "Anti-Reverse Power Flow Margin" (adds "Flow"), "Power
     On/Off" vs "On/Off" (drops "Power"), "Dynamic Capacity Expansion
     Margin" vs "...Increase Margin" ("Expansion"/"Increase" are different
     words, not a formatting variant) — 6 pairs, deliberately NOT folded.
     Asserting these mean the same point would be guessing at intent, which
     the internal specification forbids inventing identities; a normalizer that folds
     synonyms it wasn't told about is exactly how a catalog goes silently
     wrong. If a future map revision needs these treated as equal, that
     belongs in catalog/overrides.yaml, decided by a person, not guessed
     here.

None of this changes today's IEC-104<->Modbus join outcome (already
148/148 without it) — it exists to satisfy §4.9's literal requirement and
to keep join_key() correct if it's ever reused (e.g. task 2 checking WO
vs readback name consistency, or a future revision of the point map).
"""
from __future__ import annotations

import re
import unicodedata

from pypinyin import Style, pinyin

# Safe, evidence-based phrase/word-order foldings (see module docstring,
# kind 1 and 2). Applied case-insensitively before casefolding.
_WORD_ORDER_PATTERNS = [
    (re.compile(r"-\s*Operation Phase", re.IGNORECASE), "During Operation"),
    (re.compile(r"-\s*Idle Phase", re.IGNORECASE), "During Idle"),
    (re.compile(r"Fan Following (Power|Temperature)", re.IGNORECASE), r"Fan \1 Following"),
]


def join_key(name: str) -> str:
    """Normalized key used to match the same point across IEC-104/Modbus."""
    s = name.replace("\xa0", " ")
    s = " ".join(s.split())
    s = s.replace("°", "")
    s = s.replace("惟", "Ω")
    for pattern, repl in _WORD_ORDER_PATTERNS:
        s = pattern.sub(repl, s)
    return s.strip().casefold()


_UNIT_RE = re.compile(r"\(([^()]*)\)\s*$")

# Units recognized by task 1.6. Matched case-sensitively
# against the parenthesized suffix once °/nbsp noise is stripped.
_KNOWN_UNITS = {
    "kW", "V", "mV", "°C", "C", "%", "kWh", "kvar", "kVA", "A", "s", "min",
}


def extract_unit(name: str) -> str | None:
    """Pull a trailing `(unit)` off a point name, e.g. 'Set Active Power (kW)' -> 'kW'."""
    m = _UNIT_RE.search(name.replace("\xa0", " ").strip())
    if not m:
        return None
    candidate = m.group(1).strip()
    if candidate in _KNOWN_UNITS:
        return "°C" if candidate == "C" else candidate
    return None


_ENUM_ITEM_RE = re.compile(r"(-?\d+)\s*:\s*([^;]+?)\s*(?:;|$)")


def parse_enum(description: str | None) -> dict[str, str] | None:
    """Parse '0: Manual; 1: Auto Strategy; 2: Remote;' -> {'0': 'Manual', ...}."""
    if not description:
        return None
    items = _ENUM_ITEM_RE.findall(description)
    if not items:
        return None
    return {code: label.strip() for code, label in items}


def validate_range(range_value: object) -> str | None:
    """Validate the shape of a point's `range` field (AGENT-TASK §6 item 1):
    `null`, or `{"min": <number|null>, "max": <number|null>}` with both keys
    present, no other keys, both bounds finite numbers when not null, at
    least one bound non-null, and min <= max when both are given. Returns
    None if valid, or a human-readable reason if not — never silently
    coerces a malformed shape into something usable.
    """
    if range_value is None:
        return None
    if not isinstance(range_value, dict):
        return f"range must be null or an object, got {type(range_value).__name__}"
    extra = set(range_value) - {"min", "max"}
    if extra:
        return f"range has unknown key(s): {sorted(extra)}"
    missing = {"min", "max"} - set(range_value)
    if missing:
        return f"range is missing key(s): {sorted(missing)}"
    lo, hi = range_value["min"], range_value["max"]
    for name, v in (("min", lo), ("max", hi)):
        if v is None:
            continue
        # bool is an int subclass in Python — reject it explicitly so
        # `{"min": true}` doesn't silently pass as 1.
        if isinstance(v, bool) or not isinstance(v, (int, float)):
            return f"range.{name} must be a number or null, got {type(v).__name__}"
        if isinstance(v, float) and (v != v or v in (float("inf"), float("-inf"))):
            return f"range.{name} must be finite"
    if lo is None and hi is None:
        return "range must have at least one non-null bound"
    if lo is not None and hi is not None and lo > hi:
        return f"range.min ({lo}) must be <= range.max ({hi})"
    return None


_SLUG_NONALNUM_RE = re.compile(r"[^a-z0-9]+")


def slugify(name: str) -> str:
    """ASCII, lower-case, underscore-separated slug. Transliterates Chinese
    names via pinyin (AGENT-TASK §1.8) rather than dropping them.
    """
    s = name.replace("\xa0", " ").strip()

    if any("一" <= ch <= "鿿" for ch in s):
        syllables = pinyin(s, style=Style.NORMAL, errors="default")
        s = " ".join(part for group in syllables for part in group)
    else:
        # NFKD-normalize so accented / full-width latin chars degrade to ascii
        s = unicodedata.normalize("NFKD", s)
        s = s.encode("ascii", "ignore").decode("ascii")

    s = s.lower()
    s = _SLUG_NONALNUM_RE.sub("_", s).strip("_")
    return s
