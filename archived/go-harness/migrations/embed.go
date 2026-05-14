// Package migrations embeds the SQL migration files so they ship inside the
// harness binary. The internal/db package applies them at startup via
// golang-migrate's iofs source.
package migrations

import "embed"

// FS is the embedded filesystem containing all *.sql migration files.
//
//go:embed *.sql
var FS embed.FS
