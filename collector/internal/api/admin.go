// admin.go — 플랫폼 전체를 관리하는 최종관리자(users.role='system_admin')
// 전용 화면(#/admin)의 백엔드. 스키마에 system_admin 값 자체는 이미
// 있었지만(REVIEWER_ROLES에 포함, 검토대기 화면 접근 등) 별도 관리자
// 대시보드/회원관리 화면은 이번에 처음 만든다.
//
// "회원"의 단위: 이 서비스는 계정(users)이 아니라 조직(company_profiles)이
// 결제 단위다(팀기능). 그래서 "플랜별 회원 분포"는 각 계정이 소속된
// 조직의 실효 플랜(effectivePlanFromRow) 기준으로 센다 — Business 조직
// 멤버 3명이면 3명 다 business 버킷에 들어간다. 아직 조직이 없는 계정은
// free로 취급(company_members에 없으면 애초에 유료 혜택을 받을 수 없음).
//
// "이번달 AI 분석 사용 건수"는 요청 문구에 notification_log가 언급됐지만
// 실제 근거 테이블이 아니다(그건 이메일/SMS 알림 로그) — billing.go의
// checkAIAnalysisQuota와 같은 진짜 근거인 company_documents(업로드 1건
// = Claude 호출 1건)를 그대로 재사용했다.
package api

import (
	"context"
	"database/sql"
	"net/http"
	"strings"
	"time"

	"biz-platform/collector/internal/billing"
)

// requireSystemAdmin — #/admin이 쓰는 모든 엔드포인트가 공유하는 접근
// 검사. 기존 관리자 배치 트리거(handleRunNotifications 등)는 각자 인라인
// 검사를 반복하고 있어 그대로 두고(이미 동작 중인 코드를 건드릴 이유가
// 없음), 이번에 새로 추가하는 admin.go 핸들러들만 이걸 공유한다.
func (s *Server) requireSystemAdmin(w http.ResponseWriter, r *http.Request) (userID string, ok bool) {
	userID, authed := s.currentUserID(r)
	if !authed {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return "", false
	}
	role, err := s.userRole(r.Context(), userID)
	if err != nil {
		s.logger.Error("admin: role lookup failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return "", false
	}
	if role != "system_admin" {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
		return "", false
	}
	return userID, true
}

type adminPaymentItem struct {
	Email              string    `json:"email"`
	Plan               string    `json:"plan"`
	PlanName           string    `json:"planName"`
	Amount             int64     `json:"amount"`
	Status             string    `json:"status"`
	RequestedAt        time.Time `json:"requestedAt"`
	PaymentMethodLabel *string   `json:"paymentMethodLabel"`
}

type adminDashboardResponse struct {
	TotalMembers        int                `json:"totalMembers"`
	NewMembersThisMonth int                `json:"newMembersThisMonth"`
	PlanDistribution    map[string]int     `json:"planDistribution"`
	RevenueThisMonthKRW int64              `json:"revenueThisMonthKRW"`
	AIAnalysisThisMonth int                `json:"aiAnalysisThisMonth"`
	RecentPayments      []adminPaymentItem `json:"recentPayments"`
}

func (s *Server) handleAdminDashboard(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireSystemAdmin(w, r); !ok {
		return
	}
	ctx := r.Context()
	var resp adminDashboardResponse

	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM users`).Scan(&resp.TotalMembers); err != nil {
		s.logger.Error("admin-dashboard: total members query failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM users WHERE created_at >= date_trunc('month', now())`,
	).Scan(&resp.NewMembersThisMonth); err != nil {
		s.logger.Error("admin-dashboard: new members query failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}

	planDist, err := s.computePlanDistribution(ctx)
	if err != nil {
		s.logger.Error("admin-dashboard: plan distribution query failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	resp.PlanDistribution = planDist

	if err := s.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(amount), 0) FROM payment_log
		WHERE status = '승인' AND requested_at >= date_trunc('month', now())`,
	).Scan(&resp.RevenueThisMonthKRW); err != nil {
		s.logger.Error("admin-dashboard: revenue query failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}

	if err := s.db.QueryRowContext(ctx, `
		SELECT count(*) FROM company_documents WHERE uploaded_at >= date_trunc('month', now())`,
	).Scan(&resp.AIAnalysisThisMonth); err != nil {
		s.logger.Error("admin-dashboard: AI usage query failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}

	recentPayments, err := s.fetchRecentPayments(ctx, 20)
	if err != nil {
		s.logger.Error("admin-dashboard: recent payments query failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	resp.RecentPayments = recentPayments

	writeJSON(w, http.StatusOK, resp)
}

// computePlanDistribution — 계정(users) 전체를 조직 소속 여부/조직의
// 실효 플랜으로 분류한다. LEFT JOIN이라 조직이 없는 계정도 한 행씩
// 나오고(plan/status가 전부 NULL), 그 경우 effectivePlanFromRow 없이
// 바로 Free로 센다.
func (s *Server) computePlanDistribution(ctx context.Context) (map[string]int, error) {
	dist := map[string]int{
		string(billing.PlanFree): 0, string(billing.PlanBasic): 0,
		string(billing.PlanPro): 0, string(billing.PlanBusiness): 0,
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT sub.plan, sub.status, sub.expires_at
		FROM users u
		LEFT JOIN company_members cm ON cm.user_id = u.id
		LEFT JOIN subscriptions sub ON sub.company_profile_id = cm.company_profile_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var planStr, statusStr sql.NullString
		var expiresAt sql.NullTime
		if err := rows.Scan(&planStr, &statusStr, &expiresAt); err != nil {
			continue
		}
		plan := billing.PlanFree
		if planStr.Valid {
			var exp *time.Time
			if expiresAt.Valid {
				t := expiresAt.Time
				exp = &t
			}
			plan = effectivePlanFromRow(billing.Plan(planStr.String), statusStr.String, exp)
		}
		dist[string(plan)]++
	}
	return dist, rows.Err()
}

// fetchRecentPayments — 전체 회원 대상 최신 결제 내역. payment_log는
// 결제자 이메일을 직접 갖고 있지 않아(조직 단위 로그) owner 멤버의
// 이메일로 조인한다 — 결제/플랜변경 API가 owner에게만 허용되므로
// (billing.go) 이게 항상 실제 결제자다.
func (s *Server) fetchRecentPayments(ctx context.Context, limit int) ([]adminPaymentItem, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT u.email, pl.toss_order_id, pl.amount, pl.status, pl.requested_at, pl.payment_method
		FROM payment_log pl
		JOIN subscriptions sub ON sub.id = pl.subscription_id
		JOIN company_members cm ON cm.company_profile_id = sub.company_profile_id AND cm.role = 'owner'
		JOIN users u ON u.id = cm.user_id
		ORDER BY pl.requested_at DESC
		LIMIT `+itoa(limit))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	items := []adminPaymentItem{}
	for rows.Next() {
		var it adminPaymentItem
		var orderID string
		var paymentMethod sql.NullString
		if err := rows.Scan(&it.Email, &orderID, &it.Amount, &it.Status, &it.RequestedAt, &paymentMethod); err != nil {
			continue
		}
		if plan, ok := billing.DecodePlanFromOrderID(orderID); ok {
			it.Plan = string(plan)
			it.PlanName = billing.Plans[plan].Name
		}
		if paymentMethod.Valid {
			label := paymentMethodLabel(paymentMethod.String)
			it.PaymentMethodLabel = &label
		}
		items = append(items, it)
	}
	return items, rows.Err()
}

type adminMemberItem struct {
	ID          string     `json:"id"`
	Email       string     `json:"email"`
	CreatedAt   time.Time  `json:"createdAt"`
	LastLoginAt *time.Time `json:"lastLoginAt"`
	Plan        string     `json:"plan"`
	PlanName    string     `json:"planName"`
}

// handleAdminListMembers — GET /api/admin/members?q=이메일검색어&plan=free|basic|pro|business.
// 운영 규모(<2천 회사 프로필 전제, 계정 수도 비슷한 자릿수)에서는 전체를
// 한 번에 읽어 메모리에서 필터링해도 무리가 없다 — 공고 목록 페이지네이션
// 때와 같은 판단.
func (s *Server) handleAdminListMembers(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireSystemAdmin(w, r); !ok {
		return
	}
	q := r.URL.Query()
	emailFilter := strings.ToLower(strings.TrimSpace(q.Get("q")))
	planFilter := strings.TrimSpace(q.Get("plan"))

	rows, err := s.db.QueryContext(r.Context(), `
		SELECT u.id, u.email, u.created_at, u.last_login_at, sub.plan, sub.status, sub.expires_at
		FROM users u
		LEFT JOIN company_members cm ON cm.user_id = u.id
		LEFT JOIN subscriptions sub ON sub.company_profile_id = cm.company_profile_id
		ORDER BY u.created_at DESC`)
	if err != nil {
		s.logger.Error("admin-list-members: query failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	defer rows.Close()

	items := []adminMemberItem{}
	for rows.Next() {
		var it adminMemberItem
		var lastLogin sql.NullTime
		var planStr, statusStr sql.NullString
		var expiresAt sql.NullTime
		if err := rows.Scan(&it.ID, &it.Email, &it.CreatedAt, &lastLogin, &planStr, &statusStr, &expiresAt); err != nil {
			s.logger.Error("admin-list-members: scan failed", "error", err)
			continue
		}
		if lastLogin.Valid {
			it.LastLoginAt = &lastLogin.Time
		}
		plan := billing.PlanFree
		if planStr.Valid {
			var exp *time.Time
			if expiresAt.Valid {
				t := expiresAt.Time
				exp = &t
			}
			plan = effectivePlanFromRow(billing.Plan(planStr.String), statusStr.String, exp)
		}
		it.Plan = string(plan)
		it.PlanName = billing.Plans[plan].Name

		if emailFilter != "" && !strings.Contains(strings.ToLower(it.Email), emailFilter) {
			continue
		}
		if planFilter != "" && it.Plan != planFilter {
			continue
		}
		items = append(items, it)
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

type adminMemberDetail struct {
	ID               string               `json:"id"`
	Email            string               `json:"email"`
	Role             string               `json:"role"`
	CreatedAt        time.Time            `json:"createdAt"`
	LastLoginAt      *time.Time           `json:"lastLoginAt"`
	CompanyProfileID *string              `json:"companyProfileId"`
	Plan             string               `json:"plan"`
	PlanName         string               `json:"planName"`
	PaymentHistory   []paymentHistoryItem `json:"paymentHistory"`
	PipelineEntries  []pipelineEntry      `json:"pipelineEntries"`
}

// handleAdminGetMember — GET /api/admin/members/{id}. 읽기 전용(수정
// 기능은 이번 범위 밖 — 사용자 요청). 결제이력/파이프라인은 이 계정이
// 속한 "조직 전체"의 것을 보여준다(개인 소유 데이터가 아니라 조직
// 단위 데이터라서 — 팀기능 원칙과 동일).
func (s *Server) handleAdminGetMember(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireSystemAdmin(w, r); !ok {
		return
	}
	memberID := r.PathValue("id")
	ctx := r.Context()

	var detail adminMemberDetail
	var lastLogin sql.NullTime
	var companyProfileID sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT u.id, u.email, u.role, u.created_at, u.last_login_at, cm.company_profile_id
		FROM users u
		LEFT JOIN company_members cm ON cm.user_id = u.id
		WHERE u.id = $1`, memberID,
	).Scan(&detail.ID, &detail.Email, &detail.Role, &detail.CreatedAt, &lastLogin, &companyProfileID)
	if err == sql.ErrNoRows {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "member_not_found"})
		return
	}
	if err != nil {
		s.logger.Error("admin-get-member: query failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if lastLogin.Valid {
		detail.LastLoginAt = &lastLogin.Time
	}
	detail.Plan = string(billing.PlanFree)
	detail.PlanName = billing.Plans[billing.PlanFree].Name
	detail.PaymentHistory = []paymentHistoryItem{}
	detail.PipelineEntries = []pipelineEntry{}

	if !companyProfileID.Valid {
		writeJSON(w, http.StatusOK, detail)
		return
	}
	profileID := companyProfileID.String
	detail.CompanyProfileID = &profileID

	planStr, status, _, expiresAt, _, _, err := s.currentSubscription(ctx, profileID)
	if err != nil {
		s.logger.Error("admin-get-member: subscription lookup failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	effective := effectivePlanFromRow(planStr, status, expiresAt)
	detail.Plan = string(effective)
	detail.PlanName = billing.Plans[effective].Name

	payRows, err := s.db.QueryContext(ctx, `
		SELECT pl.id, pl.toss_order_id, pl.amount, pl.status, pl.requested_at, pl.approved_at, pl.payment_method
		FROM payment_log pl
		JOIN subscriptions sub ON sub.id = pl.subscription_id
		WHERE sub.company_profile_id = $1
		ORDER BY pl.requested_at DESC`, profileID)
	if err != nil {
		s.logger.Error("admin-get-member: payment history query failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	for payRows.Next() {
		var it paymentHistoryItem
		var orderID string
		var approvedAt sql.NullTime
		var paymentMethod sql.NullString
		if err := payRows.Scan(&it.ID, &orderID, &it.Amount, &it.Status, &it.RequestedAt, &approvedAt, &paymentMethod); err != nil {
			s.logger.Error("admin-get-member: payment scan failed", "error", err)
			continue
		}
		if plan, ok := billing.DecodePlanFromOrderID(orderID); ok {
			it.Plan = string(plan)
			it.PlanName = billing.Plans[plan].Name
		}
		if approvedAt.Valid {
			it.ApprovedAt = &approvedAt.Time
		}
		if paymentMethod.Valid {
			it.PaymentMethod = &paymentMethod.String
			label := paymentMethodLabel(paymentMethod.String)
			it.PaymentMethodLabel = &label
		}
		detail.PaymentHistory = append(detail.PaymentHistory, it)
	}
	payRows.Close()

	peRows, err := s.db.QueryContext(ctx,
		pipelineEntrySelect+` WHERE pe.company_profile_id = $1 ORDER BY pe.submission_deadline ASC NULLS LAST`,
		profileID)
	if err != nil {
		s.logger.Error("admin-get-member: pipeline query failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	for peRows.Next() {
		entry, err := scanPipelineEntry(peRows)
		if err != nil {
			s.logger.Error("admin-get-member: pipeline scan failed", "error", err)
			continue
		}
		detail.PipelineEntries = append(detail.PipelineEntries, *entry)
	}
	peRows.Close()

	writeJSON(w, http.StatusOK, detail)
}

type adminNotificationFailureItem struct {
	ID             string    `json:"id"`
	EventType      string    `json:"eventType"`
	Channel        string    `json:"channel"`
	RecipientEmail *string   `json:"recipientEmail"`
	RecipientPhone *string   `json:"recipientPhone"`
	Subject        string    `json:"subject"`
	ErrorMessage   *string   `json:"errorMessage"`
	CreatedAt      time.Time `json:"createdAt"`
}

// handleAdminNotificationFailures — GET /api/admin/notification-failures
// (Phase 5 2단계). notification_log에 실패(status='failed')가 계속
// 쌓여도 지금까지 서버 로그로만 남고 볼 방법이 없었다 — 최근 실패 건을
// 그대로 노출해 원인 파악에 쓸 수 있게 한다. 재시도 버튼은 없다(다음날
// 배치가 dedup 조건(status='sent'만 체크)에 안 걸려 자연스럽게 재시도되는
// 기존 동작을 그대로 따른다 — notifications.go 참고).
func (s *Server) handleAdminNotificationFailures(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireSystemAdmin(w, r); !ok {
		return
	}
	limit := parseListingIntParam(r.URL.Query().Get("limit"), 50)
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.db.QueryContext(r.Context(), `
		SELECT id, event_type, channel, recipient_email, recipient_phone, subject, error_message, created_at
		FROM notification_log
		WHERE status = 'failed'
		ORDER BY created_at DESC
		LIMIT $1`, limit)
	if err != nil {
		s.logger.Error("admin-notification-failures: query failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	defer rows.Close()

	items := []adminNotificationFailureItem{}
	for rows.Next() {
		var it adminNotificationFailureItem
		var recipientEmail, recipientPhone, errMsg sql.NullString
		if err := rows.Scan(&it.ID, &it.EventType, &it.Channel, &recipientEmail, &recipientPhone, &it.Subject, &errMsg, &it.CreatedAt); err != nil {
			s.logger.Error("admin-notification-failures: scan failed", "error", err)
			continue
		}
		if recipientEmail.Valid {
			it.RecipientEmail = &recipientEmail.String
		}
		if recipientPhone.Valid {
			it.RecipientPhone = &recipientPhone.String
		}
		if errMsg.Valid {
			it.ErrorMessage = &errMsg.String
		}
		items = append(items, it)
	}
	if err := rows.Err(); err != nil {
		s.logger.Error("admin-notification-failures: rows iteration failed", "error", err)
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}
