// Package migrations embeds the SQL migrations so the server, kbctl and the
// tests apply exactly the files that ship in deployments/migrations.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
