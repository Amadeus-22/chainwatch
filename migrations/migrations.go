// Package migrations embeds the SQL schema migrations, applied in file-name order
// by the Postgres adapter at startup.
package migrations

import "embed"

// FS holds every *.sql migration.
//
//go:embed *.sql
var FS embed.FS
