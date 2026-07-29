// Package migrate applies db/migrations/001_init.sql on startup if the
// schema doesn't exist yet. This exists so the service can be deployed to
// a bare Postgres instance (e.g. Render/Railway free tier) without a
// separate migration step in the deploy pipeline.
//
// NOTE: the SQL embedded here is a copy of ../../../../db/migrations/001_init.sql
// (go:embed cannot reach outside the collector module). Keep both in sync;
// db/migrations/001_init.sql remains the canonical, human-edited copy.
package migrate

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
)

//go:embed sql/001_init.sql
var initSQL string

// Apply runs the embedded schema exactly once, detected by checking whether
// the "notices" table already exists. It then runs small idempotent
// follow-up migrations against any DB (fresh or pre-existing) — this project
// has no versioned migration runner yet, so schema changes made after the
// initial deploy are expressed here as individually-guarded ALTERs instead.
func Apply(ctx context.Context, db *sql.DB) error {
	var exists bool
	err := db.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM information_schema.tables
			WHERE table_schema = 'public' AND table_name = 'notices'
		)`).Scan(&exists)
	if err != nil {
		return fmt.Errorf("check schema: %w", err)
	}
	if !exists {
		if _, err := db.ExecContext(ctx, initSQL); err != nil {
			return fmt.Errorf("apply initial schema: %w", err)
		}
	}

	if err := ensureCompanyProfileArrayColumns(ctx, db); err != nil {
		return fmt.Errorf("migrate company_profiles array columns: %w", err)
	}
	return nil
}

// ensureCompanyProfileArrayColumns converts company_profiles.business_type
// and .industry from a single TEXT value to TEXT[] (multi-select, OR-matched)
// on any database still running the original scalar schema. Existing single
// values are preserved as one-element arrays. Safe to run on every startup:
// it checks the live column type first and does nothing once migrated.
func ensureCompanyProfileArrayColumns(ctx context.Context, db *sql.DB) error {
	for _, col := range []string{"business_type", "industry"} {
		var udtName string
		err := db.QueryRowContext(ctx, `
			SELECT udt_name FROM information_schema.columns
			WHERE table_schema = 'public' AND table_name = 'company_profiles' AND column_name = $1`, col,
		).Scan(&udtName)
		if err == sql.ErrNoRows {
			continue // column doesn't exist yet (shouldn't happen post-initial-schema, but don't fail startup over it)
		}
		if err != nil {
			return fmt.Errorf("check %s column type: %w", col, err)
		}
		if udtName == "_text" {
			continue // already TEXT[]
		}

		stmt := fmt.Sprintf(`
			ALTER TABLE company_profiles ALTER COLUMN %s TYPE TEXT[]
			USING (CASE WHEN %s IS NULL THEN NULL ELSE ARRAY[%s] END)`, col, col, col)
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("alter %s to array: %w", col, err)
		}
	}
	return nil
}
