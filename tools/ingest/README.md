# tools/ingest

One-off loader for the [Kaggle 64k recipes corpus](https://www.kaggle.com/datasets/prashantsingh001/recipes-dataset-64k-dishes).

Python rather than Go purely because the ingredient-line parsing libraries live
here. The **schema is still owned by `core-api`**: this tool only writes rows that
the goose migrations defined, and it must run after `migrate`.

## Why this is the hard part

Retrieval, allergen safety and any future RAG ranking all sit on top of one
thing: turning `"1 1/2 cups all-purpose flour, sifted"` into the canonical
ingredient id for `all-purpose flour`. If that mapping is wrong, set coverage
returns nonsense and the allergen deny-list silently fails to match.

The acceptance criterion from the service plan is therefore about parsing, not
about row counts: **fewer than 5% unresolved ingredient lines**, plus a manual
review of a 200-row sample.

That share does not cover allergens. `load` drops a line without a catalog id,
and the deny-list cannot filter what was dropped. So a name the allergen rules
tag becomes a catalog entry however rare it is (`crab`, `feta`), and `catalog`
fails if any recipe would still lose a tagged line. `load` repeats the check
before writing anything.

A synonym hides an allergen the same way: its lines are stored under the
target's id, so `sardine in oil` mapped onto `oil` would pass a fish deny-list.
`catalog` maps a rare name onto its head noun only when the target carries every
tag the name has; otherwise the name is promoted like any other tagged name.
`catalog` fails if any synonym still loses a tag, and `load` repeats that check
against the reviewed tags.

Part of the corpus lists fewer ingredients than its steps use: "Red Velvet
Brownies" (`kaggle64k-10059`) lists one line of butter, while the steps add flour,
cocoa and eggs. `load` therefore also writes `recipes.step_ingredient_ids`, the
catalog ids (canonical names and synonyms, longest name first) that the
directions name, and the search deny-list reads them next to the listed ids.
Coverage ignores them. Any mention counts, "grease with butter" included: a
spurious id only hides a recipe from a user allergic to it. The bare words
`sauce`, `seasoning`, `roll`, `dough`, `crust`, `pastry` and `cream` are skipped
(`STEP_GENERIC_NAMES`), because in steps they mean the dish's own part, and
`sauce` and `seasoning` carry all 13 tags. Longer names (`soy sauce`, `ice cream`)
still count.

On the 2026-11-20 corpus `load` reports 8 368 of 62 126 recipes whose steps add an
allergen that no listed line has (484 of them list one or two ingredients):
wheat 2547, gluten 2538, soy 2359, dairy 2307, eggs 1806, peanuts 1199,
tree_nuts 1135, sulfites 1060, celery 702, fish 669, mustard 634, sesame 607,
shellfish 527. The most frequent sources are `egg`, `flour`, `butter`,
`cooking spray` and `nut`. With every catalog name, the bare words included, it
was 15 082, and `sauce` alone hid 6 663 recipes from every allergy.

## Pipeline

```
eda      → what the raw columns actually contain
parse    → rule-based parse of every ingredient line into (qty, unit, name)
catalog  → canonical vocabulary from frequency analysis + synonym mapping
allergens→ keyword pass producing a review file for manual confirmation
load     → COPY recipes, recipe_ingredients, catalog, synonyms, allergens
```

Each stage writes an artefact into `/data/work/`, so a stage can be re-run
without redoing the ones before it.

## Usage

```bash
# from the repo root, with the stack already up
docker compose --profile ingest run --rm ingest eda
docker compose --profile ingest run --rm ingest parse
docker compose --profile ingest run --rm ingest catalog
docker compose --profile ingest run --rm ingest allergens
docker compose --profile ingest run --rm ingest load

# or the whole thing; on a fresh run it stops at `load` until the review is done
docker compose --profile ingest run --rm ingest all
```

Put the Kaggle CSV at `data/recipes.csv` (mounted as `/data/recipes.csv`).

## Allergen review

`allergens` writes `data/work/allergens_review.csv` with one row per catalog
entry: the proposed tags and an empty `confirmed` column. Go through every row,
fix `allergens` where the proposal is wrong (space-separated tags from
`user.KnownAllergens`, empty for none) and set `confirmed` to `yes`. Untagged
rows need the same look as tagged ones: a missed tag is the dangerous mistake.

`load` refuses to start while any catalog entry lacks `confirmed=yes`, a tag is
outside the vocabulary or the file is missing. A catalog rebuilt after the review
adds names the reviewer never saw, so `load` refuses that file too.

`allergens` never overwrites an existing file, so your edits survive a re-run.
After changing the rules, delete the file to get new proposals, and review it again.

Rules match whole words, every matching rule adds its tags, and a longer rule
hides the words it matched from shorter ones (`coconut milk` is not dairy).
They live in `ingest/allergens.py`; tests are in `tests/`:

```bash
make test-ingest
```

## Long tail

The rule-based parser handles the bulk. Two escape hatches exist for the
remainder, both optional and both off by default:

- `--use-crf` routes unparsed lines through `ingredient-parser` (a CRF model).
  Install with `pip install ingredient-parser-nlp`.
- `--use-llm` batches whatever is still unresolved to `llm-service`, which is the
  only process allowed to talk to a model.

Check `data/work/unresolved.csv` before reaching for either: most of what lands
there is worth fixing with a rule.

## Search latency on the loaded corpus

The plan's acceptance target is p95 < 150 ms for `POST /cook/matches`. The
integration seed is four recipes, so speed is measured on the ingested corpus:

```bash
make bench-search   # BENCH_DATABASE_URL defaults to the dev database
```

`BenchmarkSearch` (`internal/cook/retrieval_bench_test.go`) never runs DDL. It
adds throwaway users whose fridges hold the 5, 15 or 40 most frequent
non-staple ingredients (the worst case: they pull the most recipes into the
search), with no allergens and with `gluten` + `dairy`, then deletes them. It
times `RetrievalRepo.Coverage` alone and the whole `Service.Matches`, 200
iterations each, and reports p50/p95/max.

### 2026-11-20

Dev database: 62 126 recipes, 533 211 ingredient rows, 1 584 catalog entries,
loaded before the TASK-01–03 fixes (TASK-21). Postgres 16 from compose with
default settings (`work_mem` 4MB, `shared_buffers` 128MB), Apple M4 Pro, warm
cache.

| Fridge | Allergens | `Coverage` p50 / p95, ms | `Matches` p50 / p95, ms |
|---|---|---|---|
| 5 | — | 127 / 149 | 128 / 141 |
| 5 | gluten, dairy | 156 / 178 | 153 / 162 |
| 15 | — | 174 / 185 | 177 / 198 |
| 15 | gluten, dairy | 204 / 230 | 193 / 213 |
| 40 | — | 190 / 208 | 197 / 216 |
| 40 | gluten, dairy | 203 / 234 | 197 / 208 |

**The target is not met** for any fridge above 5 items, or for 5 items with
allergens. Hydrating the 20 results costs almost nothing next to the coverage
query; the two columns differ by noise.

`EXPLAIN (ANALYZE, BUFFERS)` for 40 items with allergens (334 ms under
`EXPLAIN`): the `candidates` CTE keeps 56 836 of 62 126 recipes, because
frequent ingredients appear almost everywhere. The coverage aggregate then
groups 510 480 rows of a sequential scan of `recipe_ingredients`, and the
HashAggregate spills to disk in 5 batches with 4MB `work_mem`. The allergen
`NOT EXISTS` is a second full scan of `recipe_ingredients` hashed against 265
tagged ingredients. Indexes are not the issue: the plan reads most of the table
on purpose. The next step is the denormalisation described at `setCoverageSQL`
(TASK-39).

### 2026-11-20, after TASK-39

Same database, settings and machine. Migration `00007` adds
`recipes.ingredient_ids int[]`, a copy of the `recipe_ingredients` links kept in
step by statement-level triggers, and the `intarray` extension. `setCoverageSQL`
counts coverage with `icount` and `&` on that array and excludes allergens and
dislikes by overlap with ids read from `ingredient_allergens` on every call, so
allergen tags are not copied anywhere.

| Fridge | Allergens | `Coverage` p50 / p95, ms | `Matches` p50 / p95, ms |
|---|---|---|---|
| 5 | — | 28 / 31 | 29 / 32 |
| 5 | gluten, dairy | 29 / 30 | 30 / 32 |
| 15 | — | 40 / 42 | 43 / 45 |
| 15 | gluten, dairy | 41 / 44 | 41 / 44 |
| 40 | — | 62 / 68 | 65 / 72 |
| 40 | gluten, dairy | 64 / 68 | 65 / 71 |

The target is met in every scenario. What it took:

- Counting through `unnest` instead of `intarray` cost a subquery per candidate
  recipe (~1.6 µs × 56 836) and stayed at 150–280 ms.
- `intarray` sorts both operands on every call. Postgres' sort returns at once
  on sorted input, so the stored arrays and every array the query builds are
  sorted. With an unsorted deny-list (265 ids for `gluten` + `dairy`) the
  allergen overlap alone took ~190 ms.

Arrays are int4 because `intarray` is; catalog ids are `bigint`. The trigger
casts and so refuses a link whose id does not fit, and the query leaves such ids
out of the fridge and dislike arrays instead of failing.

Not measured: how much the triggers slow down `load`. `executemany` sends one
statement per link, so each link updates its recipe's array once; for the
~530 000 links of a full load that is the same number of `UPDATE recipes`.
Repeat this benchmark after the corpus reload (TASK-21).
