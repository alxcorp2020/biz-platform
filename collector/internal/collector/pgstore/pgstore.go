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

// nullIfEmpty — 빈 문자열은 SQL NULL로 저장(TEXT 컬럼에서 ""와 NULL을 섞지
// 않게). success_bid_method_name처럼 값이 없으면 미상으로 두는 게 맞는 필드용.
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
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
		SELECT id, title, organization_name, department_name, region, industry, status,
		       application_start_at, application_end_at, budget_amount, support_amount,
		       official_url, current_version
		FROM notices WHERE source_id = $1 AND external_notice_id = $2`, p.sourceID, externalID)

	var rec store.NoticeRecord
	var n collector.NormalizedNotice
	err := row.Scan(&rec.ID, &n.Title, &n.OrganizationName, &n.DepartmentName, &n.Region, &n.Industry, &n.Status,
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

func (p *PgStore) CreateNotice(ctx context.Context, notice collector.NormalizedNotice, rawDocID string) (string, string, error) {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return "", "", err
	}
	defer tx.Rollback()

	var noticeID string
	err = tx.QueryRowContext(ctx, `
		INSERT INTO notices
			(source_id, external_notice_id, notice_type, title, organization_name, department_name, region, industry,
			 published_at, application_start_at, application_end_at, budget_amount, support_amount,
			 status, official_url, region_restricted,
			 procurement_class_code, procurement_class_large, procurement_class_detail, industry_restricted,
			 application_start_datetime, application_end_datetime, qualification_deadline_at, opening_at, rebid_opening_at, success_bid_method_name,
			 current_version)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,1)
		RETURNING id`,
		p.sourceID, notice.ExternalNoticeID, notice.NoticeType, notice.Title, notice.OrganizationName, notice.DepartmentName,
		notice.Region, notice.Industry, notice.PublishedAt, notice.ApplicationStartAt, notice.ApplicationEndAt,
		notice.BudgetAmount, notice.SupportAmount, notice.Status, notice.OfficialURL, notice.RegionRestricted,
		notice.ProcurementClassCode, notice.ProcurementClassLarge, notice.ProcurementClassDetail, notice.IndustryRestricted,
		notice.ApplicationStartDatetime, notice.ApplicationEndDatetime, notice.QualificationDeadlineAt, notice.OpeningAt, notice.RebidOpeningAt, nullIfEmpty(notice.SuccessBidMethodName),
	).Scan(&noticeID)
	if err != nil {
		return "", "", fmt.Errorf("insert notice: %w", err)
	}

	var versionID string
	err = tx.QueryRowContext(ctx, `
		INSERT INTO notice_versions (notice_id, version_number, raw_document_id, change_type, is_current)
		VALUES ($1, 1, $2, 'initial', true) RETURNING id`, noticeID, rawDocID,
	).Scan(&versionID)
	if err != nil {
		return "", "", fmt.Errorf("insert notice_version: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return "", "", err
	}
	// 지원사업 전용 상세는 notice 커밋 후 별도 UPSERT(비원자적) — 상세 저장 실패가
	// 공고 수집 자체를 롤백시키지 않도록(부차 데이터, 재수집 시 재갱신). g2b는 nil.
	p.upsertSupportProgramDetail(ctx, noticeID, notice.SupportDetail)
	return noticeID, versionID, nil
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
			title=$2, organization_name=$3, department_name=$4, region=$5, industry=$6, status=$7,
			application_start_at=$8, application_end_at=$9, budget_amount=$10, support_amount=$11,
			official_url=$12, current_version=$13, region_restricted=$14,
			procurement_class_code=$15, procurement_class_large=$16, procurement_class_detail=$17, industry_restricted=$18,
			application_start_datetime=$19, application_end_datetime=$20, qualification_deadline_at=$21,
			opening_at=$22, rebid_opening_at=$23, success_bid_method_name=$24,
			last_verified_at=now()
		WHERE id=$1`,
		noticeID, notice.Title, notice.OrganizationName, notice.DepartmentName, notice.Region, notice.Industry, notice.Status,
		notice.ApplicationStartAt, notice.ApplicationEndAt, notice.BudgetAmount, notice.SupportAmount,
		notice.OfficialURL, newVerNum, notice.RegionRestricted,
		notice.ProcurementClassCode, notice.ProcurementClassLarge, notice.ProcurementClassDetail, notice.IndustryRestricted,
		notice.ApplicationStartDatetime, notice.ApplicationEndDatetime, notice.QualificationDeadlineAt,
		notice.OpeningAt, notice.RebidOpeningAt, nullIfEmpty(notice.SuccessBidMethodName))
	if err != nil {
		return "", 0, err
	}

	if err := tx.Commit(); err != nil {
		return "", 0, err
	}
	p.upsertSupportProgramDetail(ctx, noticeID, notice.SupportDetail)
	return versionID, newVerNum, nil
}

// upsertSupportProgramDetail — 지원사업(support_program) 전용 공식 데이터를
// support_program_details에 notice_id 기준 UPSERT한다. detail이 nil(입찰 등)이면
// no-op. 공식 API 재수집마다 최신 값으로 덮어쓴다(수집기 정책과 일관 — notices
// upsert와 동일). 실패는 로그만 남기고 공고 수집을 막지 않는다(부차 데이터).
func (p *PgStore) upsertSupportProgramDetail(ctx context.Context, noticeID string, d *collector.SupportProgramDetail) {
	if d == nil {
		return
	}
	_, err := p.db.ExecContext(ctx, `
		INSERT INTO support_program_details
			(notice_id, support_target, business_summary_html, business_summary_text,
			 application_method, reference_contact, application_url,
			 support_category_major, support_category_middle, hashtags, inquiry_count, source_updated_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12, now())
		ON CONFLICT (notice_id) DO UPDATE SET
			support_target = EXCLUDED.support_target,
			business_summary_html = EXCLUDED.business_summary_html,
			business_summary_text = EXCLUDED.business_summary_text,
			application_method = EXCLUDED.application_method,
			reference_contact = EXCLUDED.reference_contact,
			application_url = EXCLUDED.application_url,
			support_category_major = EXCLUDED.support_category_major,
			support_category_middle = EXCLUDED.support_category_middle,
			hashtags = EXCLUDED.hashtags,
			inquiry_count = EXCLUDED.inquiry_count,
			source_updated_at = EXCLUDED.source_updated_at,
			updated_at = now()`,
		noticeID, nullIfEmpty(d.SupportTarget), nullIfEmpty(d.BusinessSummaryHTML), nullIfEmpty(d.BusinessSummaryText),
		nullIfEmpty(d.ApplicationMethod), nullIfEmpty(d.ReferenceContact), nullIfEmpty(d.ApplicationURL),
		nullIfEmpty(d.CategoryMajor), nullIfEmpty(d.CategoryMiddle), nullIfEmpty(d.Hashtags), d.InquiryCount, d.SourceUpdatedAt)
	if err != nil {
		// 로거가 pgstore엔 없어(원본 계층) 조용히 무시 — 공고/원본은 이미 저장됨.
		// 재수집 시 다시 UPSERT되므로 일시 실패는 자연 복구된다.
		_ = err
	}
}

// RecordChanges resolves each ChangeRecord's from/to version *numbers* into
// the notice_versions row ids the notice_changes table's foreign keys
// actually require (to_version_id is NOT NULL).
func (p *PgStore) RecordChanges(ctx context.Context, changes []store.ChangeRecord) error {
	for _, c := range changes {
		var toVersionID string
		if err := p.db.QueryRowContext(ctx,
			`SELECT id FROM notice_versions WHERE notice_id = $1 AND version_number = $2`,
			c.NoticeID, c.ToVersion,
		).Scan(&toVersionID); err != nil {
			return fmt.Errorf("resolve to_version_id: %w", err)
		}

		var fromVersionID sql.NullString
		if c.FromVersion > 0 {
			var id string
			if err := p.db.QueryRowContext(ctx,
				`SELECT id FROM notice_versions WHERE notice_id = $1 AND version_number = $2`,
				c.NoticeID, c.FromVersion,
			).Scan(&id); err == nil {
				fromVersionID = sql.NullString{String: id, Valid: true}
			}
		}

		_, err := p.db.ExecContext(ctx, `
			INSERT INTO notice_changes (notice_id, from_version_id, to_version_id, changed_field, old_value, new_value, importance)
			VALUES ($1,$2,$3,$4,$5,$6,$7)`,
			c.NoticeID, fromVersionID, toVersionID, c.Field, c.OldValue, c.NewValue, c.Importance)
		if err != nil {
			return fmt.Errorf("insert notice_change: %w", err)
		}
	}
	return nil
}

func (p *PgStore) SaveAttachment(ctx context.Context, att store.AttachmentRecord) (string, error) {
	var id string
	err := p.db.QueryRowContext(ctx, `
		INSERT INTO attachments
			(notice_version_id, original_filename, stored_filename, file_type, file_size_bytes,
			 file_hash, download_url, download_status)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		RETURNING id`,
		att.NoticeVersionID, att.OriginalFilename, att.StoredKey, att.FileType, att.FileSizeBytes,
		att.FileHash, att.DownloadURL, att.DownloadStatus,
	).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("insert attachment: %w", err)
	}
	return id, nil
}

func (p *PgStore) FindAttachmentByDownloadURL(ctx context.Context, downloadURL string) (*store.AttachmentRecord, bool, error) {
	var rec store.AttachmentRecord
	rec.DownloadURL = downloadURL
	err := p.db.QueryRowContext(ctx, `
		SELECT original_filename, stored_filename, file_type, file_size_bytes, file_hash, download_status
		FROM attachments WHERE download_url = $1 AND download_status = 'completed'
		ORDER BY created_at DESC LIMIT 1`, downloadURL,
	).Scan(&rec.OriginalFilename, &rec.StoredKey, &rec.FileType, &rec.FileSizeBytes, &rec.FileHash, &rec.DownloadStatus)
	if err == sql.ErrNoRows {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("query attachment: %w", err)
	}
	return &rec, true, nil
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
