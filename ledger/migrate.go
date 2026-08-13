package ledger

import (
	"database/sql"
	"fmt"

	// Registers the "pgx" database/sql driver, which goose needs. Runtime
	// queries go through pgxpool directly rather than database/sql.
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/stelfin/stelfin/ledger/migrations"
)

// Migrate applies every pending ledger migration to the database at dsn.
//
// Tests, local development and production all call this, so the schema under
// test is the schema that ships. There is no separate migration binary that
// could drift.
func Migrate(dsn string) error {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return fmt.Errorf("ledger: open for migration: %w", err)
	}
	defer db.Close()

	goose.SetBaseFS(migrations.FS)
	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("ledger: set goose dialect: %w", err)
	}
	if err := goose.Up(db, "."); err != nil {
		return fmt.Errorf("ledger: apply migrations: %w", err)
	}
	return nil
}
