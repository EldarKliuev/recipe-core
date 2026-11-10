"""Tests for allergen proposals and the review file that gates `load`.

Run from tools/ingest: python3 -m unittest discover -s tests
"""

from __future__ import annotations

import csv
import re
import tempfile
import unittest
from pathlib import Path

from ingest import allergens

GO_ALLERGENS = Path(__file__).resolve().parents[3] / "internal" / "user" / "user.go"


class TagTest(unittest.TestCase):
    def assertTags(self, name: str, *expected: str) -> None:
        self.assertEqual(set(allergens.tag(name)), set(expected), name)

    def test_plain_flour_is_wheat(self) -> None:
        for name in ("flour", "all purpose flour", "all-purpose flour",
                     "baking spray with flour", "spelt flour"):
            self.assertTags(name, "wheat", "gluten")

    def test_gluten_free_flours_are_not_wheat(self) -> None:
        for name in ("rice flour", "white rice flour", "buckwheat flour", "coconut flour",
                     "gluten-free all-purpose flour", "gluten-free flour blend"):
            self.assertTags(name)
        self.assertTags("almond flour", "tree_nuts")

    def test_every_matching_rule_contributes(self) -> None:
        self.assertTags("egg noodles", "eggs", "wheat", "gluten")
        self.assertTags("cheese ravioli", "dairy", "eggs", "wheat", "gluten")
        self.assertTags("egg roll wrapper", "eggs", "wheat", "gluten")

    def test_words_are_not_matched_inside_other_words(self) -> None:
        self.assertTags("eggplant")
        self.assertTags("fryer chicken")
        self.assertTags("pepita")
        self.assertTags("tamarind paste")
        self.assertTags("cheesecloth")
        self.assertTags("nutmeg")
        self.assertTags("coconut")

    def test_longer_phrase_consumes_its_words(self) -> None:
        self.assertTags("coconut milk")
        self.assertTags("cream of tartar")
        self.assertTags("peanut butter", "peanuts")
        self.assertTags("almond butter", "tree_nuts")
        self.assertTags("oyster mushroom")
        self.assertTags("spaghetti squash")
        self.assertTags("corn tortilla chip")
        self.assertTags("pine nut", "tree_nuts")

    def test_exemption_keeps_the_allergens_the_phrase_still_has(self) -> None:
        self.assertTags("condensed cream of celery soup", "dairy", "wheat", "gluten", "celery")
        self.assertTags("everything bagel seasoning", "sesame")

    def test_plurals_match(self) -> None:
        self.assertTags("eggs", "eggs")
        self.assertTags("rice noodles")
        self.assertTags("breadcrumbs", "wheat", "gluten")

    def test_compound_words_with_an_allergen_have_rules(self) -> None:
        self.assertTags("eggnog", "dairy", "eggs")
        self.assertTags("cornbread mix", "wheat", "gluten")
        self.assertTags("flatbread", "wheat", "gluten")
        self.assertTags("crabmeat", "shellfish")
        self.assertTags("crawfish tail", "shellfish")
        self.assertTags("buttermilk", "dairy")

    def test_names_the_old_rules_missed(self) -> None:
        self.assertTags("hamburger bun", "wheat", "gluten")
        self.assertTags("yellow cake mix", "wheat", "gluten")
        self.assertTags("pizza dough", "wheat", "gluten")
        self.assertTags("pesto", "dairy", "tree_nuts")
        self.assertTags("pie shell", "wheat", "gluten")
        self.assertTags("refrigerated crescent roll", "wheat", "gluten")
        self.assertTags("nut", "tree_nuts", "peanuts")
        self.assertTags("panko", "wheat", "gluten")
        self.assertTags("gnocchi", "wheat", "gluten")
        self.assertTags("alfredo sauce", "dairy")
        self.assertTags("half and half", "dairy")
        self.assertTags("fish stock", "fish")

    def test_tags_are_in_vocabulary_order(self) -> None:
        self.assertEqual(allergens.tag("soy sauce"), ("wheat", "gluten", "soy"))

    def test_rules_use_only_known_tags(self) -> None:
        for needle, tags in allergens.RULES:
            for t in tags:
                self.assertIn(t, allergens.ALLERGENS, needle)

    @unittest.skipUnless(GO_ALLERGENS.exists(), "Go sources are not next to this copy of ingest")
    def test_vocabulary_matches_the_go_service(self) -> None:
        source = GO_ALLERGENS.read_text(encoding="utf-8")
        block = re.search(r"var KnownAllergens = \[\]string\{(.*?)\}", source, re.S)
        self.assertIsNotNone(block)
        self.assertEqual(tuple(re.findall(r'"([a-z_]+)"', block.group(1))), allergens.ALLERGENS)


class ReadReviewTest(unittest.TestCase):
    def setUp(self) -> None:
        self.dir = tempfile.TemporaryDirectory()
        self.path = Path(self.dir.name) / "allergens_review.csv"

    def tearDown(self) -> None:
        self.dir.cleanup()

    def write(self, *rows: tuple[str, str, str]) -> None:
        with self.path.open("w", newline="", encoding="utf-8") as handle:
            writer = csv.writer(handle)
            writer.writerow(allergens.REVIEW_COLUMNS)
            for name, tags, confirmed in rows:
                writer.writerow([name, tags, 1, confirmed])

    def test_returns_reviewed_tags_including_empty_ones(self) -> None:
        self.write(("flour", "wheat gluten", "yes"), ("salt", "", "YES"))
        self.assertEqual(allergens.read_review(self.path, ["flour", "salt"]),
                         {"flour": ("wheat", "gluten"), "salt": ()})

    def test_reviewer_corrections_win_over_the_rules(self) -> None:
        self.write(("eggplant", "", "yes"), ("mystery sauce", "soy", "yes"))
        self.assertEqual(allergens.read_review(self.path, ["eggplant", "mystery sauce"]),
                         {"eggplant": (), "mystery sauce": ("soy",)})

    def test_missing_file_is_refused(self) -> None:
        with self.assertRaisesRegex(allergens.ReviewError, "missing"):
            allergens.read_review(self.path, ["flour"])

    def test_unconfirmed_row_is_refused(self) -> None:
        self.write(("flour", "wheat gluten", "yes"), ("salt", "", ""))
        with self.assertRaisesRegex(allergens.ReviewError, r"1 catalog entries.*salt"):
            allergens.read_review(self.path, ["flour", "salt"])

    def test_no_is_not_a_confirmation(self) -> None:
        self.write(("flour", "wheat gluten", "no"))
        with self.assertRaises(allergens.ReviewError):
            allergens.read_review(self.path, ["flour"])

    def test_catalog_name_absent_from_the_file_is_refused(self) -> None:
        # The catalog was rebuilt after the review: the new name was never looked at.
        self.write(("flour", "wheat gluten", "yes"))
        with self.assertRaisesRegex(allergens.ReviewError, "crab"):
            allergens.read_review(self.path, ["flour", "crab"])

    def test_unknown_tag_is_refused(self) -> None:
        self.write(("milk", "diary", "yes"))
        with self.assertRaisesRegex(allergens.ReviewError, "unknown tags.*diary"):
            allergens.read_review(self.path, ["milk"])


if __name__ == "__main__":
    unittest.main()
