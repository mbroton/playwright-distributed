package migrations

import "embed"

// Files contains the SQL migrations used by the server at startup.
//
//go:embed *.sql
var Files embed.FS
