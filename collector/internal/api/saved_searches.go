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
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/lib/pq"
)

type savedSearchItem struct {
	ID               string   `json:"id"`
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
	// Origin — 2026-08-06. 'onboarding'이면 온보딩 완료 시 자동 생성된
	// "내 기본 조건"(프론트가 이 값으로 "온보딩 시 자동 생성됨" 배지를
	// 붙인다). nil이면 사용자가 직접 만든 일반 조건.
	Origin *string `json:"origin"`
	// RecipientContactIDs/ReminderEnabled/ReminderDaysBefore — 2026-08-06,
	// 기업프로필의 "담당자 관리"/"알림 설정"을 맞춤공고 화면으로 통합.
	// RecipientContactIDs가 비어 있으면 발송 시점에 검색 소유자 로그인
	// 이메일로 폴백한다(saved_search_digest.go resolveSavedSearchRecipients).
	// ReminderDaysBefore는 7/3/1 중 선택(company_profiles.notification_days_before와
	// 동일한 다중선택 패턴이지만 파이프라인이 아니라 이 검색에 매칭되는
	// 공고 전체를 대상으로 한다는 점이 다르다).
	RecipientContactIDs []string  `json:"recipientContactIds"`
	ReminderEnabled     bool      `json:"reminderEnabled"`
	ReminderDaysBefore  []int     `json:"reminderDaysBefore"`
	CreatedAt           time.Time `json:"createdAt"`
	UpdatedAt           time.Time `json:"updatedAt"`
}

const savedSearchSelect = `
	SELECT id, name, notice_type, region, industry, organization_name, budget_min, budget_max,
	       keywords_include, keywords_exclude, alert_enabled, origin,
	       recipient_contact_ids, reminder_enabled, reminder_days_before, created_at, updated_at
	FROM saved_searches`

func scanSavedSearch(row interface{ Scan(dest ...any) error }) (*savedSearchItem, error) {
	var it savedSearchItem
	var noticeType, region, industry, orgName, origin sql.NullString
	var budgetMin, budgetMax sql.NullInt64
	var include, exclude, recipientContactIDs pq.StringArray
	var reminderDaysBefore pq.Int64Array
	if err := row.Scan(&it.ID, &it.Name, &noticeType, &region, &industry, &orgName, &budgetMin, &budgetMax,
		&include, &exclude, &it.AlertEnabled, &origin,
		&recipientContactIDs, &it.ReminderEnabled, &reminderDaysBefore, &it.CreatedAt, &it.UpdatedAt); err != nil {
		return nil, err
	}
	it.NoticeType = nullStringPtr(noticeType)
	it.Region = nullStringPtr(region)
	it.Industry = nullStringPtr(industry)
	it.OrganizationName = nullStringPtr(orgName)
	it.Origin = nullStringPtr(origin)
	if budgetMin.Valid {
		it.BudgetMin = &budgetMin.Int64
	}
	if budgetMax.Valid {
		it.BudgetMax = &budgetMax.Int64
	}
	it.KeywordsInclude = []string(include)
	it.KeywordsExclude = []string(exclude)
	it.RecipientContactIDs = []string(recipientContactIDs)
	it.ReminderDaysBefore = make([]int, len(reminderDaysBefore))
	for i, v := range reminderDaysBefore {
		it.ReminderDaysBefore[i] = int(v)
	}
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
	Name                string   `json:"name"`
	NoticeType          *string  `json:"noticeType"`
	Region              *string  `json:"region"`
	Industry            *string  `json:"industry"`
	OrganizationName    *string  `json:"organizationName"`
	BudgetMin           *int64   `json:"budgetMin"`
	BudgetMax           *int64   `json:"budgetMax"`
	KeywordsInclude     []string `json:"keywordsInclude"`
	KeywordsExclude     []string `json:"keywordsExclude"`
	AlertEnabled        bool     `json:"alertEnabled"`
	RecipientContactIDs []string `json:"recipientContactIds"`
	ReminderEnabled     bool     `json:"reminderEnabled"`
	ReminderDaysBefore  []int    `json:"reminderDaysBefore"`
}

// validSavedSearchReminderDays keeps only the allowed offsets(7/3/1, same
// set sendSavedSearchDeadlineReminders schedules) — dedup + drop anything
// else a malformed request might send instead of 400ing(다른 saved_search
// 필드 검증 관례와 동일하게 관대하게 정리해서 저장).
func validSavedSearchReminderDays(in []int) []int {
	allowed := map[int]bool{7: true, 3: true, 1: true}
	seen := map[int]bool{}
	out := []int{}
	for _, d := range in {
		if allowed[d] && !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
	}
	return out
}

// filterOwnedContactIDs keeps only the ids that are actually company_contacts
// belonging to userID's own company — a saved search must never be able to
// designate another company's contact as a notification recipient (IDOR
// 방지). 회사 프로필이 없으면(이론상 온보딩 필수라 발생하지 않음) 빈
// 목록을 반환한다.
func (s *Server) filterOwnedContactIDs(ctx context.Context, userID string, ids []string) ([]string, error) {
	if len(ids) == 0 {
		return []string{}, nil
	}
	profileID, err := s.companyProfileIDForUser(ctx, userID)
	if err != nil || profileID == "" {
		return []string{}, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id FROM company_contacts WHERE company_profile_id = $1 AND id = ANY($2)`,
		profileID, pq.Array(ids))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			continue
		}
		out = append(out, id)
	}
	return out, rows.Err()
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

	ctx := r.Context()
	ownedContactIDs, err := s.filterOwnedContactIDs(ctx, userID, req.RecipientContactIDs)
	if err != nil {
		s.logger.Error("create-saved-search: contact ownership check failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}

	var id string
	err = s.db.QueryRowContext(ctx, `
		INSERT INTO saved_searches
			(user_id, name, notice_type, region, industry, organization_name, budget_min, budget_max,
			 keywords_include, keywords_exclude, alert_enabled, recipient_contact_ids, reminder_enabled, reminder_days_before)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
		RETURNING id`,
		userID, req.Name, req.NoticeType, req.Region, req.Industry, req.OrganizationName, req.BudgetMin, req.BudgetMax,
		pq.Array(cleanKeywords(req.KeywordsInclude)), pq.Array(cleanKeywords(req.KeywordsExclude)), req.AlertEnabled,
		pq.Array(ownedContactIDs), req.ReminderEnabled, pq.Array(validSavedSearchReminderDays(req.ReminderDaysBefore)),
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

	ctx := r.Context()
	ownedContactIDs, err := s.filterOwnedContactIDs(ctx, userID, req.RecipientContactIDs)
	if err != nil {
		s.logger.Error("update-saved-search: contact ownership check failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}

	res, err := s.db.ExecContext(ctx, `
		UPDATE saved_searches SET
			name = $1, notice_type = $2, region = $3, industry = $4, organization_name = $5,
			budget_min = $6, budget_max = $7, keywords_include = $8, keywords_exclude = $9,
			alert_enabled = $10, recipient_contact_ids = $11, reminder_enabled = $12, reminder_days_before = $13,
			updated_at = now()
		WHERE id = $14 AND user_id = $15`,
		req.Name, req.NoticeType, req.Region, req.Industry, req.OrganizationName, req.BudgetMin, req.BudgetMax,
		pq.Array(cleanKeywords(req.KeywordsInclude)), pq.Array(cleanKeywords(req.KeywordsExclude)), req.AlertEnabled,
		pq.Array(ownedContactIDs), req.ReminderEnabled, pq.Array(validSavedSearchReminderDays(req.ReminderDaysBefore)),
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
