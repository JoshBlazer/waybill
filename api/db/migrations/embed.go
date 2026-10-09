// Package migrations embeds the goose SQL migrations into the binary.
package migrations

import "embed"

// FS holds every *.sql migration in this directory.
//
//go:embed *.sql
var FS embed.FS
