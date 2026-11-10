"""Allergen tagging of canonical ingredient names.

Keyword rules produce a *proposal*, not a verdict. The plan calls for manual
confirmation, and the reason is asymmetric risk: a missed tag can hurt someone,
while a spurious tag only narrows results. So `allergens` writes a review file,
`load` reads tags only from that file, and it refuses to run until every catalog
entry in it is marked as reviewed.

Tag vocabulary must stay in sync with `user.KnownAllergens` in the Go code.
"""

from __future__ import annotations

import collections
import csv
import re
from pathlib import Path
from typing import Container, Iterable, Mapping

# Same order as user.KnownAllergens, which is also the order tags are proposed in.
ALLERGENS: tuple[str, ...] = (
    "dairy", "eggs", "fish", "shellfish", "tree_nuts", "peanuts",
    "wheat", "gluten", "soy", "sesame", "mustard", "celery", "sulfites",
)

_WHEAT = ("wheat", "gluten")

# Each rule matches whole words of the canonical name, with an optional plural
# "s"/"es". Every matching rule contributes its tags, but a longer rule consumes
# the words it matched, so shorter rules cannot match inside them. That is how
# the exemptions work: "coconut milk" consumes "milk" and contributes nothing,
# "peanut butter" contributes peanuts and keeps "butter" from adding dairy.
# Because an exemption hides tags, each one must list every allergen the whole
# phrase still carries ("cream of celery soup" keeps celery).
#
# Compound words do not contain their parts ("eggplant" is not "egg",
# "cornbread" is not "bread"), so the ones that do carry an allergen are listed
# as rules of their own.
RULES: list[tuple[str, tuple[str, ...]]] = [
    # Dairy, and the dairy-sounding names that are not.
    ("milk", ("dairy",)),
    ("buttermilk", ("dairy",)),
    ("coconut milk", ()),
    ("almond milk", ("tree_nuts",)),
    ("almondmilk", ("tree_nuts",)),
    ("soy milk", ("soy",)),
    ("oat milk", ()),
    ("rice milk", ()),
    ("cream", ("dairy",)),
    ("coconut cream", ()),
    ("cream of coconut", ()),
    ("cream of tartar", ()),
    ("marshmallow cream", ()),
    ("marshmallow creme", ()),
    ("cream of celery soup", ("dairy", *_WHEAT, "celery")),
    ("cream of chicken soup", ("dairy", *_WHEAT)),
    ("cream of mushroom soup", ("dairy", *_WHEAT)),
    ("cream of potato soup", ("dairy", *_WHEAT)),
    ("creamy salad dressing", ("dairy", "eggs")),
    ("creamed corn", ("dairy",)),
    ("creme fraiche", ("dairy",)),
    ("crema", ("dairy",)),
    ("whipped topping", ("dairy",)),
    ("half and half", ("dairy",)),
    ("half-and-half", ("dairy",)),
    ("butter", ("dairy",)),
    ("buttery", ("dairy",)),
    ("butterscotch", ("dairy",)),
    ("peanut butter", ("peanuts",)),
    ("almond butter", ("tree_nuts",)),
    ("cocoa butter", ()),
    ("apple butter", ()),
    ("butter lettuce", ()),
    ("butter bean", ()),
    ("ghee", ("dairy",)),
    ("cheese", ("dairy",)),
    ("cheesecake", ("dairy", "eggs", *_WHEAT)),
    ("yogurt", ("dairy",)),
    ("yoghurt", ("dairy",)),
    ("ricotta", ("dairy",)),
    ("mascarpone", ("dairy",)),
    ("mozzarella", ("dairy",)),
    ("parmesan", ("dairy",)),
    ("cheddar", ("dairy",)),
    ("provolone", ("dairy",)),
    ("gouda", ("dairy",)),
    ("brie", ("dairy",)),
    ("feta", ("dairy",)),
    ("paneer", ("dairy",)),
    ("whey", ("dairy",)),
    ("casein", ("dairy",)),
    ("custard", ("dairy", "eggs")),
    ("caramel", ("dairy",)),
    ("dulce de leche", ("dairy",)),
    ("alfredo", ("dairy",)),
    ("tzatziki", ("dairy",)),
    ("white chocolate", ("dairy",)),
    # Most commercial chips list milk; bars vary too much for a rule.
    ("chocolate chip", ("dairy",)),
    ("vegan chocolate chip", ()),

    # Eggs.
    ("egg", ("eggs",)),
    ("eggnog", ("dairy", "eggs")),
    ("mayonnaise", ("eggs",)),
    ("meringue", ("eggs",)),
    ("aioli", ("eggs",)),
    ("lemon curd", ("dairy", "eggs")),
    ("ranch", ("dairy", "eggs")),
    ("thousand island dressing", ("eggs",)),

    # Nuts. "nut" alone is the corpus's unnamed "chopped nuts", which may be
    # peanuts; migration 00006 tags it the same way.
    ("nut", ("tree_nuts", "peanuts")),
    ("peanut", ("peanuts",)),
    ("almond", ("tree_nuts",)),
    ("walnut", ("tree_nuts",)),
    ("pecan", ("tree_nuts",)),
    ("cashew", ("tree_nuts",)),
    ("pistachio", ("tree_nuts",)),
    ("pistachio nut", ("tree_nuts",)),
    ("hazelnut", ("tree_nuts",)),
    ("macadamia", ("tree_nuts",)),
    ("macadamia nut", ("tree_nuts",)),
    ("brazil nut", ("tree_nuts",)),
    ("pine nut", ("tree_nuts",)),
    ("chestnut", ("tree_nuts",)),
    ("water chestnut", ()),
    ("praline", ("tree_nuts",)),
    ("marzipan", ("tree_nuts",)),
    ("pesto", ("dairy", "tree_nuts")),
    ("nutella", ("dairy", "tree_nuts")),
    ("chocolate-hazelnut spread", ("dairy", "tree_nuts")),

    # Shellfish.
    ("shellfish", ("shellfish",)),
    ("shrimp", ("shellfish",)),
    ("prawn", ("shellfish",)),
    ("crab", ("shellfish",)),
    ("crabmeat", ("shellfish",)),
    ("imitation crabmeat", ("fish", "shellfish")),
    ("lobster", ("shellfish",)),
    ("crayfish", ("shellfish",)),
    ("crawfish", ("shellfish",)),
    ("clam", ("shellfish",)),
    ("mussel", ("shellfish",)),
    ("oyster", ("shellfish",)),
    ("oyster mushroom", ()),
    ("scallop", ("shellfish",)),
    ("squid", ("shellfish",)),
    ("calamari", ("shellfish",)),
    ("octopus", ("shellfish",)),

    # Fish.
    ("fish", ("fish",)),
    ("whitefish", ("fish",)),
    ("anchovy", ("fish",)),
    ("salmon", ("fish",)),
    ("tuna", ("fish",)),
    ("cod", ("fish",)),
    ("halibut", ("fish",)),
    ("tilapia", ("fish",)),
    ("trout", ("fish",)),
    ("sardine", ("fish",)),
    ("mackerel", ("fish",)),
    ("haddock", ("fish",)),
    ("snapper", ("fish",)),
    ("bass", ("fish",)),
    ("mahi", ("fish",)),
    ("orange roughy", ("fish",)),
    ("caviar", ("fish",)),
    ("worcestershire", ("fish",)),
    # The parser sometimes drops the first letter of a line.
    ("orcestershire", ("fish",)),

    # Wheat and gluten. Flour is wheat unless the name says otherwise.
    ("wheat", _WHEAT),
    ("flour", _WHEAT),
    ("rice flour", ()),
    ("buckwheat flour", ()),
    ("almond flour", ("tree_nuts",)),
    ("coconut flour", ()),
    ("chickpea flour", ()),
    ("millet flour", ()),
    ("tapioca flour", ()),
    ("oat flour", ()),
    ("soy flour", ("soy",)),
    ("gluten-free all purpose baking flour", ()),
    ("gluten-free all-purpose baking flour", ()),
    ("gluten-free all-purpose flour", ()),
    ("gluten-free flour blend", ()),
    ("semolina", _WHEAT),
    ("farina", _WHEAT),
    ("couscous", _WHEAT),
    ("seitan", _WHEAT),
    ("pasta", _WHEAT),
    ("pasta sauce", ()),
    ("spaghetti", _WHEAT),
    ("spaghetti sauce", ()),
    ("spaghetti squash", ()),
    ("macaroni", _WHEAT),
    ("penne", _WHEAT),
    ("orzo", _WHEAT),
    ("fettuccine", _WHEAT),
    ("linguine", _WHEAT),
    ("rigatoni", _WHEAT),
    ("rotini", _WHEAT),
    ("ziti", _WHEAT),
    ("fusilli", _WHEAT),
    ("farfalle", _WHEAT),
    ("lasagna", _WHEAT),
    ("ravioli", ("eggs", *_WHEAT)),
    ("tortellini", ("eggs", *_WHEAT)),
    ("gnocchi", _WHEAT),
    ("noodle", _WHEAT),
    ("rice noodle", ()),
    ("wonton", _WHEAT),
    ("bread", _WHEAT),
    ("bread machine yeast", ()),
    ("breadcrumb", _WHEAT),
    ("flatbread", _WHEAT),
    ("cornbread", _WHEAT),
    ("sourdough", _WHEAT),
    ("bun", _WHEAT),
    ("roll", _WHEAT),
    ("bagel", _WHEAT),
    ("everything bagel", ("sesame", *_WHEAT)),
    ("everything bagel seasoning", ("sesame",)),
    ("baguette", _WHEAT),
    ("croissant", ("dairy", *_WHEAT)),
    ("muffin", _WHEAT),
    ("biscuit", _WHEAT),
    ("cookie", _WHEAT),
    ("cooky", _WHEAT),
    ("wafer", _WHEAT),
    ("vanilla wafer", ("eggs", *_WHEAT)),
    ("cracker", _WHEAT),
    ("crouton", _WHEAT),
    ("pretzel", _WHEAT),
    ("matzo", _WHEAT),
    ("panko", _WHEAT),
    ("tortilla", _WHEAT),
    ("corn tortilla", ()),
    ("pita", _WHEAT),
    ("phyllo", _WHEAT),
    ("pastry", _WHEAT),
    ("crust", _WHEAT),
    ("dough", _WHEAT),
    ("pie shell", _WHEAT),
    ("cake mix", _WHEAT),
    ("brownie mix", _WHEAT),
    ("pancake mix", _WHEAT),
    ("baking mix", _WHEAT),
    ("bran cereal", _WHEAT),
    ("gravy mix", _WHEAT),
    ("fried onion", _WHEAT),
    ("barley", ("gluten",)),
    ("rye", ("gluten",)),
    ("malt", ("gluten",)),
    # Malt flavouring, which is barley.
    ("corn flake", ("gluten",)),
    ("cornflake", ("gluten",)),
    ("crisp rice cereal", ("gluten",)),
    ("crispy rice cereal", ("gluten",)),
    ("crispy rice cereal square", ()),
    ("beer", ("gluten", "sulfites")),

    # Soy.
    ("soy", ("soy",)),
    ("soybean", ("soy",)),
    ("soy sauce", ("soy", *_WHEAT)),
    ("tamari", ("soy",)),
    ("miso", ("soy",)),
    ("tofu", ("soy",)),
    ("tempeh", ("soy",)),
    ("edamame", ("soy",)),
    ("teriyaki", ("soy", *_WHEAT)),
    ("hoisin", ("soy", "sesame", *_WHEAT)),
    ("gochujang", ("soy", *_WHEAT)),

    # Sesame, mustard, celery.
    ("sesame", ("sesame",)),
    ("tahini", ("sesame",)),
    ("mustard", ("mustard",)),
    ("pickling spice", ("mustard",)),
    ("celery", ("celery",)),
    ("celeriac", ("celery",)),
    ("old bay", ("mustard", "celery")),
    ("seafood seasoning", ("mustard", "celery")),

    # Sulfites.
    ("wine", ("sulfites",)),
    ("vinegar", ("sulfites",)),
    ("sherry", ("sulfites",)),
    ("champagne", ("sulfites",)),
    ("prosecco", ("sulfites",)),
    ("vermouth", ("sulfites",)),
    ("dried apricot", ("sulfites",)),
    ("golden raisin", ("sulfites",)),
    ("maraschino", ("sulfites",)),
]

# Longest first, so a phrase consumes its words before the single-word rules run.
_COMPILED = [
    (re.compile(rf"(?<![a-z]){re.escape(needle)}(?:s|es)?(?![a-z])"), tags)
    for needle, tags in sorted(RULES, key=lambda rule: -len(rule[0]))
]


def tag(canonical_name: str) -> tuple[str, ...]:
    """Propose allergen tags for one canonical ingredient name."""
    text = canonical_name.lower()
    found: set[str] = set()
    for pattern, tags in _COMPILED:
        def consume(match: re.Match[str], tags: tuple[str, ...] = tags) -> str:
            found.update(tags)
            # Blank the match so shorter rules cannot see inside it.
            return " " * len(match.group(0))
        text = pattern.sub(consume, text)
    return tuple(t for t in ALLERGENS if t in found)


def recipes_losing_allergens(records: Iterable[dict], resolvable: Container[str]
                             ) -> tuple[int, collections.Counter[str], collections.Counter[str]]:
    """Count recipes where a line the rules tag would get no catalog id.

    `load` drops such a line, so the recipe is stored without the allergen and the
    SQL deny-list cannot see it. Returns the number of such recipes, the same
    count per tag, and the dropped names with their occurrences.
    """
    tags_of: dict[str, tuple[str, ...]] = {}
    recipes = 0
    by_tag: collections.Counter[str] = collections.Counter()
    names: collections.Counter[str] = collections.Counter()
    for record in records:
        lost: set[str] = set()
        for line in record["lines"]:
            name = line["name"]
            if name in resolvable:
                continue
            if name not in tags_of:
                tags_of[name] = tag(name)
            if tags_of[name]:
                lost.update(tags_of[name])
                names[name] += 1
        if lost:
            recipes += 1
            by_tag.update(lost)
    return recipes, by_tag, names


def synonyms_losing_allergens(synonyms: Mapping[str, str],
                              target_tags: Mapping[str, Iterable[str]] | None = None
                              ) -> dict[str, str]:
    """Return the synonyms whose name the rules tag with more than their target has.

    A recipe line carrying a synonym is stored under the target's id, so the
    deny-list sees only the target's tags: "sardine in oil" mapped onto "oil"
    reaches a user allergic to fish. Target tags come from `target_tags` when
    given (the reviewed file at `load`), otherwise from the rules.
    """
    lost: dict[str, str] = {}
    for name, target in synonyms.items():
        have = set(target_tags[target]) if target_tags is not None and target in target_tags \
            else set(tag(target))
        if not set(tag(name)) <= have:
            lost[name] = target
    return lost


REVIEW_COLUMNS = ["canonical_name", "allergens", "occurrences", "confirmed"]


class ReviewError(Exception):
    """The review file cannot be trusted as a source of allergen tags."""


def read_review(path: Path, catalog_names: Iterable[str]) -> dict[str, tuple[str, ...]]:
    """Return reviewed tags for every catalog name, or raise ReviewError.

    A row counts only when `confirmed` is `yes`: that is the reviewer's word that
    the `allergens` cell is right, including an empty cell meaning "no allergens".
    Any catalog name without such a row stops the load, because an unreviewed name
    is exactly where a missed tag hides.
    """
    if not path.exists():
        raise ReviewError(f"{path} missing; run `allergens` and review it before `load`")

    reviewed: dict[str, tuple[str, ...]] = {}
    unknown: list[str] = []
    with path.open(newline="", encoding="utf-8") as handle:
        for row in csv.DictReader(handle):
            name = (row.get("canonical_name") or "").strip().lower()
            if not name or (row.get("confirmed") or "").strip().lower() != "yes":
                continue
            tags = tuple((row.get("allergens") or "").split())
            bad = [t for t in tags if t not in ALLERGENS]
            if bad:
                # A misspelt tag would filter nothing, which is the dangerous way to fail.
                unknown.append(f"{name}: {' '.join(bad)}")
                continue
            reviewed[name] = tags

    problems: list[str] = []
    if unknown:
        problems.append(f"{len(unknown)} rows have unknown tags, e.g. {unknown[:5]}; "
                        f"allowed: {' '.join(ALLERGENS)}")
    pending = [name for name in catalog_names if name not in reviewed]
    if pending:
        problems.append(f"{len(pending)} catalog entries are not marked confirmed=yes, "
                        f"e.g. {pending[:5]}")
    if problems:
        raise ReviewError(f"{path} is not fully reviewed: " + "; ".join(problems))
    return reviewed
