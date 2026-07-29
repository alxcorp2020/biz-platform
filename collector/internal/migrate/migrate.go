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
// the "notices" table already exists.
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
	if exists {
		return nil
	}

	if _, err := db.ExecContext(ctx, initSQL); err != nil {
		return fmt.Errorf("apply initial schema: %w", err)
	}
	return nil
}
