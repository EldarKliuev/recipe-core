//go:build integration

// Latency of the search path on the real corpus. The integration seed has four
// recipes and says nothing about speed, and pgtest resets the schema, so this
// benchmark takes its own BENCH_DATABASE_URL pointing at a database that already
// holds the ingested corpus (normally the dev database).
//
// It never runs DDL. Each run inserts a few throwaway users with fridges and
// deletes them afterwards; ON DELETE CASCADE removes their prefs and fridge.
//
//	make bench-search
//
// The target is p95 < 150 ms (see setCoverageSQL). Go reports the mean as
// ns/op, so every iteration is timed and p50/p95 are reported as extra metrics.
package cook

import (
	"context"
	"fmt"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/recipe-ai/core-api/internal/catalog"
	"github.com/recipe-ai/core-api/internal/user"
)

// minBenchRecipes guards against measuring an empty or test database by mistake:
// a fast result there would be meaningless.
const minBenchRecipes = 10000

// benchFridgeSizes span a near-empty fridge to a well-stocked one.
var benchFridgeSizes = []int{5, 15, 40}

// benchAllergens are the most common tags in the corpus, so the allergen NOT
// EXISTS has real work to do.
var benchAllergens = []string{"gluten", "dairy"}

func BenchmarkSearch(b *testing.B) {
	pool := benchPool(b)
	ctx := context.Background()

	// The most frequent non-staple ingredients are the worst case: they put the
	// largest share of the corpus into the candidates CTE.
	frequent := mostFrequentIngredients(b, pool, slices.Max(benchFridgeSizes))

	repo := NewRetrievalRepo(pool)
	catalogRepo := catalog.NewRepo(pool)
	svc := NewService(repo, catalogRepo, user.NewRepo(pool), nil, nil, nil)

	for _, size := range benchFridgeSizes {
		for _, allergens := range [][]string{nil, benchAllergens} {
			name := fmt.Sprintf("fridge=%d/allergens=%d", size, len(allergens))
			userID := benchUser(b, pool, frequent[:size], allergens)

			b.Run(name+"/Coverage", func(b *testing.B) {
				q, _, err := svc.buildQuery(ctx, userID, MatchesRequest{})
				if err != nil {
					b.Fatalf("buildQuery: %v", err)
				}
				timeEach(b, func() error {
					_, err := repo.Coverage(ctx, q)
					return err
				})
			})
			b.Run(name+"/Matches", func(b *testing.B) {
				timeEach(b, func() error {
					resp, err := svc.Matches(ctx, userID, MatchesRequest{})
					if err == nil && len(resp.Items) == 0 {
						// An empty result would be fast for the wrong reason.
						err = fmt.Errorf("no matches for a fridge of %d frequent ingredients", size)
					}
					return err
				})
			})
		}
	}
}

// timeEach runs fn once per benchmark iteration and reports latency percentiles
// alongside the mean.
func timeEach(b *testing.B, fn func() error) {
	b.Helper()
	var samples []time.Duration
	for b.Loop() {
		start := time.Now()
		if err := fn(); err != nil {
			b.Fatal(err)
		}
		samples = append(samples, time.Since(start))
	}
	slices.Sort(samples)
	b.ReportMetric(msAt(samples, 0.50), "p50-ms")
	b.ReportMetric(msAt(samples, 0.95), "p95-ms")
	b.ReportMetric(msAt(samples, 1.00), "max-ms")
}

// msAt is the nearest-rank percentile of sorted samples, in milliseconds.
func msAt(sorted []time.Duration, p float64) float64 {
	i := int(float64(len(sorted))*p+0.5) - 1
	i = max(0, min(i, len(sorted)-1))
	return float64(sorted[i].Microseconds()) / 1000
}

func benchPool(b *testing.B) *pgxpool.Pool {
	b.Helper()
	dsn := os.Getenv("BENCH_DATABASE_URL")
	if dsn == "" {
		b.Skip("BENCH_DATABASE_URL is not set; it must point at a database with the ingested corpus")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		b.Fatalf("connect: %v", err)
	}
	b.Cleanup(pool.Close)

	var recipes int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM recipes`).Scan(&recipes); err != nil {
		b.Fatalf("count recipes: %v", err)
	}
	if recipes < minBenchRecipes {
		b.Fatalf("BENCH_DATABASE_URL has %d recipes, want at least %d: load the corpus first", recipes, minBenchRecipes)
	}
	return pool
}

func mostFrequentIngredients(b *testing.B, pool *pgxpool.Pool, n int) []int64 {
	b.Helper()
	rows, err := pool.Query(context.Background(), `
SELECT ri.ingredient_id
FROM recipe_ingredients ri
WHERE ri.ingredient_id NOT IN (SELECT ingredient_id FROM pantry_staples)
GROUP BY ri.ingredient_id
ORDER BY count(*) DESC, ri.ingredient_id
LIMIT $1`, n)
	if err != nil {
		b.Fatalf("select frequent ingredients: %v", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			b.Fatalf("scan ingredient: %v", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		b.Fatalf("read frequent ingredients: %v", err)
	}
	if len(ids) < n {
		b.Fatalf("corpus has %d non-staple ingredients, want %d", len(ids), n)
	}
	return ids
}

// benchUser creates a throwaway user with the given fridge and allergens, and
// deletes it when the benchmark ends. Half the fridge expires soon, so the
// use-soon tie-breaker is exercised too.
func benchUser(b *testing.B, pool *pgxpool.Pool, fridge []int64, allergens []string) uuid.UUID {
	b.Helper()
	ctx := context.Background()
	id := uuid.New()
	if allergens == nil {
		allergens = []string{}
	}

	if _, err := pool.Exec(ctx,
		`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'bench')`,
		id, "bench-"+id.String()+"@bench.invalid"); err != nil {
		b.Fatalf("insert bench user: %v", err)
	}
	b.Cleanup(func() {
		if _, err := pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, id); err != nil {
			b.Errorf("delete bench user %s: %v", id, err)
		}
	})

	if _, err := pool.Exec(ctx,
		`INSERT INTO user_prefs (user_id, allergens) VALUES ($1, $2)`, id, allergens); err != nil {
		b.Fatalf("insert bench prefs: %v", err)
	}
	soon := time.Now().AddDate(0, 0, 1)
	for i, ing := range fridge {
		var expires *time.Time
		if i%2 == 0 {
			expires = &soon
		}
		if _, err := pool.Exec(ctx,
			`INSERT INTO user_ingredients (user_id, ingredient_id, expires_at) VALUES ($1, $2, $3)`,
			id, ing, expires); err != nil {
			b.Fatalf("insert bench fridge: %v", err)
		}
	}
	return id
}
