-- +goose Up
-- Ingredient catalog is the canonical vocabulary: every recipe line and every
-- fridge item resolves to one of these ids. Retrieval and allergen checks work
-- on ids only, never on raw text.
CREATE TABLE ingredients_catalog (
    id           bigserial PRIMARY KEY,
    canonical_name text NOT NULL,
    category     text,
    created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX ingredients_catalog_canonical_name_key
    ON ingredients_catalog (lower(canonical_name));

CREATE TABLE ingredient_synonyms (
    ingredient_id bigint NOT NULL REFERENCES ingredients_catalog (id) ON DELETE CASCADE,
    synonym       text NOT NULL,
    PRIMARY KEY (ingredient_id, synonym)
);
CREATE UNIQUE INDEX ingredient_synonyms_synonym_key ON ingredient_synonyms (lower(synonym));

CREATE TABLE ingredient_allergens (
    ingredient_id bigint NOT NULL REFERENCES ingredients_catalog (id) ON DELETE CASCADE,
    allergen      text NOT NULL,
    PRIMARY KEY (ingredient_id, allergen)
);
CREATE INDEX ingredient_allergens_ingredient_id_idx ON ingredient_allergens (ingredient_id);
CREATE INDEX ingredient_allergens_allergen_idx ON ingredient_allergens (allergen);

-- Staples are assumed present in every kitchen (salt, water, pepper) so they
-- never count as missing during set coverage.
CREATE TABLE pantry_staples (
    ingredient_id bigint PRIMARY KEY REFERENCES ingredients_catalog (id) ON DELETE CASCADE
);

-- The corpus. `steps` holds the source directions: an internal reference only,
-- stored for retrieval and for sending to llm-service, never served verbatim
-- over the API (see the licensing risk in the root plan).
CREATE TABLE recipes (
    id               bigserial PRIMARY KEY,
    external_id      text UNIQUE,
    title            text NOT NULL,
    category         text,
    subcategory      text,
    description      text,
    steps            jsonb NOT NULL DEFAULT '[]'::jsonb,
    num_ingredients  int NOT NULL DEFAULT 0,
    num_steps        int NOT NULL DEFAULT 0,
    source           text,
    created_at       timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX recipes_category_idx ON recipes (category);
CREATE INDEX recipes_complexity_idx ON recipes (num_steps, num_ingredients);

CREATE TABLE recipe_ingredients (
    recipe_id     bigint NOT NULL REFERENCES recipes (id) ON DELETE CASCADE,
    ingredient_id bigint NOT NULL REFERENCES ingredients_catalog (id),
    raw_text      text NOT NULL,
    qty           numeric,
    unit          text,
    PRIMARY KEY (recipe_id, ingredient_id)
);
CREATE INDEX recipe_ingredients_ingredient_id_idx ON recipe_ingredients (ingredient_id);

-- +goose Down
DROP TABLE recipe_ingredients;
DROP TABLE recipes;
DROP TABLE pantry_staples;
DROP TABLE ingredient_allergens;
DROP TABLE ingredient_synonyms;
DROP TABLE ingredients_catalog;
