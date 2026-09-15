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
DELETE FROM ingredients_catalog WHERE category IN ('staple', 'dairy');
