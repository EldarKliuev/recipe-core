"""Postgres writes for the ingest pipeline.

The schema is owned by core-api's goose migrations; nothing here creates or alters
tables. Loading is idempotent per canonical name and per external recipe id, so a
re-run repairs a partial load instead of duplicating it.
"""

from __future__ import annotations

import json
import os
from collections.abc import Iterable, Iterator

import psycopg
from psycopg.rows import tuple_row


def connect() -> psycopg.Connection:
    dsn = os.environ.get("DATABASE_URL")
    if not dsn:
        raise SystemExit("DATABASE_URL is required")
    # psycopg understands the same URL shape as pgx.
    return psycopg.connect(dsn, row_factory=tuple_row, autocommit=False)


def upsert_catalog(conn: psycopg.Connection, names: Iterable[tuple[str, str | None]]) -> dict[str, int]:
    """Insert canonical ingredients and return name -> id for all of them."""
    rows = list(names)
    with conn.cursor() as cur:
        cur.executemany(
            """INSERT INTO ingredients_catalog (canonical_name, category)
               VALUES (%s, %s)
               ON CONFLICT (lower(canonical_name)) DO UPDATE
                   SET category = COALESCE(excluded.category, ingredients_catalog.category)""",
            rows,
        )
        cur.execute("SELECT lower(canonical_name), id FROM ingredients_catalog")
        return {name: ident for name, ident in cur.fetchall()}


def upsert_synonyms(conn: psycopg.Connection, pairs: Iterable[tuple[int, str]]) -> int:
    rows = list(pairs)
    if not rows:
        return 0
    with conn.cursor() as cur:
        # A synonym may already point at a different canonical id from an earlier
        # run with different frequency thresholds; keep the first mapping.
        cur.executemany(
            """INSERT INTO ingredient_synonyms (ingredient_id, synonym)
               VALUES (%s, %s) ON CONFLICT DO NOTHING""",
            rows,
        )
    return len(rows)


def upsert_allergens(conn: psycopg.Connection, pairs: Iterable[tuple[int, str]]) -> int:
    rows = list(pairs)
    if not rows:
        return 0
    with conn.cursor() as cur:
        cur.executemany(
            """INSERT INTO ingredient_allergens (ingredient_id, allergen)
               VALUES (%s, %s) ON CONFLICT DO NOTHING""",
            rows,
        )
    return len(rows)


def upsert_staples(conn: psycopg.Connection, ingredient_ids: Iterable[int]) -> int:
    rows = [(i,) for i in ingredient_ids]
    if not rows:
        return 0
    with conn.cursor() as cur:
        cur.executemany(
            "INSERT INTO pantry_staples (ingredient_id) VALUES (%s) ON CONFLICT DO NOTHING",
            rows,
        )
    return len(rows)


def load_recipes(conn: psycopg.Connection, recipes: Iterator[dict], name_to_id: dict[str, int],
                 batch_size: int = 1000) -> tuple[int, int]:
    """Insert recipes and their ingredient links. Returns (recipes, links)."""
    recipe_count = 0
    link_count = 0
    batch: list[dict] = []

    def flush(rows: list[dict]) -> tuple[int, int]:
        if not rows:
            return 0, 0
        with conn.cursor() as cur:
            cur.executemany(
                """INSERT INTO recipes
                       (external_id, title, category, subcategory, description,
                        steps, num_ingredients, num_steps, source)
                   VALUES (%s, %s, %s, %s, %s, %s, %s, %s, %s)
                   ON CONFLICT (external_id) DO UPDATE SET title = excluded.title
                   RETURNING id, external_id""",
                [
                    (
                        r["external_id"], r["title"], r["category"], r["subcategory"],
                        r["description"], json.dumps(r["directions"]),
                        r["num_ingredients"], r["num_steps"], r["source"],
                    )
                    for r in rows
                ],
                returning=True,
            )
            ids: dict[str, int] = {}
            while True:
                record = cur.fetchone()
                if record is not None:
                    ids[record[1]] = record[0]
                if not cur.nextset():
                    break

            links: list[tuple] = []
            for r in rows:
                recipe_id = ids.get(r["external_id"])
                if recipe_id is None:
                    continue
                seen: set[int] = set()
                for line in r["lines"]:
                    ingredient_id = name_to_id.get(line["name"])
                    if ingredient_id is None or ingredient_id in seen:
                        continue
                    seen.add(ingredient_id)
                    links.append((recipe_id, ingredient_id, line["raw"], line["qty"], line["unit"]))

            if links:
                cur.executemany(
                    """INSERT INTO recipe_ingredients
                           (recipe_id, ingredient_id, raw_text, qty, unit)
                       VALUES (%s, %s, %s, %s, %s)
                       ON CONFLICT (recipe_id, ingredient_id) DO NOTHING""",
                    links,
                )
        return len(rows), len(links)

    for recipe in recipes:
        batch.append(recipe)
        if len(batch) >= batch_size:
            r, l = flush(batch)
            recipe_count += r
            link_count += l
            conn.commit()
            batch = []

    r, l = flush(batch)
    recipe_count += r
    link_count += l
    conn.commit()
    return recipe_count, link_count


def refresh_num_ingredients(conn: psycopg.Connection) -> None:
    """Align num_ingredients with the rows that actually resolved.

    The corpus count includes lines the parser dropped; retrieval divides by the
    resolved count, so leaving the raw number in place would make coverage
    percentages lie.
    """
    with conn.cursor() as cur:
        cur.execute(
            """UPDATE recipes r
               SET num_ingredients = c.n
               FROM (SELECT recipe_id, count(*) AS n FROM recipe_ingredients GROUP BY recipe_id) c
               WHERE c.recipe_id = r.id AND r.num_ingredients <> c.n"""
        )
    conn.commit()
