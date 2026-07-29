// Package pgstore implements store.Store against PostgreSQL, matching the
// schema in db/migrations/001_init.sql. This is the production replacement
// for store.InMemoryStore — the runner package depends only on the Store
// interface, so nothing outside this file changes when swapping stores.
package pgstore

import (
	"context"
	"database/sql"
	"fmt"

	_ "github.com/lib/pq"

	"biz-platform/collector/internal/collector"
	"biz-platform/collector/internal/collector/store"
	"biz-platform/collector/internal/migrate"
)

type PgStore struct {
	db       *sql.DB
	sourceID string // resolved data_sources.id for the running collector's SourceCode()
}

// Open connects to Postgres and resolves (or creates) the data_sources row
// for the given source code, so callers don't need to manage that separately.
func Open(ctx context.Context, dsn string, sourceCode, sourceName, sourceType, baseURL string) (*PgStore, error) {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	if err := db.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("ping db: %w", err)
	}
	if err := migrate.Apply(ctx, db); err != nil {
		return nil, fmt.Errorf("apply schema: %w", err)
	}

	var sourceID string
	err = db.QueryRowContext(ctx, `
		INSERT INTO data_sources (code, name, source_type, base_url)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (code) DO UPDATE SET base_url = EXCLUDED.base_url
		RETURNING id`, sourceCode, sourceName, sourceType, baseURL).Scan(&sourceID)
	if err != nil {
		return nil, fmt.Errorf("upsert data_source: %w", err)
	}

	return &PgStore{db: db, sourceID: sourceID}, nil
}

func (p *PgStore) Close() error { return p.db.Close() }

func (p *PgStore) SaveRawDocument(ctx context.Context, doc collector.RawDocument) (string, string, error) {
	var id, hash string
	err := p.db.QueryRowContext(ctx, `
		INSERT INTO raw_documents
			(source_id, external_notice_id, request_url, response_status, raw_content, content_hash, collector_version)
		VALUES ($1, $2, $3, $4, $5, encode(digest($5, 'sha256'), 'hex'), 'v0.1')
		RETURNING id, content_hash`,
		p.sourceID, doc.ExternalNoticeID, doc.RequestURL, doc.ResponseStatus, doc.RawContent,
	).Scan(&id, &hash)
	if err != nil {
		return "", "", fmt.Errorf("insert raw_document: %w", err)
	}
	return id, hash, nil
}

func (p *PgStore) FindNoticeBySourceAndExternalID(ctx context.Context, sourceID, externalID string) (*store.NoticeRecord, bool, error) {
	row := p.db.QueryRowContext(ctx, `
		SELECT id, title, organization_name, region, industry, status,
		       application_start_at, application_end_at, budget_amount, support_amount,
		       official_url, current_version
		FROM notices WHERE source_id = $1 AND external_notice_id = $2`, p.sourceID, externalID)

	var rec store.NoticeRecord
	var n collector.NormalizedNotice
	err := row.Scan(&rec.ID, &n.Title, &n.OrganizationName, &n.Region, &n.Industry, &n.Status,
		&n.ApplicationStartAt, &n.ApplicationEndAt, &n.BudgetAmount, &n.SupportAmount,
		&n.OfficialURL, &rec.CurrentVersion)
	if err == sql.ErrNoRows {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("query notice: %w", err)
	}
	n.SourceID = sourceID
	n.ExternalNoticeID = externalID
	rec.Notice = n
	return &rec, true, nil
}

func (p *PgStore) CreateNotice(ctx context.Context, notice collector.NormalizedNotice, rawDocID string) (string, error) {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()

	var noticeID string
	err = tx.QueryRowContext(ctx, `
		INSERT INTO notices
			(source_id, external_notice_id, notice_type, title, organization_name, region, industry,
			 published_at, application_start_at, application_end_at, budget_amount, support_amount,
			 status, official_url, current_version)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,1)
		RETURNING id`,
		p.sourceID, notice.ExternalNoticeID, notice.NoticeType, notice.Title, notice.OrganizationName,
		notice.Region, notice.Industry, notice.PublishedAt, notice.ApplicationStartAt, notice.ApplicationEndAt,
		notice.BudgetAmount, notice.SupportAmount, notice.Status, notice.OfficialURL,
	).Scan(&noticeID)
	if err != nil {
		return "", fmt.Errorf("insert notice: %w", err)
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO notice_versions (notice_id, version_number, raw_document_id, change_type, is_current)
		VALUES ($1, 1, $2, 'initial', true)`, noticeID, rawDocID)
	if err != nil {
		return "", fmt.Errorf("insert notice_version: %w", err)
	}

	return noticeID, tx.Commit()
}

func (p *PgStore) AddNewVersion(ctx context.Context, noticeID string, notice collector.NormalizedNotice, rawDocID string, changeType string) (string, int, error) {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return "", 0, err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `UPDATE notice_versions SET is_current = false WHERE notice_id = $1`, noticeID); err != nil {
		return "", 0, err
	}

	var newVerNum int
	if err := tx.QueryRowContext(ctx, `SELECT current_version + 1 FROM notices WHERE id = $1 FOR UPDATE`, noticeID).Scan(&newVerNum); err != nil {
		return "", 0, err
	}

	var versionID string
	err = tx.QueryRowContext(ctx, `
		INSERT INTO notice_versions (notice_id, version_number, raw_document_id, change_type, is_current)
		VALUES ($1, $2, $3, $4, true) RETURNING id`, noticeID, newVerNum, rawDocID, changeType).Scan(&versionID)
	if err != nil {
		return "", 0, err
	}

	_, err = tx.ExecContext(ctx, `
		UPDATE notices SET
			title=$2, organization_name=$3, region=$4, industry=$5, status=$6,
			application_start_at=$7, application_end_at=$8, budget_amount=$9, support_amount=$10,
			official_url=$11, current_version=$12, last_verified_at=now()
		WHERE id=$1`,
		noticeID, notice.Title, notice.OrganizationName, notice.Region, notice.Industry, notice.Status,
		notice.ApplicationStartAt, notice.ApplicationEndAt, notice.BudgetAmount, notice.SupportAmount,
		notice.OfficialURL, newVerNum)
	if err != nil {
		return "", 0, err
	}

	return versionID, newVerNum, tx.Commit()
}

func (p *PgStore) RecordChanges(ctx context.Context, changes []store.ChangeRecord) error {
	for _, c := range changes {
		_, err := p.db.ExecContext(ctx, `
			INSERT INTO notice_changes (notice_id, changed_field, old_value, new_value, importance)
			VALUES ($1,$2,$3,$4,$5)`, c.NoticeID, c.Field, c.OldValue, c.NewValue, c.Importance)
		if err != nil {
			return fmt.Errorf("insert notice_change: %w", err)
		}
	}
	return nil
}

func (p *PgStore) LastRawContentHash(ctx context.Context, noticeID string) (string, error) {
	var hash string
	err := p.db.QueryRowContext(ctx, `
		SELECT rd.content_hash FROM notice_versions nv
		JOIN raw_documents rd ON rd.id = nv.raw_document_id
		WHERE nv.notice_id = $1 AND nv.is_current = true`, noticeID).Scan(&hash)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return hash, err
}
