"""Allergen tagging of canonical ingredient names.

Keyword rules produce a *proposal*, not a verdict. The plan calls for manual
confirmation, and the reason is asymmetric risk: a missed tag can hurt someone,
while a spurious tag only narrows results. So `allergens` writes a review file and
the tags are only loaded from that file, which a human is expected to have looked
at.

Tag vocabulary must stay in sync with `user.KnownAllergens` in the Go code.
"""

from __future__ import annotations

# Substring rules. Matched against the canonical name with word boundaries in
# mind, longest rule first, so "coconut milk" is not tagged dairy by "milk".
RULES: list[tuple[str, tuple[str, ...]]] = [
    # Explicitly not dairy / not nuts, despite the names. These come first and
    # win, which is why the loop below stops at the first exempting match.
    ("coconut milk", ()),
    ("coconut cream", ()),
    ("coconut water", ()),
    ("almond milk", ("tree_nuts",)),
    ("soy milk", ("soy",)),
    ("oat milk", ()),
    ("rice milk", ()),
    ("peanut butter", ("peanuts",)),
    ("cocoa butter", ()),
    ("apple butter", ()),
    ("nutmeg", ()),
    ("water chestnut", ()),
    ("buttercup squash", ()),
    ("butternut squash", ()),
    ("butter lettuce", ()),
    ("butter bean", ()),
    ("buttermilk", ("dairy",)),

    ("milk", ("dairy",)),
    ("cream", ("dairy",)),
    ("cheese", ("dairy",)),
    ("butter", ("dairy",)),
    ("yogurt", ("dairy",)),
    ("yoghurt", ("dairy",)),
    ("ghee", ("dairy",)),
    ("custard", ("dairy", "eggs")),
    ("ricotta", ("dairy",)),
    ("mascarpone", ("dairy",)),
    ("mozzarella", ("dairy",)),
    ("parmesan", ("dairy",)),
    ("cheddar", ("dairy",)),
    ("provolone", ("dairy",)),
    ("gouda", ("dairy",)),
    ("brie", ("dairy",)),
    ("feta", ("dairy",)),
    ("half-and-half", ("dairy",)),
    ("whey", ("dairy",)),
    ("casein", ("dairy",)),

    ("egg", ("eggs",)),
    ("mayonnaise", ("eggs",)),
    ("meringue", ("eggs",)),
    ("aioli", ("eggs",)),

    ("peanut", ("peanuts",)),
    ("almond", ("tree_nuts",)),
    ("walnut", ("tree_nuts",)),
    ("pecan", ("tree_nuts",)),
    ("cashew", ("tree_nuts",)),
    ("pistachio", ("tree_nuts",)),
    ("hazelnut", ("tree_nuts",)),
    ("macadamia", ("tree_nuts",)),
    ("brazil nut", ("tree_nuts",)),
    ("pine nut", ("tree_nuts",)),
    ("praline", ("tree_nuts",)),
    ("marzipan", ("tree_nuts",)),
    ("nutella", ("tree_nuts", "dairy")),

    ("shrimp", ("shellfish",)),
    ("prawn", ("shellfish",)),
    ("crab", ("shellfish",)),
    ("lobster", ("shellfish",)),
    ("crayfish", ("shellfish",)),
    ("clam", ("shellfish",)),
    ("mussel", ("shellfish",)),
    ("oyster sauce", ("shellfish",)),
    ("oyster", ("shellfish",)),
    ("scallop", ("shellfish",)),
    ("squid", ("shellfish",)),
    ("calamari", ("shellfish",)),
    ("octopus", ("shellfish",)),

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
    ("fish sauce", ("fish",)),
    ("worcestershire", ("fish",)),
    ("caviar", ("fish",)),

    ("wheat", ("wheat", "gluten")),
    ("all-purpose flour", ("wheat", "gluten")),
    ("bread flour", ("wheat", "gluten")),
    ("cake flour", ("wheat", "gluten")),
    ("pastry flour", ("wheat", "gluten")),
    ("self-rising flour", ("wheat", "gluten")),
    ("semolina", ("wheat", "gluten")),
    ("farina", ("wheat", "gluten")),
    ("couscous", ("wheat", "gluten")),
    ("orzo", ("wheat", "gluten")),
    ("spaghetti", ("wheat", "gluten")),
    ("macaroni", ("wheat", "gluten")),
    ("penne", ("wheat", "gluten")),
    ("lasagna noodle", ("wheat", "gluten")),
    ("pasta", ("wheat", "gluten")),
    ("bread", ("wheat", "gluten")),
    ("breadcrumb", ("wheat", "gluten")),
    ("cracker", ("wheat", "gluten")),
    ("tortilla", ("wheat", "gluten")),
    ("pita", ("wheat", "gluten")),
    ("phyllo", ("wheat", "gluten")),
    ("puff pastry", ("wheat", "gluten")),
    ("pie crust", ("wheat", "gluten")),
    ("barley", ("gluten",)),
    ("rye", ("gluten",)),
    ("malt", ("gluten",)),
    ("seitan", ("wheat", "gluten")),
    ("beer", ("gluten", "sulfites")),

    ("soy sauce", ("soy", "wheat", "gluten")),
    ("tamari", ("soy",)),
    ("miso", ("soy",)),
    ("tofu", ("soy",)),
    ("tempeh", ("soy",)),
    ("edamame", ("soy",)),
    ("soybean", ("soy",)),
    ("soy", ("soy",)),

    ("sesame", ("sesame",)),
    ("tahini", ("sesame",)),

    ("mustard", ("mustard",)),
    ("celery", ("celery",)),
    ("celeriac", ("celery",)),

    ("wine", ("sulfites",)),
    ("vinegar", ("sulfites",)),
    ("dried apricot", ("sulfites",)),
]


def tag(canonical_name: str) -> tuple[str, ...]:
    """Propose allergen tags for one canonical ingredient name."""
    name = canonical_name.lower()
    for needle, tags in RULES:
        if needle in name:
            # The first match wins, which is how the exemptions above work:
            # "coconut milk" is checked before the generic "milk" rule.
            return tags
    return ()
