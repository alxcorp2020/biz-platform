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
	if err := ensureAttachmentTextColumns(ctx, db); err != nil {
		return fmt.Errorf("migrate attachments text columns: %w", err)
	}
	if err := ensureStructuredExtractionColumns(ctx, db); err != nil {
		return fmt.Errorf("migrate structured-extraction columns: %w", err)
	}
	if err := ensureAIExtractionColumns(ctx, db); err != nil {
		return fmt.Errorf("migrate AI-extraction columns: %w", err)
	}
	if err := ensureDocumentChecklistTable(ctx, db); err != nil {
		return fmt.Errorf("migrate document_checklist_items table: %w", err)
	}
	if err := ensureNoticeBookmarksTable(ctx, db); err != nil {
		return fmt.Errorf("migrate notice_bookmarks table: %w", err)
	}
	return nil
}

// ensureNoticeBookmarksTable adds notice_bookmarks(관심공고) for any DB
// created before this migration existed. CREATE TABLE IF NOT EXISTS makes
// it idempotent — same pattern as ensureDocumentChecklistTable.
func ensureNoticeBookmarksTable(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS notice_bookmarks (
			id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			user_id     UUID NOT NULL REFERENCES users(id),
			notice_id   UUID NOT NULL REFERENCES notices(id),
			created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
			UNIQUE (user_id, notice_id)
		);
		CREATE INDEX IF NOT EXISTS idx_notice_bookmarks_notice ON notice_bookmarks(notice_id);
	`)
	return err
}

// ensureDocumentChecklistTable adds document_checklist_items for any DB
// created before this migration existed — the first genuinely new table
// added post-initial-schema (prior migrations only ALTERed existing
// tables). CREATE TABLE IF NOT EXISTS makes it idempotent the same way.
func ensureDocumentChecklistTable(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS document_checklist_items (
			id                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			company_profile_id    UUID NOT NULL REFERENCES company_profiles(id),
			required_document_id  UUID NOT NULL REFERENCES required_documents(id),
			is_checked            BOOLEAN NOT NULL DEFAULT true,
			checked_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
			UNIQUE (company_profile_id, required_document_id)
		);
	`)
	return err
}

// ensureAIExtractionColumns supports analyzer/ai_extract.py (규칙 기반 1차 추출을
// 보완하는 AI 2차 추출): eligibility_conditions/required_documents 행이 규칙
// 기반인지 AI 기반인지 구분하고, AI 기반 행에는 재현성 확인용으로 사용한
// 모델명을 남긴다. ADD COLUMN IF NOT EXISTS makes this idempotent.
func ensureAIExtractionColumns(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		ALTER TABLE eligibility_conditions ADD COLUMN IF NOT EXISTS extraction_method TEXT NOT NULL DEFAULT 'rule'
			CHECK (extraction_method IN ('rule','ai'));
		ALTER TABLE eligibility_conditions ADD COLUMN IF NOT EXISTS model_version TEXT;
		ALTER TABLE required_documents ADD COLUMN IF NOT EXISTS extraction_method TEXT NOT NULL DEFAULT 'rule'
			CHECK (extraction_method IN ('rule','ai'));
		ALTER TABLE required_documents ADD COLUMN IF NOT EXISTS model_version TEXT;
	`)
	return err
}

// ensureStructuredExtractionColumns supports analyzer/extract_sections.py:
//   - eligibility_conditions.review_status gains 'review_required' (already
//     used by notice_versions.review_status elsewhere in this schema).
//   - required_documents gains the same provenance/confidence columns
//     eligibility_conditions already has (confidence, review_status,
//     source_attachment_id, source_page) — it had none of them before.
//
// DROP CONSTRAINT IF EXISTS + re-ADD makes the CHECK update idempotent;
// ADD COLUMN IF NOT EXISTS makes the new columns idempotent.
func ensureStructuredExtractionColumns(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		ALTER TABLE eligibility_conditions DROP CONSTRAINT IF EXISTS eligibility_conditions_review_status_check;
		ALTER TABLE eligibility_conditions ADD CONSTRAINT eligibility_conditions_review_status_check
			CHECK (review_status IN ('pending','confirmed','rejected','review_required'));

		ALTER TABLE required_documents ADD COLUMN IF NOT EXISTS source_page INTEGER;
		ALTER TABLE required_documents ADD COLUMN IF NOT EXISTS source_attachment_id UUID REFERENCES attachments(id);
		ALTER TABLE required_documents ADD COLUMN IF NOT EXISTS confidence NUMERIC(3,2) NOT NULL DEFAULT 0.70;
		ALTER TABLE required_documents ADD COLUMN IF NOT EXISTS review_status TEXT NOT NULL DEFAULT 'pending';
		ALTER TABLE required_documents DROP CONSTRAINT IF EXISTS required_documents_review_status_check;
		ALTER TABLE required_documents ADD CONSTRAINT required_documents_review_status_check
			CHECK (review_status IN ('pending','confirmed','rejected','review_required'));
	`)
	return err
}

// ensureAttachmentTextColumns adds the extracted_text/extraction_error
// columns analyzer/run_extraction.py writes to, for any DB created before
// this migration existed. IF NOT EXISTS makes this safe to run every startup.
func ensureAttachmentTextColumns(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		ALTER TABLE attachments ADD COLUMN IF NOT EXISTS extracted_text TEXT;
		ALTER TABLE attachments ADD COLUMN IF NOT EXISTS extraction_error TEXT;
	`)
	return err
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
