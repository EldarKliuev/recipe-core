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
