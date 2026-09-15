// Command migrate applies the goose migrations embedded in this binary. It runs
// as its own compose service before core-api starts, so the application never
// races the schema.
package main

import (
	"database/sql"
	"flag"
	"fmt"
	"log"
	"os"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/recipe-ai/core-api/migrations"
)

func main() {
	command := flag.String("command", "up", "goose command: up, up-by-one, down, status, version")
	flag.Parse()

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		log.Fatal("DATABASE_URL is required")
	}

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	defer func() { _ = db.Close() }()

	goose.SetBaseFS(migrations.FS)
	if err := goose.SetDialect("postgres"); err != nil {
		log.Fatalf("set dialect: %v", err)
	}

	args := flag.Args()
	if err := goose.Run(*command, db, ".", args...); err != nil {
		log.Fatal(fmt.Errorf("goose %s: %w", *command, err))
	}
}
