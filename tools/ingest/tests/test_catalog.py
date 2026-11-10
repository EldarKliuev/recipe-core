"""Tests for the head-noun synonyms `catalog` builds.

Run from tools/ingest: python3 -m unittest discover -s tests
"""

from __future__ import annotations

import unittest

from ingest.__main__ import _match_head_noun


class MatchHeadNounTest(unittest.TestCase):
    def test_maps_a_rare_name_onto_its_head_noun(self) -> None:
        self.assertEqual(_match_head_noun("granny smith apple", {"apple"}), "apple")

    def test_longest_tail_wins(self) -> None:
        self.assertEqual(_match_head_noun("granny smith apple", {"apple", "smith apple"}),
                         "smith apple")

    def test_tail_without_the_name_allergen_is_not_a_synonym(self) -> None:
        # The line would be stored as "oil", and the fish deny-list would not see it.
        self.assertIsNone(_match_head_noun("sardine in oil", {"oil"}))
        self.assertIsNone(_match_head_noun("tuna in olive oil", {"olive oil", "oil"}))
        self.assertIsNone(_match_head_noun("ounce can hunk white albacore tuna in water", {"water"}))

    def test_tail_missing_one_of_several_allergens_is_not_a_synonym(self) -> None:
        # Crouton carries wheat and gluten, but not the cheese's dairy.
        self.assertIsNone(_match_head_noun("cheese and garlic crouton", {"crouton"}))

    def test_tagged_name_maps_onto_a_tail_that_keeps_its_allergens(self) -> None:
        self.assertEqual(
            _match_head_noun("sugar-free peanut butter syrup", {"syrup", "peanut butter syrup"}),
            "peanut butter syrup")
        self.assertEqual(
            _match_head_noun("whole-wheat pastry flour", {"pastry flour"}), "pastry flour")

    def test_tail_with_the_same_allergens_is_a_synonym(self) -> None:
        self.assertEqual(_match_head_noun("salted butter", {"butter"}), "butter")

    def test_tail_with_extra_allergens_is_allowed(self) -> None:
        # "coconut milk" carries no dairy, "milk" does. An extra tag only narrows
        # results; a missing one is what hurts.
        self.assertEqual(_match_head_noun("light coconut milk", {"milk"}), "milk")


if __name__ == "__main__":
    unittest.main()
