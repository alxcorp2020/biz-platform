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
)

type Server struct {
	db     *sql.DB
	logger *slog.Logger
}

func New(db *sql.DB, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{db: db, logger: logger}
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /api/notices", s.handleListNotices)
	mux.HandleFunc("GET /api/notices/{id}", s.handleGetNotice)
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

	writeJSON(w, http.StatusOK, map[string]any{"notice": it, "changes": changes})
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
		w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
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
