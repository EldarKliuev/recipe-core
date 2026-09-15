"""Rule-based parsing of raw ingredient lines into (qty, unit, name).

This is the load-bearing piece of the whole ingest: everything downstream keys off
the canonical name this module produces. It is deliberately a pile of explicit
rules rather than a model, because the failure modes are inspectable and the
corpus is one fixed dataset.
"""

from __future__ import annotations

import re
import unicodedata
from dataclasses import dataclass
from fractions import Fraction

# Volume/weight/count units seen in the corpus, mapped to a canonical spelling.
UNITS: dict[str, str] = {
    "teaspoon": "tsp", "teaspoons": "tsp", "tsp": "tsp", "tsps": "tsp", "t": "tsp",
    "tablespoon": "tbsp", "tablespoons": "tbsp", "tbsp": "tbsp", "tbsps": "tbsp", "tbs": "tbsp",
    "cup": "cup", "cups": "cup", "c": "cup",
    "pint": "pint", "pints": "pint", "quart": "quart", "quarts": "quart",
    "gallon": "gallon", "gallons": "gallon",
    "ounce": "oz", "ounces": "oz", "oz": "oz",
    "fluid ounce": "fl oz", "fluid ounces": "fl oz",
    "pound": "lb", "pounds": "lb", "lb": "lb", "lbs": "lb",
    "gram": "g", "grams": "g", "g": "g",
    "kilogram": "kg", "kilograms": "kg", "kg": "kg",
    "milliliter": "ml", "milliliters": "ml", "ml": "ml",
    "liter": "l", "liters": "l", "litre": "l", "l": "l",
    "clove": "clove", "cloves": "clove",
    "slice": "slice", "slices": "slice",
    "can": "can", "cans": "can", "jar": "jar", "jars": "jar",
    "package": "package", "packages": "package", "pkg": "package",
    "container": "container", "containers": "container",
    "bunch": "bunch", "bunches": "bunch",
    "sprig": "sprig", "sprigs": "sprig",
    "stalk": "stalk", "stalks": "stalk",
    "head": "head", "heads": "head",
    "pinch": "pinch", "pinches": "pinch",
    "dash": "dash", "dashes": "dash",
    "envelope": "envelope", "envelopes": "envelope",
    "bottle": "bottle", "bottles": "bottle",
    "square": "square", "squares": "square",
    "sheet": "sheet", "sheets": "sheet",
    "loaf": "loaf", "loaves": "loaf",
    "piece": "piece", "pieces": "piece",
}

# Size and preparation words that describe the ingredient rather than identify it.
# Stripping them is what collapses "2 large ripe bananas, mashed" and "1 banana"
# onto the same canonical id.
DESCRIPTORS = {
    "large", "small", "medium", "extra", "jumbo", "thin", "thick", "whole",
    "fresh", "freshly", "frozen", "canned", "dried", "dry", "ripe", "raw",
    "cooked", "uncooked", "prepared", "packed", "lightly", "firmly", "loosely",
    "chopped", "minced", "diced", "sliced", "shredded", "grated", "crushed",
    "ground", "mashed", "melted", "softened", "beaten", "peeled", "seeded",
    "cored", "trimmed", "rinsed", "drained", "divided", "halved", "quartered",
    "cubed", "julienned", "halves", "torn", "toasted", "roasted", "blanched", "boiling",
    "cold", "warm", "hot", "room", "temperature", "optional", "or", "to",
    "taste", "needed", "plus", "more", "for", "garnish", "serving", "such",
    "as", "about", "approximately", "very", "finely", "coarsely", "roughly",
    "thinly", "cut", "into", "inch", "pieces", "wedges", "strips", "rounds",
    "at", "if", "desired", "unsweetened", "sweetened", "reduced", "low",
    "fat", "free", "nonfat", "skinless", "boneless", "lean", "extra-virgin",
    "virgin", "your", "favorite", "any", "unbleached", "sifted", "packed",
}

# Words that must never be stripped even though they look like descriptors,
# because they distinguish genuinely different ingredients.
KEEP = {"ground beef", "ground turkey", "ground pork", "ground chicken",
        "ground lamb", "ground cinnamon", "ground cumin", "ground ginger",
        "ground nutmeg", "ground cloves", "ground mustard", "ground coriander",
        "ground black pepper", "ground white pepper", "ground allspice",
        "dry mustard", "dry sherry", "dry white wine", "dry red wine",
        "fresh mozzarella", "cooked ham", "cooked rice",
        "frozen peas", "frozen corn", "frozen spinach"}

_VULGAR = {
    "½": "1/2", "⅓": "1/3", "⅔": "2/3", "¼": "1/4", "¾": "3/4",
    "⅕": "1/5", "⅖": "2/5", "⅗": "3/5", "⅘": "4/5", "⅙": "1/6",
    "⅚": "5/6", "⅛": "1/8", "⅜": "3/8", "⅝": "5/8", "⅞": "7/8",
}

_PAREN = re.compile(r"\([^)]*\)")
_NUMBER = re.compile(r"^(\d+\s+\d+/\d+|\d+/\d+|\d+\.\d+|\d+)")
# "2-3", "1 to 2" and "1 or 2" all mean a range; the lower bound is kept.
_RANGE = re.compile(r"^\s*(?:-|–|to\b|or\b)\s*(\d+\s+\d+/\d+|\d+/\d+|\d+\.\d+|\d+)")
_NON_NAME = re.compile(r"[^a-z0-9\s\-']")
_MULTISPACE = re.compile(r"\s+")

# Irregular plurals that a trailing-s rule would mangle.
_PLURAL_EXCEPTIONS = {
    "leaves": "leaf", "loaves": "loaf", "halves": "half", "knives": "knife",
    "tomatoes": "tomato", "potatoes": "potato", "berries": "berry",
    "cherries": "cherry", "strawberries": "strawberry",
    "blueberries": "blueberry", "raspberries": "raspberry",
    "cranberries": "cranberry", "anchovies": "anchovy",
    "molasses": "molasses", "asparagus": "asparagus", "hummus": "hummus",
    "couscous": "couscous", "swiss": "swiss", "bass": "bass",
    "watercress": "watercress", "grits": "grits", "greens": "greens",
    "oats": "oats", "chives": "chives", "capers": "capers", "peas": "peas",
    "beans": "beans", "lentils": "lentils", "noodles": "noodles",
    "sprouts": "sprouts", "grapes": "grapes", "raisins": "raisins",
    "breadcrumbs": "breadcrumbs", "cloves": "clove",
}


@dataclass(slots=True)
class ParsedLine:
    raw: str
    name: str
    qty: float | None
    unit: str | None
    # confident is False when the line produced a name but the parse looked
    # unusual (no quantity, very long name). These are the rows worth sampling.
    confident: bool


def normalise_unicode(text: str) -> str:
    # Vulgar fractions are replaced before NFKC on purpose: NFKC turns "½" into
    # "1⁄2" with a U+2044 fraction slash, which then fails every numeric rule.
    for glyph, ascii_form in _VULGAR.items():
        text = text.replace(glyph, f" {ascii_form}")
    text = unicodedata.normalize("NFKC", text)
    return text.replace("\u2044", "/")


def _to_float(token: str) -> float | None:
    token = token.strip()
    try:
        if " " in token:  # mixed number: "1 1/2"
            whole, frac = token.split(None, 1)
            return float(int(whole) + Fraction(frac))
        if "/" in token:
            return float(Fraction(token))
        return float(token)
    except (ValueError, ZeroDivisionError):
        return None


def singularise(word: str) -> str:
    if word in _PLURAL_EXCEPTIONS:
        return _PLURAL_EXCEPTIONS[word]
    if len(word) > 3 and word.endswith("ies"):
        return word[:-3] + "y"
    if len(word) > 3 and word.endswith("es") and not word.endswith(("ses", "ces", "ges", "zes", "shes", "ches")):
        return word[:-1]
    if len(word) > 3 and word.endswith("s") and not word.endswith("ss"):
        return word[:-1]
    return word


def parse_line(raw: str) -> ParsedLine | None:
    """Parse one raw ingredient line. Returns None if nothing usable is left."""
    original = raw.strip()
    if not original:
        return None

    text = normalise_unicode(original).lower()
    # Parenthetical content is almost always a size note: "(15 ounce) can".
    text = _PAREN.sub(" ", text)
    # Everything after the first comma is usually preparation, not identity. But
    # "4 skinless, boneless chicken breasts" puts the identity *after* the comma,
    # so try the head first and fall back to the whole line if it yields nothing.
    head, _, tail = text.partition(",")
    candidates = [_tidy(head)]
    if tail:
        candidates.append(_tidy(head + " " + tail.replace(",", " ")))

    for candidate in candidates:
        if not candidate:
            continue
        qty, unit, name = _parse_core(candidate)
        if name:
            confident = len(name.split()) <= 4 and not name.isdigit()
            return ParsedLine(raw=original, name=name, qty=qty, unit=unit, confident=confident)
    return None


def _tidy(text: str) -> str:
    """Drop trailing clauses that describe usage rather than identity."""
    for marker in (" for ", " as needed", " to taste", " or more", " plus ",
                   " divided", " if desired", " optional"):
        text = text.split(marker, 1)[0]
    return _MULTISPACE.sub(" ", text).strip()


def _parse_core(text: str) -> tuple[float | None, str | None, str]:
    qty: float | None = None
    match = _NUMBER.match(text)
    if match:
        qty = _to_float(match.group(1))
        text = text[match.end():]
        # Collapse "1 to 2 cups" and "2-3 cloves" to the lower bound.
        range_match = _RANGE.match(text)
        if range_match:
            text = text[range_match.end():]
        text = text.lstrip(" -\u2013")

    unit: str | None = None
    tokens = text.split()
    if tokens:
        # Try the two-word units ("fluid ounces") before the one-word ones.
        two = " ".join(tokens[:2])
        if two in UNITS:
            unit, tokens = UNITS[two], tokens[2:]
        elif tokens[0] in UNITS:
            unit, tokens = UNITS[tokens[0]], tokens[1:]

    # A second quantity right after the unit is a package size: "2 (15 oz) cans".
    if tokens and _NUMBER.match(tokens[0]):
        tokens = tokens[1:]
        if tokens and tokens[0] in UNITS:
            tokens = tokens[1:]

    return qty, unit, _clean_name(" ".join(tokens))


def _clean_name(text: str) -> str:
    text = _NON_NAME.sub(" ", text)
    text = _MULTISPACE.sub(" ", text).strip()
    if not text:
        return ""

    # Protect compounds whose descriptor is part of the identity.
    for phrase in KEEP:
        if text.startswith(phrase):
            rest = _strip_descriptors(text[len(phrase):])
            return f"{phrase} {rest}".strip() if rest else phrase

    cleaned = _strip_descriptors(text)
    return COMPOUNDS.get(cleaned, cleaned)


# Lines naming two ingredients at once. Collapsing them to one canonical name
# loses nothing for set coverage here, because every counterpart is a pantry
# staple and therefore never counted as missing anyway.
COMPOUNDS = {
    "salt and pepper": "salt",
    "salt and black pepper": "salt",
    "pepper and salt": "salt",
    "salt and freshly pepper": "salt",
    "oil and vinegar": "vegetable oil",
}


def _strip_descriptors(text: str) -> str:
    words = [w for w in text.split() if w and w not in DESCRIPTORS]
    if not words:
        return ""
    words = [singularise(w) for w in words]
    # Drop dangling connectives left behind by descriptor removal.
    while words and words[0] in {"of", "and", "with", "in", "the", "a", "an"}:
        words.pop(0)
    while words and words[-1] in {"of", "and", "with", "in", "the", "a", "an", "-"}:
        words.pop()
    return " ".join(words).strip(" -")
