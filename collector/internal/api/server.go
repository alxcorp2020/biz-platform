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

	"github.com/lib/pq"

	"biz-platform/collector/internal/webui"
)

type Server struct {
	db            *sql.DB
	logger        *slog.Logger
	sessionSecret []byte
}

func New(db *sql.DB, logger *slog.Logger, sessionSecret []byte) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{db: db, logger: logger, sessionSecret: sessionSecret}
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
	mux.HandleFunc("GET /api/dashboard", s.handleDashboard)
	mux.HandleFunc("POST /api/notices/{id}/evaluate", s.handleEvaluateNotice)
	mux.HandleFunc("PUT /api/notices/{noticeId}/documents/{documentId}/checklist", s.handleToggleChecklistItem)
	mux.HandleFunc("PUT /api/notices/{id}/bookmark", s.handleToggleBookmark)
	mux.HandleFunc("GET /api/me/bookmarks", s.handleListBookmarks)
	mux.HandleFunc("GET /api/review/queue", s.handleReviewQueue)
	mux.HandleFunc("POST /api/review/eligibility-conditions/{id}", s.handleReviewEligibilityCondition)
	mux.HandleFunc("POST /api/review/required-documents/{id}", s.handleReviewRequiredDocument)
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

func (s *Server) handleListNotices(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	region := q.Get("region")
	industry := q.Get("industry")
	keyword := q.Get("q")
	userID, loggedIn := s.currentUserID(r)

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
	query := `
		SELECT n.id, n.title, n.organization_name, n.region, n.industry, n.status,
		       n.application_end_at, n.budget_amount, n.official_url, n.current_version,
		       (nb.id IS NOT NULL) AS is_bookmarked
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
	query += " ORDER BY n.published_at DESC NULLS LAST LIMIT 50"

	rows, err := s.db.QueryContext(r.Context(), query, args...)
	if err != nil {
		s.logger.Error("list notices query failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	defer rows.Close()

	items := []noticeListItem{}
	for rows.Next() {
		var it noticeListItem
		var org, region, industry, officialURL sql.NullString
		var budget sql.NullInt64
		var deadline sql.NullTime
		if err := rows.Scan(&it.ID, &it.Title, &org, &region, &industry, &it.Status,
			&deadline, &budget, &officialURL, &it.CurrentVersion, &it.IsBookmarked); err != nil {
			s.logger.Error("scan notice row failed", "error", err)
			continue
		}
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
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "count": len(items)})
}

func (s *Server) handleGetNotice(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	userID, loggedIn := s.currentUserID(r)

	var it noticeListItem
	var org, region, industry, officialURL sql.NullString
	var budget sql.NullInt64
	var deadline sql.NullTime

	err := s.db.QueryRowContext(r.Context(), `
		SELECT n.id, n.title, n.organization_name, n.region, n.industry, n.status,
		       n.application_end_at, n.budget_amount, n.official_url, n.current_version,
		       (nb.id IS NOT NULL) AS is_bookmarked
		FROM notices n
		LEFT JOIN notice_bookmarks nb ON nb.notice_id = n.id AND nb.user_id = $2
		WHERE n.id = $1`, id, sql.NullString{String: userID, Valid: loggedIn},
	).Scan(&it.ID, &it.Title, &org, &region, &industry, &it.Status,
		&deadline, &budget, &officialURL, &it.CurrentVersion, &it.IsBookmarked)

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
	if loggedIn {
		var companyRegion, companySize sql.NullString
		var companyIndustry pq.StringArray
		err := s.db.QueryRowContext(r.Context(),
			`SELECT id, region, industry, company_size FROM company_profiles WHERE user_id = $1`, userID,
		).Scan(&profileID, &companyRegion, &companyIndustry, &companySize)
		if err != nil && err != sql.ErrNoRows {
			s.logger.Error("get notice: profile lookup failed", "error", err)
		}
		if err == nil {
			company := companyScoringInput{Region: companyRegion, Industry: []string(companyIndustry), Size: companySize}
			computed := scoreNoticeForCompany(
				noticeScoringInput{Region: region, Industry: industry, BudgetAmount: budget},
				company,
			)
			score = &computed
		}
	}

	eligibilityConditions := []eligibilityConditionItem{}
	requiredDocuments := []requiredDocumentItem{}
	attachments := []attachmentItem{}
	var rawDetail *noticeRawDetail
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
	}

	checkedCount := 0
	for _, d := range requiredDocuments {
		if d.Checked {
			checkedCount++
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"notice":                it,
		"changes":               changes,
		"eligibilityConditions": eligibilityConditions,
		"requiredDocuments":     requiredDocuments,
		"documentReadiness":     map[string]int{"total": len(requiredDocuments), "checked": checkedCount},
		"attachments":           attachments,
		"detail":                rawDetail,
		"participationScore":    score,
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
