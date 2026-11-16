-- +goose Up
-- A minimal staples set so that set-coverage behaves sanely before ingest runs.
-- The ingest tool extends both the catalog and this list.
INSERT INTO ingredients_catalog (canonical_name, category)
VALUES
    ('water', 'staple'),
    ('salt', 'staple'),
    ('black pepper', 'staple'),
    ('sugar', 'staple'),
    ('vegetable oil', 'staple'),
    ('olive oil', 'staple'),
    ('all-purpose flour', 'staple'),
    ('baking powder', 'staple'),
    ('baking soda', 'staple'),
    ('white vinegar', 'staple'),
    ('butter', 'dairy')
ON CONFLICT DO NOTHING;

INSERT INTO pantry_staples (ingredient_id)
SELECT id FROM ingredients_catalog WHERE category = 'staple'
ON CONFLICT DO NOTHING;

INSERT INTO ingredient_allergens (ingredient_id, allergen)
SELECT id, 'dairy' FROM ingredients_catalog WHERE lower(canonical_name) = 'butter'
ON CONFLICT DO NOTHING;

-- +goose Down
-- Only rows nothing links to go. After ingest the corpus and fridges point at these
-- ids without ON DELETE CASCADE, so a blanket delete would fail; and a row that stays
-- keeps its staple mark and its allergen tag, because a used "butter" without "dairy"
-- would slip past an allergic user's deny-list. Rows are matched by the names inserted
-- above, not by category, so nothing ingest added is touched.
DELETE FROM ingredients_catalog c
WHERE lower(c.canonical_name) IN (
        'water', 'salt', 'black pepper', 'sugar', 'vegetable oil', 'olive oil',
        'all-purpose flour', 'baking powder', 'baking soda', 'white vinegar', 'butter')
  AND NOT EXISTS (SELECT 1 FROM recipe_ingredients r WHERE r.ingredient_id = c.id)
  AND NOT EXISTS (SELECT 1 FROM user_ingredients u WHERE u.ingredient_id = c.id);
