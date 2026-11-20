-- +goose Up
-- Catalog ids named in a recipe's directions, written by ingest (TASK-40). Part of
-- the corpus lists fewer ingredients than the steps use: the source kept only the
-- first ingredient group, so "Red Velvet Brownies" lists one line of butter while
-- the steps add flour, cocoa and eggs. The allergen deny-list reads this array as
-- well as ingredient_ids, so an allergen named only in the steps still excludes
-- the recipe. Coverage and missing counts ignore it: a step mention is not a line
-- with a quantity, and "serve with bread" should not make a recipe harder to cook.
--
-- Only ids are stored, never tags: tags stay in ingredient_allergens and are read
-- on every search, as for ingredient_ids (migration 00007). The array is int4 for
-- intarray's `&&`, and kept sorted for the same reason as ingredient_ids.
ALTER TABLE recipes ADD COLUMN step_ingredient_ids int[] NOT NULL DEFAULT '{}';

-- +goose Down
ALTER TABLE recipes DROP COLUMN step_ingredient_ids;
