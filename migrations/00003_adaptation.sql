-- +goose Up
CREATE TABLE adapted_recipes (
    id             uuid PRIMARY KEY,
    user_id        uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    base_recipe_id bigint NOT NULL REFERENCES recipes (id),
    result         jsonb NOT NULL,
    model          text,
    adapted        boolean NOT NULL DEFAULT true,
    rating         smallint CHECK (rating BETWEEN 1 AND 5),
    created_at     timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX adapted_recipes_user_created_idx ON adapted_recipes (user_id, created_at DESC);

-- One row per attempt at calling llm-service, including rejected ones, so that
-- validator rejection rate is measurable.
CREATE TABLE generation_runs (
    id             uuid PRIMARY KEY,
    user_id        uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    job_id         uuid NOT NULL,
    model          text,
    prompt_version text,
    latency_ms     int,
    prompt_tokens  int,
    output_tokens  int,
    retries        int NOT NULL DEFAULT 0,
    status         text NOT NULL,
    error          text,
    created_at     timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX generation_runs_job_id_idx ON generation_runs (job_id);
CREATE INDEX generation_runs_user_created_idx ON generation_runs (user_id, created_at DESC);

-- +goose Down
DROP TABLE generation_runs;
DROP TABLE adapted_recipes;
