-- +goose Up
-- The ingest keyword rules leave these common names untagged, so an allergic user's
-- deny-list passes them. Rows are created here when missing because migrations run
-- before ingest, which keeps existing rows and only ever adds tags, so these survive
-- a load.
--
-- "nut" is what the corpus calls an unnamed "chopped nuts", which may be peanuts, so
-- it carries both tags: a missed tag can hurt someone, a spare one only narrows results.
INSERT INTO ingredients_catalog (canonical_name)
VALUES ('nut'), ('pastry'), ('all purpose flour')
ON CONFLICT DO NOTHING;

INSERT INTO ingredient_allergens (ingredient_id, allergen)
SELECT c.id, tag.allergen
FROM ingredients_catalog c
JOIN (VALUES
    ('nut', 'tree_nuts'),
    ('nut', 'peanuts'),
    ('pastry', 'wheat'),
    ('pastry', 'gluten'),
    ('all purpose flour', 'wheat'),
    ('all purpose flour', 'gluten')
) AS tag(name, allergen) ON lower(c.canonical_name) = tag.name
ON CONFLICT DO NOTHING;

-- +goose Down
-- Only the tags go: by now recipes may link to the catalog rows.
DELETE FROM ingredient_allergens a
USING ingredients_catalog c
WHERE a.ingredient_id = c.id
  AND lower(c.canonical_name) IN ('nut', 'pastry', 'all purpose flour')
  AND a.allergen IN ('tree_nuts', 'peanuts', 'wheat', 'gluten');
