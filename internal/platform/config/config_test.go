package config

import (
	"bufio"
	"os"
	"strings"
	"testing"
)

// setRequiredEnv sets everything Load requires, with the given JWT secret.
func setRequiredEnv(t *testing.T, secret string) {
	t.Helper()
	t.Setenv("DATABASE_URL", "postgres://u:p@localhost:5432/db")
	t.Setenv("REDIS_URL", "redis://localhost:6379/0")
	t.Setenv("JWT_SECRET", secret)
}

// exampleJWTSecret reads JWT_SECRET from the repository's .env.example, so the
// test follows the file `make bootstrap` copies rather than a copy of its value.
func exampleJWTSecret(t *testing.T) string {
	t.Helper()
	f, err := os.Open("../../../.env.example")
	if err != nil {
		t.Fatalf("open .env.example: %v", err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(strings.TrimSpace(sc.Text()), "JWT_SECRET="); ok {
			return v
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("read .env.example: %v", err)
	}
	t.Fatal("JWT_SECRET not found in .env.example")
	return ""
}

func TestLoadAcceptsARealSecret(t *testing.T) {
	secret := strings.Repeat("a1", 32)
	setRequiredEnv(t, secret)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.JWTSecret != secret {
		t.Fatalf("JWTSecret = %q, want %q", cfg.JWTSecret, secret)
	}
}

func TestLoadRejectsThePlaceholderSecretFromEnvExample(t *testing.T) {
	secret := exampleJWTSecret(t)
	if len(secret) < 32 {
		t.Fatalf("placeholder %q is shorter than 32 bytes; the length check already covers it and this test proves nothing", secret)
	}
	setRequiredEnv(t, secret)

	_, err := Load()
	if err == nil {
		t.Fatal("Load accepted the placeholder JWT_SECRET from .env.example")
	}
	if !strings.Contains(err.Error(), "placeholder") {
		t.Fatalf("error = %q, want it to name the placeholder", err)
	}
}

func TestLoadRejectsAShortSecret(t *testing.T) {
	setRequiredEnv(t, strings.Repeat("x", 31))

	if _, err := Load(); err == nil {
		t.Fatal("Load accepted a 31-byte JWT_SECRET")
	}
}
