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
	mux.HandleFunc("POST /api/notices/{id}/evaluate", s.handleEvaluateNotice)
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

	query := `
		SELECT id, title, organization_name, region, industry, status,
		       application_end_at, budget_amount, official_url, current_version
		FROM notices WHERE 1=1`
	args := []any{}
	argN := 0
	addArg := func(v any) string {
		argN++
		args = append(args, v)
		return "$" + itoa(argN)
	}
	if region != "" {
		query += " AND region = " + addArg(region)
	}
	if industry != "" {
		query += " AND industry = " + addArg(industry)
	}
	if keyword != "" {
		query += " AND title ILIKE " + addArg("%"+keyword+"%")
	}
	query += " ORDER BY published_at DESC NULLS LAST LIMIT 50"

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
			&deadline, &budget, &officialURL, &it.CurrentVersion); err != nil {
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

	var it noticeListItem
	var org, region, industry, officialURL sql.NullString
	var budget sql.NullInt64
	var deadline sql.NullTime

	err := s.db.QueryRowContext(r.Context(), `
		SELECT id, title, organization_name, region, industry, status,
		       application_end_at, budget_amount, official_url, current_version
		FROM notices WHERE id = $1`, id,
	).Scan(&it.ID, &it.Title, &org, &region, &industry, &it.Status,
		&deadline, &budget, &officialURL, &it.CurrentVersion)

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
		requiredDocuments, err = s.listRequiredDocuments(r.Context(), versionID)
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

	writeJSON(w, http.StatusOK, map[string]any{
		"notice":                it,
		"changes":               changes,
		"eligibilityConditions": eligibilityConditions,
		"requiredDocuments":     requiredDocuments,
		"attachments":           attachments,
		"detail":                rawDetail,
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
	DocumentName     string `json:"documentName"`
	SourceText       string `json:"sourceText"`
	IsRequired       bool   `json:"isRequired"`
	ExtractionMethod string `json:"extractionMethod"`
}

func (s *Server) listRequiredDocuments(ctx context.Context, versionID string) ([]requiredDocumentItem, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT document_name, COALESCE(source_text, ''), is_required, extraction_method
		FROM required_documents
		WHERE notice_version_id = $1 AND review_status != 'rejected'
		ORDER BY document_name`, versionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []requiredDocumentItem{}
	for rows.Next() {
		var it requiredDocumentItem
		if err := rows.Scan(&it.DocumentName, &it.SourceText, &it.IsRequired, &it.ExtractionMethod); err != nil {
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
