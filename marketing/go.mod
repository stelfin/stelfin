// This file exists only to give the marketing/ directory its own Go module
// boundary. marketing/ is a Next.js app with no Go code of its own — some of
// its npm dependencies ship stray .go files (e.g. flatted's golang port),
// and without this, `go build ./...`/`go vet ./...`/`go test ./...` run from
// the repo root would walk into marketing/node_modules and try to build
// them. A nested go.mod stops the parent module's ./... at this boundary.
module github.com/stelfin/stelfin/marketing-unused

go 1.25
