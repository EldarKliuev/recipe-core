// Package migrations embeds the SQL migration files so the migrate binary is
// self-contained and cannot drift from the code it ships with.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
