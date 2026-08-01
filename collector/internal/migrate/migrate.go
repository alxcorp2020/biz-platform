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
	if err := ensureAISummaryColumns(ctx, db); err != nil {
		return fmt.Errorf("migrate AI-summary columns: %w", err)
	}
	if err := ensureCompanyLicenseTables(ctx, db); err != nil {
		return fmt.Errorf("migrate company_documents/licenses/certifications tables: %w", err)
	}
	if err := ensureCompanyFinancialTables(ctx, db); err != nil {
		return fmt.Errorf("migrate company_financials/track_records/personnel tables: %w", err)
	}
	if err := ensureEmployeeCountVerificationColumns(ctx, db); err != nil {
		return fmt.Errorf("migrate employee count verification columns: %w", err)
	}
	if err := ensurePipelineTables(ctx, db); err != nil {
		return fmt.Errorf("migrate notice_pipeline_entries/pipeline_checklist_items tables: %w", err)
	}
	if err := ensureNotificationColumns(ctx, db); err != nil {
		return fmt.Errorf("migrate notification columns: %w", err)
	}
	if err := ensureNotificationLogTable(ctx, db); err != nil {
		return fmt.Errorf("migrate notification_log table: %w", err)
	}
	if err := ensureSMSNotificationColumns(ctx, db); err != nil {
		return fmt.Errorf("migrate SMS notification columns: %w", err)
	}
	if err := ensureDocumentCategoryExpansion(ctx, db); err != nil {
		return fmt.Errorf("migrate document category expansion (source_document_type/intellectual property): %w", err)
	}
	if err := ensureBillingTables(ctx, db); err != nil {
		return fmt.Errorf("migrate subscriptions/payment_log tables: %w", err)
	}
	if err := ensureTeamTables(ctx, db); err != nil {
		return fmt.Errorf("migrate company_members/company_invitations tables: %w", err)
	}
	if err := ensureAwardHistoryTable(ctx, db); err != nil {
		return fmt.Errorf("migrate notice_award_history table: %w", err)
	}
	if err := ensureCompanyContactsTable(ctx, db); err != nil {
		return fmt.Errorf("migrate company_contacts table: %w", err)
	}
	if err := ensureDocumentKindColumn(ctx, db); err != nil {
		return fmt.Errorf("migrate company_documents.document_kind column: %w", err)
	}
	if err := ensureReportsTable(ctx, db); err != nil {
		return fmt.Errorf("migrate reports table: %w", err)
	}
	if err := ensureReportEventTypes(ctx, db); err != nil {
		return fmt.Errorf("migrate notification_log report event types: %w", err)
	}
	if err := ensureAwardedAmountColumn(ctx, db); err != nil {
		return fmt.Errorf("migrate notice_pipeline_entries.awarded_amount column: %w", err)
	}
	if err := ensurePendingPlanColumn(ctx, db); err != nil {
		return fmt.Errorf("migrate subscriptions.pending_plan column: %w", err)
	}
	if err := ensureLastLoginColumn(ctx, db); err != nil {
		return fmt.Errorf("migrate users.last_login_at column: %w", err)
	}
	if err := ensureCompanyProfileSnapshotColumn(ctx, db); err != nil {
		return fmt.Errorf("migrate notice_pipeline_entries.company_profile_snapshot column: %w", err)
	}
	if err := ensureDeadlineD7EventType(ctx, db); err != nil {
		return fmt.Errorf("migrate notification_log deadline_d7 event type: %w", err)
	}
	if err := ensurePaymentMethodColumn(ctx, db); err != nil {
		return fmt.Errorf("migrate payment_log.payment_method column: %w", err)
	}
	if err := ensureContactNotificationColumns(ctx, db); err != nil {
		return fmt.Errorf("migrate company_contacts notification columns: %w", err)
	}
	if err := ensureNotificationDaysBeforeColumn(ctx, db); err != nil {
		return fmt.Errorf("migrate company_profiles.notification_days_before column: %w", err)
	}
	if err := ensureNotificationLogContactIDColumn(ctx, db); err != nil {
		return fmt.Errorf("migrate notification_log.contact_id column: %w", err)
	}
	if err := ensureInAppNotificationsTable(ctx, db); err != nil {
		return fmt.Errorf("migrate in_app_notifications table: %w", err)
	}
	if err := ensureDocumentExtractionAutomationColumns(ctx, db); err != nil {
		return fmt.Errorf("migrate document extraction automation columns: %w", err)
	}
	return nil
}

// ensureInAppNotificationsTable adds in_app_notifications — "인앱 알림함"
// 전용 테이블(notification_log와 별개, db/migrations/001_init.sql 주석
// 참고: 채널별로 갈라지지 않고 이벤트 1건당 1행만 쌓는다). CREATE TABLE
// IF NOT EXISTS makes this idempotent for DBs created before this migration.
func ensureInAppNotificationsTable(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS in_app_notifications (
			id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			company_profile_id UUID REFERENCES company_profiles(id),
			user_id            UUID REFERENCES users(id),
			event_type         TEXT NOT NULL,
			title              TEXT NOT NULL,
			body               TEXT NOT NULL,
			pipeline_entry_id  UUID REFERENCES notice_pipeline_entries(id),
			notice_id          UUID REFERENCES notices(id),
			read_at            TIMESTAMPTZ,
			created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
			CONSTRAINT in_app_notifications_scope_check CHECK (
				(company_profile_id IS NOT NULL AND user_id IS NULL) OR
				(company_profile_id IS NULL AND user_id IS NOT NULL)
			)
		);
		CREATE INDEX IF NOT EXISTS idx_in_app_notifications_profile ON in_app_notifications(company_profile_id, created_at DESC);
		CREATE INDEX IF NOT EXISTS idx_in_app_notifications_user ON in_app_notifications(user_id, created_at DESC);
	`)
	return err
}

// ensureDocumentExtractionAutomationColumns adds the watermark columns Phase
// 4(공고→제출서류 추출 자동화, collector/internal/api/document_extraction.go)
// uses to avoid reprocessing the same attachment/row on every hourly batch —
// attachments.section_extraction_processed_at(규칙기반 1차) and
// eligibility_conditions/required_documents.ai_supplement_attempted_at(AI
// 보완 2차). db/migrations/001_init.sql 주석 참고.
func ensureDocumentExtractionAutomationColumns(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		ALTER TABLE attachments ADD COLUMN IF NOT EXISTS section_extraction_processed_at TIMESTAMPTZ;
		ALTER TABLE eligibility_conditions ADD COLUMN IF NOT EXISTS ai_supplement_attempted_at TIMESTAMPTZ;
		ALTER TABLE required_documents ADD COLUMN IF NOT EXISTS ai_supplement_attempted_at TIMESTAMPTZ;
	`)
	return err
}

// ensurePaymentMethodColumn adds payment_log.payment_method — Toss의 결제
// 승인 응답(ConfirmResult.Method, 이미 "카드"/"가상계좌"/"계좌이체" 등
// 한글로 내려옴)을 raw_response(JSONB) 안에만 묻어두지 않고 별도 컬럼으로
// 뽑아, 결제내역 화면에서 바로 표시/조회할 수 있게 한다.
func ensurePaymentMethodColumn(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		ALTER TABLE payment_log ADD COLUMN IF NOT EXISTS payment_method TEXT;
	`)
	return err
}

// ensureContactNotificationColumns adds company_contacts.{email,sms,push}_
// notifications_enabled — 알림 수신 여부를 조직 단위(company_profiles.
// phone_number/sms_notifications_enabled)가 아니라 담당자 개인 단위로
// 바꾼다. 기존 회사 공용 SMS 설정을 대체하는 것이므로 email 기본값은
// true(기존 조직 이메일 알림 기본값과 동일), sms는 false(기존과 동일),
// push는 인프라 자체가 아직 없어 항상 false로 시작(나중에 실제로 붙일 때
// 이 컬럼만 켜면 되도록 미리 만들어둠).
func ensureContactNotificationColumns(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		ALTER TABLE company_contacts ADD COLUMN IF NOT EXISTS email_notifications_enabled BOOLEAN NOT NULL DEFAULT true;
		ALTER TABLE company_contacts ADD COLUMN IF NOT EXISTS sms_notifications_enabled BOOLEAN NOT NULL DEFAULT false;
		ALTER TABLE company_contacts ADD COLUMN IF NOT EXISTS push_notifications_enabled BOOLEAN NOT NULL DEFAULT false;
	`)
	return err
}

// ensureNotificationDaysBeforeColumn adds company_profiles.notification_days_
// before — 제출마감 리마인더를 어느 D-N에 보낼지 조직이 고를 수 있게 한다
// (선택 가능한 값은 실제 배치가 도는 7/3/1뿐 — notifications.go의
// sendDeadlineReminders 호출부와 정확히 맞아야 함). 기본값 '{3,1}'은 이
// 설정이 생기기 전까지의 실제 동작(D-3/D-1 고정)과 동일하다.
func ensureNotificationDaysBeforeColumn(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		ALTER TABLE company_profiles ADD COLUMN IF NOT EXISTS notification_days_before INTEGER[] NOT NULL DEFAULT '{3,1}';
	`)
	return err
}

// ensureNotificationLogContactIDColumn adds notification_log.contact_id —
// 마감 리마인더/담당자 상태변경 알림의 중복발송 방지 키가 이제
// company_members(user_id)가 아니라 company_contacts(담당자, 로그인
// 계정이 없을 수도 있음) 기준이라 별도 컬럼이 필요하다. user_id는
// 추천공고 다이제스트(여전히 회원 단위)에서만 계속 쓰인다.
func ensureNotificationLogContactIDColumn(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		ALTER TABLE notification_log ADD COLUMN IF NOT EXISTS contact_id UUID REFERENCES company_contacts(id);
	`)
	return err
}

// ensureCompanyProfileSnapshotColumn adds notice_pipeline_entries.
// company_profile_snapshot — 원클릭 참여검토(Phase 1) 시점의 company_profiles
// 행 전체를 JSONB로 남겨, 나중에 "그때는 이 정보로 판정했다"를 감사할 수
// 있게 한다. ADD COLUMN IF NOT EXISTS makes this idempotent.
func ensureCompanyProfileSnapshotColumn(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		ALTER TABLE notice_pipeline_entries ADD COLUMN IF NOT EXISTS company_profile_snapshot JSONB;
	`)
	return err
}

// ensureDeadlineD7EventType widens notification_log.event_type's CHECK to
// allow 'deadline_d7' — 원클릭 참여검토(Phase 1)가 요구하는 D-7/D-3/D-1
// 알림 예약 중 D-7 추가분. DROP+ADD 방식은 ensureReportEventTypes와 동일.
func ensureDeadlineD7EventType(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		ALTER TABLE notification_log DROP CONSTRAINT IF EXISTS notification_log_event_type_check;
		ALTER TABLE notification_log ADD CONSTRAINT notification_log_event_type_check
			CHECK (event_type IN ('deadline_d7','deadline_d3','deadline_d1','recommendation_digest','assignee_status_change',
			                       'weekly_report','monthly_report'));
	`)
	return err
}

// ensureCompanyFinancialTables adds company_financials/company_track_records/
// company_personnel for any DB created before this migration existed — same
// CREATE TABLE IF NOT EXISTS pattern as ensureCompanyLicenseTables. These
// tables have no status column (unlike company_licenses/certifications) —
// there's no "보유/미보유" concept for a financial figure or a track record,
// just a value that's known or NULL.
func ensureCompanyFinancialTables(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS company_financials (
			id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			company_profile_id  UUID NOT NULL REFERENCES company_profiles(id),
			fiscal_year         INTEGER NOT NULL,
			revenue             BIGINT,
			operating_profit    BIGINT,
			net_income          BIGINT,
			capital             BIGINT,
			total_assets        BIGINT,
			total_liabilities   BIGINT,
			debt_ratio          NUMERIC(6,2),
			current_ratio       NUMERIC(6,2),
			credit_rating       TEXT,
			tax_delinquent      BOOLEAN,
			capital_impairment  BOOLEAN,
			source_document_id  UUID REFERENCES company_documents(id),
			confidence          TEXT NOT NULL CHECK (confidence IN ('A','B','C','D')),
			verified_at         TIMESTAMPTZ,
			created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
			updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
			UNIQUE (company_profile_id, fiscal_year)
		);

		CREATE TABLE IF NOT EXISTS company_track_records (
			id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			company_profile_id  UUID NOT NULL REFERENCES company_profiles(id),
			project_name        TEXT NOT NULL,
			client_name         TEXT,
			contract_date       DATE,
			period_start        DATE,
			period_end          DATE,
			contract_amount     BIGINT,
			project_type        TEXT,
			industry_field      TEXT,
			region              TEXT,
			is_joint_venture    BOOLEAN,
			share_ratio         NUMERIC(5,2),
			scope               TEXT,
			core_technology     TEXT,
			is_completed        BOOLEAN,
			source_document_id  UUID REFERENCES company_documents(id),
			confidence          TEXT NOT NULL CHECK (confidence IN ('A','B','C','D')),
			verified_at         TIMESTAMPTZ,
			created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
			updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
		);
		CREATE INDEX IF NOT EXISTS idx_company_track_records_profile ON company_track_records(company_profile_id);

		CREATE TABLE IF NOT EXISTS company_personnel (
			id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			company_profile_id  UUID NOT NULL REFERENCES company_profiles(id),
			role                TEXT,
			tech_field          TEXT,
			career_years        NUMERIC(4,1),
			tech_grade          TEXT,
			qualifications      TEXT[],
			recent_project      TEXT,
			available_from      DATE,
			source_document_id  UUID REFERENCES company_documents(id),
			confidence          TEXT NOT NULL CHECK (confidence IN ('A','B','C','D')),
			verified_at         TIMESTAMPTZ,
			created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
			updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
		);
		CREATE INDEX IF NOT EXISTS idx_company_personnel_profile ON company_personnel(company_profile_id);
	`)
	return err
}

// ensureCompanyLicenseTables adds company_documents/company_licenses/
// company_certifications for any DB created before this migration existed —
// same CREATE TABLE IF NOT EXISTS pattern as ensureDocumentChecklistTable.
// company_profiles.licenses/certifications (TEXT[]) stay untouched for
// backward compatibility; these tables become the source of truth going
// forward.
func ensureCompanyLicenseTables(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS company_documents (
			id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			company_profile_id UUID NOT NULL REFERENCES company_profiles(id),
			original_filename  TEXT NOT NULL,
			stored_filename    TEXT NOT NULL,
			file_type          TEXT NOT NULL,
			file_size_bytes    BIGINT NOT NULL,
			file_hash          TEXT NOT NULL,
			uploaded_at        TIMESTAMPTZ NOT NULL DEFAULT now()
		);

		CREATE TABLE IF NOT EXISTS company_licenses (
			id                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			company_profile_id    UUID NOT NULL REFERENCES company_profiles(id),
			category              TEXT NOT NULL,
			name                  TEXT NOT NULL,
			registration_number   TEXT,
			issuing_authority     TEXT,
			issued_at             DATE,
			expires_at            DATE,
			applicable_industry   TEXT,
			source_document_id    UUID REFERENCES company_documents(id),
			confidence            TEXT NOT NULL CHECK (confidence IN ('A','B','C','D')),
			status                TEXT NOT NULL CHECK (status IN ('보유','미보유','확인되지않음')),
			verified_at           TIMESTAMPTZ,
			created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
			updated_at            TIMESTAMPTZ NOT NULL DEFAULT now()
		);
		CREATE INDEX IF NOT EXISTS idx_company_licenses_profile ON company_licenses(company_profile_id);

		CREATE TABLE IF NOT EXISTS company_certifications (
			id                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			company_profile_id    UUID NOT NULL REFERENCES company_profiles(id),
			category              TEXT NOT NULL,
			name                  TEXT NOT NULL,
			registration_number   TEXT,
			issuing_authority     TEXT,
			issued_at             DATE,
			expires_at            DATE,
			applicable_industry   TEXT,
			source_document_id    UUID REFERENCES company_documents(id),
			confidence            TEXT NOT NULL CHECK (confidence IN ('A','B','C','D')),
			status                TEXT NOT NULL CHECK (status IN ('보유','미보유','확인되지않음')),
			verified_at           TIMESTAMPTZ,
			created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
			updated_at            TIMESTAMPTZ NOT NULL DEFAULT now()
		);
		CREATE INDEX IF NOT EXISTS idx_company_certifications_profile ON company_certifications(company_profile_id);
	`)
	return err
}

// ensureAISummaryColumns supports analyzer/ai_summarize.py("핵심 3줄 요약",
// claude-sonnet-5): notice_versions에 요약 결과를 저장한다. 재현성 확인용으로
// 사용 모델명(ai_summary_model)과 생성 시각을 함께 남긴다. ADD COLUMN IF NOT
// EXISTS makes this idempotent.
func ensureAISummaryColumns(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		ALTER TABLE notice_versions ADD COLUMN IF NOT EXISTS ai_summary_lines TEXT[];
		ALTER TABLE notice_versions ADD COLUMN IF NOT EXISTS ai_summary_model TEXT;
		ALTER TABLE notice_versions ADD COLUMN IF NOT EXISTS ai_summary_generated_at TIMESTAMPTZ;
	`)
	return err
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

// ensureEmployeeCountVerificationColumns adds company_profiles.employee_count
// verification columns (4대보험 사업장 가입자명부 증빙) for any DB created
// before this migration existed. ADD COLUMN IF NOT EXISTS makes it
// idempotent — same pattern as ensureAISummaryColumns.
func ensureEmployeeCountVerificationColumns(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		ALTER TABLE company_profiles ADD COLUMN IF NOT EXISTS employee_count_source_document_id UUID REFERENCES company_documents(id);
		ALTER TABLE company_profiles ADD COLUMN IF NOT EXISTS employee_count_confidence TEXT CHECK (employee_count_confidence IN ('A','B','C','D'));
		ALTER TABLE company_profiles ADD COLUMN IF NOT EXISTS employee_count_verified_at TIMESTAMPTZ;
	`)
	return err
}

// ensurePipelineTables adds notice_pipeline_entries/pipeline_checklist_items
// for any DB created before this migration existed — same CREATE TABLE IF
// NOT EXISTS pattern as ensureCompanyLicenseTables.
func ensurePipelineTables(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS notice_pipeline_entries (
			id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			company_profile_id  UUID NOT NULL REFERENCES company_profiles(id),
			notice_id           UUID NOT NULL REFERENCES notices(id),
			status              TEXT NOT NULL DEFAULT '검토전'
			                        CHECK (status IN ('검토전','참여검토','승인대기','준비중','제출완료','낙찰','탈락','보류','제외')),
			assignee_name       TEXT,
			decided_at          TIMESTAMPTZ,
			submission_deadline DATE,
			memo                TEXT,
			created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
			updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
			UNIQUE (company_profile_id, notice_id)
		);
		CREATE INDEX IF NOT EXISTS idx_pipeline_entries_company ON notice_pipeline_entries(company_profile_id);

		CREATE TABLE IF NOT EXISTS pipeline_checklist_items (
			id                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			pipeline_entry_id     UUID NOT NULL REFERENCES notice_pipeline_entries(id),
			document_name         TEXT NOT NULL,
			status                TEXT NOT NULL DEFAULT '확인필요'
			                          CHECK (status IN ('보유','갱신필요','신규작성','발급필요','확인필요')),
			required_document_id  UUID REFERENCES required_documents(id),
			created_at            TIMESTAMPTZ NOT NULL DEFAULT now()
		);
		CREATE INDEX IF NOT EXISTS idx_pipeline_checklist_entry ON pipeline_checklist_items(pipeline_entry_id);
	`)
	return err
}

// ensureNotificationColumns adds users.email_notifications_enabled and
// notice_pipeline_entries.assignee_email for any DB created before this
// migration existed. ADD COLUMN IF NOT EXISTS makes it idempotent — same
// pattern as ensureEmployeeCountVerificationColumns.
func ensureNotificationColumns(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		ALTER TABLE users ADD COLUMN IF NOT EXISTS email_notifications_enabled BOOLEAN NOT NULL DEFAULT true;
		ALTER TABLE notice_pipeline_entries ADD COLUMN IF NOT EXISTS assignee_email TEXT;
	`)
	return err
}

// ensureNotificationLogTable adds notification_log for any DB created before
// this migration existed — same CREATE TABLE IF NOT EXISTS pattern as
// ensurePipelineTables. Dedup lookups (skip already-sent notifications) key
// off (event_type, pipeline_entry_id, notice_id, user_id) depending on the
// event, so the index covers all four.
func ensureNotificationLogTable(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS notification_log (
			id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			event_type         TEXT NOT NULL CHECK (event_type IN
			                       ('deadline_d3','deadline_d1','recommendation_digest','assignee_status_change')),
			recipient_email    TEXT NOT NULL,
			user_id            UUID REFERENCES users(id),
			pipeline_entry_id  UUID REFERENCES notice_pipeline_entries(id),
			notice_id          UUID REFERENCES notices(id),
			subject            TEXT NOT NULL,
			status             TEXT NOT NULL CHECK (status IN ('sent','failed')),
			error_message      TEXT,
			created_at         TIMESTAMPTZ NOT NULL DEFAULT now()
		);
		CREATE INDEX IF NOT EXISTS idx_notification_log_dedup
			ON notification_log(event_type, pipeline_entry_id, notice_id, user_id);
	`)
	return err
}

// ensureSMSNotificationColumns adds SMS notification support on top of the
// existing email notification schema: users gain phone_number/
// sms_notifications_enabled (같은 이메일 알림 on/off 패턴), notice_pipeline_entries
// gains assignee_phone (assignee_email과 동일한 자유텍스트 패턴 — 담당자
// 상태변경 알림의 SMS 수신처), notification_log gains channel(email/sms
// 구분)과 recipient_phone. recipient_email은 SMS 단독 발송 행에서 비어있을
// 수 있어 NOT NULL을 해제하고, 대신 "이메일 또는 전화번호 중 최소 하나는
// 있어야 한다"는 테이블 CHECK로 대체한다 — DROP CONSTRAINT IF EXISTS + 재
// ADD로 이 부분만 재실행에도 안전하게 만든다(나머지는 ADD COLUMN IF NOT
// EXISTS/DROP NOT NULL이 이미 멱등적).
func ensureSMSNotificationColumns(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		ALTER TABLE users ADD COLUMN IF NOT EXISTS phone_number TEXT;
		ALTER TABLE users ADD COLUMN IF NOT EXISTS sms_notifications_enabled BOOLEAN NOT NULL DEFAULT false;
		ALTER TABLE notice_pipeline_entries ADD COLUMN IF NOT EXISTS assignee_phone TEXT;
		ALTER TABLE notification_log ADD COLUMN IF NOT EXISTS channel TEXT NOT NULL DEFAULT 'email'
			CHECK (channel IN ('email','sms'));
		ALTER TABLE notification_log ADD COLUMN IF NOT EXISTS recipient_phone TEXT;
		ALTER TABLE notification_log ALTER COLUMN recipient_email DROP NOT NULL;
		ALTER TABLE notification_log DROP CONSTRAINT IF EXISTS notification_log_recipient_check;
		ALTER TABLE notification_log ADD CONSTRAINT notification_log_recipient_check
			CHECK (recipient_email IS NOT NULL OR recipient_phone IS NOT NULL);
		DROP INDEX IF EXISTS idx_notification_log_dedup;
		CREATE INDEX IF NOT EXISTS idx_notification_log_dedup
			ON notification_log(event_type, pipeline_entry_id, notice_id, user_id, channel);
	`)
	return err
}

// ensureDocumentCategoryExpansion adds 증빙서류 17종 확대: source_document_type
// on company_financials/company_track_records (어느 문서로 검증됐는지 구분 —
// 신용평가서/표준재무제표증명/부가가치세과세표준증명은 financials로, 계약서/
// 세금계산서는 track_records로 흡수) + 신규 테이블 company_intellectual_property
// (특허·상표·디자인·실용신안 — 면허/인증의 "보유 여부"와 달리 출원~등록~소멸
// 단계가 이어지는 별개 개념이라 새 테이블로 분리). 두 ALTER는 신규 컬럼 +
// 인라인 CHECK라 ADD COLUMN IF NOT EXISTS만으로 이미 멱등적이다(제약도 컬럼과
// 함께 생기므로 노티피케이션 로그 때처럼 별도 DROP/재ADD가 필요 없음).
func ensureDocumentCategoryExpansion(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		ALTER TABLE company_financials ADD COLUMN IF NOT EXISTS source_document_type TEXT
			CHECK (source_document_type IN ('재무제표','신용평가서','표준재무제표증명','부가가치세과세표준증명','기타'));
		ALTER TABLE company_track_records ADD COLUMN IF NOT EXISTS source_document_type TEXT
			CHECK (source_document_type IN ('수행실적증명서','계약서','세금계산서','기타'));

		CREATE TABLE IF NOT EXISTS company_intellectual_property (
			id                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			company_profile_id    UUID NOT NULL REFERENCES company_profiles(id),
			ip_type               TEXT NOT NULL CHECK (ip_type IN ('특허','상표','디자인','실용신안')),
			title                 TEXT NOT NULL,
			application_number    TEXT,
			registration_number   TEXT,
			applicant_name        TEXT,
			application_date      DATE,
			registration_date     DATE,
			expires_at            DATE,
			status                TEXT NOT NULL CHECK (status IN ('등록','출원중','거절','소멸','확인필요')),
			source_document_id    UUID REFERENCES company_documents(id),
			confidence            TEXT NOT NULL CHECK (confidence IN ('A','B','C','D')),
			verified_at           TIMESTAMPTZ,
			created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
			updated_at            TIMESTAMPTZ NOT NULL DEFAULT now()
		);
		CREATE INDEX IF NOT EXISTS idx_company_ip_profile ON company_intellectual_property(company_profile_id);
	`)
	return err
}

// ensureBillingTables adds subscriptions/payment_log for any DB created
// before this migration existed — same CREATE TABLE IF NOT EXISTS pattern
// as ensurePipelineTables. The updated_at trigger is created conditionally
// since CREATE TRIGGER has no IF NOT EXISTS form.
func ensureBillingTables(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS subscriptions (
			id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			company_profile_id  UUID NOT NULL REFERENCES company_profiles(id),
			plan                TEXT NOT NULL DEFAULT 'free'
			                        CHECK (plan IN ('free','basic','pro','business')),
			status              TEXT NOT NULL DEFAULT 'pending'
			                        CHECK (status IN ('active','cancelled','expired','pending')),
			billing_key         TEXT,
			started_at          TIMESTAMPTZ,
			expires_at          TIMESTAMPTZ,
			amount              BIGINT,
			created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
			updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
			UNIQUE (company_profile_id)
		);

		CREATE TABLE IF NOT EXISTS payment_log (
			id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			subscription_id    UUID NOT NULL REFERENCES subscriptions(id),
			toss_payment_key   TEXT NOT NULL,
			toss_order_id      TEXT NOT NULL,
			amount             BIGINT NOT NULL,
			status             TEXT NOT NULL CHECK (status IN ('승인','실패','취소')),
			requested_at       TIMESTAMPTZ NOT NULL,
			approved_at        TIMESTAMPTZ,
			raw_response       JSONB,
			created_at         TIMESTAMPTZ NOT NULL DEFAULT now()
		);
		CREATE INDEX IF NOT EXISTS idx_payment_log_subscription ON payment_log(subscription_id);
	`); err != nil {
		return err
	}

	var triggerExists bool
	if err := db.QueryRowContext(ctx, `
		SELECT EXISTS (SELECT 1 FROM pg_trigger WHERE tgname = 'trg_subscriptions_updated_at')`,
	).Scan(&triggerExists); err != nil {
		return err
	}
	if !triggerExists {
		if _, err := db.ExecContext(ctx, `
			CREATE TRIGGER trg_subscriptions_updated_at BEFORE UPDATE ON subscriptions
				FOR EACH ROW EXECUTE FUNCTION set_updated_at();
		`); err != nil {
			return err
		}
	}
	return nil
}

// ensureTeamTables adds the team-feature schema for any DB created before
// this migration existed: company_profiles gains org-level notification
// settings(email/phone/sms — 팀기능 스펙: "알림설정은 조직 단위로 공유,
// 로그인 계정은 개별 유지"라 users에 있던 것과 별개로 여기 새로 만든다 —
// users의 동일 이름 컬럼은 지우지 않고 그냥 더 이상 안 쓴다, 히스토리
// 보존 목적), company_members(조직 소속), company_invitations(이메일
// 초대) 테이블을 추가한다.
//
// 데이터 마이그레이션(기존 실사용자 → 조직 자동 이관): 지금까지는
// company_profiles 1건당 소유자 user_id가 정확히 하나였다(1:1 관례,
// DB 제약은 아니었지만 애플리케이션이 항상 그렇게 다뤘음) — 그 user_id를
// company_members에 role='owner'로 그대로 옮기고, 그 사용자가 users에
// 갖고 있던 알림 설정 값을 company_profiles로 복사한다(값 유실 없이
// 그대로 승계). WITH ... RETURNING으로 "이번에 새로 옮겨진 행"만 골라
// 알림 설정을 복사하므로, 재실행해도 이미 옮겨진 행을 다시 건드리지
// 않는다(예: 그 사이 owner가 company_profiles의 알림 설정을 직접 바꿨어도
// 두 번째 실행이 users 쪽 옛값으로 덮어쓰지 않음 — ON CONFLICT (user_id)
// DO NOTHING이 이미 옮겨진 프로필을 RETURNING에서 제외시킨다).
func ensureTeamTables(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `
		ALTER TABLE company_profiles ADD COLUMN IF NOT EXISTS email_notifications_enabled BOOLEAN NOT NULL DEFAULT true;
		ALTER TABLE company_profiles ADD COLUMN IF NOT EXISTS phone_number TEXT;
		ALTER TABLE company_profiles ADD COLUMN IF NOT EXISTS sms_notifications_enabled BOOLEAN NOT NULL DEFAULT false;

		CREATE TABLE IF NOT EXISTS company_members (
			id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			company_profile_id  UUID NOT NULL REFERENCES company_profiles(id),
			user_id             UUID NOT NULL REFERENCES users(id),
			role                TEXT NOT NULL CHECK (role IN ('owner','member')),
			created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
			UNIQUE (user_id)
		);
		CREATE INDEX IF NOT EXISTS idx_company_members_profile ON company_members(company_profile_id);

		CREATE TABLE IF NOT EXISTS company_invitations (
			id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			company_profile_id  UUID NOT NULL REFERENCES company_profiles(id),
			email               TEXT NOT NULL,
			token               TEXT NOT NULL UNIQUE,
			invited_by_user_id  UUID NOT NULL REFERENCES users(id),
			status              TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','accepted','expired','cancelled')),
			expires_at          TIMESTAMPTZ NOT NULL,
			created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
			accepted_at         TIMESTAMPTZ
		);
		CREATE INDEX IF NOT EXISTS idx_company_invitations_token ON company_invitations(token);
		CREATE INDEX IF NOT EXISTS idx_company_invitations_profile ON company_invitations(company_profile_id);
	`); err != nil {
		return err
	}

	_, err := db.ExecContext(ctx, `
		WITH newly_migrated AS (
			INSERT INTO company_members (company_profile_id, user_id, role)
			SELECT cp.id, cp.user_id, 'owner'
			FROM company_profiles cp
			ON CONFLICT (user_id) DO NOTHING
			RETURNING company_profile_id, user_id
		)
		UPDATE company_profiles cp
		SET email_notifications_enabled = u.email_notifications_enabled,
		    phone_number = u.phone_number,
		    sms_notifications_enabled = u.sms_notifications_enabled
		FROM newly_migrated nm
		JOIN users u ON u.id = nm.user_id
		WHERE cp.id = nm.company_profile_id;
	`)
	return err
}

// ensureAwardHistoryTable adds notice_award_history for any DB created
// before this migration existed — same CREATE TABLE IF NOT EXISTS pattern
// as ensurePipelineTables. 조달청 나라장터 낙찰정보서비스(ScsbidInfoService)
// 수집기는 API 활용신청 승인 후 별도로 붙는다 — 이 테이블은 그 전까지
// 빈 상태로 있어도 notice-detail의 "동일 발주기관 과거 낙찰 이력" 조회가
// 정상적으로 "아직 수집된 낙찰 이력이 없습니다"를 반환하도록 미리 만들어둔다.
func ensureAwardHistoryTable(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS notice_award_history (
			id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			source_id         UUID NOT NULL REFERENCES data_sources(id),
			external_bid_id   TEXT NOT NULL,
			organization_name TEXT NOT NULL,
			industry          TEXT,
			title             TEXT,
			winner_name       TEXT,
			award_amount      BIGINT,
			award_rate        NUMERIC(6,3),
			budget_amount     BIGINT,
			opened_at         DATE,
			raw_payload       TEXT,
			collected_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
			UNIQUE (source_id, external_bid_id)
		);
		CREATE INDEX IF NOT EXISTS idx_award_history_org ON notice_award_history(organization_name);
		CREATE INDEX IF NOT EXISTS idx_award_history_industry ON notice_award_history(industry);
	`)
	return err
}

// ensureCompanyContactsTable adds company_contacts — 참여 검토(파이프라인
// 생성) 시 담당자 정보를 자동 채우기 위해 회사가 미리 등록해두는 담당자
// 목록. is_default를 하나로 유지하는 책임은 애플리케이션 코드
// (company_contacts.go)에 있다 — 자세한 이유는 db/migrations/001_init.sql의
// 같은 테이블 주석 참고.
func ensureCompanyContactsTable(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS company_contacts (
			id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			company_profile_id UUID NOT NULL REFERENCES company_profiles(id),
			name               TEXT NOT NULL,
			email              TEXT,
			phone              TEXT,
			is_default         BOOLEAN NOT NULL DEFAULT false,
			created_at         TIMESTAMPTZ NOT NULL DEFAULT now()
		);
		CREATE INDEX IF NOT EXISTS idx_company_contacts_profile ON company_contacts(company_profile_id);
	`)
	return err
}

// ensureReportsTable adds the weekly/monthly report table for any DB
// created before this migration existed — see db/migrations/001_init.sql's
// comment on the same table for why it's one table with period_type rather
// than two identical weekly_reports/monthly_reports tables.
func ensureReportsTable(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS reports (
			id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			company_profile_id UUID NOT NULL REFERENCES company_profiles(id),
			period_type        TEXT NOT NULL CHECK (period_type IN ('weekly','monthly')),
			period_start       DATE NOT NULL,
			period_end         DATE NOT NULL,
			summary            JSONB NOT NULL,
			generated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
			UNIQUE (company_profile_id, period_type, period_start)
		);
		CREATE INDEX IF NOT EXISTS idx_reports_profile ON reports(company_profile_id, period_start DESC);
	`)
	return err
}

// ensureReportEventTypes widens notification_log.event_type's CHECK to allow
// 'weekly_report'/'monthly_report' — an ALTER, not just an ADD COLUMN, so it
// needs the explicit DROP+ADD dance. The name used here
// (notification_log_event_type_check) matches Postgres's own auto-generated
// name for the original single-column inline CHECK in 001_init.sql, so this
// is safe on both a fresh install (where the constraint already has this
// exact name) and a pre-existing DB (where DROP IF EXISTS + re-ADD replaces
// the narrower check) — no duplicate-constraint risk either way.
func ensureReportEventTypes(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		ALTER TABLE notification_log DROP CONSTRAINT IF EXISTS notification_log_event_type_check;
		ALTER TABLE notification_log ADD CONSTRAINT notification_log_event_type_check
			CHECK (event_type IN ('deadline_d3','deadline_d1','recommendation_digest','assignee_status_change',
			                       'weekly_report','monthly_report'));
	`)
	return err
}

// ensureDocumentKindColumn adds company_documents.document_kind — records
// which of the 6 AI-extraction upload endpoints created the row, so the AI
// 사용내역 화면(billing_ai_usage.go) can show "어떤 서류를" without a
// separate log table. Rows uploaded before this column existed get NULL.
func ensureDocumentKindColumn(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		ALTER TABLE company_documents ADD COLUMN IF NOT EXISTS document_kind TEXT;
	`)
	return err
}

// ensureAwardedAmountColumn adds notice_pipeline_entries.awarded_amount —
// 성장분석(growth_analytics.go) ROI 계산의 근거. 사용자가 상태를 '낙찰'로
// 바꿀 때 직접 입력한다(공고 예산액과 실제 낙찰금액은 다를 수 있어
// notices.budget_amount로 대체하지 않는다).
func ensureAwardedAmountColumn(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		ALTER TABLE notice_pipeline_entries ADD COLUMN IF NOT EXISTS awarded_amount BIGINT;
	`)
	return err
}

// ensurePendingPlanColumn adds subscriptions.pending_plan — 예약 다운그레이드
// (즉시 업그레이드/예약 다운그레이드 정책, api/billing.go 참고). 단일
// 컬럼 inline CHECK라 ADD COLUMN IF NOT EXISTS만으로 이미 멱등이다(DROP+
// ADD CONSTRAINT 춤이 필요 없음 — notification_log.event_type 때와 다른
// 케이스: 그건 기존 값의 CHECK 범위를 "넓히는" 변경이었고, 이건 신규
// 컬럼 추가라 fresh install/기존 DB 둘 다 이 한 줄로 동일하게 끝난다).
func ensurePendingPlanColumn(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		ALTER TABLE subscriptions ADD COLUMN IF NOT EXISTS pending_plan TEXT
			CHECK (pending_plan IN ('free','basic','pro','business'));
	`)
	return err
}

// ensureLastLoginColumn adds users.last_login_at — 관리자 화면(admin.go)
// 회원목록의 "마지막 로그인" 컬럼 근거. handleLogin이 로그인 성공마다 갱신.
func ensureLastLoginColumn(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		ALTER TABLE users ADD COLUMN IF NOT EXISTS last_login_at TIMESTAMPTZ;
	`)
	return err
}
