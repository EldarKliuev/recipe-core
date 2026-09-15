-- +goose Up
-- Autocomplete matches on prefix first and falls back to trigram similarity, so
-- typos in fridge input still resolve to a catalog entry.
CREATE EXTENSION IF NOT EXISTS pg_trgm;

CREATE INDEX ingredients_catalog_name_trgm_idx
    ON ingredients_catalog USING gin (lower(canonical_name) gin_trgm_ops);
CREATE INDEX ingredient_synonyms_synonym_trgm_idx
    ON ingredient_synonyms USING gin (lower(synonym) gin_trgm_ops);

-- +goose Down
DROP INDEX ingredient_synonyms_synonym_trgm_idx;
DROP INDEX ingredients_catalog_name_trgm_idx;
