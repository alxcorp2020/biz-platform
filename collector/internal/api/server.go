// Package api exposes the read-only public endpoints from spec 13.1
// (GET /api/notices, GET /api/notices/{id}) directly against Postgres.
package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/lib/pq"

	"biz-platform/collector/internal/billing"
	"biz-platform/collector/internal/collector/sources/scsbid"
	"biz-platform/collector/internal/notify"
	"biz-platform/collector/internal/webui"
)

type Server struct {
	db              *sql.DB
	logger          *slog.Logger
	sessionSecret   []byte
	attachmentDir   string
	anthropicClient *anthropic.Client
	notify          *notify.Client
	smsNotify       *notify.SMSClient
	toss            *billing.TossClient
	tossClientKey   string
	// appBaseURL — 팀 초대 이메일 링크 생성에만 쓰인다(company_team.go).
	// 프론트의 다른 리다이렉트(Toss 성공/실패 URL 등)는 location.origin을
	// 클라이언트에서 직접 쓰므로 이 값이 필요 없다 — 서버가 직접 링크
	// 문자열을 만들어야 하는 유일한 경우가 이메일 발송이라 여기만 필요.
	appBaseURL string
	// scsbidSource — nil이면(서비스키 미설정) 낙찰이력 수집이 비활성화된
	// 상태. handleRunAwardHistoryIngestion의 수동 트리거와 cmd/apiserver의
	// 일일 티커 둘 다 이 필드를 쓴다.
	scsbidSource *scsbid.Source
}

func New(db *sql.DB, logger *slog.Logger, sessionSecret []byte, attachmentDir string, anthropicClient *anthropic.Client, notifyClient *notify.Client, smsNotifyClient *notify.SMSClient, tossClient *billing.TossClient, tossClientKey string, appBaseURL string, scsbidSource *scsbid.Source) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{
		db:              db,
		logger:          logger,
		sessionSecret:   sessionSecret,
		attachmentDir:   attachmentDir,
		anthropicClient: anthropicClient,
		notify:          notifyClient,
		smsNotify:       smsNotifyClient,
		toss:            tossClient,
		tossClientKey:   tossClientKey,
		appBaseURL:      appBaseURL,
		scsbidSource:    scsbidSource,
	}
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /api/notices", s.handleListNotices)
	mux.HandleFunc("GET /api/notices/{id}", s.handleGetNotice)
	mux.HandleFunc("POST /api/auth/signup", s.handleSignup)
	mux.HandleFunc("POST /api/auth/login", s.handleLogin)
	mux.HandleFunc("POST /api/auth/logout", s.handleLogout)
	mux.HandleFunc("GET /api/me", s.handleMe)
	mux.HandleFunc("PUT /api/me/company-profile", s.handleUpsertCompanyProfile)
	mux.HandleFunc("GET /api/me/company/members", s.handleListCompanyMembers)
	mux.HandleFunc("DELETE /api/me/company/members/{id}", s.handleRemoveCompanyMember)
	mux.HandleFunc("POST /api/me/company/invitations", s.handleCreateInvitation)
	mux.HandleFunc("GET /api/invitations/{token}", s.handleGetInvitation)
	mux.HandleFunc("POST /api/invitations/{token}/accept", s.handleAcceptInvitation)
	mux.HandleFunc("GET /api/dashboard", s.handleDashboard)
	mux.HandleFunc("POST /api/notices/{id}/evaluate", s.handleEvaluateNotice)
	mux.HandleFunc("PUT /api/notices/{noticeId}/documents/{documentId}/checklist", s.handleToggleChecklistItem)
	mux.HandleFunc("PUT /api/notices/{id}/bookmark", s.handleToggleBookmark)
	mux.HandleFunc("GET /api/me/bookmarks", s.handleListBookmarks)
	mux.HandleFunc("GET /api/review/queue", s.handleReviewQueue)
	mux.HandleFunc("POST /api/review/eligibility-conditions/{id}", s.handleReviewEligibilityCondition)
	mux.HandleFunc("POST /api/review/required-documents/{id}", s.handleReviewRequiredDocument)
	mux.HandleFunc("POST /api/me/company-profile/documents", s.handleUploadCompanyDocument)
	mux.HandleFunc("POST /api/me/licenses", s.handleCreateLicense)
	mux.HandleFunc("GET /api/me/licenses", s.handleListLicenses)
	mux.HandleFunc("POST /api/me/certifications", s.handleCreateCertification)
	mux.HandleFunc("GET /api/me/certifications", s.handleListCertifications)
	mux.HandleFunc("POST /api/me/financials/documents", s.handleUploadFinancialDocument)
	mux.HandleFunc("POST /api/me/financials", s.handleCreateFinancial)
	mux.HandleFunc("GET /api/me/financials", s.handleListFinancials)
	mux.HandleFunc("POST /api/me/track-records/documents", s.handleUploadTrackRecordDocument)
	mux.HandleFunc("POST /api/me/track-records", s.handleCreateTrackRecord)
	mux.HandleFunc("GET /api/me/track-records", s.handleListTrackRecords)
	mux.HandleFunc("POST /api/me/personnel/documents", s.handleUploadPersonnelDocument)
	mux.HandleFunc("POST /api/me/personnel", s.handleCreatePersonnel)
	mux.HandleFunc("GET /api/me/personnel", s.handleListPersonnel)
	mux.HandleFunc("POST /api/me/intellectual-property/documents", s.handleUploadIPDocument)
	mux.HandleFunc("POST /api/me/intellectual-property", s.handleCreateIP)
	mux.HandleFunc("GET /api/me/intellectual-property", s.handleListIP)
	mux.HandleFunc("GET /api/me/profile-completeness", s.handleGetProfileCompleteness)
	mux.HandleFunc("GET /api/me/contacts", s.handleListContacts)
	mux.HandleFunc("POST /api/me/contacts", s.handleCreateContact)
	mux.HandleFunc("PATCH /api/me/contacts/{id}", s.handleUpdateContact)
	mux.HandleFunc("DELETE /api/me/contacts/{id}", s.handleDeleteContact)
	mux.HandleFunc("POST /api/me/company-profile/employee-verification/documents", s.handleUploadEmployeeVerificationDocument)
	mux.HandleFunc("POST /api/me/company-profile/employee-verification", s.handleConfirmEmployeeVerification)
	mux.HandleFunc("POST /api/notices/{id}/pipeline", s.handleCreatePipelineEntry)
	mux.HandleFunc("PATCH /api/pipeline/{id}", s.handleUpdatePipelineEntry)
	mux.HandleFunc("PATCH /api/pipeline/{id}/checklist/{itemId}", s.handleUpdateChecklistItem)
	mux.HandleFunc("GET /api/pipeline", s.handleListPipeline)
	mux.HandleFunc("GET /api/pipeline/{id}", s.handleGetPipelineEntry)
	mux.HandleFunc("GET /api/pipeline/{id}/calendar.ics", s.handleGetPipelineCalendar)
	mux.HandleFunc("PATCH /api/me/notification-settings", s.handleUpdateNotificationSettings)
	mux.HandleFunc("POST /api/admin/run-notifications", s.handleRunNotifications)
	mux.HandleFunc("POST /api/admin/run-pipeline-auto-transitions", s.handleRunPipelineAutoTransitions)
	mux.HandleFunc("POST /api/admin/run-award-history-ingestion", s.handleRunAwardHistoryIngestion)
	mux.HandleFunc("GET /api/me/subscription", s.handleGetSubscription)
	mux.HandleFunc("GET /api/me/ai-usage", s.handleGetAIUsage)
	mux.HandleFunc("GET /api/billing/config", s.handleGetBillingConfig)
	mux.HandleFunc("POST /api/billing/checkout", s.handleBillingCheckout)
	mux.HandleFunc("POST /api/billing/confirm", s.handleBillingConfirm)
	mux.Handle("/", webui.Handler())
	return withLogging(s.logger, withCORS(mux))
}

type noticeListItem struct {
	ID               string     `json:"id"`
	Title            string     `json:"title"`
	OrganizationName string     `json:"organizationName"`
	Region           string     `json:"region"`
	Industry         string     `json:"industry"`
	Status           string     `json:"status"`
	ApplicationEndAt *time.Time `json:"applicationEndAt"`
	BudgetAmount     *int64     `json:"budgetAmount"`
	OfficialURL      string     `json:"officialUrl"`
	CurrentVersion   int        `json:"currentVersion"`
	IsBookmarked     bool       `json:"isBookmarked"`
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if err := s.db.PingContext(r.Context()); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "db_unreachable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

const (
	defaultNoticeListLimit = 20
	maxNoticeListLimit     = 100
)

// parseListingIntParam parses an offset/limit query param, falling back to
// def on empty/invalid/negative input — callers never see a malformed page
// as a 400, they just get sane defaults (같은 관용: 목록 조회는 사용자 입력
// 실수로 에러 화면을 띄우기보다 조용히 기본값으로 복구하는 쪽이 낫다).
func parseListingIntParam(raw string, def int) int {
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return def
	}
	return n
}

func (s *Server) handleListNotices(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	region := q.Get("region")
	industry := q.Get("industry")
	keyword := q.Get("q")
	userID, loggedIn := s.currentUserID(r)

	offset := parseListingIntParam(q.Get("offset"), 0)
	limit := parseListingIntParam(q.Get("limit"), defaultNoticeListLimit)
	if limit <= 0 || limit > maxNoticeListLimit {
		limit = defaultNoticeListLimit
	}

	args := []any{}
	argN := 0
	addArg := func(v any) string {
		argN++
		args = append(args, v)
		return "$" + itoa(argN)
	}

	// LEFT JOIN + NULL 파라미터 트릭: 비로그인이면 sql.NullString{Valid:false}를
	// 바인딩해서 "x = NULL"이 항상 거짓이 되게 만든다 — listRequiredDocuments가
	// 이미 쓰고 있는 것과 같은 패턴(별도 인증 분기 없이 isBookmarked가 자연히 false).
	// COUNT(*) OVER()로 전체 매칭 건수를 같은 쿼리 안에서 함께 받는다 — 운영
	// 규모(2천건 미만)에서는 페이지당 별도 COUNT 쿼리를 또 날릴 필요가 없다.
	query := `
		SELECT n.id, n.title, n.organization_name, n.region, n.industry, n.status,
		       n.application_end_at, n.budget_amount, n.official_url, n.current_version,
		       (nb.id IS NOT NULL) AS is_bookmarked,
		       COUNT(*) OVER() AS total_count
		FROM notices n
		LEFT JOIN notice_bookmarks nb ON nb.notice_id = n.id AND nb.user_id = ` + addArg(sql.NullString{String: userID, Valid: loggedIn}) + `
		WHERE 1=1`
	if region != "" {
		query += " AND n.region = " + addArg(region)
	}
	if industry != "" {
		query += " AND n.industry = " + addArg(industry)
	}
	if keyword != "" {
		query += " AND n.title ILIKE " + addArg("%"+keyword+"%")
	}
	query += " ORDER BY n.published_at DESC NULLS LAST LIMIT " + addArg(limit) + " OFFSET " + addArg(offset)

	rows, err := s.db.QueryContext(r.Context(), query, args...)
	if err != nil {
		s.logger.Error("list notices query failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	defer rows.Close()

	items := []noticeListItem{}
	total := 0
	for rows.Next() {
		var it noticeListItem
		var org, region, industry, officialURL sql.NullString
		var budget sql.NullInt64
		var deadline sql.NullTime
		var totalCount int
		if err := rows.Scan(&it.ID, &it.Title, &org, &region, &industry, &it.Status,
			&deadline, &budget, &officialURL, &it.CurrentVersion, &it.IsBookmarked, &totalCount); err != nil {
			s.logger.Error("scan notice row failed", "error", err)
			continue
		}
		total = totalCount
		it.OrganizationName = org.String
		it.Region = region.String
		it.Industry = industry.String
		it.OfficialURL = officialURL.String
		if budget.Valid {
			it.BudgetAmount = &budget.Int64
		}
		if deadline.Valid {
			it.ApplicationEndAt = &deadline.Time
		}
		items = append(items, it)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items": items, "count": len(items),
		"offset": offset, "limit": limit, "total": total,
		"hasMore": offset+len(items) < total,
	})
}

func (s *Server) handleGetNotice(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	userID, loggedIn := s.currentUserID(r)

	var it noticeListItem
	var org, region, industry, officialURL, department sql.NullString
	var budget sql.NullInt64
	var deadline sql.NullTime

	err := s.db.QueryRowContext(r.Context(), `
		SELECT n.id, n.title, n.organization_name, n.region, n.industry, n.status,
		       n.application_end_at, n.budget_amount, n.official_url, n.current_version,
		       (nb.id IS NOT NULL) AS is_bookmarked, n.department_name
		FROM notices n
		LEFT JOIN notice_bookmarks nb ON nb.notice_id = n.id AND nb.user_id = $2
		WHERE n.id = $1`, id, sql.NullString{String: userID, Valid: loggedIn},
	).Scan(&it.ID, &it.Title, &org, &region, &industry, &it.Status,
		&deadline, &budget, &officialURL, &it.CurrentVersion, &it.IsBookmarked, &department)

	if err == sql.ErrNoRows {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "notice_not_found"})
		return
	}
	if err != nil {
		s.logger.Error("get notice query failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	it.OrganizationName, it.Region, it.Industry, it.OfficialURL = org.String, region.String, industry.String, officialURL.String
	if budget.Valid {
		it.BudgetAmount = &budget.Int64
	}
	if deadline.Valid {
		it.ApplicationEndAt = &deadline.Time
	}

	changes, err := s.listChanges(r.Context(), id)
	if err != nil {
		s.logger.Error("list changes failed", "error", err)
	}

	// 로그인 + 회사 프로필이 있으면 이 자리에서 바로 참여 가능성을 계산해
	// 응답에 얹는다(비영속 — DB에 안 씀). "AI 참여 분석" 섹션이 상세 페이지
	// 로드 시 자동으로 채워지도록 하기 위함이며, 같은 계산을 dashboard.go의
	// scoreNoticeForCompany와 공유한다.
	var profileID string
	var score *participationScore
	var company companyScoringInput
	var isMinimalProfile bool
	if loggedIn {
		companyProfile, err := s.getCompanyProfile(r, userID)
		if err != nil {
			s.logger.Error("get notice: profile lookup failed", "error", err)
		}
		if err == nil && companyProfile != nil {
			profileID = companyProfile.ID
			var companyRegion, companySize sql.NullString
			if companyProfile.Region != nil {
				companyRegion = sql.NullString{String: *companyProfile.Region, Valid: true}
			}
			if companyProfile.CompanySize != nil {
				companySize = sql.NullString{String: *companyProfile.CompanySize, Valid: true}
			}
			companyIndustry := pq.StringArray(companyProfile.Industry)
			trackRecordMax, err := s.fetchTrackRecordMaxAmount(r.Context(), profileID)
			if err != nil {
				s.logger.Error("get notice: track record max amount query failed", "error", err)
			}
			company = companyScoringInput{
				Region: companyRegion, Industry: []string(companyIndustry), Size: companySize,
				TrackRecordMaxAmount: trackRecordMax,
			}
			computed := scoreNoticeForCompany(
				noticeScoringInput{Region: region, Industry: industry, BudgetAmount: budget},
				company,
			)
			score = &computed

			isMinimalProfile, err = s.profileHasNoOptionalData(r.Context(), profileID)
			if err != nil {
				s.logger.Error("get notice: minimal-profile check failed", "error", err)
			}
		}
	}

	eligibilityConditions := []eligibilityConditionItem{}
	requiredDocuments := []requiredDocumentItem{}
	attachments := []attachmentItem{}
	var rawDetail *noticeRawDetail
	var aiSummary *noticeAISummary
	versionID, err := s.currentVersionID(r.Context(), id, it.CurrentVersion)
	if err != nil {
		s.logger.Error("get notice: current version lookup failed", "error", err)
	} else {
		eligibilityConditions, err = s.listEligibilityConditions(r.Context(), versionID)
		if err != nil {
			s.logger.Error("list eligibility conditions failed", "error", err)
		}
		requiredDocuments, err = s.listRequiredDocuments(r.Context(), versionID, profileID)
		if err != nil {
			s.logger.Error("list required documents failed", "error", err)
		}
		attachments, err = s.listAttachments(r.Context(), versionID)
		if err != nil {
			s.logger.Error("list attachments failed", "error", err)
		}
		rawDetail, err = s.fetchNoticeRawDetail(r.Context(), versionID)
		if err != nil {
			s.logger.Error("fetch notice raw detail failed", "error", err)
		}
		aiSummary, err = s.fetchNoticeAISummary(r.Context(), versionID)
		if err != nil {
			s.logger.Error("fetch notice AI summary failed", "error", err)
		}
	}

	var impact *changeImpact
	if score != nil && versionID != "" {
		impact, err = s.computeLatestChangeImpact(r.Context(), versionID,
			noticeScoringInput{Region: region, Industry: industry, BudgetAmount: budget}, company, *score)
		if err != nil {
			s.logger.Error("compute change impact failed", "error", err)
			impact = nil
		}
	}

	checkedCount := 0
	for _, d := range requiredDocuments {
		if d.Checked {
			checkedCount++
		}
	}

	// 경쟁사/낙찰이력: notice_award_history가 비어 있어도(수집기가 아직
	// 없음) awardHistory는 count=0인 정상 응답을 내려준다 — 프론트가
	// 이 경우 "아직 수집된 낙찰 이력이 없습니다"로 자연스럽게 표시.
	awardHistory, err := s.fetchOrganizationAwardHistory(r.Context(), it.OrganizationName, department.String)
	if err != nil {
		s.logger.Error("fetch organization award history failed", "error", err)
	}
	var hasCompetitiveOverlap bool
	if profileID != "" {
		hasCompetitiveOverlap, err = s.hasTrackRecordOverlap(r.Context(), profileID, it.OrganizationName, industry.String)
		if err != nil {
			s.logger.Error("check track record overlap failed", "error", err)
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"notice":                   it,
		"changes":                  changes,
		"eligibilityConditions":    eligibilityConditions,
		"requiredDocuments":        requiredDocuments,
		"documentReadiness":        map[string]int{"total": len(requiredDocuments), "checked": checkedCount},
		"attachments":              attachments,
		"detail":                   rawDetail,
		"participationScore":       score,
		"isMinimalProfile":         isMinimalProfile,
		"aiSummary":                aiSummary,
		"changeImpact":             impact,
		"organizationAwardHistory": awardHistory,
		"hasCompetitiveOverlap":    hasCompetitiveOverlap,
	})
}

func (s *Server) currentVersionID(ctx context.Context, noticeID string, currentVersion int) (string, error) {
	var versionID string
	err := s.db.QueryRowContext(ctx,
		`SELECT id FROM notice_versions WHERE notice_id = $1 AND version_number = $2`,
		noticeID, currentVersion,
	).Scan(&versionID)
	return versionID, err
}

type eligibilityConditionItem struct {
	Category         string  `json:"category"`
	ConditionName    string  `json:"conditionName"`
	SourceText       string  `json:"sourceText"`
	Confidence       float64 `json:"confidence"`
	ReviewStatus     string  `json:"reviewStatus"`
	ExtractionMethod string  `json:"extractionMethod"`
}

// listEligibilityConditions returns only document-derived rows (source_attachment_id
// IS NOT NULL) — it excludes the synthetic 지역/업종/예산규모 rows that
// handleEvaluateNotice auto-creates to satisfy eligibility_evaluations' FK,
// which carry condition_name "auto:region" etc. and aren't real extracted text.
// Rule-based and AI-supplemented rows are both included (extractionMethod
// tells the frontend which is which) — a rule-based review_required row
// isn't superseded just because an AI row exists alongside it, so hiding it
// would drop information rather than just deduplicate.
func (s *Server) listEligibilityConditions(ctx context.Context, versionID string) ([]eligibilityConditionItem, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT category, condition_name, source_text, confidence, review_status, extraction_method
		FROM eligibility_conditions
		WHERE notice_version_id = $1 AND source_attachment_id IS NOT NULL AND review_status != 'rejected'
		ORDER BY created_at`, versionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []eligibilityConditionItem{}
	for rows.Next() {
		var it eligibilityConditionItem
		if err := rows.Scan(&it.Category, &it.ConditionName, &it.SourceText, &it.Confidence, &it.ReviewStatus, &it.ExtractionMethod); err != nil {
			continue
		}
		out = append(out, it)
	}
	return out, nil
}

type requiredDocumentItem struct {
	ID               string `json:"id"`
	DocumentName     string `json:"documentName"`
	SourceText       string `json:"sourceText"`
	IsRequired       bool   `json:"isRequired"`
	ExtractionMethod string `json:"extractionMethod"`
	Checked          bool   `json:"checked"`
}

// listRequiredDocuments takes profileID so it can report each item's
// checklist state for the current user. When profileID is "" (not logged
// in / no company profile), it's bound as SQL NULL — the join condition
// never matches NULL, so every item comes back unchecked without a
// separate query path.
func (s *Server) listRequiredDocuments(ctx context.Context, versionID, profileID string) ([]requiredDocumentItem, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT rd.id, rd.document_name, COALESCE(rd.source_text, ''), rd.is_required, rd.extraction_method,
		       COALESCE(dci.is_checked, false)
		FROM required_documents rd
		LEFT JOIN document_checklist_items dci
		       ON dci.required_document_id = rd.id AND dci.company_profile_id = $2
		WHERE rd.notice_version_id = $1 AND rd.review_status != 'rejected'
		ORDER BY rd.document_name`, versionID, sql.NullString{String: profileID, Valid: profileID != ""})
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []requiredDocumentItem{}
	for rows.Next() {
		var it requiredDocumentItem
		if err := rows.Scan(&it.ID, &it.DocumentName, &it.SourceText, &it.IsRequired, &it.ExtractionMethod, &it.Checked); err != nil {
			continue
		}
		out = append(out, it)
	}
	return out, nil
}

type changeItem struct {
	Field      string    `json:"field"`
	OldValue   string    `json:"oldValue"`
	NewValue   string    `json:"newValue"`
	Importance string    `json:"importance"`
	CreatedAt  time.Time `json:"createdAt"`
}

func (s *Server) listChanges(ctx context.Context, noticeID string) ([]changeItem, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT changed_field, COALESCE(old_value,''), COALESCE(new_value,''), importance, created_at
		FROM notice_changes WHERE notice_id = $1 ORDER BY created_at DESC LIMIT 50`, noticeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []changeItem
	for rows.Next() {
		var c changeItem
		if err := rows.Scan(&c.Field, &c.OldValue, &c.NewValue, &c.Importance, &c.CreatedAt); err != nil {
			continue
		}
		out = append(out, c)
	}
	return out, nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func withLogging(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		logger.Info("request", "method", r.Method, "path", r.URL.Path, "duration_ms", time.Since(start).Milliseconds())
	})
}

func itoa(n int) string {
	return strconv.Itoa(n)
}
