"""Corpus ingest CLI.

Stages write artefacts into /data/work so any stage can be re-run alone:

    eda       inspect the raw columns
    parse     raw ingredient lines -> (qty, unit, name)
    catalog   canonical vocabulary from frequency analysis + synonyms
    sample    200-row sample for the manual review the plan requires
    allergens keyword proposals -> review file
    load      write everything to Postgres
    all       parse, catalog, sample, allergens, load
"""

from __future__ import annotations

import argparse
import ast
import collections
import csv
import json
import os
import sys
from pathlib import Path
from typing import Any, Iterator

from . import allergens as allergen_rules
from . import db
from .parser import parse_line

DATA = Path(os.environ.get("INGEST_DATA_DIR", "/data"))
WORK = DATA / "work"
CSV_PATH = Path(os.environ.get("INGEST_CSV", str(DATA / "recipes.csv")))

PARSED = WORK / "parsed.jsonl"
CATALOG = WORK / "catalog.json"
UNRESOLVED = WORK / "unresolved.csv"
SAMPLE = WORK / "sample200.csv"
ALLERGEN_REVIEW = WORK / "allergens_review.csv"

# A name needs this many corpus occurrences to become a canonical entry. Below it,
# the name is either mapped onto a canonical head noun or reported as unresolved.
MIN_CANONICAL_FREQ = 20

# Acceptance criterion from the service plan.
MAX_UNRESOLVED_SHARE = 0.05

STAPLE_NAMES = {
    "water", "salt", "black pepper", "pepper", "sugar", "vegetable oil",
    "olive oil", "all-purpose flour", "baking powder", "baking soda",
    "white vinegar", "cooking spray", "ice", "ice water",
}

# Column name candidates, since Kaggle mirrors of this dataset differ slightly.
COLUMN_ALIASES = {
    "title": ["recipe_title", "title", "name", "recipe_name"],
    "category": ["category", "cuisine"],
    "subcategory": ["subcategory", "sub_category"],
    "description": ["description", "summary"],
    "ingredients": ["ingredients", "ingredient_list", "ingredients_list"],
    "directions": ["directions", "instructions", "steps", "method"],
    "num_ingredients": ["num_ingredients", "n_ingredients"],
    "num_steps": ["num_steps", "n_steps"],
}


def _resolve_columns(header: list[str]) -> dict[str, str | None]:
    lowered = {h.lower().strip(): h for h in header}
    resolved: dict[str, str | None] = {}
    for field, aliases in COLUMN_ALIASES.items():
        resolved[field] = next((lowered[a] for a in aliases if a in lowered), None)
    if resolved["title"] is None or resolved["ingredients"] is None:
        raise SystemExit(
            f"could not find title/ingredients columns in {header}; "
            "adjust COLUMN_ALIASES in tools/ingest/ingest/__main__.py"
        )
    return resolved


def _as_list(value: Any) -> list[str]:
    """Corpus list columns arrive as stringified Python or JSON lists."""
    if value is None:
        return []
    if isinstance(value, list):
        return [str(v).strip() for v in value if str(v).strip()]
    text = str(value).strip()
    if not text or text in {"[]", "nan", "None"}:
        return []
    for loader in (json.loads, ast.literal_eval):
        try:
            parsed = loader(text)
            if isinstance(parsed, list):
                return [str(v).strip() for v in parsed if str(v).strip()]
        except (ValueError, SyntaxError):
            continue
    # Fall back to a delimiter split for plain-text columns.
    for sep in ("\n", "|", ";"):
        if sep in text:
            return [part.strip() for part in text.split(sep) if part.strip()]
    return [text]


def _rows() -> Iterator[dict[str, Any]]:
    if not CSV_PATH.exists():
        raise SystemExit(f"{CSV_PATH} not found; put the Kaggle CSV at data/recipes.csv")
    # The description field can be long, and some rows embed newlines.
    csv.field_size_limit(10_000_000)
    with CSV_PATH.open(newline="", encoding="utf-8", errors="replace") as handle:
        reader = csv.DictReader(handle)
        columns = _resolve_columns(list(reader.fieldnames or []))
        for index, row in enumerate(reader):
            yield {"_index": index, "_columns": columns, **row}


def _field(row: dict[str, Any], field: str) -> Any:
    column = row["_columns"].get(field)
    return row.get(column) if column else None


def cmd_eda(_: argparse.Namespace) -> int:
    """Look at what the raw columns actually contain before trusting them."""
    total = 0
    empty_ingredients = 0
    line_lengths: list[int] = []
    categories: collections.Counter[str] = collections.Counter()
    unit_hits: collections.Counter[str] = collections.Counter()
    samples: list[str] = []

    for row in _rows():
        total += 1
        lines = _as_list(_field(row, "ingredients"))
        if not lines:
            empty_ingredients += 1
        line_lengths.append(len(lines))
        categories[str(_field(row, "category") or "?")] += 1
        if len(samples) < 25:
            samples.extend(lines[:3])
        for line in lines:
            parsed = parse_line(line)
            unit_hits[parsed.unit or "(none)" if parsed else "(unparsed)"] += 1

    WORK.mkdir(parents=True, exist_ok=True)
    report = [
        f"rows:                {total}",
        f"rows w/o ingredients {empty_ingredients}",
        f"avg lines/recipe     {sum(line_lengths) / max(total, 1):.2f}",
        f"max lines/recipe     {max(line_lengths, default=0)}",
        "",
        "top categories:",
        *(f"  {name:30s} {count}" for name, count in categories.most_common(15)),
        "",
        "unit distribution:",
        *(f"  {unit:10s} {count}" for unit, count in unit_hits.most_common(20)),
        "",
        "sample ingredient lines:",
        *(f"  {line}" for line in samples[:25]),
    ]
    (WORK / "eda.txt").write_text("\n".join(report), encoding="utf-8")
    print("\n".join(report))
    return 0


def cmd_parse(args: argparse.Namespace) -> int:
    """Parse every ingredient line and write one JSON object per recipe."""
    WORK.mkdir(parents=True, exist_ok=True)
    crf = _load_crf() if args.use_crf else None

    total_lines = 0
    parsed_lines = 0
    failed: collections.Counter[str] = collections.Counter()

    with PARSED.open("w", encoding="utf-8") as out:
        for row in _rows():
            raw_lines = _as_list(_field(row, "ingredients"))
            directions = _as_list(_field(row, "directions"))
            lines: list[dict[str, Any]] = []

            for raw in raw_lines:
                total_lines += 1
                parsed = parse_line(raw)
                if parsed is None and crf is not None:
                    parsed = crf(raw)
                if parsed is None or not parsed.name:
                    failed[raw] += 1
                    continue
                parsed_lines += 1
                lines.append({
                    "raw": parsed.raw, "name": parsed.name, "qty": parsed.qty,
                    "unit": parsed.unit, "confident": parsed.confident,
                })

            title = str(_field(row, "title") or "").strip()
            if not title or not lines:
                continue

            record = {
                "external_id": f"kaggle64k-{row['_index']}",
                "title": title,
                "category": _clean(_field(row, "category")),
                "subcategory": _clean(_field(row, "subcategory")),
                "description": _clean(_field(row, "description")),
                "directions": directions,
                "num_ingredients": _as_int(_field(row, "num_ingredients"), len(raw_lines)),
                "num_steps": _as_int(_field(row, "num_steps"), len(directions)),
                "source": "kaggle-recipes-64k",
                "lines": lines,
            }
            out.write(json.dumps(record, ensure_ascii=False) + "\n")

    share = 1 - parsed_lines / max(total_lines, 1)
    print(f"lines: {total_lines}  parsed: {parsed_lines}  unparsed: {share:.2%}")
    if failed:
        with (WORK / "unparsed_lines.csv").open("w", newline="", encoding="utf-8") as handle:
            writer = csv.writer(handle)
            writer.writerow(["raw", "occurrences"])
            writer.writerows(failed.most_common())
        print(f"wrote {WORK / 'unparsed_lines.csv'} ({len(failed)} distinct lines)")
    print(f"wrote {PARSED}")
    return 0


def cmd_catalog(args: argparse.Namespace) -> int:
    """Build the canonical vocabulary from name frequency."""
    if not PARSED.exists():
        raise SystemExit(f"{PARSED} missing; run `parse` first")

    freq: collections.Counter[str] = collections.Counter()
    for record in _read_parsed():
        for line in record["lines"]:
            freq[line["name"]] += 1

    canonical = {name for name, count in freq.items() if count >= args.min_freq}

    synonyms: dict[str, str] = {}
    unresolved: collections.Counter[str] = collections.Counter()
    for name, count in freq.items():
        if name in canonical:
            continue
        head = _match_head_noun(name, canonical)
        if head:
            synonyms[name] = head
        else:
            unresolved[name] = count

    if args.use_llm and unresolved:
        from .llm import normalise_batch
        proposals = normalise_batch(list(unresolved))
        for name, suggestion in proposals.items():
            if suggestion in canonical:
                synonyms[name] = suggestion
                del unresolved[name]

    occurrences = sum(freq.values())
    unresolved_occurrences = sum(unresolved.values())
    share = unresolved_occurrences / max(occurrences, 1)

    WORK.mkdir(parents=True, exist_ok=True)
    CATALOG.write_text(json.dumps({
        "min_freq": args.min_freq,
        "canonical": sorted(canonical),
        "synonyms": synonyms,
        "frequency": {name: freq[name] for name in sorted(canonical)},
    }, ensure_ascii=False, indent=1), encoding="utf-8")

    with UNRESOLVED.open("w", newline="", encoding="utf-8") as handle:
        writer = csv.writer(handle)
        writer.writerow(["name", "occurrences"])
        writer.writerows(unresolved.most_common())

    print(f"distinct names:      {len(freq)}")
    print(f"canonical entries:   {len(canonical)}  (min_freq={args.min_freq})")
    print(f"synonym mappings:    {len(synonyms)}")
    print(f"unresolved names:    {len(unresolved)}  ({share:.2%} of all line occurrences)")
    print(f"wrote {CATALOG} and {UNRESOLVED}")

    if share > MAX_UNRESOLVED_SHARE:
        print(f"\nFAIL: unresolved share {share:.2%} exceeds the {MAX_UNRESOLVED_SHARE:.0%} "
              f"acceptance threshold. Inspect {UNRESOLVED} and add parser rules.")
        return 1
    print(f"\nOK: unresolved share is within the {MAX_UNRESOLVED_SHARE:.0%} threshold.")
    return 0


def cmd_sample(args: argparse.Namespace) -> int:
    """Write a fixed-size sample for the manual review the plan requires."""
    if not PARSED.exists():
        raise SystemExit(f"{PARSED} missing; run `parse` first")

    catalog = json.loads(CATALOG.read_text(encoding="utf-8")) if CATALOG.exists() else {}
    synonyms: dict[str, str] = catalog.get("synonyms", {})
    canonical = set(catalog.get("canonical", []))

    # Deterministic stride sampling, so the reviewed sample is reproducible.
    records = list(_read_parsed())
    stride = max(len(records) // args.size, 1)
    WORK.mkdir(parents=True, exist_ok=True)

    written = 0
    with SAMPLE.open("w", newline="", encoding="utf-8") as handle:
        writer = csv.writer(handle)
        writer.writerow(["recipe_title", "raw_line", "parsed_name", "resolved_to",
                         "qty", "unit", "status", "correct_yes_no"])
        for record in records[::stride]:
            if written >= args.size:
                break
            for line in record["lines"]:
                name = line["name"]
                resolved = name if name in canonical else synonyms.get(name, "")
                status = ("canonical" if name in canonical
                          else "synonym" if name in synonyms else "unresolved")
                writer.writerow([record["title"], line["raw"], name, resolved,
                                 line["qty"], line["unit"], status, ""])
                written += 1
                if written >= args.size:
                    break

    print(f"wrote {SAMPLE} ({written} rows) — fill in correct_yes_no and count the misses")
    return 0


def cmd_allergens(_: argparse.Namespace) -> int:
    """Propose allergen tags for manual confirmation."""
    if not CATALOG.exists():
        raise SystemExit(f"{CATALOG} missing; run `catalog` first")
    catalog = json.loads(CATALOG.read_text(encoding="utf-8"))

    if ALLERGEN_REVIEW.exists():
        print(f"{ALLERGEN_REVIEW} already exists; leaving your edits alone.")
        print("Delete it if you want to regenerate the proposals from scratch.")
        return 0

    WORK.mkdir(parents=True, exist_ok=True)
    tagged = 0
    with ALLERGEN_REVIEW.open("w", newline="", encoding="utf-8") as handle:
        writer = csv.writer(handle)
        writer.writerow(["canonical_name", "allergens", "occurrences", "confirmed"])
        for name in catalog["canonical"]:
            tags = allergen_rules.tag(name)
            if tags:
                tagged += 1
            writer.writerow([name, " ".join(tags), catalog["frequency"].get(name, 0),
                             "yes" if tags else ""])

    print(f"wrote {ALLERGEN_REVIEW}: {tagged} of {len(catalog['canonical'])} entries tagged")
    print("Review it before `load`: a missed tag is a safety bug, a spurious one only narrows results.")
    return 0


def cmd_load(args: argparse.Namespace) -> int:
    """Write catalog, synonyms, allergens and recipes to Postgres."""
    if not PARSED.exists() or not CATALOG.exists():
        raise SystemExit("run `parse` and `catalog` first")

    catalog = json.loads(CATALOG.read_text(encoding="utf-8"))
    canonical: list[str] = catalog["canonical"]
    synonyms: dict[str, str] = catalog["synonyms"]

    conn = db.connect()
    try:
        print(f"upserting {len(canonical)} canonical ingredients")
        name_to_id = db.upsert_catalog(conn, [(name, None) for name in canonical])
        conn.commit()

        synonym_rows = [
            (name_to_id[target], synonym)
            for synonym, target in synonyms.items()
            if target in name_to_id and synonym not in name_to_id
        ]
        print(f"upserting {len(synonym_rows)} synonyms")
        db.upsert_synonyms(conn, synonym_rows)
        conn.commit()

        staple_ids = [name_to_id[name] for name in STAPLE_NAMES if name in name_to_id]
        print(f"marking {len(staple_ids)} pantry staples")
        db.upsert_staples(conn, staple_ids)
        conn.commit()

        allergen_rows = _confirmed_allergens(name_to_id)
        print(f"upserting {len(allergen_rows)} allergen tags")
        db.upsert_allergens(conn, allergen_rows)
        conn.commit()

        # Synonyms resolve to their canonical id at load time, so recipe lines
        # carrying a synonym still link to a real ingredient.
        resolution = dict(name_to_id)
        for synonym, target in synonyms.items():
            if target in name_to_id:
                resolution.setdefault(synonym, name_to_id[target])

        print("loading recipes")
        recipes, links = db.load_recipes(conn, _read_parsed(), resolution, args.batch_size)
        print(f"loaded {recipes} recipes and {links} ingredient links")

        db.refresh_num_ingredients(conn)
        print("aligned recipes.num_ingredients with resolved links")
    finally:
        conn.close()
    return 0


def _confirmed_allergens(name_to_id: dict[str, int]) -> list[tuple[int, str]]:
    """Read allergen tags from the review file, honouring the confirmed column."""
    if not ALLERGEN_REVIEW.exists():
        print(f"WARNING: {ALLERGEN_REVIEW} missing; falling back to unreviewed keyword rules")
        return [
            (ingredient_id, tag)
            for name, ingredient_id in name_to_id.items()
            for tag in allergen_rules.tag(name)
        ]

    rows: list[tuple[int, str]] = []
    skipped = 0
    with ALLERGEN_REVIEW.open(newline="", encoding="utf-8") as handle:
        for row in csv.DictReader(handle):
            tags = (row.get("allergens") or "").split()
            if not tags:
                continue
            if (row.get("confirmed") or "").strip().lower() in {"no", "n", "false"}:
                skipped += 1
                continue
            ingredient_id = name_to_id.get((row.get("canonical_name") or "").strip().lower())
            if ingredient_id is None:
                continue
            rows.extend((ingredient_id, tag) for tag in tags)
    if skipped:
        print(f"  skipped {skipped} entries marked not confirmed")
    return rows


def _match_head_noun(name: str, canonical: set[str]) -> str | None:
    """Map a rare name onto a canonical one by trailing head-noun match.

    "granny smith apple" -> "apple", but "apple cider vinegar" is not mapped onto
    "apple", because the head noun there is vinegar. Matching on the tail rather
    than any substring is what keeps that distinction. The longest tail wins, so
    "granny smith apple" prefers "smith apple" over "apple" if both are canonical.
    """
    words = name.split()
    for start in range(1, len(words)):
        tail = " ".join(words[start:])
        if tail in canonical:
            return tail
    return None


def _read_parsed() -> Iterator[dict[str, Any]]:
    with PARSED.open(encoding="utf-8") as handle:
        for line in handle:
            if line.strip():
                yield json.loads(line)


def _load_crf():
    """Wrap the ingredient-parser CRF model, if it is installed."""
    try:
        from ingredient_parser import parse_ingredient  # type: ignore
    except ImportError:
        print("--use-crf requested but ingredient-parser-nlp is not installed; skipping")
        return None

    from .parser import ParsedLine

    def run(raw: str) -> ParsedLine | None:
        try:
            result = parse_ingredient(raw)
        except Exception:
            return None
        name = getattr(result.name, "text", None) if result.name else None
        if not name:
            return None
        amount = result.amount[0] if result.amount else None
        qty = None
        unit = None
        if amount is not None:
            try:
                qty = float(str(amount.quantity))
            except (TypeError, ValueError):
                qty = None
            unit = str(amount.unit) if amount.unit else None
        return ParsedLine(raw=raw, name=name.strip().lower(), qty=qty, unit=unit, confident=False)

    return run


def _clean(value: Any) -> str | None:
    if value is None:
        return None
    text = str(value).strip()
    return text or None


def _as_int(value: Any, default: int) -> int:
    try:
        return int(float(str(value)))
    except (TypeError, ValueError):
        return default


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(prog="ingest", description=__doc__,
                                     formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = parser.add_subparsers(dest="command", required=True)

    sub.add_parser("eda", help="inspect the raw CSV").set_defaults(func=cmd_eda)

    p_parse = sub.add_parser("parse", help="parse ingredient lines")
    p_parse.add_argument("--use-crf", action="store_true",
                        help="route unparsed lines through the ingredient-parser CRF model")
    p_parse.set_defaults(func=cmd_parse)

    p_catalog = sub.add_parser("catalog", help="build the canonical vocabulary")
    p_catalog.add_argument("--min-freq", type=int, default=MIN_CANONICAL_FREQ)
    p_catalog.add_argument("--use-llm", action="store_true",
                          help="batch leftover names to llm-service for normalisation")
    p_catalog.set_defaults(func=cmd_catalog)

    p_sample = sub.add_parser("sample", help="write a manual review sample")
    p_sample.add_argument("--size", type=int, default=200)
    p_sample.set_defaults(func=cmd_sample)

    sub.add_parser("allergens", help="propose allergen tags").set_defaults(func=cmd_allergens)

    p_load = sub.add_parser("load", help="write everything to Postgres")
    p_load.add_argument("--batch-size", type=int, default=1000)
    p_load.set_defaults(func=cmd_load)

    p_all = sub.add_parser("all", help="parse, catalog, sample, allergens, load")
    p_all.add_argument("--min-freq", type=int, default=MIN_CANONICAL_FREQ)
    p_all.add_argument("--use-crf", action="store_true")
    p_all.add_argument("--use-llm", action="store_true")
    p_all.add_argument("--batch-size", type=int, default=1000)
    p_all.set_defaults(func=cmd_all)

    args = parser.parse_args(argv)
    return args.func(args)


def cmd_all(args: argparse.Namespace) -> int:
    args.size = 200
    for stage in (cmd_parse, cmd_catalog, cmd_sample, cmd_allergens, cmd_load):
        print(f"\n=== {stage.__name__.removeprefix('cmd_')} ===")
        code = stage(args)
        if code != 0:
            return code
    return 0


if __name__ == "__main__":
    sys.exit(main())
