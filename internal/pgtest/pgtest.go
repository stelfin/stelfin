// Package pgtest brings up a real Postgres for tests.
//
// stelfin's ledger invariants are database constraints — deferred triggers,
// check constraints, unique indexes — so a fake driver or an in-memory stand-in
// would verify nothing. Tests run against the real engine, migrated by the same
// code that migrates production.
//
// Each caller gets its own instance on its own port, because `go test ./...`
// runs packages in parallel and two servers cannot share one port. Each also
// gets a fresh data directory: reusing one would carry over rows from the
// previous run, and a suite that passes only because of leftover state is worse
// than no suite.
package pgtest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DB is a running Postgres instance owned by a test binary.
type DB struct {
	// Pool is a connection pool against the migrated database.
	Pool *pgxpool.Pool
	// DSN addresses the same database, for code that needs database/sql.
	DSN string

	pg      *embeddedpostgres.EmbeddedPostgres
	dataDir string
}

// Start launches Postgres on port, applies migrations with migrate, and returns
// a connected pool. The caller must call Stop.
//
// The first run downloads a Postgres binary and caches it under $HOME; later
// runs start in a couple of seconds.
func Start(port uint32, migrate func(dsn string) error) (*DB, error) {
	// One temp root per instance holding both the data directory and the
	// runtime directory.
	//
	// The runtime path matters as much as the data path: by default it is the
	// shared extraction directory under $HOME, which Stop deletes. With two
	// packages running in parallel, whichever finishes first would delete the
	// binaries out from under the other. Binaries stay on the shared cache so
	// they are still downloaded and extracted only once.
	root, err := os.MkdirTemp("", "stelfin-pgtest-*")
	if err != nil {
		return nil, fmt.Errorf("pgtest: create temp root: %w", err)
	}
	dataDir := filepath.Join(root, "data")
	runtimeDir := filepath.Join(root, "runtime")

	cacheDir, err := os.UserHomeDir()
	if err != nil {
		os.RemoveAll(root)
		return nil, fmt.Errorf("pgtest: locate home directory: %w", err)
	}
	binariesDir := filepath.Join(cacheDir, ".embedded-postgres-go", "extracted")

	dsn := fmt.Sprintf("postgres://stelfin:stelfin@localhost:%d/stelfin_test?sslmode=disable", port)

	pg := embeddedpostgres.NewDatabase(
		embeddedpostgres.DefaultConfig().
			Username("stelfin").
			Password("stelfin").
			Database("stelfin_test").
			Port(port).
			BinariesPath(binariesDir).
			RuntimePath(runtimeDir).
			DataPath(dataDir),
	)
	if err := pg.Start(); err != nil {
		os.RemoveAll(root)
		return nil, fmt.Errorf("pgtest: start postgres on port %d: %w", port, err)
	}

	db := &DB{DSN: dsn, pg: pg, dataDir: root}

	if err := migrate(dsn); err != nil {
		_ = db.Stop()
		return nil, fmt.Errorf("pgtest: migrate: %w", err)
	}

	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		_ = db.Stop()
		return nil, fmt.Errorf("pgtest: connect: %w", err)
	}
	db.Pool = pool

	return db, nil
}

// Stop shuts the instance down and removes its data directory.
func (d *DB) Stop() error {
	if d.Pool != nil {
		d.Pool.Close()
	}
	err := d.pg.Stop()
	if rmErr := os.RemoveAll(d.dataDir); err == nil {
		err = rmErr
	}
	if err != nil {
		return fmt.Errorf("pgtest: stop: %w", err)
	}
	return nil
}
