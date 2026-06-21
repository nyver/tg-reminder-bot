// Package sqlite embeds SQLite-specific goose migration files.
package sqlite

import "embed"

//go:embed *.sql
var FS embed.FS
