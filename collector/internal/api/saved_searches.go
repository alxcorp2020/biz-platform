// saved_searches.go — "맞춤공고"(2026-08-06, 경쟁서비스 비드큐 격차점검
// 4번). 사용자가 검색조건(지역/업종/발주기관/금액범위/포함·제외 키워드)을
// 여러 개 저장해두고, 매일 새로 매칭되는 공고를 알림으로 받는 기능.
// company_profiles 기반 AI 자동추천(scoreNoticeForCompany/eligibility.go,
// notifications.go의 sendRecommendationDigest)과는 완전히 별개다 — 저건
// 조직당 1개(지역/업종)로 판정 로직까지 얽혀있고, 이건 사용자가 자기
// 계정(user_id) 단위로 순수 필터 조건을 몇 개든 만들 수 있다(팀 공유
// 아님 — 사용자 확정). 매칭 자체는 GET /api/notices의 필터 확장을 그대로
// 재사용한다(server.go, noticeType/organizationName/budgetMin/budgetMax/
// keywordsInclude/keywordsExclude 파라미터) — 별도 매칭 쿼리를 새로
// 만들지 않는다.
package api

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/lib/pq"
)

type savedSearchItem struct {
	ID               string    `json:"id"`
	Name             string    `json:"name"`
	NoticeType       *string   `json:"noticeType"`
	Region           *string   `json:"region"`
	Industry         *string   `json:"industry"`
	OrganizationName *string   `json:"organizationName"`
	BudgetMin        *int64    `json:"budgetMin"`
	BudgetMax        *int64    `json:"budgetMax"`
	KeywordsInclude  []string  `json:"keywordsInclude"`
	KeywordsExclude  []string  `json:"keywordsExclude"`
	AlertEnabled     bool      `json:"alertEnabled"`
	CreatedAt        time.Time `json:"createdAt"`
	UpdatedAt        time.Time `json:"updatedAt"`
}

const savedSearchSelect = `
	SELECT id, name, notice_type, region, industry, organization_name, budget_min, budget_max,
	       keywords_include, keywords_exclude, alert_enabled, created_at, updated_at
	FROM saved_searches`

func scanSavedSearch(row interface{ Scan(dest ...any) error }) (*savedSearchItem, error) {
	var it savedSearchItem
	var noticeType, region, industry, orgName sql.NullString
	var budgetMin, budgetMax sql.NullInt64
	var include, exclude pq.StringArray
	if err := row.Scan(&it.ID, &it.Name, &noticeType, &region, &industry, &orgName, &budgetMin, &budgetMax,
		&include, &exclude, &it.AlertEnabled, &it.CreatedAt, &it.UpdatedAt); err != nil {
		return nil, err
	}
	it.NoticeType = nullStringPtr(noticeType)
	it.Region = nullStringPtr(region)
	it.Industry = nullStringPtr(industry)
	it.OrganizationName = nullStringPtr(orgName)
	if budgetMin.Valid {
		it.BudgetMin = &budgetMin.Int64
	}
	if budgetMax.Valid {
		it.BudgetMax = &budgetMax.Int64
	}
	it.KeywordsInclude = []string(include)
	it.KeywordsExclude = []string(exclude)
	return &it, nil
}

func (s *Server) handleListSavedSearches(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.currentUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	rows, err := s.db.QueryContext(r.Context(), savedSearchSelect+` WHERE user_id = $1 ORDER BY created_at ASC`, userID)
	if err != nil {
		s.logger.Error("list-saved-searches: query failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	defer rows.Close()

	items := []savedSearchItem{}
	for rows.Next() {
		it, err := scanSavedSearch(rows)
		if err != nil {
			s.logger.Error("list-saved-searches: scan failed", "error", err)
			continue
		}
		items = append(items, *it)
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// savedSearchRequest — keywordsInclude/Exclude는 프론트에서 콤마로 구분한
// 문자열을 잘라 보낸다(빈 항목은 프론트가 미리 거르지만, 방어적으로
// 여기서도 빈 문자열은 저장 직전에 걸러낸다).
type savedSearchRequest struct {
	Name             string   `json:"name"`
	NoticeType       *string  `json:"noticeType"`
	Region           *string  `json:"region"`
	Industry         *string  `json:"industry"`
	OrganizationName *string  `json:"organizationName"`
	BudgetMin        *int64   `json:"budgetMin"`
	BudgetMax        *int64   `json:"budgetMax"`
	KeywordsInclude  []string `json:"keywordsInclude"`
	KeywordsExclude  []string `json:"keywordsExclude"`
	AlertEnabled     bool     `json:"alertEnabled"`
}

func cleanKeywords(in []string) []string {
	out := make([]string, 0, len(in))
	for _, k := range in {
		k = strings.TrimSpace(k)
		if k != "" {
			out = append(out, k)
		}
	}
	return out
}

func (s *Server) handleCreateSavedSearch(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.currentUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	var req savedSearchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_request_body"})
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name_required"})
		return
	}

	var id string
	err := s.db.QueryRowContext(r.Context(), `
		INSERT INTO saved_searches
			(user_id, name, notice_type, region, industry, organization_name, budget_min, budget_max,
			 keywords_include, keywords_exclude, alert_enabled)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		RETURNING id`,
		userID, req.Name, req.NoticeType, req.Region, req.Industry, req.OrganizationName, req.BudgetMin, req.BudgetMax,
		pq.Array(cleanKeywords(req.KeywordsInclude)), pq.Array(cleanKeywords(req.KeywordsExclude)), req.AlertEnabled,
	).Scan(&id)
	if err != nil {
		s.logger.Error("create-saved-search: insert failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": id, "status": "created"})
}

func (s *Server) handleUpdateSavedSearch(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.currentUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	id := r.PathValue("id")
	var req savedSearchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_request_body"})
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name_required"})
		return
	}

	res, err := s.db.ExecContext(r.Context(), `
		UPDATE saved_searches SET
			name = $1, notice_type = $2, region = $3, industry = $4, organization_name = $5,
			budget_min = $6, budget_max = $7, keywords_include = $8, keywords_exclude = $9,
			alert_enabled = $10, updated_at = now()
		WHERE id = $11 AND user_id = $12`,
		req.Name, req.NoticeType, req.Region, req.Industry, req.OrganizationName, req.BudgetMin, req.BudgetMax,
		pq.Array(cleanKeywords(req.KeywordsInclude)), pq.Array(cleanKeywords(req.KeywordsExclude)), req.AlertEnabled,
		id, userID,
	)
	if err != nil {
		s.logger.Error("update-saved-search: update failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "saved_search_not_found"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

func (s *Server) handleDeleteSavedSearch(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.currentUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	id := r.PathValue("id")
	res, err := s.db.ExecContext(r.Context(), `DELETE FROM saved_searches WHERE id = $1 AND user_id = $2`, id, userID)
	if err != nil {
		s.logger.Error("delete-saved-search: delete failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "saved_search_not_found"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}
