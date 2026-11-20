-- +goose Up
-- A copy of each recipe's recipe_ingredients ids, so set coverage reads one array
-- per recipe instead of aggregating half a million link rows (TASK-39). Only the
-- links are copied: allergen tags stay in ingredient_allergens and are read on
-- every search, so a tag added later applies at once with nothing to recompute.
--
-- intarray (a contrib extension shipped with Postgres) intersects arrays in C;
-- counting the missing ingredients with unnest instead cost a subquery per recipe
-- and kept p95 above 150 ms. It works on int4 only, so the column is int[] while
-- catalog ids are bigint. The casts below raise "integer out of range" for an id
-- that does not fit, so such an id fails the migration or the link insert instead
-- of being dropped from the array.
CREATE EXTENSION IF NOT EXISTS intarray;

ALTER TABLE recipes ADD COLUMN ingredient_ids int[] NOT NULL DEFAULT '{}';

UPDATE recipes r
SET ingredient_ids = l.ids
FROM (SELECT recipe_id, array_agg(ingredient_id::int ORDER BY ingredient_id) AS ids
      FROM recipe_ingredients GROUP BY recipe_id) l
WHERE l.recipe_id = r.id;

CREATE INDEX recipes_ingredient_ids_idx ON recipes USING gin (ingredient_ids gin__int_ops);

-- The database keeps the array in step rather than each writer, so ingest, test
-- fixtures and any later cleanup cannot leave it stale. The triggers are
-- statement-level and recompute only the recipes the statement touched.
-- plpgsql plans a query only when it runs, so each branch names just the
-- transition tables its own trigger declares.
-- +goose StatementBegin
CREATE FUNCTION refresh_recipe_ingredient_ids() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    touched bigint[];
BEGIN
    IF TG_OP = 'TRUNCATE' THEN
        UPDATE recipes SET ingredient_ids = '{}' WHERE ingredient_ids <> '{}';
        RETURN NULL;
    ELSIF TG_OP = 'INSERT' THEN
        SELECT array_agg(DISTINCT recipe_id) INTO touched FROM new_rows;
    ELSIF TG_OP = 'DELETE' THEN
        SELECT array_agg(DISTINCT recipe_id) INTO touched FROM old_rows;
    ELSE
        SELECT array_agg(DISTINCT recipe_id) INTO touched
        FROM (SELECT recipe_id FROM old_rows UNION SELECT recipe_id FROM new_rows) u;
    END IF;

    UPDATE recipes r
    SET ingredient_ids = coalesce(
        (SELECT array_agg(ri.ingredient_id::int ORDER BY ri.ingredient_id)
         FROM recipe_ingredients ri WHERE ri.recipe_id = r.id),
        '{}')
    WHERE r.id = ANY(touched);
    RETURN NULL;
END
$$;
-- +goose StatementEnd

-- A trigger with transition tables may have only one event, hence one per event.
CREATE TRIGGER recipe_ingredients_ids_insert
    AFTER INSERT ON recipe_ingredients REFERENCING NEW TABLE AS new_rows
    FOR EACH STATEMENT EXECUTE FUNCTION refresh_recipe_ingredient_ids();
CREATE TRIGGER recipe_ingredients_ids_update
    AFTER UPDATE ON recipe_ingredients REFERENCING OLD TABLE AS old_rows NEW TABLE AS new_rows
    FOR EACH STATEMENT EXECUTE FUNCTION refresh_recipe_ingredient_ids();
CREATE TRIGGER recipe_ingredients_ids_delete
    AFTER DELETE ON recipe_ingredients REFERENCING OLD TABLE AS old_rows
    FOR EACH STATEMENT EXECUTE FUNCTION refresh_recipe_ingredient_ids();
CREATE TRIGGER recipe_ingredients_ids_truncate
    AFTER TRUNCATE ON recipe_ingredients
    FOR EACH STATEMENT EXECUTE FUNCTION refresh_recipe_ingredient_ids();

-- +goose Down
DROP TRIGGER recipe_ingredients_ids_truncate ON recipe_ingredients;
DROP TRIGGER recipe_ingredients_ids_delete ON recipe_ingredients;
DROP TRIGGER recipe_ingredients_ids_update ON recipe_ingredients;
DROP TRIGGER recipe_ingredients_ids_insert ON recipe_ingredients;
DROP FUNCTION refresh_recipe_ingredient_ids();
DROP INDEX recipes_ingredient_ids_idx;
ALTER TABLE recipes DROP COLUMN ingredient_ids;
DROP EXTENSION intarray;
