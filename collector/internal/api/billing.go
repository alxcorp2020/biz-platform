// billing.go — 토스페이먼츠 API 개별 연동(결제창) 기반 구독 결제.
// 결제위젯 연동 키는 별도 승인이 필요해 발급 전까지 API 개별 연동 키로
// 대체했다 — 서버 승인(confirm) 절차는 두 방식이 완전히 동일해 이 파일은
// 변경 없이 그대로 재사용된다(클라이언트 쪽만 tossPayments.widgets(...)
// 대신 tossPayments.requestPayment(...)를 직접 호출하도록 바뀜 —
// index.html의 renderBillingCheckout 참고). 정기결제(빌링키 자동결제)는
// 별도 승인이 필요해 이번 범위에서 제외하고, 1회성 결제로 매달 사용자가
// 직접 갱신하는 흐름만 구현한다.
//
// 승인은 반드시 서버(여기)에서 토스 결제승인 API를 호출해 확정한다 —
// 클라이언트가 결제창에서 받은 값만으로 구독을 활성화하지 않는다(보안
// 원칙: 금액도 클라이언트가 보낸 값을 그대로 믿지 않고 orderId에서
// 복원한 플랜의 정가와 대조한 뒤에만 토스 승인 API를 호출한다).
package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"time"

	"biz-platform/collector/internal/billing"
)

// currentSubscription returns the caller's persisted subscription row, or
// an implicit free/active zero-state when no row exists yet — signup
// doesn't create one, so "no row" simply means "never subscribed".
// pendingPlan is non-nil only when a downgrade has been paid for but not
// yet applied (즉시 업그레이드/예약 다운그레이드 정책) — see
// handleBillingConfirm and ApplyScheduledDowngrades.
func (s *Server) currentSubscription(ctx context.Context, profileID string) (
	plan billing.Plan, status string, startedAt, expiresAt *time.Time, amount *int64, pendingPlan *billing.Plan, err error,
) {
	var planStr, statusStr string
	var started, expires sql.NullTime
	var amt sql.NullInt64
	var pendingPlanStr sql.NullString
	err = s.db.QueryRowContext(ctx,
		`SELECT plan, status, started_at, expires_at, amount, pending_plan FROM subscriptions WHERE company_profile_id = $1`,
		profileID,
	).Scan(&planStr, &statusStr, &started, &expires, &amt, &pendingPlanStr)
	if err == sql.ErrNoRows {
		return billing.PlanFree, "active", nil, nil, nil, nil, nil
	}
	if err != nil {
		return "", "", nil, nil, nil, nil, err
	}
	if started.Valid {
		startedAt = &started.Time
	}
	if expires.Valid {
		expiresAt = &expires.Time
	}
	if amt.Valid {
		amount = &amt.Int64
	}
	if pendingPlanStr.Valid {
		p := billing.Plan(pendingPlanStr.String)
		pendingPlan = &p
	}
	return billing.Plan(planStr), statusStr, startedAt, expiresAt, amount, pendingPlan, nil
}

// effectivePlanFromRow is the single source of truth for "which plan
// actually governs feature limits right now", given an already-fetched
// subscription row — only an 'active' subscription that hasn't passed its
// expires_at counts; pending/cancelled/expired all fall back to Free. Split
// out from effectivePlan so handleGetSubscription (which already has the
// row) doesn't need a second query just to compute the same thing.
func effectivePlanFromRow(plan billing.Plan, status string, expiresAt *time.Time) billing.Plan {
	if status != "active" {
		return billing.PlanFree
	}
	if expiresAt != nil && expiresAt.Before(time.Now()) {
		return billing.PlanFree
	}
	return plan
}

// displayStatus adjusts the raw persisted status for API responses only: an
// 'active' row whose expires_at has already passed is reported as 'expired'
// even though nothing has rewritten the DB row yet — there's no cron job
// that flips status on a schedule, since effectivePlanFromRow already
// treats a past-expiry row as Free at every enforcement check, in real
// time. This function exists purely so GET /api/me/subscription doesn't
// keep telling the user "active" once that stops being true.
func displayStatus(status string, expiresAt *time.Time) string {
	if status == "active" && expiresAt != nil && expiresAt.Before(time.Now()) {
		return "expired"
	}
	return status
}

// effectivePlan is the choke point checkPipelineEntryQuota/
// checkAIAnalysisQuota go through. handleGetSubscription shows the raw
// persisted plan/status instead (a user should see "Business 결제 대기중" as
// what it is), but must still report *limits* from the effective plan —
// otherwise a checked-out-but-unpaid subscription would display paid-tier
// limits it doesn't actually get (see effectivePlanFromRow above).
func (s *Server) effectivePlan(ctx context.Context, profileID string) (billing.Plan, error) {
	plan, status, _, expiresAt, _, _, err := s.currentSubscription(ctx, profileID)
	if err != nil {
		return "", err
	}
	return effectivePlanFromRow(plan, status, expiresAt), nil
}

// checkPipelineEntryQuota enforces plan.MaxPipelineEntries before a NEW
// pipeline entry is created. '제외' 상태는 사용자가 명시적으로 뺀 건이라
// 카운트에서 제외한다 — 그렇지 않으면 제외할수록 한도가 영구히 줄어드는
// 꼴이 되어 사용자가 혼란스럽다. '낙찰'/'탈락'도 되돌릴 수 없는 최종
// 결과라 같은 이유로 제외한다(안 그러면 예전에 끝난 사업이 새 공고
// 검토를 영구히 막는 꼴이 됨) — '제출완료'/'보류'는 여전히 결과 대기 중
// 이거나 재개 가능한 상태라 계속 카운트한다.
func (s *Server) checkPipelineEntryQuota(ctx context.Context, profileID string) (ok bool, limit int, err error) {
	plan, err := s.effectivePlan(ctx, profileID)
	if err != nil {
		return false, 0, err
	}
	max := s.effectivePlanInfo(ctx, plan).MaxPipelineEntries
	if max < 0 {
		return true, -1, nil
	}
	var count int
	if err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM notice_pipeline_entries WHERE company_profile_id = $1 AND status NOT IN ('제외', '낙찰', '탈락')`,
		profileID,
	).Scan(&count); err != nil {
		return false, max, err
	}
	return count < max, max, nil
}

// checkAIAnalysisQuota enforces the effective AI 분석 한도 before an
// AI-extraction document upload (license/cert, financials, track-records,
// personnel, intellectual-property, employee-verification — see PlanInfo doc
// comment for why these 6 endpoints specifically). "Effective" 한도는
// effectiveAIAnalysisLimit이 결정한다 — 관리자가 이번달 임시조정을 걸어뒀으면
// 그 값, 아니면 플랜 기본값(관리자 화면에서 오버라이드됐을 수 있음).
// company_documents 카운트는 성공(extraction_status='success')한 업로드만
// 센다(countAIAnalysisThisMonth) — 실패는 이유를 막론하고 절대 한도를
// 깎지 않는다(2026-08-03 정책). 실패 건이 반복되는 비용 남용은 여기가 아니라
// checkFileRetryRateLimit(company_documents.go, 같은 파일 해시가 1시간 안에
// 3회 이상 실패하면 그 파일에 한해 재시도를 막음)이 별도로 막는다.
func (s *Server) checkAIAnalysisQuota(ctx context.Context, profileID string) (ok bool, limit int, err error) {
	plan, err := s.effectivePlan(ctx, profileID)
	if err != nil {
		return false, 0, err
	}
	max, err := s.effectiveAIAnalysisLimit(ctx, profileID, plan)
	if err != nil {
		return false, 0, err
	}
	if max < 0 {
		return true, -1, nil
	}
	if max == 0 {
		return false, 0, nil
	}
	count, err := s.countAIAnalysisThisMonth(ctx, profileID)
	if err != nil {
		return false, max, err
	}
	return count < max, max, nil
}

// countAIAnalysisThisMonth counts company_documents rows *successfully
// extracted* (extraction_status='success') since the start of the current
// calendar month — shared by checkAIAnalysisQuota(billing.go), dashboard.go's
// "AI 분석 N/M건" summary tile, and billing_ai_usage.go's usage-history
// screen, so all three always agree.
//
// 정책(2026-08-03, 사용자 확정): "AI 분석 실패는 절대 한도를 차감하면 안
// 된다" — 예전엔 Claude API를 실제로 호출만 했으면(성공/실패 무관) 한도가
// 깎였는데, 그 정책을 폐기했다. 이제 실패(extraction_status='failed') 또는
// 아직 처리중(NULL)인 행은 몇 번을 시도해도 절대 한도에 안 잡히고, 오직
// 성공한 분석만 카운트된다. 비용 남용 방지는 한도가 아니라 별도의 파일
// 단위 재시도 제한(checkFileRetryRateLimit, company_documents.go)이 담당한다.
func (s *Server) countAIAnalysisThisMonth(ctx context.Context, profileID string) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `
		SELECT count(*) FROM company_documents
		WHERE company_profile_id = $1 AND uploaded_at >= date_trunc('month', now())
		  AND extraction_status = 'success'`,
		profileID,
	).Scan(&count)
	return count, err
}

// handleGetBillingConfig exposes the Toss *client* key (public by design —
// it only opens the checkout widget, never authorizes anything) to the
// static frontend. There's no build step for index.html to inject env vars
// into, so this tiny endpoint is the simplest way for the checkout screen
// to learn which key to initialize the widget SDK with.
func (s *Server) handleGetBillingConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"tossClientKey": s.tossClientKey,
		"plans":         s.billingPlansForConfig(r.Context()),
	})
}

// billingPlansForConfig renders billing.Plans(관리자 오버라이드 반영,
// effectivePlanInfo) in billing.PlanOrder order — map iteration order isn't
// stable, and the pricing page needs a fixed low-to-high display order.
func (s *Server) billingPlansForConfig(ctx context.Context) []map[string]any {
	out := make([]map[string]any, 0, len(billing.PlanOrder))
	for _, p := range billing.PlanOrder {
		info := s.effectivePlanInfo(ctx, p)
		out = append(out, map[string]any{
			"plan":                  string(p),
			"name":                  info.Name,
			"amount":                info.AmountKRW,
			"maxPipelineEntries":    info.MaxPipelineEntries,
			"maxAIAnalysisPerMonth": info.MaxAIAnalysisPerMonth,
			"purchasable":           p.Purchasable(),
			// 2026-08-18 공개 요금제 페이지(#/pricing)용 additive 필드 — 플랜 정책의 단일 원천은
			// billing/plan.go PlanInfo(+관리자 오버라이드)이고, 프론트는 한도표를 하드코딩하지
			// 않고 이 값을 그대로 표시한다. -1 = 무제한, 0 = 이용 불가.
			"maxTeamMembers":                     info.MaxTeamMembers,
			"maxSavedSearches":                   info.MaxSavedSearches,
			"maxParticipationReviewsPerMonth":    info.MaxParticipationReviewsPerMonth,
			"maxProposalDraftsPerMonth":          info.MaxProposalDraftsPerMonth,
			"freeProposalTrialLifetime":          proposalTrialForPlan(p),
			"maxSMSPerMonth":                     info.MaxSMSPerMonth,
			"maxBusinessRegistrationOCRPerMonth": info.MaxBusinessRegistrationOCRPerMonth,
		})
	}
	return out
}

// proposalTrialForPlan — Free 회사만 평생 체험 1회(billing.FreeProposalTrialLifetime). 유료 플랜은
// 월 한도(maxProposalDraftsPerMonth)만 있어 0.
func proposalTrialForPlan(p billing.Plan) int {
	if p == billing.PlanFree {
		return billing.FreeProposalTrialLifetime
	}
	return 0
}

func (s *Server) handleGetSubscription(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.currentUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	profile, err := s.getCompanyProfile(r, userID)
	if err != nil {
		s.logger.Error("get-subscription: profile lookup failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if profile == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "company_profile_required"})
		return
	}

	plan, status, startedAt, expiresAt, amount, pendingPlan, err := s.currentSubscription(r.Context(), profile.ID)
	if err != nil {
		s.logger.Error("get-subscription: query failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	info := billing.Plans[plan]
	// 한도는 raw plan이 아니라 effective plan 기준 — status='pending'인
	// 동안(체크아웃만 하고 결제를 완료하지 않은 상태) 유료 플랜의 한도를
	// 그대로 노출하면 실제로는 Free로 막히는 기능을 이용 가능한 것처럼
	// 잘못 보여주게 된다(실제 차단 로직 자체는 이 버그의 영향을 받지
	// 않았음 — effectivePlan을 직접 쓰는 checkPipelineEntryQuota/
	// checkAIAnalysisQuota는 원래도 정확했다). effectivePlanInfo로
	// 관리자 오버라이드(#/admin/plan-settings)도 반영한다.
	effPlan := effectivePlanFromRow(plan, status, expiresAt)
	limits := s.effectivePlanInfo(r.Context(), effPlan)
	// AI 분석 한도는 개별 회원 임시조정(#/admin/members/{id})까지 한 번 더
	// 반영 — checkAIAnalysisQuota/billing_ai_usage.go/dashboard.go와 항상
	// 같은 숫자를 보여줘야 한다.
	if aiLimit, err := s.effectiveAIAnalysisLimit(r.Context(), profile.ID, effPlan); err == nil {
		limits.MaxAIAnalysisPerMonth = aiLimit
	}

	var pendingPlanStr, pendingPlanName *string
	if pendingPlan != nil {
		pv := string(*pendingPlan)
		nv := billing.Plans[*pendingPlan].Name
		pendingPlanStr, pendingPlanName = &pv, &nv
	}

	// 환불 요청 버튼을 보여줄지(사용 이력 없음) 아니면 해지 신청 버튼을
	// 보여줄지(사용 이력 있음)는 프론트가 매번 별도 요청으로 판단하지 않고
	// 구독 조회 응답에 이미 포함해서 내려준다 — 화면 진입 시 버튼이 잠깐
	// "환불 요청"으로 잘못 보였다가 바뀌는 깜빡임을 막기 위함.
	var cancelAtPeriodEnd bool
	var refundEligible bool
	var usage *serviceUsageSummary
	if sub, subOK, subErr := s.fetchActivePaidSubscription(r.Context(), profile.ID); subErr != nil {
		s.logger.Error("get-subscription: active subscription lookup failed", "error", subErr)
	} else if subOK {
		cancelAtPeriodEnd = sub.cancelAtPeriodEnd
		if u, usageErr := s.serviceUsageSince(r.Context(), profile.ID, sub.startedAt); usageErr != nil {
			s.logger.Error("get-subscription: usage lookup failed", "error", usageErr)
		} else {
			usage = &u
			refundEligible = !u.hasUsage()
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"plan":                  string(plan),
		"planName":              info.Name,
		"status":                displayStatus(status, expiresAt),
		"startedAt":             startedAt,
		"expiresAt":             expiresAt,
		"amount":                amount,
		"maxPipelineEntries":    limits.MaxPipelineEntries,
		"maxAIAnalysisPerMonth": limits.MaxAIAnalysisPerMonth,
		"pendingPlan":           pendingPlanStr,
		"pendingPlanName":       pendingPlanName,
		"cancelAtPeriodEnd":     cancelAtPeriodEnd,
		"refundEligible":        refundEligible,
		"usageThisCycle":        usage,
	})
}

type paymentHistoryItem struct {
	ID                 string     `json:"id"`
	Plan               string     `json:"plan"`
	PlanName           string     `json:"planName"`
	Amount             int64      `json:"amount"`
	Status             string     `json:"status"` // '승인' | '실패' | '취소' | '환불'
	RequestedAt        time.Time  `json:"requestedAt"`
	ApprovedAt         *time.Time `json:"approvedAt"`
	PaymentMethod      *string    `json:"paymentMethod"`      // 토스 원본 값("카드"/"가상계좌" 등) — 실패 건은 nil
	PaymentMethodLabel *string    `json:"paymentMethodLabel"` // 사용자에게 보여줄 표기(paymentMethodLabel 함수로 변환)
	RefundReason       *string    `json:"refundReason"`
	RefundedAt         *time.Time `json:"refundedAt"`
}

// handleGetPaymentHistory — GET /api/me/payment-history. 읽기 전용 정보라
// handleGetSubscription과 같은 접근 범위(owner/member 둘 다 조회 가능 —
// 구독 화면이 이미 amount를 두 역할 모두에게 보여주는 것과 같은 원칙).
// plan은 payment_log에 직접 저장돼 있지 않아 toss_order_id에서 복원한다
// (subscriptions.plan은 "지금" 플랜이라 과거 결제 건과 다를 수 있음 —
// 플랜을 여러 번 바꾼 이력이 있으면 각 결제가 그 시점에 실제로 어떤
// 플랜이었는지 정확히 보여줘야 함).
func (s *Server) handleGetPaymentHistory(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.currentUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	profile, err := s.getCompanyProfile(r, userID)
	if err != nil {
		s.logger.Error("payment-history: profile lookup failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if profile == nil {
		writeJSON(w, http.StatusOK, map[string]any{"items": []paymentHistoryItem{}})
		return
	}

	rows, err := s.db.QueryContext(r.Context(), `
		SELECT pl.id, pl.toss_order_id, pl.amount, pl.status, pl.requested_at, pl.approved_at, pl.payment_method,
		       pl.refund_reason, pl.refunded_at
		FROM payment_log pl
		JOIN subscriptions sub ON sub.id = pl.subscription_id
		WHERE sub.company_profile_id = $1
		ORDER BY pl.requested_at DESC`, profile.ID)
	if err != nil {
		s.logger.Error("payment-history: query failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	defer rows.Close()

	items := []paymentHistoryItem{}
	for rows.Next() {
		var it paymentHistoryItem
		var orderID string
		var approvedAt sql.NullTime
		var paymentMethod sql.NullString
		var refundReason sql.NullString
		var refundedAt sql.NullTime
		if err := rows.Scan(&it.ID, &orderID, &it.Amount, &it.Status, &it.RequestedAt, &approvedAt, &paymentMethod,
			&refundReason, &refundedAt); err != nil {
			s.logger.Error("payment-history: scan failed", "error", err)
			continue
		}
		if refundReason.Valid {
			it.RefundReason = &refundReason.String
		}
		if refundedAt.Valid {
			it.RefundedAt = &refundedAt.Time
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
		items = append(items, it)
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// paymentMethodLabel translates Toss's payment_log.payment_method value
// (already Korean, e.g. "카드"/"가상계좌"/"계좌이체") into the friendlier
// label 사용자가 보길 원하는 문구로 바꾼다. 매핑에 없는 값(새로운 결제수단
// 등)은 원본 그대로 보여준다 — 값을 숨기거나 지어내지 않는다.
func paymentMethodLabel(method string) string {
	labels := map[string]string{
		"카드":      "신용카드",
		"가상계좌":    "무통장입금(가상계좌)",
		"계좌이체":    "실시간 계좌이체",
		"휴대폰":     "휴대폰 소액결제",
		"문화상품권":   "문화상품권",
		"도서문화상품권": "도서문화상품권",
		"게임문화상품권": "게임문화상품권",
	}
	if label, ok := labels[method]; ok {
		return label
	}
	return method
}

type billingCheckoutRequest struct {
	Plan string `json:"plan"`
}

// handleBillingCheckout generates the order identity Toss's requestPayment()
// needs (orderId/orderName/amount) and, only if the caller has never
// subscribed before, inserts a placeholder status='pending' row so
// payment_log has something to attach to no matter how the confirm attempt
// turns out. It never touches an existing row — see the note further down
// on why overwriting status here caused an already-active subscription to
// drop to 'pending' the instant the user clicked "구독하기" on a different
// plan, before any new payment even happened. The amount always comes from
// the server-side plan table — never trust a client-supplied price.
func (s *Server) handleBillingCheckout(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.currentUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	profile, err := s.getCompanyProfile(r, userID)
	if err != nil {
		s.logger.Error("billing-checkout: profile lookup failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if profile == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "company_profile_required"})
		return
	}
	if profile.Role != "owner" {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "owner_only"})
		return
	}

	var req billingCheckoutRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_body"})
		return
	}
	plan, ok := billing.ParsePlan(req.Plan)
	if !ok || !plan.Purchasable() {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_plan"})
		return
	}
	// 가격은 관리자가 #/admin/plan-settings에서 바꿨을 수 있으니 항상 그
	// 순간의 값(effectivePlanInfo)으로 견적을 낸다 — 클라이언트가 보낸
	// 값은 절대 안 믿는다는 원칙(파일 상단 주석)은 그대로 유지.
	info := s.effectivePlanInfo(r.Context(), plan)

	orderID, err := billing.EncodeOrderID(plan)
	if err != nil {
		s.logger.Error("billing-checkout: order id generation failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal_error"})
		return
	}

	// checkout이 할 일은 딱 하나 — payment_log.subscription_id(NOT NULL)가
	// 가리킬 row가 존재하도록 보장하는 것뿐이다. 이미 row가 있으면(구독
	// 이력이 있는 사용자 — active든 pending이든 뭐든) 절대 건드리지 않는다.
	//
	// 예전엔 여기서 매번 status='pending'으로 덮어썼는데, 그러면 이미
	// 결제를 완료해 status='active'인 사용자가 다른 플랜의 "구독하기"만
	// 눌러도(새 결제가 완료되기도 전에) 기존 유료 구독이 즉시 pending으로
	// 떨어지는 회귀가 생긴다 — 실제로 발생한 버그. 플랜/상태 전환은
	// 오직 handleBillingConfirm이 토스 승인에 성공했을 때만 일어난다.
	var existingID string
	err = s.db.QueryRowContext(r.Context(),
		`SELECT id FROM subscriptions WHERE company_profile_id = $1`, profile.ID,
	).Scan(&existingID)
	if err == sql.ErrNoRows {
		if _, err := s.db.ExecContext(r.Context(), `
			INSERT INTO subscriptions (company_profile_id, plan, status, amount)
			VALUES ($1, $2, 'pending', $3)`,
			profile.ID, string(plan), info.AmountKRW,
		); err != nil {
			s.logger.Error("billing-checkout: initial subscription insert failed", "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
			return
		}
	} else if err != nil {
		s.logger.Error("billing-checkout: subscription lookup failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"orderId":     orderID,
		"orderName":   info.Name + " 플랜 구독",
		"amount":      info.AmountKRW,
		"customerKey": profile.ID, // tossPayments.payment({customerKey})가 요구 — 유추 가능한 값(이메일 등) 대신 불투명 UUID 사용
	})
}

type billingConfirmRequest struct {
	PaymentKey string `json:"paymentKey"`
	OrderID    string `json:"orderId"`
	Amount     int64  `json:"amount"`
}

// handleBillingConfirm is the only place a subscription actually becomes
// active. It re-derives the plan from orderId and compares against the
// server's own price table before ever calling Toss — a tampered client
// amount never reaches the Toss API. Both success and failure are logged to
// payment_log with Toss's raw response body preserved verbatim.
func (s *Server) handleBillingConfirm(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.currentUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	profile, err := s.getCompanyProfile(r, userID)
	if err != nil {
		s.logger.Error("billing-confirm: profile lookup failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if profile == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "company_profile_required"})
		return
	}
	if profile.Role != "owner" {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "owner_only"})
		return
	}

	var req billingConfirmRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_body"})
		return
	}

	plan, ok := billing.DecodePlanFromOrderID(req.OrderID)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_order_id"})
		return
	}
	// checkout 때와 동일하게 그 순간의(관리자 오버라이드 반영) 가격과 대조한다.
	// 가격이 checkout~confirm 사이에 바뀌면(드문 관리자 조작 타이밍) 이미
	// 승인된 결제가 amount_mismatch로 거부될 수 있다는 걸 알고 있음 —
	// 가격 변경 자체가 드문 관리자 행위라 감수 가능한 리스크로 판단.
	info := s.effectivePlanInfo(r.Context(), plan)
	if req.Amount != info.AmountKRW {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "amount_mismatch"})
		return
	}

	var subscriptionID string
	err = s.db.QueryRowContext(r.Context(),
		`SELECT id FROM subscriptions WHERE company_profile_id = $1`, profile.ID,
	).Scan(&subscriptionID)
	if err == sql.ErrNoRows {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "checkout_required"})
		return
	}
	if err != nil {
		s.logger.Error("billing-confirm: subscription lookup failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}

	// 다운그레이드 여부는 결제 승인(Toss)이 실제로 이 row를 덮어쓰기 전,
	// "지금 이 순간의 실효 플랜"을 기준으로 판단해야 한다 — 즉시 업그레이드/
	// 예약 다운그레이드 정책(사용자와 확정, 2026-08-02). 만료된 구독에서
	// 재구독하는 경우(effective=Free)는 어떤 유료 플랜을 사도 "업그레이드"로
	// 취급돼 즉시 적용된다 — 예약할 "남은 혜택"이 없기 때문.
	currentPlan, currentStatus, _, currentExpiresAt, _, _, err := s.currentSubscription(r.Context(), profile.ID)
	if err != nil {
		s.logger.Error("billing-confirm: current subscription lookup failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	effectiveCurrent := effectivePlanFromRow(currentPlan, currentStatus, currentExpiresAt)
	isDowngrade := billing.PlanRank(plan) < billing.PlanRank(effectiveCurrent)

	if s.toss == nil || !s.toss.Configured() {
		s.logger.Warn("billing-confirm: TOSS_SECRET_KEY not configured, cannot call Toss")
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "payment_provider_not_configured"})
		return
	}

	requestedAt := time.Now()
	result, rawBody, tossErr := s.toss.Confirm(r.Context(), req.PaymentKey, req.OrderID, req.Amount)

	status := "실패"
	var approvedAt sql.NullTime
	var paymentMethod sql.NullString
	if tossErr == nil {
		status = "승인"
		approvedAt = sql.NullTime{Time: result.ApprovedAt, Valid: true}
		if result.Method != "" {
			paymentMethod = sql.NullString{String: result.Method, Valid: true}
		}
	}
	if _, logErr := s.db.ExecContext(r.Context(), `
		INSERT INTO payment_log (subscription_id, toss_payment_key, toss_order_id, amount, status, requested_at, approved_at, payment_method, raw_response)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		subscriptionID, req.PaymentKey, req.OrderID, req.Amount, status, requestedAt, approvedAt, paymentMethod, rawBody,
	); logErr != nil {
		s.logger.Error("billing-confirm: payment_log insert failed", "error", logErr)
	}

	if tossErr != nil {
		s.logger.Error("billing-confirm: toss rejected payment", "error", tossErr)
		writeJSON(w, http.StatusPaymentRequired, map[string]string{"error": "payment_failed", "detail": tossErr.Error()})
		return
	}

	if isDowngrade {
		// 결제는 이미 승인되어 payment_log에 남았지만(환불 대상 아님 —
		// 다음 주기 요금을 미리 낸 것), 지금 활성 중인 상위 플랜 혜택은
		// currentExpiresAt까지 그대로 유지한다. plan/status/started_at/
		// expires_at은 손대지 않고 pending_plan만 기록 — ApplyScheduledDowngrades
		// 배치가 만료 시점에 실제로 전환한다.
		if _, err := s.db.ExecContext(r.Context(),
			`UPDATE subscriptions SET pending_plan = $1 WHERE id = $2`,
			string(plan), subscriptionID,
		); err != nil {
			s.logger.Error("billing-confirm: schedule downgrade failed", "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"scheduled":   true,
			"currentPlan": string(currentPlan),
			"pendingPlan": string(plan),
			"effectiveAt": currentExpiresAt,
		})
		return
	}

	// 즉시 적용 — 업그레이드, 또는 만료 후 재구독. 이전에 예약해둔
	// 다운그레이드가 있었다면 이번 결제로 의미가 없어지므로 같이 지운다.
	//
	// previous_plan/previous_plan_expires_at — 2026-08-06, 상위 플랜 환불
	// 사고 이후 도입. 지금 덮어쓰려는 게 아직 유효한 유료 플랜이면(Free나
	// 이미 만료된 플랜이면 지킬 게 없음) 그 plan/expires_at을 스냅샷으로
	// 남겨둔다. 매 즉시적용마다 무조건 다시 쓴다(유효한 게 없으면 NULL로
	// 지우는 것까지 포함) — 조건부로만 쓰면 몇 달 전 스냅샷이 그대로
	// 남아있는 상태 오염이 생긴다. 실제 환불 판단은 이 컬럼이 아니라
	// billing_refund.go가 payment_log를 다시 계산해서 한다(이 컬럼은
	// 감사·지원용 참고 정보).
	var previousPlan *string
	var previousPlanExpiresAt *time.Time
	if effectiveCurrent != billing.PlanFree {
		p := string(currentPlan)
		previousPlan = &p
		previousPlanExpiresAt = currentExpiresAt
	}
	expiresAt := requestedAt.AddDate(0, 1, 0)
	if _, err := s.db.ExecContext(r.Context(), `
		UPDATE subscriptions SET plan = $1, status = 'active', started_at = $2, expires_at = $3, amount = $4, pending_plan = NULL,
		    previous_plan = $6, previous_plan_expires_at = $7
		WHERE id = $5`,
		string(plan), requestedAt, expiresAt, req.Amount, subscriptionID, previousPlan, previousPlanExpiresAt,
	); err != nil {
		s.logger.Error("billing-confirm: subscription activation failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"plan":      string(plan),
		"status":    "active",
		"startedAt": requestedAt,
		"expiresAt": expiresAt,
	})
}

// handleCancelDowngrade — owner-only. 예약된 다운그레이드를 취소하고
// 현재 플랜을 만료일까지(그리고 재구독 시 그 플랜으로) 그대로 유지한다.
// 이미 낸 하위 플랜 결제 자체는 환불하지 않는다(별도 정책 범위 밖) —
// payment_log 기록은 그대로 남는다.
func (s *Server) handleCancelDowngrade(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.currentUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	profile, err := s.getCompanyProfile(r, userID)
	if err != nil {
		s.logger.Error("cancel-downgrade: profile lookup failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if profile == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "company_profile_required"})
		return
	}
	if profile.Role != "owner" {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "owner_only"})
		return
	}

	res, err := s.db.ExecContext(r.Context(),
		`UPDATE subscriptions SET pending_plan = NULL WHERE company_profile_id = $1 AND pending_plan IS NOT NULL`,
		profile.ID,
	)
	if err != nil {
		s.logger.Error("cancel-downgrade: update failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no_pending_downgrade"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "cancelled"})
}

// ApplyScheduledDowngrades runs on the existing daily 09:00 KST ticker
// (cmd/apiserver) alongside RunPipelineAutoTransitions/RunScheduledReports.
// Every subscription whose paid-through period(expires_at) has passed and
// still carries a pending_plan gets switched over — seamlessly, as if the
// lower-tier payment already made at downgrade-request time covers this new
// cycle(started_at = old expires_at, not "now", so there's no gap even if
// the batch runs a bit late).
func (s *Server) ApplyScheduledDowngrades(ctx context.Context) (int, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, pending_plan, expires_at FROM subscriptions
		WHERE pending_plan IS NOT NULL AND status = 'active' AND expires_at <= now()`)
	if err != nil {
		return 0, err
	}
	type row struct {
		id, pendingPlan string
		expiresAt       time.Time
	}
	var targets []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.pendingPlan, &r.expiresAt); err != nil {
			continue
		}
		targets = append(targets, r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()

	applied := 0
	for _, t := range targets {
		plan, ok := billing.ParsePlan(t.pendingPlan)
		if !ok {
			s.logger.Error("apply-scheduled-downgrade: unknown pending_plan", "subscriptionId", t.id, "pendingPlan", t.pendingPlan)
			continue
		}
		newExpiresAt := t.expiresAt.AddDate(0, 1, 0)
		if _, err := s.db.ExecContext(ctx, `
			UPDATE subscriptions
			SET plan = $1, started_at = $2, expires_at = $3, amount = $4, pending_plan = NULL,
			    previous_plan = NULL, previous_plan_expires_at = NULL
			WHERE id = $5`,
			string(plan), t.expiresAt, newExpiresAt, s.effectivePlanInfo(ctx, plan).AmountKRW, t.id,
		); err != nil {
			s.logger.Error("apply-scheduled-downgrade: update failed", "subscriptionId", t.id, "error", err)
			continue
		}
		applied++
	}
	return applied, nil
}

// handleRunScheduledDowngrades manually fires ApplyScheduledDowngrades on
// demand — same system_admin-only pattern as handleRunPipelineAutoTransitions/
// handleRunNotifications. The only other trigger is the daily ticker in
// cmd/apiserver.
func (s *Server) handleRunScheduledDowngrades(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.currentUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	role, err := s.userRole(r.Context(), userID)
	if err != nil {
		s.logger.Error("run-scheduled-downgrades: role lookup failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if role != "system_admin" {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
		return
	}

	applied, err := s.ApplyScheduledDowngrades(r.Context())
	if err != nil {
		s.logger.Error("run-scheduled-downgrades: batch failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "completed", "appliedCount": applied})
}
