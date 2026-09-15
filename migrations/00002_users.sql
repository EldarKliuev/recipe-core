-- +goose Up
CREATE TABLE users (
    id            uuid PRIMARY KEY,
    email         text NOT NULL,
    password_hash text NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX users_email_key ON users (lower(email));

CREATE TABLE user_prefs (
    user_id                uuid PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    allergens              text[] NOT NULL DEFAULT '{}',
    diets                  text[] NOT NULL DEFAULT '{}',
    disliked_ingredient_ids bigint[] NOT NULL DEFAULT '{}',
    cuisines               text[] NOT NULL DEFAULT '{}',
    updated_at             timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE user_ingredients (
    user_id       uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    ingredient_id bigint NOT NULL REFERENCES ingredients_catalog (id),
    qty           numeric,
    unit          text,
    expires_at    date,
    created_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, ingredient_id)
);
CREATE INDEX user_ingredients_expires_at_idx ON user_ingredients (user_id, expires_at)
    WHERE expires_at IS NOT NULL;

-- Refresh tokens are stored hashed so a database leak cannot be replayed.
CREATE TABLE refresh_tokens (
    token_hash bytea PRIMARY KEY,
    user_id    uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    expires_at timestamptz NOT NULL,
    revoked_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX refresh_tokens_user_id_idx ON refresh_tokens (user_id);

-- +goose Down
DROP TABLE refresh_tokens;
DROP TABLE user_ingredients;
DROP TABLE user_prefs;
DROP TABLE users;
