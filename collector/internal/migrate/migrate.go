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
	if err := ensureCompanyDocumentsExtractionStatusColumns(ctx, db); err != nil {
		return fmt.Errorf("migrate company_documents extraction status columns: %w", err)
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
	if err := ensureRefundAndCancellationColumns(ctx, db); err != nil {
		return fmt.Errorf("migrate refund/cancellation columns: %w", err)
	}
	if err := ensureTeamInviteEventTypes(ctx, db); err != nil {
		return fmt.Errorf("migrate team invite event types: %w", err)
	}
	if err := ensurePushSubscriptionsTable(ctx, db); err != nil {
		return fmt.Errorf("migrate push_subscriptions table: %w", err)
	}
	if err := ensureSystemSettingsTable(ctx, db); err != nil {
		return fmt.Errorf("migrate system_settings table: %w", err)
	}
	if err := ensurePlanSettingsSeed(ctx, db); err != nil {
		return fmt.Errorf("migrate plan settings seed: %w", err)
	}
	if err := ensureNotificationLogSkippedStatus(ctx, db); err != nil {
		return fmt.Errorf("migrate notification_log skipped status: %w", err)
	}
	if err := ensureOAuthIdentitiesTable(ctx, db); err != nil {
		return fmt.Errorf("migrate user_oauth_identities table: %w", err)
	}
	if err := ensureBannersTable(ctx, db); err != nil {
		return fmt.Errorf("migrate banners table: %w", err)
	}
	if err := ensurePopupsTable(ctx, db); err != nil {
		return fmt.Errorf("migrate popups table: %w", err)
	}
	if err := ensureAnnouncementsTable(ctx, db); err != nil {
		return fmt.Errorf("migrate announcements table: %w", err)
	}
	if err := ensureBroadcastMessagesTable(ctx, db); err != nil {
		return fmt.Errorf("migrate broadcast_messages table: %w", err)
	}
	if err := ensureAdminBroadcastEventType(ctx, db); err != nil {
		return fmt.Errorf("migrate admin_broadcast event type: %w", err)
	}
	if err := ensureCompanyInfoTable(ctx, db); err != nil {
		return fmt.Errorf("migrate company_info table: %w", err)
	}
	if err := ensureCompanyInfoBrandNameColumn(ctx, db); err != nil {
		return fmt.Errorf("migrate company_info.brand_name column: %w", err)
	}
	if err := ensureCompanyInfoMailOrderNumberColumn(ctx, db); err != nil {
		return fmt.Errorf("migrate company_info.mail_order_registration_number column: %w", err)
	}
	// company_info와 banners가 둘 다 있어야 하므로 맨 마지막에 실행.
	if err := ensureBannersBrandNameTokenBackfill(ctx, db); err != nil {
		return fmt.Errorf("migrate banners brand_name token backfill: %w", err)
	}
	if err := ensureUserDeactivationColumn(ctx, db); err != nil {
		return fmt.Errorf("migrate users.deactivated_at column: %w", err)
	}
	if err := ensureCompanyProfileCustomAILimitColumns(ctx, db); err != nil {
		return fmt.Errorf("migrate company_profiles custom AI limit columns: %w", err)
	}
	if err := ensureTermsAgreementsTable(ctx, db); err != nil {
		return fmt.Errorf("migrate terms_agreements table: %w", err)
	}
	if err := ensureLegalDocumentsTable(ctx, db); err != nil {
		return fmt.Errorf("migrate legal_documents table: %w", err)
	}
	if err := ensureLegalDocumentsSeed(ctx, db); err != nil {
		return fmt.Errorf("migrate legal_documents seed: %w", err)
	}
	if err := ensurePhoneVerificationTables(ctx, db); err != nil {
		return fmt.Errorf("migrate phone verification tables: %w", err)
	}
	if err := ensureAuthLookupAndPasswordResetTables(ctx, db); err != nil {
		return fmt.Errorf("migrate auth lookup/password reset tables: %w", err)
	}
	if err := ensurePasswordResetEventType(ctx, db); err != nil {
		return fmt.Errorf("migrate password reset event type: %w", err)
	}
	if err := ensurePhoneVerificationRequiredSetting(ctx, db); err != nil {
		return fmt.Errorf("migrate phone verification required setting: %w", err)
	}
	if err := ensureEmailVerificationTables(ctx, db); err != nil {
		return fmt.Errorf("migrate email verification tables: %w", err)
	}
	if err := ensureEmailVerificationEventType(ctx, db); err != nil {
		return fmt.Errorf("migrate email verification event type: %w", err)
	}
	if err := ensureAuthLookupKindEmailVerifyResend(ctx, db); err != nil {
		return fmt.Errorf("migrate auth lookup kind email verify resend: %w", err)
	}
	if err := ensureBusinessRegistrationColumns(ctx, db); err != nil {
		return fmt.Errorf("migrate business registration columns: %w", err)
	}
	if err := ensureAuthLookupKindBizRegExtract(ctx, db); err != nil {
		return fmt.Errorf("migrate auth lookup kind biz reg extract: %w", err)
	}
	if err := ensureOnboardingCompletedAtColumn(ctx, db); err != nil {
		return fmt.Errorf("migrate company_profiles.onboarding_completed_at column: %w", err)
	}
	if err := ensureNoticeRegionRestrictedColumn(ctx, db); err != nil {
		return fmt.Errorf("migrate notices.region_restricted column: %w", err)
	}
	if err := ensureNoticeProcurementClassColumns(ctx, db); err != nil {
		return fmt.Errorf("migrate notices procurement class columns: %w", err)
	}
	if err := ensureIndustryTaxonomyTable(ctx, db); err != nil {
		return fmt.Errorf("migrate industry_taxonomy table: %w", err)
	}
	if err := migrateCompanyIndustryGroupsToMids(ctx, db); err != nil {
		return fmt.Errorf("migrate company industry groups to mids: %w", err)
	}
	if err := migratePipelineStatusesToSixStage(ctx, db); err != nil {
		return fmt.Errorf("migrate pipeline statuses to 6-stage: %w", err)
	}
	if err := ensurePipelineAutomationSchema(ctx, db); err != nil {
		return fmt.Errorf("migrate pipeline automation schema: %w", err)
	}
	if err := ensureNoticeDatetimeColumns(ctx, db); err != nil {
		return fmt.Errorf("migrate notice datetime columns: %w", err)
	}
	if err := ensureDeadlineSchedulerSchema(ctx, db); err != nil {
		return fmt.Errorf("migrate deadline scheduler schema: %w", err)
	}
	if err := ensureResultLookupSchema(ctx, db); err != nil {
		return fmt.Errorf("migrate result lookup schema: %w", err)
	}
	if err := ensureChecklistMatchColumns(ctx, db); err != nil {
		return fmt.Errorf("migrate checklist match columns: %w", err)
	}
	if err := ensureSupportProgramDetailsTable(ctx, db); err != nil {
		return fmt.Errorf("migrate support_program_details: %w", err)
	}
	if err := ensureAttachmentRoleColumn(ctx, db); err != nil {
		return fmt.Errorf("migrate attachment role column: %w", err)
	}
	if err := ensureSupportProgramConditionsTable(ctx, db); err != nil {
		return fmt.Errorf("migrate support_program_conditions: %w", err)
	}
	if err := ensureSavedSearchesTable(ctx, db); err != nil {
		return fmt.Errorf("migrate saved_searches table: %w", err)
	}
	if err := ensureSavedSearchMatchEventType(ctx, db); err != nil {
		return fmt.Errorf("migrate saved_search_match event type: %w", err)
	}
	if err := ensureAwardHistoryParticipantCountColumn(ctx, db); err != nil {
		return fmt.Errorf("migrate notice_award_history.participant_count column: %w", err)
	}
	if err := ensureSavedSearchesOriginColumn(ctx, db); err != nil {
		return fmt.Errorf("migrate saved_searches.origin column: %w", err)
	}
	if err := ensurePipelineAssigneeUserIDColumn(ctx, db); err != nil {
		return fmt.Errorf("migrate notice_pipeline_entries.assignee_user_id column: %w", err)
	}
	if err := ensureSubscriptionsPreviousPlanColumns(ctx, db); err != nil {
		return fmt.Errorf("migrate subscriptions.previous_plan columns: %w", err)
	}
	if err := ensureSavedSearchNotificationColumns(ctx, db); err != nil {
		return fmt.Errorf("migrate saved_searches notification columns: %w", err)
	}
	if err := ensureCompanyProfileFoundingDateColumn(ctx, db); err != nil {
		return fmt.Errorf("migrate company_profiles.founding_date column: %w", err)
	}
	if err := ensureSavedSearchIsActiveColumn(ctx, db); err != nil {
		return fmt.Errorf("migrate saved_searches.is_active column: %w", err)
	}
	if err := ensureNoticeEnrichmentTables(ctx, db); err != nil {
		return fmt.Errorf("migrate notice enrichment tables: %w", err)
	}
	return nil
}

// ensureSavedSearchesTable adds saved_searches — 2026-08-06 "맞춤공고"
// (경쟁서비스 비드큐 격차점검 4번). 개인(user_id) 단위 저장(팀 공유가
// 아니라 각자 자기 관심사대로 여러 개 만드는 개념 — 사용자 확정). 기존
// company_profiles 기반 AI 자동추천(sendRecommendationDigest)과는 완전히
// 별개 기능 — 저 쪽은 조직당 1개(지역/업종)로 판정 로직까지 얽혀있고,
// 이건 사용자가 임의로 여러 조건(발주기관/금액대/키워드 포함·제외 등)을
// 만들어 순수 필터로만 쓴다. keywords_include/exclude는 콤마로 구분한
// 여러 키워드를 TEXT[]로 저장(제목 ILIKE OR/AND NOT 매칭, GET /api/notices
// 필터 확장에서 그대로 씀).
func ensureSavedSearchesTable(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS saved_searches (
			id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			user_id            UUID NOT NULL REFERENCES users(id),
			name               TEXT NOT NULL,
			notice_type        TEXT,
			region             TEXT,
			industry           TEXT,
			organization_name  TEXT,
			budget_min         BIGINT,
			budget_max         BIGINT,
			keywords_include   TEXT[],
			keywords_exclude   TEXT[],
			alert_enabled      BOOLEAN NOT NULL DEFAULT true,
			created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
			updated_at         TIMESTAMPTZ NOT NULL DEFAULT now()
		);
		CREATE INDEX IF NOT EXISTS idx_saved_searches_user ON saved_searches(user_id);
	`)
	return err
}

// ensureSavedSearchMatchEventType widens notification_log_event_type_check
// for 'saved_search_match' — 이 제약을 건드리는 다른 6개 함수와 마찬가지로
// 항상 "현재 최종" 전체 목록을 써야 한다(ensureDeadlineD7EventType 주석의
// 사고 이력 참고, 이 함수도 그 그룹에 합류). 2026-08-06: Run() 호출
// 순서상 이 함수가 이 제약을 건드리는 함수들 중 지금 "마지막"으로
// 실행되므로(그 뒤로는 saved_search_deadline_d7/d3/d1을 추가한 이번
// 변경까지 포함해 항상 여기가 최종 목록이다), 새 이벤트 타입을 추가할
// 땐 이후에도 계속 이 함수의 목록을 갱신할 것 — Run()에 새 constraint-
// widening 함수를 이 함수보다 뒤에 추가하지 않는 한.
func ensureSavedSearchMatchEventType(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		ALTER TABLE notification_log DROP CONSTRAINT IF EXISTS notification_log_event_type_check;
		ALTER TABLE notification_log ADD CONSTRAINT notification_log_event_type_check
			CHECK (event_type IN ('deadline_d7','deadline_d3','deadline_d1','recommendation_digest','assignee_status_change',
			                       'weekly_report','monthly_report','team_invite','team_invite_accepted','admin_broadcast','password_reset','email_verification','notice_corrected','saved_search_match','notice_cancelled',
			                       'saved_search_deadline_d7','saved_search_deadline_d3','saved_search_deadline_d1'));
	`)
	return err
}

// ensureNoticeRegionRestrictedColumn adds notices.region_restricted —
// 2026-08-06, 공고 목록의 지역제한 아이콘용. 신규 컬럼 추가라
// ensurePendingPlanColumn과 같은 이유로 ADD COLUMN IF NOT EXISTS 한 줄로
// 충분하다(CHECK 목록을 넓히는 변경이 아님). g2b만 이 값을 채운다 —
// bizinfo/scsbid/demo는 NULL로 남아 "정보 없음"으로 정직하게 처리된다.
func ensureNoticeRegionRestrictedColumn(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		ALTER TABLE notices ADD COLUMN IF NOT EXISTS region_restricted BOOLEAN;
	`)
	return err
}

// ensureNoticeProcurementClassColumns — 2026-08-08 Phase 0. g2b 목록 응답이
// 이미 주던 공공조달분류 코드/계층/업종제한 플래그를 그동안 industry(중분류명)
// 하나만 쓰고 버리던 것을 살린다. region_restricted와 동일하게 ADD COLUMN IF
// NOT EXISTS + 최초 1회 백필. 백필은 현재버전(notice_versions.is_current)의
// raw_documents.raw_content(원본 JSON)에서 직접 재파싱한다 — g2b가 아닌 소스는
// 이 키들이 없어 LIKE로 자연히 제외되고 NULL로 남는다. industry 컬럼 자체는
// 손대지 않으므로 판정엔진/검색 매칭에는 영향 없다(순수 데이터 확보).
func ensureNoticeProcurementClassColumns(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `
		ALTER TABLE notices ADD COLUMN IF NOT EXISTS procurement_class_code   TEXT;
		ALTER TABLE notices ADD COLUMN IF NOT EXISTS procurement_class_large  TEXT;
		ALTER TABLE notices ADD COLUMN IF NOT EXISTS procurement_class_detail TEXT;
		ALTER TABLE notices ADD COLUMN IF NOT EXISTS industry_restricted      BOOLEAN;
	`); err != nil {
		return err
	}
	// 🚨 2026-08-09: 이 백필도 raw_documents를 LIKE로 스캔하는 무거운 쿼리라
	// startup에서 돌리면 배포가 블로킹된다. 컬럼 추가만 startup에 두고, 백필은
	// 관리자 수동 실행(RunProcurementClassBackfill)으로 분리한다.
	return nil
}

// RunProcurementClassBackfill — 관리자 수동 실행용(startup 아님). 현재버전의
// raw_content에서 공공조달분류 코드/계층/업종제한을 재파싱해 채운다. 멱등:
// procurement_class_code IS NULL인 행만 채운다. 반환값은 갱신 행 수.
func RunProcurementClassBackfill(ctx context.Context, db *sql.DB) (int64, error) {
	res, err := db.ExecContext(ctx, `
		UPDATE notices n SET
			procurement_class_code   = NULLIF(r.raw_content::jsonb->>'pubPrcrmntClsfcNo',''),
			procurement_class_large  = NULLIF(r.raw_content::jsonb->>'pubPrcrmntLrgClsfcNm',''),
			procurement_class_detail = NULLIF(r.raw_content::jsonb->>'pubPrcrmntClsfcNm',''),
			industry_restricted      = CASE r.raw_content::jsonb->>'indstrytyLmtYn'
			                             WHEN 'Y' THEN true WHEN 'N' THEN false ELSE NULL END
		FROM notice_versions v
		JOIN raw_documents r ON r.id = v.raw_document_id
		WHERE v.notice_id = n.id AND v.is_current
		  AND n.procurement_class_code IS NULL
		  AND r.raw_content LIKE '%pubPrcrmntClsfcNo%'`)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// migratePipelineStatusesToSixStage — 2026-08-09. 파이프라인 상태를 9단계에서
// 6단계로 축소한다(소상공인 사용성). 검토전·참여검토·보류 → 검토중, 승인대기 →
// 준비중으로 합치고, 제출완료/낙찰/탈락/제외는 그대로. CHECK 제약도 새 6개로 교체.
// 🚨순서 중요: 새 값('검토중')이 옛 CHECK(9개, '검토중' 없음)에 걸리므로 반드시
// (1) 옛 제약 DROP → (2) 값 remap(무제약 상태) → (3) 새 제약 ADD 순서여야 한다.
// (처음엔 UPDATE를 DROP보다 먼저 둬서 운영 마이그레이션이 실패했다.) 멱등:
// DROP IF EXISTS, remap UPDATE는 옮긴 뒤 no-op, ADD도 앞의 DROP과 짝이라 반복 안전.
func migratePipelineStatusesToSixStage(ctx context.Context, db *sql.DB) error {
	stmts := []string{
		`ALTER TABLE notice_pipeline_entries DROP CONSTRAINT IF EXISTS notice_pipeline_entries_status_check`,
		`UPDATE notice_pipeline_entries SET status = '검토중' WHERE status IN ('검토전','참여검토','보류')`,
		`UPDATE notice_pipeline_entries SET status = '준비중' WHERE status = '승인대기'`,
		`ALTER TABLE notice_pipeline_entries ADD CONSTRAINT notice_pipeline_entries_status_check ` +
			`CHECK (status = ANY (ARRAY['검토중','준비중','제출완료','낙찰','탈락','제외']))`,
	}
	for _, q := range stmts {
		if _, err := db.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("%s: %w", q, err)
		}
	}
	return nil
}

// ensureNoticeDatetimeColumns — 2026-08-09 Phase C 후속. g2b 응답의 시각 포함
// 일정 필드를 담을 TIMESTAMPTZ 컬럼을 추가한다(기존 DATE 컬럼은 유지).
// 🚨 2026-08-09 운영 배포 실패 수정: 예전엔 여기서 raw_documents 전체를 LIKE로
// 스캔하는 무거운 백필 UPDATE를 startup에서 돌렸는데, 수집 데이터가 커지면서
// 이 쿼리가 migrate.Apply(로깅/ListenAndServe보다 먼저 돎)를 오래 블로킹해 Render
// 헬스체크 포트 오픈 전에 배포가 "No open ports"로 실패했다. → startup에선 가벼운
// 컬럼 추가만 하고, 무거운 백필은 관리자 수동 실행(RunNoticeDatetimeBackfill)으로
// 분리한다(서비스가 살아있는 상태에서 여유있게 1회 실행).
func ensureNoticeDatetimeColumns(ctx context.Context, db *sql.DB) error {
	cols := []string{
		`ALTER TABLE notices ADD COLUMN IF NOT EXISTS application_start_datetime TIMESTAMPTZ`,
		`ALTER TABLE notices ADD COLUMN IF NOT EXISTS application_end_datetime TIMESTAMPTZ`,
		`ALTER TABLE notices ADD COLUMN IF NOT EXISTS qualification_deadline_at TIMESTAMPTZ`,
		`ALTER TABLE notices ADD COLUMN IF NOT EXISTS opening_at TIMESTAMPTZ`,
		`ALTER TABLE notices ADD COLUMN IF NOT EXISTS rebid_opening_at TIMESTAMPTZ`,
		`ALTER TABLE notices ADD COLUMN IF NOT EXISTS success_bid_method_name TEXT`,
	}
	for _, q := range cols {
		if _, err := db.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("%s: %w", q, err)
		}
	}
	return nil
}

// RunNoticeDatetimeBackfill — 관리자 수동 실행용(startup 아님). external_notice_id별
// 최신 raw에서 datetime을 뽑아 채운다. 값 형식은 'YYYY-MM-DD HH:MM(:SS)?'라 정규식
// 검증한 것만 캐스팅. 멱등: NULL인 행만 COALESCE로 채운다. 반환값은 갱신 행 수.
func RunNoticeDatetimeBackfill(ctx context.Context, db *sql.DB) (int64, error) {
	res, err := db.ExecContext(ctx, `
		WITH latest_raw AS (
		  SELECT DISTINCT ON (external_notice_id) external_notice_id,
		    CASE WHEN raw_content::jsonb->>'bidBeginDt'     ~ '^\d{4}-\d{2}-\d{2} \d{2}:\d{2}' THEN (raw_content::jsonb->>'bidBeginDt')::timestamp     AT TIME ZONE 'Asia/Seoul' END AS start_dt,
		    CASE WHEN raw_content::jsonb->>'bidClseDt'      ~ '^\d{4}-\d{2}-\d{2} \d{2}:\d{2}' THEN (raw_content::jsonb->>'bidClseDt')::timestamp      AT TIME ZONE 'Asia/Seoul' END AS end_dt,
		    CASE WHEN raw_content::jsonb->>'bidQlfctRgstDt' ~ '^\d{4}-\d{2}-\d{2} \d{2}:\d{2}' THEN (raw_content::jsonb->>'bidQlfctRgstDt')::timestamp AT TIME ZONE 'Asia/Seoul' END AS qual_dt,
		    CASE WHEN raw_content::jsonb->>'opengDt'        ~ '^\d{4}-\d{2}-\d{2} \d{2}:\d{2}' THEN (raw_content::jsonb->>'opengDt')::timestamp        AT TIME ZONE 'Asia/Seoul' END AS openg_dt,
		    CASE WHEN raw_content::jsonb->>'rbidOpengDt'    ~ '^\d{4}-\d{2}-\d{2} \d{2}:\d{2}' THEN (raw_content::jsonb->>'rbidOpengDt')::timestamp    AT TIME ZONE 'Asia/Seoul' END AS rbid_dt,
		    NULLIF(raw_content::jsonb->>'sucsfbidMthdNm','') AS method
		  FROM raw_documents
		  WHERE raw_content LIKE '%opengDt%'
		  ORDER BY external_notice_id, collected_at DESC
		)
		UPDATE notices n SET
		  application_start_datetime = COALESCE(n.application_start_datetime, lr.start_dt),
		  application_end_datetime   = COALESCE(n.application_end_datetime, lr.end_dt),
		  qualification_deadline_at  = COALESCE(n.qualification_deadline_at, lr.qual_dt),
		  opening_at                 = COALESCE(n.opening_at, lr.openg_dt),
		  rebid_opening_at           = COALESCE(n.rebid_opening_at, lr.rbid_dt),
		  success_bid_method_name    = COALESCE(n.success_bid_method_name, lr.method)
		FROM latest_raw lr
		WHERE lr.external_notice_id = n.external_notice_id
		  AND (n.opening_at IS NULL OR n.application_end_datetime IS NULL OR n.qualification_deadline_at IS NULL
		       OR n.application_start_datetime IS NULL OR n.success_bid_method_name IS NULL)`)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// ensureAttachmentRoleColumn — 2026-08-09 B-2. 첨부 역할 구분(지원사업의 별첨 vs
// 본문출력 공고문). 가벼운 nullable ADD COLUMN 1개 — 기존 g2b 첨부는 NULL로 남아
// 동작 불변. startup 안전(backfill/무거운 작업 없음).
func ensureAttachmentRoleColumn(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `ALTER TABLE attachments ADD COLUMN IF NOT EXISTS attachment_role TEXT`)
	return err
}

// ensureSupportProgramDetailsTable — 2026-08-09 B-2. 기업마당 지원사업 전용 공식
// 필드(지원대상/사업개요/신청방법/문의처/신청URL/대·중분류/해시태그/조회수/수정일)를
// notices 공용 테이블 비대화 없이 별도 저장한다. notice_type=support_program에만
// row가 생긴다. 🚨 notices로의 외래키는 만들지 않는다(순수 notice_id PK) — 롤링
// 배포 중 ALTER/CREATE의 FK가 hot 테이블(notices, 수집이 계속 upsert)에 락을 걸어
// 부팅을 블로킹한 사고(Step 3) 재발 방지. 무결성은 앱 레벨(수집기가 support만 기록,
// 조인은 notice_id)로 지킨다. 가벼운 CREATE TABLE 1회(startup 안전, backfill 없음).
// ensureSupportProgramConditionsTable — 2026-08-09 B-3. 지원사업 공고문(공고문
// 첨부, SUPPORT_PRINT_DOCUMENT)에서 규칙 기반으로 뽑은 '상세 신청조건'을 담는다.
// 공식 API 분류(support_program_details)와 역할이 분리된다 — 이 테이블은 덮어쓰지
// 않고 보완만 한다. notice_id PK(공고당 1행, FK 없이 — 핫 테이블 FK 회피 정책).
// required_documents는 JSONB 배열([{name,required,source_text}]). extraction_method
// 는 RULE/RULE_AI/MANUAL, ai_version은 AI 붙기 전까지 NULL. 재분석 방지는
// source_file_hash + extractor_version로 판단한다.
func ensureSupportProgramConditionsTable(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS support_program_conditions (
			notice_id               UUID PRIMARY KEY,
			source_document_id      UUID,
			source_file_hash        TEXT,
			eligibility_text        TEXT,
			required_documents      JSONB NOT NULL DEFAULT '[]'::jsonb,
			support_amount_text     TEXT,
			support_limit_text      TEXT,
			support_limit_amount    BIGINT,
			support_rate_text       TEXT,
			support_scale_text      TEXT,
			business_age_condition  TEXT,
			revenue_condition       TEXT,
			region_condition        TEXT,
			exclusion_conditions    JSONB NOT NULL DEFAULT '[]'::jsonb,
			preference_conditions   JSONB NOT NULL DEFAULT '[]'::jsonb,
			selection_process       TEXT,
			text_length             INTEGER,
			text_poor               BOOLEAN NOT NULL DEFAULT false,
			needs_ai                BOOLEAN NOT NULL DEFAULT false,
			extraction_method       TEXT NOT NULL DEFAULT 'RULE',
			confidence              TEXT,
			extractor_version       TEXT,
			ai_version              TEXT,
			extracted_at            TIMESTAMPTZ,
			created_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
			updated_at              TIMESTAMPTZ NOT NULL DEFAULT now()
		)`)
	return err
}

func ensureSupportProgramDetailsTable(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS support_program_details (
			notice_id               UUID PRIMARY KEY,
			support_target          TEXT,
			business_summary_html   TEXT,
			business_summary_text   TEXT,
			application_method      TEXT,
			reference_contact       TEXT,
			application_url         TEXT,
			support_category_major  TEXT,
			support_category_middle TEXT,
			hashtags                TEXT,
			inquiry_count           BIGINT,
			source_updated_at       TIMESTAMPTZ,
			created_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
			updated_at              TIMESTAMPTZ NOT NULL DEFAULT now()
		)`)
	return err
}

// ensureChecklistMatchColumns — 2026-08-09 Step 3 재적용. 회사 문서 자동매칭
// 추적용 컬럼. 🚨 matched_document_id에 DB 외래키를 만들지 않는다(순수 UUID) —
// 롤링 배포 중 ALTER TABLE ... ADD COLUMN ... REFERENCES가 참조 테이블
// (company_documents)까지 락을 잡아 마이그레이션이 블로킹되고 배포가 실패한
// 사고가 있었다(b5f955c). 무결성은 애플리케이션 레벨에서 검증한다:
// 조회는 항상 company_profile_id로 스코프하고, dangling 참조(하드삭제된 문서)는
// reevaluateChecklistMatches에서 자동 해제 후 재평가한다(FK cascade 없음).
func ensureChecklistMatchColumns(ctx context.Context, db *sql.DB) error {
	stmts := []string{
		`ALTER TABLE pipeline_checklist_items ADD COLUMN IF NOT EXISTS matched_document_id UUID`,
		`ALTER TABLE pipeline_checklist_items ADD COLUMN IF NOT EXISTS match_method TEXT`,
		`ALTER TABLE pipeline_checklist_items ADD COLUMN IF NOT EXISTS matched_at TIMESTAMPTZ`,
	}
	for _, q := range stmts {
		if _, err := db.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("%.60s...: %w", q, err)
		}
	}
	return nil
}

// ensureResultLookupSchema — 2026-08-09 우선순위5. 개찰일시 기반 공식 결과
// 자동조회. 사용자 노출 상태는 6개 유지 — 조회 진행/결과유형은 내부 컬럼으로만
// 관리한다(개찰대기/결과조회중/유찰/재입찰 같은 상태를 만들지 않는다).
//   - notice_pipeline_entries: backoff 조회 추적 + 결과 스냅샷.
//     result_type(내부): WON/LOST/REBID/FAILED_BID/PENDING/NEEDS_REVIEW/NAME_MATCH.
//   - notice_award_history: 낙찰업체 사업자번호 컬럼(자동 낙찰/탈락 판정·경쟁사
//     분석 근거). raw_payload는 이제 진짜 원본 JSON을 담는다(ingest 수정과 짝).
func ensureResultLookupSchema(ctx context.Context, db *sql.DB) error {
	stmts := []string{
		`ALTER TABLE notice_pipeline_entries ADD COLUMN IF NOT EXISTS result_check_started_at TIMESTAMPTZ`,
		`ALTER TABLE notice_pipeline_entries ADD COLUMN IF NOT EXISTS last_result_checked_at  TIMESTAMPTZ`,
		`ALTER TABLE notice_pipeline_entries ADD COLUMN IF NOT EXISTS result_check_attempts   INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE notice_pipeline_entries ADD COLUMN IF NOT EXISTS result_finalized_at     TIMESTAMPTZ`,
		`ALTER TABLE notice_pipeline_entries ADD COLUMN IF NOT EXISTS result_type             TEXT`,
		`ALTER TABLE notice_pipeline_entries ADD COLUMN IF NOT EXISTS winner_bizno            TEXT`,
		`ALTER TABLE notice_pipeline_entries ADD COLUMN IF NOT EXISTS winner_name             TEXT`,
		`ALTER TABLE notice_pipeline_entries ADD COLUMN IF NOT EXISTS award_rate              NUMERIC`,
		`ALTER TABLE notice_award_history ADD COLUMN IF NOT EXISTS winner_bizno TEXT`,
	}
	for _, q := range stmts {
		if _, err := db.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("%.60s...: %w", q, err)
		}
	}
	return nil
}

// ensureDeadlineSchedulerSchema — 2026-08-09 Phase B+. 시간단위 마감 자동화의
// 스키마. 두 부분이다:
//  1. pipeline_deadline_events — 이벤트 발송 dedup 원장(메모리 아닌 DB 기준).
//     UNIQUE(pipeline_entry_id, event_type, deadline_at)이 "동일 이벤트 1회"를
//     보장한다. deadline_at을 키에 포함하는 게 핵심 — 공고 정정으로 마감시각이
//     바뀌면 같은 event_type이라도 새 행이 되어(새 날짜 기준) 재발송되고, 과거
//     마감 기준으로 이미 보낸 건은 그대로 남아 재발송되지 않는다.
//  2. notice_pipeline_entries에 마감 스냅샷 4컬럼 — "이 엔트리에 대해 마지막으로
//     인지한 마감시각"과 "그 값을 언제부터 알았는지". 스냅샷과 현재 공고 마감이
//     다르면 정정으로 보고 "마감일 변경" 알림 + 소급방지 기준시각(seen_at) 갱신.
func ensureDeadlineSchedulerSchema(ctx context.Context, db *sql.DB) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS pipeline_deadline_events (
			id                 BIGSERIAL PRIMARY KEY,
			pipeline_entry_id  UUID NOT NULL REFERENCES notice_pipeline_entries(id) ON DELETE CASCADE,
			notice_id          UUID NOT NULL,
			company_profile_id UUID NOT NULL,
			event_type         TEXT NOT NULL,
			deadline_at        TIMESTAMPTZ NOT NULL,
			sent_at            TIMESTAMPTZ NOT NULL DEFAULT now()
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_pipeline_deadline_events
			ON pipeline_deadline_events (pipeline_entry_id, event_type, deadline_at)`,
		`ALTER TABLE notice_pipeline_entries ADD COLUMN IF NOT EXISTS submission_deadline_snapshot    TIMESTAMPTZ`,
		`ALTER TABLE notice_pipeline_entries ADD COLUMN IF NOT EXISTS submission_deadline_seen_at     TIMESTAMPTZ`,
		`ALTER TABLE notice_pipeline_entries ADD COLUMN IF NOT EXISTS qualification_deadline_snapshot TIMESTAMPTZ`,
		`ALTER TABLE notice_pipeline_entries ADD COLUMN IF NOT EXISTS qualification_deadline_seen_at  TIMESTAMPTZ`,
	}
	for _, q := range stmts {
		if _, err := db.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("%.60s...: %w", q, err)
		}
	}
	return nil
}

// ensurePipelineAutomationSchema — 2026-08-09 Phase A. "사용자가 상태를 직접
// 관리하지 않는" 자동화의 토대: (1) 제외 사유(exclude_reason)와 제출 확인시각
// (submission_confirmed_at) 컬럼, (2) 모든 자동/수동 상태 변경 이력을 남기는
// pipeline_status_history 테이블. 사용자 노출 상태는 6개 그대로 — reason은
// 내부 필드로만 쓰고 화면에는 항상 "제외" 하나로 보여준다.
func ensurePipelineAutomationSchema(ctx context.Context, db *sql.DB) error {
	stmts := []string{
		`ALTER TABLE notice_pipeline_entries ADD COLUMN IF NOT EXISTS exclude_reason TEXT`,
		`ALTER TABLE notice_pipeline_entries ADD COLUMN IF NOT EXISTS submission_confirmed_at TIMESTAMPTZ`,
		`CREATE TABLE IF NOT EXISTS pipeline_status_history (
			id                BIGSERIAL PRIMARY KEY,
			pipeline_entry_id UUID NOT NULL REFERENCES notice_pipeline_entries(id) ON DELETE CASCADE,
			from_status       TEXT,
			to_status         TEXT NOT NULL,
			changed_by        TEXT,           -- user_id 또는 'SYSTEM'
			reason            TEXT,           -- USER_EXCLUDED/DEADLINE_PASSED_UNCONFIRMED 등(내부용)
			trigger_type      TEXT NOT NULL,  -- 'USER' | 'SYSTEM'
			trigger_at        TIMESTAMPTZ,
			created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
		)`,
		`CREATE INDEX IF NOT EXISTS idx_pipeline_status_history_entry ON pipeline_status_history(pipeline_entry_id)`,
	}
	for _, q := range stmts {
		if _, err := db.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("%s: %w", q, err)
		}
	}
	return nil
}

// ensureIndustryTaxonomyTable — 2026-08-08 Phase 2a. 조달청 공공조달분류(대/중분류)를
// 담는 참조 테이블. 회사 업종 선택·공고 매칭을 임의 10그룹 대신 이 공식 분류로
// 맞추기 위한 토대다(Phase 2b에서 매칭/UI가 이 테이블을 쓴다). Phase 3 관리자
// CMS의 편집 대상이기도 하다.
//
// 매 기동마다 notices에서 관측된 (중분류, 대분류)를 동기화한다 — ON CONFLICT
// DO NOTHING이라 기존 행은 건드리지 않고 새 중분류만 편입된다(수집기가 새 분류를
// 저장하면 다음 배포/재기동 때 자동 반영). 한 중분류가 여러 대분류에 걸치는
// 예외(예: "기타")는 가장 빈번한 대분류를 대표로 고른다. 앞뒤 공백은 정규화.
func ensureIndustryTaxonomyTable(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS industry_taxonomy (
			id          BIGSERIAL PRIMARY KEY,
			mid_name    TEXT NOT NULL UNIQUE,   -- 중분류명(= notices.industry, 선택·매칭 단위)
			large_name  TEXT NOT NULL,          -- 대분류명
			active      BOOLEAN NOT NULL DEFAULT true,
			sort_order  INTEGER NOT NULL DEFAULT 0,
			created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
		);
	`); err != nil {
		return err
	}
	// 관측 데이터에서 동기화(멱등). 각 중분류의 대표 대분류는 최빈값으로 정한다.
	_, err := db.ExecContext(ctx, `
		INSERT INTO industry_taxonomy (mid_name, large_name)
		SELECT mid, large FROM (
			SELECT trim(industry) AS mid, trim(procurement_class_large) AS large,
			       ROW_NUMBER() OVER (PARTITION BY trim(industry) ORDER BY count(*) DESC) AS rn
			FROM notices
			WHERE procurement_class_large IS NOT NULL AND trim(procurement_class_large) <> ''
			  AND industry IS NOT NULL AND trim(industry) <> ''
			GROUP BY trim(industry), trim(procurement_class_large)
		) t WHERE rn = 1
		ON CONFLICT (mid_name) DO NOTHING;
	`)
	if err != nil {
		return err
	}
	// "기타"는 미분류 버킷이라 선택지에서 제외한다(2026-08-08). 회사가 "기타"를
	// 고르면 지역·예산만 걸러진 셈이라 공고 검색과 차이가 없어 정확도에 기여하지
	// 않는다. 매칭(scoreIndustry)에서도 "기타"는 met으로 치지 않으므로 선택지로
	// 남겨둘 이유가 없다. 위 동기화가 notices의 "기타"를 active=true로 새로 넣을 수
	// 있어(신규 DB) 매 시작 시 확실히 비활성화한다 — active로 필터하는 picker/AI enum
	// 두 producer 모두에서 자동으로 빠진다. (notices.industry의 "기타"는 그대로 두어
	// 공고 분류 자체는 보존한다.)
	_, err = db.ExecContext(ctx, `UPDATE industry_taxonomy SET active = false WHERE mid_name = '기타' AND active`)
	return err
}

// migrateCompanyIndustryGroupsToMids — 2026-08-08 Phase 2c. 기존 회사의
// company_profiles.industry에 저장된 레거시 10그룹명을 조달청 중분류명들로
// 전개한다(그룹 하나 → 그 그룹에 속한 중분류 여러 개). 전개 결과는 기존
// 그룹 매칭과 동치라(expandCompanyIndustries와 동일 매핑) 판정 결과는 변하지
// 않고, 저장 값만 신규 체계로 통일된다. industry가 레거시 그룹을 포함하는
// 행만 건드리므로(&&) 재실행 시 no-op(멱등). group→mid 매핑은 api 패키지의
// industryRawToGroup(35종)의 역인덱스와 동일하다.
func migrateCompanyIndustryGroupsToMids(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		WITH gm(grp, mid) AS (VALUES
			('ICT/SW','SW 및 시스템 개발'),('ICT/SW','시스템 운영환경 구축'),('ICT/SW','DB구축 및 자료입력'),
			('ICT/SW','디지털콘텐츠 개발'),('ICT/SW','ICT사업 컨설팅'),('ICT/SW','통신서비스'),
			('연구/조사/컨설팅','학술연구서비스'),('연구/조사/컨설팅','시장 및 여론조사'),
			('연구/조사/컨설팅','문화재 조사/발굴 및 수리'),('연구/조사/컨설팅','기술시험,검사 및 분석'),
			('설계/감리/CM','설계'),('설계/감리/CM','감리'),('설계/감리/CM','CM'),('설계/감리/CM','측량'),
			('행사/홍보/미디어','행사 기획 및 대행'),('행사/홍보/미디어','매체제작'),('행사/홍보/미디어','홍보 및 마케팅'),
			('행사/홍보/미디어','전시관 및 홍보관 설치'),('행사/홍보/미디어','디자인'),
			('시설관리/유지보수','시설물관리, 청소 등'),('시설관리/유지보수','운영 및 유지관리'),
			('시설관리/유지보수','수리'),('시설관리/유지보수','임대'),
			('환경/폐기물','폐기물 처리'),('환경/폐기물','폐기물 재활용'),
			('생활서비스','운송서비스'),('생활서비스','여행서비스'),('생활서비스','숙박서비스'),
			('생활서비스','음식서비스'),('생활서비스','보건서비스'),
			('전문서비스','보험서비스'),('전문서비스','회계서비스'),('전문서비스','사업장 위탁'),
			('교육','교육서비스'),
			('기타','기타')
		),
		expanded AS (
			SELECT cp.id, array_agg(DISTINCT COALESCE(gm.mid, trim(elem))) AS new_industry
			FROM company_profiles cp
			CROSS JOIN LATERAL unnest(cp.industry) AS elem
			LEFT JOIN gm ON gm.grp = trim(elem)
			WHERE cp.industry && (SELECT array_agg(DISTINCT grp) FROM gm)
			GROUP BY cp.id
		)
		UPDATE company_profiles cp SET industry = e.new_industry
		FROM expanded e WHERE e.id = cp.id;
	`)
	return err
}

// ensureOnboardingCompletedAtColumn — 2026-08-05 온보딩 재설계("AI 분석
// 커버리지 50% 미만이면 완료 차단"). 이 컬럼이 NULL이면 새 필수 온보딩을
// 아직 안 마친 것으로 보고 route() 게이트가 강제로 온보딩 화면에
// 붙잡아둔다(handleCompleteOnboarding이 커버리지 50% 이상을 서버에서
// 다시 확인한 뒤에만 now()로 채움). ensureEmailVerificationTables와 같은
// "컬럼이 처음 생기는 이번 한 번만 기존 행 전원 백필" 패턴 — 이미 가입
// 완료한 기존 회원에게 이번 정책을 소급 적용하지 않기로 확정했으므로
// (신규 가입자만 대상), 컬럼이 새로 생기는 시점에 존재하는 모든 프로필은
// created_at 기준으로 "이미 통과한 것"으로 일괄 백필한다.
func ensureOnboardingCompletedAtColumn(ctx context.Context, db *sql.DB) error {
	var columnExists bool
	if err := db.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM information_schema.columns
			WHERE table_name = 'company_profiles' AND column_name = 'onboarding_completed_at'
		)`).Scan(&columnExists); err != nil {
		return fmt.Errorf("check onboarding_completed_at column: %w", err)
	}
	if !columnExists {
		if _, err := db.ExecContext(ctx, `
			ALTER TABLE company_profiles ADD COLUMN onboarding_completed_at TIMESTAMPTZ;
			UPDATE company_profiles SET onboarding_completed_at = created_at;
		`); err != nil {
			return fmt.Errorf("add and backfill onboarding_completed_at: %w", err)
		}
	}
	return nil
}

// ensureTermsAgreementsTable adds terms_agreements — 회원가입 시(2단계,
// 이메일/소셜 가입 공통) 이용약관·개인정보처리방침 동의를 각각 어느
// 버전에 동의했는지 기록한다. 나중에 약관이 바뀌면 이 기록을 근거로
// "재동의가 필요한 사용자"를 가려낼 수 있다(재동의 강제 플로우 자체는
// 이번 범위 밖 — 기록만 남겨둔다). ip_address는 선택 정보(요청 헤더에서
// 얻을 수 있으면 기록, 못 얻으면 NULL)라 NOT NULL을 안 건다.
func ensureTermsAgreementsTable(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS terms_agreements (
			id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			user_id         UUID NOT NULL REFERENCES users(id),
			terms_version   TEXT NOT NULL,
			privacy_version TEXT NOT NULL,
			agreed_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
			ip_address      TEXT
		);
		CREATE INDEX IF NOT EXISTS idx_terms_agreements_user ON terms_agreements(user_id, agreed_at);
	`)
	return err
}

// ensureLegalDocumentsTable adds legal_documents — 이용약관/개인정보처리방침의
// 버전 이력 테이블(관리자 #/admin/legal-documents에서 관리). 같은 type
// ('terms'/'privacy')에서 is_active=true인 행은 항상 최대 1개여야 하는데,
// 이 불변식은 DB 제약이 아니라 애플리케이션(handleAdminPublishLegalDocument,
// legal_documents.go)이 트랜잭션으로 보장한다 — 새 버전을 발행하면 그
// 트랜잭션 안에서 기존 활성 버전을 비활성화하고 새 행을 활성으로 삽입.
// 이전 버전은 삭제되지 않고 계속 남아 이력으로 조회 가능하다.
func ensureLegalDocumentsTable(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS legal_documents (
			id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			type           TEXT NOT NULL CHECK (type IN ('terms','privacy')),
			version        TEXT NOT NULL,
			content        TEXT NOT NULL,
			effective_date DATE NOT NULL,
			is_active      BOOLEAN NOT NULL DEFAULT false,
			created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
		);
		CREATE INDEX IF NOT EXISTS idx_legal_documents_type_active ON legal_documents(type, is_active);
	`)
	return err
}

// ensureLegalDocumentsSeed seeds the initial 이용약관/개인정보처리방침 버전
// (legalDocumentsSeed.go의 initialTermsContent/initialPrivacyContent) —
// WHERE NOT EXISTS로 각 type마다 한 번만, 이미 관리자가 새 버전을 발행한
// 뒤에는(행이 이미 있으므로) 다시 덮어쓰지 않는다. **주의: 이 초기
// 콘텐츠는 표준 템플릿 기반 초안이며, 실제 서비스에 발행하기 전 반드시
// 법률 전문가 검토가 필요하다** — legalDocumentsSeed.go 상단 주석과
// 문서 본문 첫 줄에도 동일한 경고를 남겨뒀다.
func ensureLegalDocumentsSeed(ctx context.Context, db *sql.DB) error {
	for _, doc := range []struct {
		docType, version, content string
	}{
		{"terms", initialLegalDocumentVersion, initialTermsContent},
		{"privacy", initialLegalDocumentVersion, initialPrivacyContent},
	} {
		if _, err := db.ExecContext(ctx, `
			INSERT INTO legal_documents (type, version, content, effective_date, is_active)
			SELECT $1, $2, $3, CURRENT_DATE, true
			WHERE NOT EXISTS (SELECT 1 FROM legal_documents WHERE type = $1)`,
			doc.docType, doc.version, doc.content,
		); err != nil {
			return err
		}
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
// 보완 2차). ai_supplement_attempts(2단계 고도화: 영구 실패 재시도 상한)는
// 실패할 때마다 증가하고, maxAISupplementAttempts에 도달하면 성공 못 해도
// attempted_at을 찍어 무한 재시도를 막는다. db/migrations/001_init.sql 주석
// 참고.
func ensureDocumentExtractionAutomationColumns(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		ALTER TABLE attachments ADD COLUMN IF NOT EXISTS section_extraction_processed_at TIMESTAMPTZ;
		ALTER TABLE eligibility_conditions ADD COLUMN IF NOT EXISTS ai_supplement_attempted_at TIMESTAMPTZ;
		ALTER TABLE required_documents ADD COLUMN IF NOT EXISTS ai_supplement_attempted_at TIMESTAMPTZ;
		ALTER TABLE eligibility_conditions ADD COLUMN IF NOT EXISTS ai_supplement_attempts INTEGER NOT NULL DEFAULT 0;
		ALTER TABLE required_documents ADD COLUMN IF NOT EXISTS ai_supplement_attempts INTEGER NOT NULL DEFAULT 0;
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
// 알림 예약 중 D-7 추가분.
//
// 버그였다가 고쳐진 이력: 이 함수와 ensureReportEventTypes/
// ensureTeamInviteEventTypes/ensureAdminBroadcastEventType/
// ensurePasswordResetEventType/ensureEmailVerificationEventType 전부
// DROP+ADD CONSTRAINT를 쓰는데, Apply()가 매 기동마다 전부 순서대로
// 재실행하는 구조라(idempotent 마이그레이션 패턴) 한 함수가 "그 함수가
// 작성된 시점의" 목록으로 재실행되면 그 사이 더 넓은 목록으로 이미
// 커진 실제 테이블 데이터(예: team_invite 행)를 위반해 ADD CONSTRAINT
// 자체가 실패한다 — 실제로 재현된 크래시. 그래서 이 여섯 함수 모두 항상
// "현재 최종" 전체 목록을 쓰도록 통일했다(어느 게 먼저/나중에 실행되든
// 중간 단계가 기존 행보다 좁아지는 순간이 생기지 않게).
//
// ⚠️ 2026-08-04 재발 이력: admin_broadcast/password_reset/email_verification
// 3개를 추가할 때 이 원칙을 깜빡하고 매번 "새 함수 하나만" 추가해서, 위
// 여섯 함수 중 이 함수를 포함한 넷(ensureDeadlineD7EventType/
// ensureReportEventTypes/ensureTeamInviteEventTypes/
// ensureAdminBroadcastEventType)이 실제로 옛날 좁은 목록에 멈춰 있었다
// — 운영에 이미 password_reset/email_verification 행이 쌓인 뒤였다면
// 다음 재기동 때 크래시했을 사고(같은 날 auth_lookup_attempts.kind에서
// 실제로 이 정확한 크래시가 한 번 났음, ensureAuthLookupKindEmailVerifyResend
// 주석 참고). 다시 발견해서 전부 최종 목록으로 동기화했다. **앞으로
// 이벤트 타입을 추가할 때는 새 함수 하나만 추가하지 말고, 이 constraint를
// 건드리는 함수 전부(및 001_init.sql)의 목록을 반드시 함께 넓힐 것** —
// grep "notification_log_event_type_check" collector/internal/migrate/migrate.go
// 로 전부 찾아서 빠짐없이 갱신.
func ensureDeadlineD7EventType(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		ALTER TABLE notification_log DROP CONSTRAINT IF EXISTS notification_log_event_type_check;
		ALTER TABLE notification_log ADD CONSTRAINT notification_log_event_type_check
			CHECK (event_type IN ('deadline_d7','deadline_d3','deadline_d1','recommendation_digest','assignee_status_change',
			                       'weekly_report','monthly_report','team_invite','team_invite_accepted','admin_broadcast','password_reset','email_verification','notice_corrected','saved_search_match','notice_cancelled'));
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

// ensureNoticeEnrichmentTables — Phase C(2026-08-11). 나라장터 추가 공식 오퍼레이션
// (참가가능지역 getBidPblancListInfoPrtcptPsblRgn, 허용업종/면허 getBidPblancListInfoLicenseLimit)
// 결과를 1:N 정규화 테이블로 저장한다(eligibility_conditions와 같은 notice_version_id FK 패턴).
// notice_versions.enrichment_status/enriched_at로 이미 보강된 버전을 재조회하지 않는다(증분).
// CREATE/ADD IF NOT EXISTS로 멱등.
func ensureNoticeEnrichmentTables(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS notice_participation_regions (
			id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			notice_version_id  UUID NOT NULL REFERENCES notice_versions(id),
			region_name        TEXT NOT NULL,
			business_division  TEXT,
			sort_no            INTEGER,
			created_at         TIMESTAMPTZ NOT NULL DEFAULT now()
		);
		CREATE INDEX IF NOT EXISTS idx_notice_participation_regions_version ON notice_participation_regions(notice_version_id);

		CREATE TABLE IF NOT EXISTS notice_license_limits (
			id                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			notice_version_id     UUID NOT NULL REFERENCES notice_versions(id),
			license_name          TEXT NOT NULL,
			permitted_industries  TEXT,
			industry_field        TEXT,
			limit_group_no        TEXT,
			business_division     TEXT,
			sort_no               INTEGER,
			created_at            TIMESTAMPTZ NOT NULL DEFAULT now()
		);
		CREATE INDEX IF NOT EXISTS idx_notice_license_limits_version ON notice_license_limits(notice_version_id);

		ALTER TABLE notice_versions ADD COLUMN IF NOT EXISTS enrichment_status TEXT;
		ALTER TABLE notice_versions ADD COLUMN IF NOT EXISTS enriched_at TIMESTAMPTZ;
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
			status              TEXT NOT NULL DEFAULT '검토중'
			                        CHECK (status IN ('검토중','준비중','제출완료','낙찰','탈락','제외')),
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

// ensureAwardHistoryParticipantCountColumn adds notice_award_history.participant_count
// — 2026-08-06, 낙찰이력 화면 보강("참가업체 수 평균") 대상. scsbid API
// 응답에 이미 있던 prtcptCnum 필드를 이제야 저장한다(수집기 자체는
// 그대로, AwardRecord/ingest 로직만 확장). 신규 컬럼 추가라
// ensurePendingPlanColumn과 같은 이유로 ADD COLUMN IF NOT EXISTS 한 줄로
// 충분하다.
func ensureAwardHistoryParticipantCountColumn(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		ALTER TABLE notice_award_history ADD COLUMN IF NOT EXISTS participant_count INTEGER;
	`)
	return err
}

// ensureSavedSearchesOriginColumn adds saved_searches.origin — 2026-08-06,
// 온보딩 완료 시 자동 생성되는 "내 기본 조건"을 표시하기 위한 컬럼.
// 값은 지금은 'onboarding' 하나뿐(그 외엔 NULL, 사용자가 직접 만든
// 조건). 두 가지 역할을 겸한다: (1) 프론트가 "온보딩 시 자동 생성됨"
// 배지를 붙이는 표시용, (2) handleUpsertCompanyProfile이 프로필 수정
// 시 이 값이 'onboarding'인 행만 지역/업종을 계속 동기화하는 대상
// 판별용(사용자가 직접 만든 다른 조건은 절대 안 건드림). 신규 컬럼
// 추가라 ADD COLUMN IF NOT EXISTS 한 줄로 충분하다.
func ensureSavedSearchesOriginColumn(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		ALTER TABLE saved_searches ADD COLUMN IF NOT EXISTS origin TEXT;
	`)
	return err
}

// ensureSavedSearchNotificationColumns adds saved_searches.recipient_contact_ids/
// reminder_enabled/reminder_days_before — 2026-08-06, 기업프로필의 "담당자
// 관리"/"알림 설정"을 맞춤공고 화면으로 통합. recipient_contact_ids는
// company_contacts.id 배열(FK 제약은 안 걺 — saved_searches.go가 저장
// 시점에 호출자 회사 소속 담당자인지 직접 검증하고, 담당자가 삭제되면
// 그냥 존재하지 않는 id로 남아 발송 대상에서 자연히 빠진다). reminder_
// days_before는 company_profiles.notification_days_before와 같은 INTEGER[]
// 패턴(7/3/1 중 선택). "컬럼이 처음 생기는 이번 한 번만 기존 행 전원
// 백필" 관례(ensureOnboardingCompletedAtColumn과 동일) — 기존 맞춤공고
// 다이제스트가 지금까지 "검색 소유자 로그인 이메일"로 발송되던 것을
// 이번에 "recipient_contact_ids 대상"으로 바꾸므로, 이 컬럼이 비어있는
// 채로 두면 기존 사용자들이 알림을 못 받게 되는 공백이 생긴다 — 그래서
// 컬럼이 새로 생기는 시점에, 그 검색을 소유한 계정이 속한 회사의 담당자
// 전원을 기본 수신자로 백필한다(담당자가 하나도 없는 회사는 빈 배열로
// 남고, 발송 시점에 검색 소유자 이메일로 폴백한다 — saved_search_digest.go
// resolveSavedSearchRecipients 참고). reminder_enabled는 새 기능이라
// 기존 행은 계속 false로 남겨 사용자가 명시적으로 켜야만 동작한다(다이제스트와
// 달리 "이미 받고 있던 알림"이 아니므로 소급 활성화하지 않음).
func ensureSavedSearchNotificationColumns(ctx context.Context, db *sql.DB) error {
	var columnExists bool
	if err := db.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM information_schema.columns
			WHERE table_name = 'saved_searches' AND column_name = 'recipient_contact_ids'
		)`).Scan(&columnExists); err != nil {
		return fmt.Errorf("check saved_searches.recipient_contact_ids column: %w", err)
	}
	if !columnExists {
		if _, err := db.ExecContext(ctx, `
			ALTER TABLE saved_searches ADD COLUMN recipient_contact_ids UUID[] NOT NULL DEFAULT '{}';
			ALTER TABLE saved_searches ADD COLUMN reminder_enabled BOOLEAN NOT NULL DEFAULT false;
			ALTER TABLE saved_searches ADD COLUMN reminder_days_before INTEGER[] NOT NULL DEFAULT '{7,3,1}';
			UPDATE saved_searches ss
			SET recipient_contact_ids = COALESCE((
				SELECT array_agg(cc.id) FROM company_contacts cc
				JOIN company_members cm ON cm.company_profile_id = cc.company_profile_id
				WHERE cm.user_id = ss.user_id
			), '{}');
		`); err != nil {
			return fmt.Errorf("add and backfill saved_searches notification columns: %w", err)
		}
	}
	return nil
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
//
// 항상 "현재 최종" 전체 목록을 쓴다(ensureDeadlineD7EventType 주석의 버그
// 설명 참고) — 이 함수만 옛날 좁은 목록을 쓰면 Apply()가 이 함수를
// ensureTeamInviteEventTypes보다 먼저 재실행할 때 이미 존재하는 team_invite
// 행을 위반해 크래시한다.
func ensureReportEventTypes(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		ALTER TABLE notification_log DROP CONSTRAINT IF EXISTS notification_log_event_type_check;
		ALTER TABLE notification_log ADD CONSTRAINT notification_log_event_type_check
			CHECK (event_type IN ('deadline_d7','deadline_d3','deadline_d1','recommendation_digest','assignee_status_change',
			                       'weekly_report','monthly_report','team_invite','team_invite_accepted','admin_broadcast','password_reset','email_verification','notice_corrected','saved_search_match','notice_cancelled'));
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

// ensureCompanyDocumentsExtractionStatusColumns adds company_documents.
// extraction_status/failure_reason — 실패/성공 여부를 사용자가 사후에
// 확인할 수 있도록(#/ai-usage 화면), Claude 호출 결과를 이제 DB에도
// 남긴다(기존엔 s.logger.Error 로그로만 남고 DB엔 아무 흔적이 없었음).
// extraction_status='success'인 행만 AI 분석 한도로 카운트된다
// (countAIAnalysisThisMonth, billing.go — 2026-08-03 정책: 실패는 어떤
// 이유든 한도를 절대 차감하지 않음). extraction_status가 NULL인 채로
// 남는 행은 "처리중" 상태로 취급한다 — 실제로는 업로드 요청 처리 도중
// 서버가 죽는 것 같은 드문 경우에만 NULL로 영구히 남는다(정상 흐름에서는
// 같은 HTTP 요청 안에서 곧바로 success/failed로 갱신됨). failure_reason은
// Claude API 원본 에러 메시지를 그대로 노출하지 않고
// classifyExtractionFailureReason(company_documents.go)이 만든 사용자
// 친화적 문구만 저장한다.
func ensureCompanyDocumentsExtractionStatusColumns(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		ALTER TABLE company_documents ADD COLUMN IF NOT EXISTS extraction_status TEXT CHECK (extraction_status IN ('success','failed'));
		ALTER TABLE company_documents ADD COLUMN IF NOT EXISTS failure_reason TEXT;
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

// ensureRefundAndCancellationColumns adds the columns billing.go's refund
// (handleBillingRefundRequest) and 해지(handleCancelRenewal/
// ApplyScheduledCancellations) flows need. subscriptions.cancel_at_period_end
// is a separate field from pending_plan on purpose — db/migrations/
// 001_init.sql 주석 참고(pending_plan은 항상 결제를 거쳐야만 설정되는
// "다음 유료 플랜 예약" 전용이라, 결제 없이 무료로 전환하는 해지엔 안
// 맞는다). payment_log.status CHECK는 기존 값 범위를 넓히는 변경이라
// DROP+ADD CONSTRAINT가 필요하다(ensureDeadlineD7EventType과 동일 패턴).
func ensureRefundAndCancellationColumns(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		ALTER TABLE subscriptions ADD COLUMN IF NOT EXISTS cancel_at_period_end BOOLEAN NOT NULL DEFAULT false;
		ALTER TABLE subscriptions ADD COLUMN IF NOT EXISTS cancel_requested_at TIMESTAMPTZ;
		ALTER TABLE payment_log ADD COLUMN IF NOT EXISTS refund_reason TEXT;
		ALTER TABLE payment_log ADD COLUMN IF NOT EXISTS refunded_at TIMESTAMPTZ;
		ALTER TABLE payment_log ADD COLUMN IF NOT EXISTS refund_processed_by TEXT;
		ALTER TABLE payment_log DROP CONSTRAINT IF EXISTS payment_log_status_check;
		ALTER TABLE payment_log ADD CONSTRAINT payment_log_status_check
			CHECK (status IN ('승인','실패','취소','환불'));
	`)
	return err
}

// ensureTeamInviteEventTypes widens notification_log.event_type's CHECK to
// allow 'team_invite'/'team_invite_accepted' (Phase 5 2단계: 팀 초대 발송/
// 수락도 다른 채널과 동일하게 notification_log를 거치게 함 —
// company_team.go 참고). DROP+ADD 방식은 ensureDeadlineD7EventType과 동일.
func ensureTeamInviteEventTypes(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		ALTER TABLE notification_log DROP CONSTRAINT IF EXISTS notification_log_event_type_check;
		ALTER TABLE notification_log ADD CONSTRAINT notification_log_event_type_check
			CHECK (event_type IN ('deadline_d7','deadline_d3','deadline_d1','recommendation_digest','assignee_status_change',
			                       'weekly_report','monthly_report','team_invite','team_invite_accepted','admin_broadcast','password_reset','email_verification','notice_corrected','saved_search_match','notice_cancelled'));
	`)
	return err
}

// ensurePushSubscriptionsTable adds Phase 6(웹푸시/PWA)의 유일한 새 테이블.
// 구독 단위는 "회원"(로그인 계정, users)이다 — 담당자(company_contacts)가
// 아니다. 이메일/SMS는 로그인 없이도 존재하는 담당자 연락처로 보내지만,
// 웹 푸시는 "로그인해서 브라우저 권한을 승인한 특정 기기"에만 보낼 수
// 있어 구조가 다르다(project_phase6_app_requirements 요구사항 확정 사항).
// endpoint에 UNIQUE를 걸어, 같은 기기가 로그아웃 후 다른 계정으로
// 재구독하면 ON CONFLICT (endpoint) DO UPDATE로 소유자를 자연스럽게
// 갈아치운다(push_notifications.go의 handleSubscribePush 참고).
func ensurePushSubscriptionsTable(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS push_subscriptions (
			id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			user_id    UUID NOT NULL REFERENCES users(id),
			endpoint   TEXT NOT NULL UNIQUE,
			p256dh_key TEXT NOT NULL,
			auth_key   TEXT NOT NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now()
		);
		CREATE INDEX IF NOT EXISTS idx_push_subscriptions_user ON push_subscriptions(user_id);
	`)
	return err
}

// ensureSystemSettingsTable adds a generic key-value table for admin-tunable
// runtime settings — 첫 사용처는 Free 플랜 월간 알림성 이메일 한도
// (free_plan_email_limit, notifications.go의 checkEmailNotificationQuota가
// 읽음)지만, 앞으로 다른 설정도 같은 테이블에 추가할 수 있게 범용으로
// 만들었다. 기본값(20)을 시드해둬 관리자가 아직 값을 설정하지 않은
// 새 배포에서도 즉시 동작한다.
func ensureSystemSettingsTable(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS system_settings (
			key        TEXT PRIMARY KEY,
			value      TEXT NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
		);
		INSERT INTO system_settings (key, value) VALUES ('free_plan_email_limit', '20')
			ON CONFLICT (key) DO NOTHING;
	`)
	return err
}

// ensurePlanSettingsSeed seeds system_settings rows for every plan-level 값
// (#/admin/plan-settings, api/plan_settings.go의 planSettingsByPlan과
// 키 이름이 정확히 일치해야 함) — 관리자가 아직 한 번도 안 바꾼 새 배포에서
// 지금 billing.Plans에 하드코딩된 값과 정확히 같은 값으로 시작하도록
// ON CONFLICT DO NOTHING으로 시드한다(즉 이 마이그레이션 자체는 기존 동작을
// 하나도 안 바꾼다 — 바뀌는 건 "이제부터 관리자가 이 값을 조정할 수 있다"는
// 것뿐). free_ai_analysis_limit은 0으로 시드하는데, 이건 billing.Plans의
// 실제 현재 값(Free는 AI분석 자체가 0건 = 이용 불가)과 일치시킨 것이다.
func ensurePlanSettingsSeed(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		INSERT INTO system_settings (key, value) VALUES
			('free_pipeline_limit', '3'),
			('free_ai_analysis_limit', '0'),
			('basic_ai_analysis_limit', '5'),
			('basic_price_krw', '19900'),
			('pro_ai_analysis_limit', '20'),
			('pro_price_krw', '49000'),
			('business_ai_analysis_limit', '60'),
			('business_price_krw', '99000'),
			('business_member_limit', '3')
		ON CONFLICT (key) DO NOTHING;
	`)
	return err
}

// ensureUserDeactivationColumn adds users.deactivated_at — 관리자 회원 탈퇴
// 처리(admin_member_deactivate.go)의 표식. NULL이 아니면 그 계정은 로그인이
// 영구히 막힌다(handleLogin/handleOAuthCallback이 확인). 계정을 실제로
// DELETE하지 않는 이유: 결제이력(payment_log)·감사기록(audit_logs) 등은
// 법적 보관기간을 고려해 유지해야 하고, 그 테이블들이 이 users 행을
// FK로 참조하기 때문이다(참조 무결성 유지).
func ensureUserDeactivationColumn(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		ALTER TABLE users ADD COLUMN IF NOT EXISTS deactivated_at TIMESTAMPTZ;
	`)
	return err
}

// ensureCompanyProfileCustomAILimitColumns adds company_profiles.
// custom_ai_analysis_limit(관리자가 "이번달 한도 임시조정"으로 지정한 값,
// NULL이면 오버라이드 없음)/custom_ai_analysis_limit_month(어느 달
// 오버라이드인지, 'YYYY-MM' 문자열 — DATE 대신 이 형식을 쓴 이유는
// api.effectiveAIAnalysisLimit이 time.Now().Format("2006-01")과 순수
// 문자열 비교만 하면 되게 해서, 타임존 경계 근처의 DATE 비교 오차 걱정을
// 아예 없애기 위함)/custom_ai_analysis_limit_reason(관리자가 남긴 사유,
// 화면에 표시용 — 전체 변경 이력은 audit_logs에 별도로 남는다)을 추가한다.
// 다음 달이 되면(현재 월 문자열과 안 맞으면) 자동으로 플랜 기본값으로
// 되돌아간다(별도 배치/스케줄 불필요 — 조회 시점에 매번 비교).
func ensureCompanyProfileCustomAILimitColumns(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		ALTER TABLE company_profiles ADD COLUMN IF NOT EXISTS custom_ai_analysis_limit INTEGER;
		ALTER TABLE company_profiles ADD COLUMN IF NOT EXISTS custom_ai_analysis_limit_month TEXT;
		ALTER TABLE company_profiles ADD COLUMN IF NOT EXISTS custom_ai_analysis_limit_reason TEXT;
	`)
	return err
}

// ensureNotificationLogSkippedStatus widens notification_log.status's CHECK
// to allow 'skipped_quota' — Free 플랜이 월간 알림성 이메일 한도를 넘겨
// 이메일 채널만 조용히 생략할 때(에러 아님, notification_log에는 남겨서
// 관리자가 "이번달 한도 초과로 스킵된 건수"를 볼 수 있게 함) 쓴다.
// status는 이 CHECK를 이전에 넓힌 적이 없어(event_type과 달리 단일
// 마이그레이션) ensureDeadlineD7EventType류의 "여러 단계가 서로 좁히는"
// 버그 위험이 없다.
func ensureNotificationLogSkippedStatus(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		ALTER TABLE notification_log DROP CONSTRAINT IF EXISTS notification_log_status_check;
		ALTER TABLE notification_log ADD CONSTRAINT notification_log_status_check
			CHECK (status IN ('sent','failed','skipped_quota'));
	`)
	return err
}

// ensureOAuthIdentitiesTable adds 간편로그인(구글/네이버/카카오) 지원 —
// (provider, provider_user_id) 쌍을 users.id에 연결하는 별도 테이블로 뒀다
// (users에 provider/provider_id 컬럼을 직접 추가하는 대신) — 한 사용자가
// 여러 소셜 계정을 동시에 연결할 수 있게 하기 위함(예: 구글로 가입한 뒤
// 나중에 카카오도 연결). 이메일이 같은 기존 계정이 있으면 신규 유저를
// 만들지 않고 이 테이블에 새 행만 추가해 그 계정에 연결한다
// (oauth_login.go의 resolveOAuthUser 참고).
//
// users.password_hash도 여기서 NULL 허용으로 바꾼다 — 소셜 전용 계정(이메일
// 비밀번호를 아예 만든 적 없는 계정)은 비밀번호 해시가 없기 때문이다.
// handleLogin은 이 컬럼이 NULL이면 "social_login_only" 에러를 돌려준다.
func ensureOAuthIdentitiesTable(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		ALTER TABLE users ALTER COLUMN password_hash DROP NOT NULL;

		CREATE TABLE IF NOT EXISTS user_oauth_identities (
			id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			user_id          UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			provider         TEXT NOT NULL CHECK (provider IN ('google','naver','kakao')),
			provider_user_id TEXT NOT NULL,
			email            TEXT,
			created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
			UNIQUE (provider, provider_user_id)
		);
		CREATE INDEX IF NOT EXISTS idx_user_oauth_identities_user ON user_oauth_identities(user_id);
	`)
	return err
}

// ensureBannersTable adds 홈 화면 배너 슬라이드(관리자 CMS 1번, banners.go의
// handleListBanners가 공개 API로 읽는다). 테이블이 완전히 비어 있을 때만
// (WHERE NOT EXISTS) 임시 플레이스홀더 배너 3개를 시드한다 — 이미지는
// 아직 실제 업로드 기능(3단계)이 없어 collector/internal/webui/static/banners/
// 아래 고정 SVG를 가리킨다. 관리자가 이후 이미지를 교체/삭제하면 테이블이
// 더 이상 비어있지 않으므로 재배포 때마다 다시 시드되지 않는다. 첫 배너
// 문구의 "{brand_name}" 토큰은 시드 시점에 고정 문자열로 안 바꾸고 그대로
// 저장한다 — banners.go의 handleListBanners가 응답할 때마다 그 순간의
// company_info.brand_name으로 치환하므로, 브랜드명을 나중에 바꾸면 이미
// 시드된 이 배너 행도 재배포 없이 자동으로 바뀐다(관리자가 #/admin/banners
// 에서 제목을 수정하면서 토큰을 지우면 그 순간부터는 고정 텍스트가 됨).
func ensureBannersTable(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS banners (
			id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			title         TEXT NOT NULL,
			image_url     TEXT NOT NULL,
			link_url      TEXT,
			display_order INTEGER NOT NULL DEFAULT 0,
			is_active     BOOLEAN NOT NULL DEFAULT true,
			starts_at     TIMESTAMPTZ,
			ends_at       TIMESTAMPTZ,
			created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
		);
	`); err != nil {
		return err
	}
	_, err := db.ExecContext(ctx, `
		INSERT INTO banners (title, image_url, link_url, display_order)
		SELECT * FROM (VALUES
			('{brand_name}, 지금 시작하세요', '/banners/banner-1.svg', CAST(NULL AS TEXT), 0),
			('AI 분석으로 서류 자동화', '/banners/banner-2.svg', '#/notices', 1),
			('우리 회사에 맞는 사업, 놓치지 마세요', '/banners/banner-3.svg', '#/growth', 2)
		) AS seed(title, image_url, link_url, display_order)
		WHERE NOT EXISTS (SELECT 1 FROM banners);
	`)
	return err
}

// ensureBannersBrandNameTokenBackfill — {brand_name} 토큰을 도입하기 전에
// 이미 시드가 실행된 환경(로컬 테스트 DB, 운영 DB 등)에서는 첫 배너
// 제목이 그때의 브랜드명이 고정 문자열로 박혀 있다("공공사업 AI 비서,
// 지금 시작하세요"). 그 정확한 문구와 일치하는 행만 토큰 형태로
// 바꿔치기해서 이후 브랜드명이 바뀌면 그 배너도 같이 바뀌게 한다 —
// 관리자가 이미 그 배너를 다른 문구로 수정했다면 문구가 더 이상
// 일치하지 않으므로 건드리지 않는다(관리자의 수동 편집을 존중).
func ensureBannersBrandNameTokenBackfill(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		UPDATE banners
		SET title = '{brand_name}, 지금 시작하세요'
		WHERE title = (SELECT brand_name FROM company_info WHERE id = 1) || ', 지금 시작하세요';
	`)
	return err
}

// ensurePopupsTable adds 관리자 CMS 5번(팝업 관리). banners와 달리 노출
// 위치가 "홈 진입 시 오버레이 레이어" 하나뿐이라 display_order/시드 데이터가
// 없다 — 관리자가 직접 만들기 전까지는 빈 테이블(팝업 없음)이 정상 상태.
func ensurePopupsTable(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS popups (
			id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			title      TEXT NOT NULL,
			image_url  TEXT,
			content    TEXT NOT NULL,
			is_active  BOOLEAN NOT NULL DEFAULT true,
			starts_at  TIMESTAMPTZ,
			ends_at    TIMESTAMPTZ,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now()
		);
	`)
	return err
}

// ensureAnnouncementsTable adds 관리자 CMS 6번(공지 게시판) — 사용자용
// #/announcements(목록/상세, 조회수 증가)와 관리자용 #/admin/announcements
// (CRUD) 양쪽이 이 테이블 하나를 공유한다(별도 관리자 전용 컬럼 없음).
func ensureAnnouncementsTable(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS announcements (
			id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			title      TEXT NOT NULL,
			content    TEXT NOT NULL,
			is_pinned  BOOLEAN NOT NULL DEFAULT false,
			view_count INTEGER NOT NULL DEFAULT 0,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now()
		);
		CREATE INDEX IF NOT EXISTS idx_announcements_pinned_created ON announcements(is_pinned DESC, created_at DESC);
	`)
	return err
}

// ensureBroadcastMessagesTable adds 관리자 CMS 4번(회원 알림 메시지) 발송
// 이력. 실제 발송(이메일/인앱/푸시)은 기존 알림 인프라(notify.Client,
// insertInAppNotification, sendPushToUser)를 그대로 재사용하고, 이 테이블은
// "언제 누구에게 무엇을 보냈는지"만 남긴다 — notification_log(채널별 1행)와
// 달리 발송 1회 = 이 테이블 1행이라 관리자 화면의 발송 이력 목록에 바로 쓸 수
// 있다. channels는 배열(이메일/인앱/푸시 중복선택 가능이라 단일 컬럼 부적합).
func ensureBroadcastMessagesTable(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS broadcast_messages (
			id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			title           TEXT NOT NULL,
			content         TEXT NOT NULL,
			target_plan     TEXT, -- NULL = 전체 회원, 값 있으면 free/basic/pro/business 중 하나만 대상
			channels        TEXT[] NOT NULL,
			recipient_count INTEGER NOT NULL DEFAULT 0,
			created_by      UUID NOT NULL REFERENCES users(id),
			created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
		);
	`)
	return err
}

// ensureAdminBroadcastEventType widens notification_log.event_type's CHECK
// to allow 'admin_broadcast'(broadcast.go가 이메일 채널 발송을 기록할 때
// 씀). 항상 지금까지의 전체 누적 목록을 다시 쓴다 — event_type CHECK를
// 여러 마이그레이션이 각자의 좁은 목록으로 갈아치우면, 나중 마이그레이션이
// 먼저 만든 값을 가진 행이 있을 때 재실행(replay) 시 위반이 나는 버그가
// 과거에 실제로 있었다(ensureTeamInviteEventTypes 주석 참고).
func ensureAdminBroadcastEventType(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		ALTER TABLE notification_log DROP CONSTRAINT IF EXISTS notification_log_event_type_check;
		ALTER TABLE notification_log ADD CONSTRAINT notification_log_event_type_check
			CHECK (event_type IN ('deadline_d7','deadline_d3','deadline_d1','recommendation_digest','assignee_status_change',
			                       'weekly_report','monthly_report','team_invite','team_invite_accepted','admin_broadcast','password_reset','email_verification','notice_corrected','saved_search_match','notice_cancelled'));
	`)
	return err
}

// ensureCompanyInfoTable adds 랜딩페이지 푸터에 표시되는 회사 정보. 싱글턴
// 테이블(항상 정확히 1행) — id를 1로 고정하고 CHECK(id=1)로 강제해 두
// 번째 행 삽입 자체가 제약 위반으로 막힌다(별도 "지금 활성 행이 뭔지"
// 조회 로직이 필요 없음, company_info.go는 항상 WHERE id=1로 읽고 쓴다).
// 처음 배포 시 전부 NULL인 빈 행을 시드해둔다 — 관리자가 #/admin/company-info
// 에서 값을 채우기 전까지 GET /api/company-info는 전부 null을 내려주고,
// 랜딩페이지는 그 경우 항목별로(그리고 전부 비어있으면 블록 전체를)
// 조용히 숨긴다(index.html의 renderLandingCompanyInfo 참고).
func ensureCompanyInfoTable(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS company_info (
			id                            INTEGER PRIMARY KEY DEFAULT 1 CHECK (id = 1),
			company_name                  TEXT,
			business_registration_number  TEXT,
			representative_name           TEXT,
			address                       TEXT,
			main_phone                    TEXT,
			contact_email                 TEXT,
			partnership_email             TEXT,
			patent_number                 TEXT,
			updated_at                    TIMESTAMPTZ NOT NULL DEFAULT now()
		);
		INSERT INTO company_info (id) VALUES (1) ON CONFLICT (id) DO NOTHING;
	`)
	return err
}

// ensureCompanyInfoBrandNameColumn adds company_info.brand_name — 다른 7개
// 필드(회사정보)와 달리 이 값은 비워둘 수 없다(NOT NULL DEFAULT). 사이트
// 곳곳(브라우저 탭 제목, 헤더, 사이드바, 랜딩페이지 로고/푸터)에 항상
// 뭔가는 표시돼야 하기 때문 — 지금 쓰는 가칭 "공공사업 AI 비서"를
// 기본값으로 시드해, 관리자가 실제 브랜드명을 확정하기 전까지도 화면이
// 깨지지 않는다. ADD COLUMN ... NOT NULL DEFAULT는 기존 행(id=1)에도
// 자동으로 이 기본값을 채운다.
func ensureCompanyInfoBrandNameColumn(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		ALTER TABLE company_info ADD COLUMN IF NOT EXISTS brand_name TEXT NOT NULL DEFAULT '공공사업 AI 비서';
	`)
	return err
}

// ensureCompanyInfoMailOrderNumberColumn adds company_info.mail_order_registration_number
// (통신판매업 신고번호, 2026-08-05 — 추천공고 다이제스트 이메일 하단
// 회사정보 표기용으로 신규 요청됨). 나머지 7개 선택 필드와 동일하게
// NULL 허용 — 비어있으면 이메일/랜딩페이지 양쪽 다 해당 줄을 숨긴다.
func ensureCompanyInfoMailOrderNumberColumn(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		ALTER TABLE company_info ADD COLUMN IF NOT EXISTS mail_order_registration_number TEXT;
	`)
	return err
}

// ensurePhoneVerificationTables — 회원가입 온보딩 재설계(2026-08-03,
// Phase 1)의 SMS 인증번호 기반. users.phone_verified_at이 이제 "온보딩
// 완료" 여부의 기준이다(과거엔 company_profiles 존재 여부였음 — 회사
// 프로필 입력을 가입에서 완전히 분리하면서 기준을 바꿈, phone_verification.go
// 참고). phone_verifications는 계정이 아직 없는 상태(이메일 가입 1단계
// 이전)에서도 조회 가능해야 해서 phone_number로 직접 조회하는 독립
// 테이블로 둔다.
func ensurePhoneVerificationTables(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		ALTER TABLE users ADD COLUMN IF NOT EXISTS phone_verified_at TIMESTAMPTZ;
		CREATE TABLE IF NOT EXISTS phone_verifications (
			id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			phone_number   TEXT NOT NULL,
			code_hash      TEXT NOT NULL,
			attempt_count  INTEGER NOT NULL DEFAULT 0,
			verified_at    TIMESTAMPTZ,
			expires_at     TIMESTAMPTZ NOT NULL,
			created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
		);
		CREATE INDEX IF NOT EXISTS idx_phone_verifications_phone ON phone_verifications(phone_number, created_at DESC);
	`)
	return err
}

// ensureAuthLookupAndPasswordResetTables — 아이디(이메일) 찾기/비밀번호
// 찾기(2026-08-04) 신규 기능. auth_lookup_attempts는 두 기능 공용 남용
// 방지 테이블(phoneVerificationRateLimited와 같은 기준 — 같은 identifier
// 1분 1회, 1일 5회, find_email.go/password_reset.go의 authLookupRateLimited
// 참고). password_reset_tokens는 재설정 링크 토큰(평문 대신 SHA-256 해시만
// 저장, 1시간 유효, 1회용).
func ensureAuthLookupAndPasswordResetTables(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS auth_lookup_attempts (
			id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			kind         TEXT NOT NULL CHECK (kind IN ('find_email','reset_password')),
			identifier   TEXT NOT NULL,
			created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
		);
		CREATE INDEX IF NOT EXISTS idx_auth_lookup_attempts_lookup ON auth_lookup_attempts(kind, identifier, created_at DESC);
		CREATE TABLE IF NOT EXISTS password_reset_tokens (
			id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			user_id     UUID NOT NULL REFERENCES users(id),
			token_hash  TEXT NOT NULL,
			expires_at  TIMESTAMPTZ NOT NULL,
			used_at     TIMESTAMPTZ,
			created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
		);
		CREATE INDEX IF NOT EXISTS idx_password_reset_tokens_hash ON password_reset_tokens(token_hash);
	`)
	return err
}

// ensurePasswordResetEventType widens notification_log.event_type's CHECK
// to allow 'password_reset'(password_reset.go가 재설정 이메일 발송 결과를
// 기록할 때 씀). ensureAdminBroadcastEventType과 같은 이유로 항상 지금까지의
// 전체 누적 목록을 다시 쓴다(재실행 시 위반 방지).
func ensurePasswordResetEventType(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		ALTER TABLE notification_log DROP CONSTRAINT IF EXISTS notification_log_event_type_check;
		ALTER TABLE notification_log ADD CONSTRAINT notification_log_event_type_check
			CHECK (event_type IN ('deadline_d7','deadline_d3','deadline_d1','recommendation_digest','assignee_status_change',
			                       'weekly_report','monthly_report','team_invite','team_invite_accepted','admin_broadcast','password_reset','email_verification','notice_corrected','saved_search_match','notice_cancelled'));
	`)
	return err
}

// ensurePhoneVerificationRequiredSetting seeds system_settings의 휴대폰
// 인증 사용 여부 토글(2026-08-04, #/admin 대시보드 "플랜 설정" 카드,
// system_settings.go의 getSystemSettingBool이 읽음). 기본값 'true'는
// 지금까지의 동작(회원가입/온보딩 재설계 Phase 1)을 그대로 유지한다 —
// 이 마이그레이션 자체는 기존 동작을 하나도 안 바꾼다(ensurePlanSettingsSeed와
// 같은 원칙).
func ensurePhoneVerificationRequiredSetting(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		INSERT INTO system_settings (key, value) VALUES ('phone_verification_required', 'true')
			ON CONFLICT (key) DO NOTHING;
	`)
	return err
}

// ensureEmailVerificationTables — 이메일 가입 필수 이메일 인증(2026-08-04,
// email_verification.go). users.email_verified_at이 이 마이그레이션
// 이전부터 이미 있었는지(재실행)를 information_schema로 직접 확인해,
// 컬럼을 "처음" 추가하는 이번 한 번만 기존 회원 전원을 일괄 백필한다
// (created_at 기준 "이미 검증된 것으로 간주") — 그렇지 않고 매 재시작마다
// "email_verified_at IS NULL인 행을 전부 백필"하는 식으로 짰다면, 이
// 기능이 배포된 뒤 새로 가입해 아직 이메일 인증을 안 마친 사용자까지
// 다음 서버 재시작 때 자동으로 "인증완료" 처리되어 이 기능 자체가
// 무력화되는 버그가 생긴다.
func ensureEmailVerificationTables(ctx context.Context, db *sql.DB) error {
	var columnExists bool
	if err := db.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM information_schema.columns
			WHERE table_name = 'users' AND column_name = 'email_verified_at'
		)`).Scan(&columnExists); err != nil {
		return fmt.Errorf("check email_verified_at column: %w", err)
	}
	if !columnExists {
		if _, err := db.ExecContext(ctx, `
			ALTER TABLE users ADD COLUMN email_verified_at TIMESTAMPTZ;
			UPDATE users SET email_verified_at = created_at;
		`); err != nil {
			return fmt.Errorf("add and backfill email_verified_at: %w", err)
		}
	}
	_, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS email_verification_tokens (
			id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			user_id     UUID NOT NULL REFERENCES users(id),
			token_hash  TEXT NOT NULL,
			expires_at  TIMESTAMPTZ NOT NULL,
			used_at     TIMESTAMPTZ,
			created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
		);
		CREATE INDEX IF NOT EXISTS idx_email_verification_tokens_hash ON email_verification_tokens(token_hash);
	`)
	return err
}

// ensureEmailVerificationEventType widens notification_log.event_type's
// CHECK to allow 'email_verification'. ensureAdminBroadcastEventType과
// 같은 이유로 항상 지금까지의 전체 누적 목록을 다시 쓴다.
func ensureEmailVerificationEventType(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		ALTER TABLE notification_log DROP CONSTRAINT IF EXISTS notification_log_event_type_check;
		ALTER TABLE notification_log ADD CONSTRAINT notification_log_event_type_check
			CHECK (event_type IN ('deadline_d7','deadline_d3','deadline_d1','recommendation_digest','assignee_status_change',
			                       'weekly_report','monthly_report','team_invite','team_invite_accepted','admin_broadcast','password_reset','email_verification','notice_corrected','saved_search_match','notice_cancelled'));
	`)
	return err
}

// ensureAuthLookupKindEmailVerifyResend widens auth_lookup_attempts.kind's
// CHECK to allow 'email_verify_resend'(email_verification.go의
// handleResendVerificationEmail이 씀) — ensurePasswordResetEventType과
// 같은 드롭+재생성 방식.
//
// ⚠️ 2026-08-04 사고 이력: 이 목록에 나중에 추가된 'biz_reg_extract'(아래
// ensureAuthLookupKindBizRegExtract 참고)를 처음엔 안 넣어뒀다가 운영에서
// migrate 실패를 냈다 — 이 함수가 매 부팅마다 실행되며 제약을 DROP한 뒤
// 이 좁은 목록으로 다시 ADD하는데, 그 사이 실제로 kind='biz_reg_extract'인
// 행이 이미 쌓여있으면(그 다음 함수가 넓혀놓은 뒤 실제 사용자가 그 기능을
// 써서) ADD CONSTRAINT 자체가 기존 행과 충돌해 실패하고 서버가 아예 못
// 뜬다. notification_log_event_type_check처럼 "이 시점까지의 누적 목록"이
// 아니라 **항상 최종/현재 전체 목록**을 써야 한다 — 이 제약을 손보는
// 함수가 앞으로 더 생기면 전부 이 목록에 동기화해서 매 부팅 재실행 시
// 절대 좁아지지 않게 할 것.
func ensureAuthLookupKindEmailVerifyResend(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		ALTER TABLE auth_lookup_attempts DROP CONSTRAINT IF EXISTS auth_lookup_attempts_kind_check;
		ALTER TABLE auth_lookup_attempts ADD CONSTRAINT auth_lookup_attempts_kind_check
			CHECK (kind IN ('find_email','reset_password','email_verify_resend','biz_reg_extract'));
	`)
	return err
}

// ensureBusinessRegistrationColumns — Phase UX-01(2026-08-04) 사업자등록증
// OCR 자동생성. business_registration.go가 AI로 추출한 값을 사용자가
// 확인한 뒤 handleUpsertCompanyProfile(auth.go)로 저장하는 4개 컬럼.
func ensureBusinessRegistrationColumns(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		ALTER TABLE company_profiles ADD COLUMN IF NOT EXISTS business_registration_number TEXT;
		ALTER TABLE company_profiles ADD COLUMN IF NOT EXISTS company_name TEXT;
		ALTER TABLE company_profiles ADD COLUMN IF NOT EXISTS representative_name TEXT;
		ALTER TABLE company_profiles ADD COLUMN IF NOT EXISTS address TEXT;
	`)
	return err
}

// ensureCompanyProfileFoundingDateColumn adds company_profiles.founding_date —
// 2026-08-06, 기업프로필-맞춤공고 통합 작업 중 발견한 근본 문제 수정.
// 이전까지는 개업일 원본을 저장하는 컬럼이 아예 없어(business_registration.go/
// 온보딩 채팅/프로필 수정모달 3곳이 전부 그 순간 computeBusinessAgeYears로
// business_age_years만 계산해 저장하고 원본 날짜는 버림), 이후 재입력하지
// 않으면 업력이 그 시점에 멈춘 채 굳어버리는 구조였다. 이제 founding_date를
// source of truth로 저장하고, business_age_years는 매 조회 시 서버에서
// 재계산해 응답한다(auth.go getCompanyProfile 참고) — "값이 아니라 원본
// 날짜를 저장해서 항상 최신으로 계산"하는 원칙으로 전환. 신규 컬럼이라
// 백필 대상 없음(과거엔 원본 자체가 없었으므로 NULL로 시작, 사용자가
// 다음에 개업일을 입력하는 순간부터 정확해짐).
func ensureCompanyProfileFoundingDateColumn(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		ALTER TABLE company_profiles ADD COLUMN IF NOT EXISTS founding_date DATE;
	`)
	return err
}

// ensureSavedSearchIsActiveColumn adds saved_searches.is_active — 2026-08-07,
// "복제본 기본 비활성화" + "활성/비활성 토글" 기능. 기존 컬럼은 전부
// 새로 만든 게 아니라 켜져 있으면 도는 값이라, "복제 직후엔 아예 아무
// 것도 하지 않는" 상태를 표현하려면 alert_enabled/reminder_enabled와
// 별개인 마스터 스위치가 필요했다. 기본값 true라 기존 조건은 전부
// 지금처럼 그대로 작동하고, 새로 복제되는 조건만 saved_searches.go가
// 명시적으로 false를 넣는다.
func ensureSavedSearchIsActiveColumn(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		ALTER TABLE saved_searches ADD COLUMN IF NOT EXISTS is_active BOOLEAN NOT NULL DEFAULT true;
	`)
	return err
}

// ensureAuthLookupKindBizRegExtract widens auth_lookup_attempts.kind's CHECK
// to allow 'biz_reg_extract'(business_registration.go의
// handleExtractBusinessRegistration이 씀) — 같은 드롭+재생성 방식.
func ensureAuthLookupKindBizRegExtract(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		ALTER TABLE auth_lookup_attempts DROP CONSTRAINT IF EXISTS auth_lookup_attempts_kind_check;
		ALTER TABLE auth_lookup_attempts ADD CONSTRAINT auth_lookup_attempts_kind_check
			CHECK (kind IN ('find_email','reset_password','email_verify_resend','biz_reg_extract'));
	`)
	return err
}

// ensurePipelineAssigneeUserIDColumn — 2026-08-06, 담당자-회원계정 FK 연결.
// 기존 assignee_name/email/phone(자유텍스트)은 조직에 등록된 로그인
// 계정이 아닌 개인사업자·외부 협력자를 담당자로 지정할 때도 여전히 필요해
// 그대로 남겨두고, 조직에 실제 회원계정(company_members)이 있는 경우
// 그 계정과 연결할 수 있도록 nullable FK 하나만 추가한다. NULL이면
// 기존처럼 자유텍스트 담당자라는 뜻 — 기존 데이터는 손대지 않는다.
func ensurePipelineAssigneeUserIDColumn(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		ALTER TABLE notice_pipeline_entries ADD COLUMN IF NOT EXISTS assignee_user_id UUID REFERENCES users(id);
	`)
	return err
}

// ensureSubscriptionsPreviousPlanColumns — 2026-08-06, 상위 플랜 환불 사고
// (Basic 유효 중 Pro로 즉시업그레이드한 뒤 Pro만 환불했더니 Basic까지
// 통째로 사라짐 — 실사고 발생, 수동 조치로 우선 복구함) 이후 재설계.
// subscriptions는 프로필당 한 행만 유지하는 구조라 즉시업그레이드가 이전
// plan/expires_at을 덮어쓰는 순간 그 정보가 영구히 사라진다. 전체
// 이력 테이블로 바꾸는 대신 "직전 한 단계"만 기억하는 절충안 컬럼 2개를
// 추가한다 — 실제 환불 판단(billing_refund.go의 bestValidPriorPayment)은
// 이 컬럼이 아니라 payment_log를 다시 계산해서 하지만(여러 건이 겹친
// 경우까지 안전하게 커버하려고), 이 컬럼은 지원팀이 계정 상태를 볼 때
// "즉시업그레이드 직전에 뭐가 있었는지"를 바로 확인할 수 있는 감사 근거로
// 남긴다.
func ensureSubscriptionsPreviousPlanColumns(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		ALTER TABLE subscriptions ADD COLUMN IF NOT EXISTS previous_plan TEXT;
		ALTER TABLE subscriptions ADD COLUMN IF NOT EXISTS previous_plan_expires_at TIMESTAMPTZ;
	`)
	return err
}
