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
	"fmt"
	"net/http"
	"sort"
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
	// IsActive — 2026-08-07, 카드의 활성/비활성 토글. false면 alert_enabled/
	// reminder_enabled 설정과 무관하게 다이제스트·리마인더 발송 대상에서
	// 완전히 제외된다(sendSavedSearchDigest/sendSavedSearchDeadlineReminders의
	// WHERE절 참고). "복제" 직후엔 항상 false로 시작 — 원본과 조건이 100%
	// 같은 상태라 확인 없이 그대로 두면 중복 매칭/중복 알림이 발생하기
	// 때문(handleDuplicateSavedSearch). 일반 생성은 DB 기본값(true)을 그대로
	// 쓴다. "결과 보기"(수동 미리보기)는 비활성 상태에서도 계속 동작한다 —
	// 사용자가 켜기 전에 내용을 확인하는 용도라 의도적으로 막지 않는다.
	IsActive bool `json:"isActive"`
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
	       recipient_contact_ids, reminder_enabled, reminder_days_before, is_active, created_at, updated_at
	FROM saved_searches`

func scanSavedSearch(row interface{ Scan(dest ...any) error }) (*savedSearchItem, error) {
	var it savedSearchItem
	var noticeType, region, industry, orgName, origin sql.NullString
	var budgetMin, budgetMax sql.NullInt64
	var include, exclude, recipientContactIDs pq.StringArray
	var reminderDaysBefore pq.Int64Array
	if err := row.Scan(&it.ID, &it.Name, &noticeType, &region, &industry, &orgName, &budgetMin, &budgetMax,
		&include, &exclude, &it.AlertEnabled, &origin,
		&recipientContactIDs, &it.ReminderEnabled, &reminderDaysBefore, &it.IsActive, &it.CreatedAt, &it.UpdatedAt); err != nil {
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

// ---------- 중복 조건 방지(2026-08-07) ----------
// savedSearchConditionFields — "이 값들이 전부 같으면 동일한 조건"의
// 비교 대상 필드만 모은 구조체. 이름/알림설정(alertEnabled)/수신담당자/
// 리마인더 설정은 사용자 스펙상 비교 대상이 아니다(이것만 달라도 중복
// 아님). companySize는 saved_searches 테이블에 컬럼 자체가 없다 — 그
// 값은 company_profiles에서 오는 전역 값이라 같은 사용자의 모든 조건에
// 항상 동일하므로(변별력이 없음) 비교에 넣으나 빼나 결과가 같아 생략.
type savedSearchConditionFields struct {
	Region           *string
	Industry         *string
	OrganizationName *string
	BudgetMin        *int64
	BudgetMax        *int64
	KeywordsInclude  []string
	KeywordsExclude  []string
}

func stringPtrEqual(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func int64PtrEqual(a, b *int64) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func stringSliceEqualSorted(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	sa := append([]string(nil), a...)
	sb := append([]string(nil), b...)
	sort.Strings(sa)
	sort.Strings(sb)
	for i := range sa {
		if sa[i] != sb[i] {
			return false
		}
	}
	return true
}

func (f savedSearchConditionFields) equal(o savedSearchConditionFields) bool {
	return stringPtrEqual(f.Region, o.Region) &&
		stringPtrEqual(f.Industry, o.Industry) &&
		stringPtrEqual(f.OrganizationName, o.OrganizationName) &&
		int64PtrEqual(f.BudgetMin, o.BudgetMin) &&
		int64PtrEqual(f.BudgetMax, o.BudgetMax) &&
		stringSliceEqualSorted(f.KeywordsInclude, o.KeywordsInclude) &&
		stringSliceEqualSorted(f.KeywordsExclude, o.KeywordsExclude)
}

// findDuplicateSavedSearch — 저장(생성/수정) 직전에 같은 사용자가 가진
// 다른 맞춤공고 중 조건 필드가 완전히 같은 게 있는지 찾는다. excludeID는
// 수정 시 자기 자신을 비교 대상에서 빼기 위함(생성 시엔 빈 문자열 —
// saved_searches.id는 항상 uuid라 빈 문자열과 절대 같을 수 없어 안전).
// 동시 저장 경합(두 탭에서 동시에 같은 조건을 저장하는 등)에 대비해
// DB 유니크 제약이 아니라 매 저장 요청마다 서버에서 재검증한다(스펙
// 요구사항 — 폼 입력 중에는 막지 않고 실제 저장 시도 시점에만 검증).
func (s *Server) findDuplicateSavedSearch(ctx context.Context, userID, excludeID string, candidate savedSearchConditionFields) (*savedSearchItem, error) {
	// excludeID가 빈 문자열(생성 시)이면 그대로 uuid 파라미터에 바인딩하지
	// 않는다 — id 컬럼이 uuid 타입이라 ""를 캐스팅하려다 "invalid input
	// syntax for type uuid" 에러가 난다. nil을 넘겨 SQL NULL로 보내고,
	// $2::uuid IS NULL이면 그 어떤 행도 제외하지 않도록 한다.
	var excludeArg any
	if excludeID != "" {
		excludeArg = excludeID
	}
	rows, err := s.db.QueryContext(ctx, savedSearchSelect+` WHERE user_id = $1 AND ($2::uuid IS NULL OR id != $2::uuid)`, userID, excludeArg)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		it, err := scanSavedSearch(rows)
		if err != nil {
			continue
		}
		existing := savedSearchConditionFields{
			Region: it.Region, Industry: it.Industry, OrganizationName: it.OrganizationName,
			BudgetMin: it.BudgetMin, BudgetMax: it.BudgetMax,
			KeywordsInclude: it.KeywordsInclude, KeywordsExclude: it.KeywordsExclude,
		}
		if candidate.equal(existing) {
			return it, nil
		}
	}
	return nil, rows.Err()
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

	includeKeywords := cleanKeywords(req.KeywordsInclude)
	excludeKeywords := cleanKeywords(req.KeywordsExclude)
	dup, err := s.findDuplicateSavedSearch(ctx, userID, "", savedSearchConditionFields{
		Region: req.Region, Industry: req.Industry, OrganizationName: req.OrganizationName,
		BudgetMin: req.BudgetMin, BudgetMax: req.BudgetMax,
		KeywordsInclude: includeKeywords, KeywordsExclude: excludeKeywords,
	})
	if err != nil {
		s.logger.Error("create-saved-search: duplicate check failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if dup != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "duplicate_saved_search", "existingName": dup.Name})
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
		pq.Array(includeKeywords), pq.Array(excludeKeywords), req.AlertEnabled,
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

	includeKeywords := cleanKeywords(req.KeywordsInclude)
	excludeKeywords := cleanKeywords(req.KeywordsExclude)
	dup, err := s.findDuplicateSavedSearch(ctx, userID, id, savedSearchConditionFields{
		Region: req.Region, Industry: req.Industry, OrganizationName: req.OrganizationName,
		BudgetMin: req.BudgetMin, BudgetMax: req.BudgetMax,
		KeywordsInclude: includeKeywords, KeywordsExclude: excludeKeywords,
	})
	if err != nil {
		s.logger.Error("update-saved-search: duplicate check failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if dup != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "duplicate_saved_search", "existingName": dup.Name})
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
		pq.Array(includeKeywords), pq.Array(excludeKeywords), req.AlertEnabled,
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

// handleDeleteSavedSearch — origin='onboarding'인 "내 기본 조건"은 삭제를
// 거부한다(2026-08-07). 프론트도 삭제 버튼을 비활성화해두지만, 직접 API
// 호출로 우회하는 경우를 막기 위해 서버에서도 반드시 재검증한다 — 이
// 항목이 삭제되면 기업정보(지역/업종/기업규모)와의 양방향 동기화 코드가
// "온보딩 기본 조건은 정확히 1개"라고 가정하는 전제가 깨진다.
func (s *Server) handleDeleteSavedSearch(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.currentUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	id := r.PathValue("id")

	var origin sql.NullString
	err := s.db.QueryRowContext(r.Context(), `SELECT origin FROM saved_searches WHERE id = $1 AND user_id = $2`, id, userID).Scan(&origin)
	if err == sql.ErrNoRows {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "saved_search_not_found"})
		return
	}
	if err != nil {
		s.logger.Error("delete-saved-search: lookup failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if origin.Valid && origin.String == "onboarding" {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "onboarding_default_not_deletable"})
		return
	}

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

// findActiveSavedSearchIndustryConflict — 2026-08-07, 활성화 시도 시 업종
// 중복 검사. 같은 사용자(user_id) 소유의 다른 "활성" 조건 중 업종이
// 정확히 같은 게 있으면 그 이름을 반환한다(없으면 빈 문자열) — 이
// 기능 전체의 기존 원칙("개인 단위, 팀 공유 아님", saved_searches.go
// 맨 위 주석 참고)을 그대로 따라 비교 범위도 이 사용자 자신의 조건으로
// 한정한다(팀원 전체로 넓혔다가 사용자 재지시로 되돌림, 2026-08-07).
// industry가 비어있으면(업종 제한 없음) 검사를 건너뛴다 — "업종이
// 겹친다"는 표현은 구체적인 업종값이 같을 때를 뜻한다고 해석했다.
func (s *Server) findActiveSavedSearchIndustryConflict(ctx context.Context, userID, excludeID, industry string) (string, error) {
	if industry == "" {
		return "", nil
	}
	var name string
	err := s.db.QueryRowContext(ctx, `
		SELECT name FROM saved_searches
		WHERE user_id = $1 AND is_active = true AND industry = $2 AND id != $3
		LIMIT 1`, userID, industry, excludeID).Scan(&name)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return name, nil
}

// handleSetSavedSearchActive — 2026-08-07, 카드의 활성/비활성 토글 스위치
// 전용 경량 엔드포인트(이름/조건 등 다른 필드는 건드리지 않는다 — 그건
// handleUpdateSavedSearch의 몫). "복제" 직후 자동으로 꺼진 상태로 시작하는
// 조건을 사용자가 확인 후 직접 켜는 용도. 검증 규칙 2가지(2026-08-07
// 최종 확정):
//  1. origin='onboarding'("내 기본 조건")은 항상 활성 상태로 고정 —
//     끄려는 시도(isActive=false) 자체를 거부한다. 프론트도 이 카드의
//     토글 UI를 아예 렌더링하지 않지만, 직접 API 호출을 막기 위해
//     서버에서도 재검증한다.
//  2. 그 외 조건을 켜려는 시도(isActive=true)는 이 사용자 자신의 다른
//     활성 조건과 업종이 겹치면 거부한다(중복 추천/중복 알림 방지) —
//     끄는 시도는 검사하지 않는다.
func (s *Server) handleSetSavedSearchActive(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.currentUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	id := r.PathValue("id")
	var req struct {
		IsActive bool `json:"isActive"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_request_body"})
		return
	}

	ctx := r.Context()
	var origin, industry sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT origin, industry FROM saved_searches WHERE id = $1 AND user_id = $2`, id, userID).Scan(&origin, &industry)
	if err == sql.ErrNoRows {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "saved_search_not_found"})
		return
	}
	if err != nil {
		s.logger.Error("set-saved-search-active: lookup failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}

	if origin.Valid && origin.String == "onboarding" {
		if !req.IsActive {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "onboarding_default_always_active"})
			return
		}
	} else if req.IsActive {
		conflictName, err := s.findActiveSavedSearchIndustryConflict(ctx, userID, id, industry.String)
		if err != nil {
			s.logger.Error("set-saved-search-active: conflict check failed", "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
			return
		}
		if conflictName != "" {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "industry_overlap", "conflictName": conflictName})
			return
		}
	}

	res, err := s.db.ExecContext(ctx,
		`UPDATE saved_searches SET is_active = $1, updated_at = now() WHERE id = $2 AND user_id = $3`,
		req.IsActive, id, userID)
	if err != nil {
		s.logger.Error("set-saved-search-active: update failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "saved_search_not_found"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

// savedSearchNamesForUser — handleDuplicateSavedSearch가 "OO 복제/복제 2/
// 복제 3..." 순번을 매길 때 이미 쓰인 이름을 피하기 위해 조회한다.
func (s *Server) savedSearchNamesForUser(ctx context.Context, userID string) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name FROM saved_searches WHERE user_id = $1`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	names := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			continue
		}
		names[n] = true
	}
	return names, rows.Err()
}

// nextDuplicateName — base("{원본이름} 복제")가 안 쓰였으면 그대로, 이미
// 있으면 "{base} 2", "{base} 3"...으로 다음 빈 순번을 찾는다.
func nextDuplicateName(existing map[string]bool, base string) string {
	if !existing[base] {
		return base
	}
	for n := 2; ; n++ {
		candidate := fmt.Sprintf("%s %d", base, n)
		if !existing[candidate] {
			return candidate
		}
	}
}

// handleDuplicateSavedSearch — 2026-08-07, 카드의 "복제" 버튼. 조건 필드를
// 그대로 복사한 새 저장검색을 만들어 프론트가 곧바로 그 새 항목의 수정
// 폼을 열 수 있게 한다. 의도적으로 findDuplicateSavedSearch(중복 조건
// 저장 방지)를 거치지 않는다 — 복제는 "일부러 똑같은 조건을 복사해서
// 그 다음에 사용자가 값을 바꾸게" 하는 흐름이라, 이 시점엔 원본과
// 조건이 100% 같은 게 당연하고 의도된 상태다. origin은 절대 그대로
// 복사하지 않는다 — 'onboarding'은 "내 기본 조건" 카드 정확히 1개를
// 가리키는 특수 값(기업정보와 지역/업종/기업규모를 양방향 동기화하는
// 코드가 origin='onboarding' 항목이 하나뿐이라고 가정한다) — 복제본은
// 항상 독립된 일반 조건(origin=NULL)으로 만든다. is_active도 원본 상태와
// 무관하게 항상 false로 시작한다(2026-08-07) — 원본과 조건이 100% 같은
// 상태에서 확인 없이 바로 활성화되면 중복 매칭/중복 알림이 발생하기
// 때문, 사용자가 내용을 확인·수정한 뒤 카드의 토글을 직접 켜야 한다.
func (s *Server) handleDuplicateSavedSearch(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.currentUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	id := r.PathValue("id")
	ctx := r.Context()

	// 이름 확인 모달(2026-08-07)에서 사용자가 직접 확정한 이름을 보낼 수
	// 있다 — 요청 본문이 아예 없거나(구버전 프론트) 비어 있으면 기존처럼
	// 서버가 자동으로 "OO 복제/복제 2..." 이름을 계산한다.
	var req struct {
		Name string `json:"name"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req) // 본문 없음/파싱 실패는 무시하고 자동 이름으로 폴백

	row := s.db.QueryRowContext(ctx, savedSearchSelect+` WHERE id = $1 AND user_id = $2`, id, userID)
	source, err := scanSavedSearch(row)
	if err == sql.ErrNoRows {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "saved_search_not_found"})
		return
	}
	if err != nil {
		s.logger.Error("duplicate-saved-search: lookup failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}

	newName := strings.TrimSpace(req.Name)
	if newName == "" {
		existingNames, err := s.savedSearchNamesForUser(ctx, userID)
		if err != nil {
			s.logger.Error("duplicate-saved-search: name lookup failed", "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
			return
		}
		newName = nextDuplicateName(existingNames, source.Name+" 복제")
	}

	var newID string
	err = s.db.QueryRowContext(ctx, `
		INSERT INTO saved_searches
			(user_id, name, notice_type, region, industry, organization_name, budget_min, budget_max,
			 keywords_include, keywords_exclude, alert_enabled, recipient_contact_ids, reminder_enabled, reminder_days_before, is_active)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,false)
		RETURNING id`,
		userID, newName, source.NoticeType, source.Region, source.Industry, source.OrganizationName,
		source.BudgetMin, source.BudgetMax, pq.Array(source.KeywordsInclude), pq.Array(source.KeywordsExclude),
		source.AlertEnabled, pq.Array(source.RecipientContactIDs), source.ReminderEnabled, pq.Array(source.ReminderDaysBefore),
	).Scan(&newID)
	if err != nil {
		s.logger.Error("duplicate-saved-search: insert failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": newID, "status": "duplicated"})
}
