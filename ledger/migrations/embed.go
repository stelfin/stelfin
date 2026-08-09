// Package migrations embeds the ledger's SQL migrations so that tests, local
// development and production all apply the exact same files through the same
// code path. There is no separate migration binary to drift out of sync.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
