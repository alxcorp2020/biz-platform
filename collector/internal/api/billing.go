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
func (s *Server) currentSubscription(ctx context.Context, profileID string) (
	plan billing.Plan, status string, startedAt, expiresAt *time.Time, amount *int64, err error,
) {
	var planStr, statusStr string
	var started, expires sql.NullTime
	var amt sql.NullInt64
	err = s.db.QueryRowContext(ctx,
		`SELECT plan, status, started_at, expires_at, amount FROM subscriptions WHERE company_profile_id = $1`,
		profileID,
	).Scan(&planStr, &statusStr, &started, &expires, &amt)
	if err == sql.ErrNoRows {
		return billing.PlanFree, "active", nil, nil, nil, nil
	}
	if err != nil {
		return "", "", nil, nil, nil, err
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
	return billing.Plan(planStr), statusStr, startedAt, expiresAt, amount, nil
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
	plan, status, _, expiresAt, _, err := s.currentSubscription(ctx, profileID)
	if err != nil {
		return "", err
	}
	return effectivePlanFromRow(plan, status, expiresAt), nil
}

// checkPipelineEntryQuota enforces plan.MaxPipelineEntries before a NEW
// pipeline entry is created. '제외' 상태는 사용자가 명시적으로 뺀 건이라
// 카운트에서 제외한다 — 그렇지 않으면 제외할수록 한도가 영구히 줄어드는
// 꼴이 되어 사용자가 혼란스럽다.
func (s *Server) checkPipelineEntryQuota(ctx context.Context, profileID string) (ok bool, limit int, err error) {
	plan, err := s.effectivePlan(ctx, profileID)
	if err != nil {
		return false, 0, err
	}
	max := billing.Plans[plan].MaxPipelineEntries
	if max < 0 {
		return true, -1, nil
	}
	var count int
	if err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM notice_pipeline_entries WHERE company_profile_id = $1 AND status != '제외'`,
		profileID,
	).Scan(&count); err != nil {
		return false, max, err
	}
	return count < max, max, nil
}

// checkAIAnalysisQuota enforces plan.MaxAIAnalysisPerMonth before an
// AI-extraction document upload (license/cert, financials, track-records,
// personnel, employee-verification — see PlanInfo doc comment for why these
// 5 endpoints specifically). Counts company_documents rows uploaded since
// the start of the current calendar month — every upload triggers exactly
// one Claude call regardless of whether the user later confirms the
// extracted candidate, so it's an accurate usage signal without needing a
// separate log table.
func (s *Server) checkAIAnalysisQuota(ctx context.Context, profileID string) (ok bool, limit int, err error) {
	plan, err := s.effectivePlan(ctx, profileID)
	if err != nil {
		return false, 0, err
	}
	max := billing.Plans[plan].MaxAIAnalysisPerMonth
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

// countAIAnalysisThisMonth counts company_documents rows uploaded since the
// start of the current calendar month — shared by checkAIAnalysisQuota
// (billing.go), dashboard.go's "AI 분석 N/M건" summary tile, and
// billing_ai_usage.go's usage-history screen, so all three always agree.
func (s *Server) countAIAnalysisThisMonth(ctx context.Context, profileID string) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `
		SELECT count(*) FROM company_documents
		WHERE company_profile_id = $1 AND uploaded_at >= date_trunc('month', now())`,
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
		"plans":         billingPlansForConfig(),
	})
}

// billingPlansForConfig renders billing.Plans in billing.PlanOrder order —
// map iteration order isn't stable, and the pricing page needs a fixed
// low-to-high display order.
func billingPlansForConfig() []map[string]any {
	out := make([]map[string]any, 0, len(billing.PlanOrder))
	for _, p := range billing.PlanOrder {
		info := billing.Plans[p]
		out = append(out, map[string]any{
			"plan":                  string(p),
			"name":                  info.Name,
			"amount":                info.AmountKRW,
			"maxPipelineEntries":    info.MaxPipelineEntries,
			"maxAIAnalysisPerMonth": info.MaxAIAnalysisPerMonth,
			"purchasable":           p.Purchasable(),
		})
	}
	return out
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

	plan, status, startedAt, expiresAt, amount, err := s.currentSubscription(r.Context(), profile.ID)
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
	// checkAIAnalysisQuota는 원래도 정확했다).
	limits := billing.Plans[effectivePlanFromRow(plan, status, expiresAt)]

	writeJSON(w, http.StatusOK, map[string]any{
		"plan":                  string(plan),
		"planName":              info.Name,
		"status":                displayStatus(status, expiresAt),
		"startedAt":             startedAt,
		"expiresAt":             expiresAt,
		"amount":                amount,
		"maxPipelineEntries":    limits.MaxPipelineEntries,
		"maxAIAnalysisPerMonth": limits.MaxAIAnalysisPerMonth,
	})
}

type paymentHistoryItem struct {
	ID          string     `json:"id"`
	Plan        string     `json:"plan"`
	PlanName    string     `json:"planName"`
	Amount      int64      `json:"amount"`
	Status      string     `json:"status"` // '승인' | '실패' | '취소'
	RequestedAt time.Time  `json:"requestedAt"`
	ApprovedAt  *time.Time `json:"approvedAt"`
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
		SELECT pl.id, pl.toss_order_id, pl.amount, pl.status, pl.requested_at, pl.approved_at
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
		if err := rows.Scan(&it.ID, &orderID, &it.Amount, &it.Status, &it.RequestedAt, &approvedAt); err != nil {
			s.logger.Error("payment-history: scan failed", "error", err)
			continue
		}
		if plan, ok := billing.DecodePlanFromOrderID(orderID); ok {
			it.Plan = string(plan)
			it.PlanName = billing.Plans[plan].Name
		}
		if approvedAt.Valid {
			it.ApprovedAt = &approvedAt.Time
		}
		items = append(items, it)
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
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
	info := billing.Plans[plan]

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
	info := billing.Plans[plan]
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

	if s.toss == nil || !s.toss.Configured() {
		s.logger.Warn("billing-confirm: TOSS_SECRET_KEY not configured, cannot call Toss")
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "payment_provider_not_configured"})
		return
	}

	requestedAt := time.Now()
	result, rawBody, tossErr := s.toss.Confirm(r.Context(), req.PaymentKey, req.OrderID, req.Amount)

	status := "실패"
	var approvedAt sql.NullTime
	if tossErr == nil {
		status = "승인"
		approvedAt = sql.NullTime{Time: result.ApprovedAt, Valid: true}
	}
	if _, logErr := s.db.ExecContext(r.Context(), `
		INSERT INTO payment_log (subscription_id, toss_payment_key, toss_order_id, amount, status, requested_at, approved_at, raw_response)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		subscriptionID, req.PaymentKey, req.OrderID, req.Amount, status, requestedAt, approvedAt, rawBody,
	); logErr != nil {
		s.logger.Error("billing-confirm: payment_log insert failed", "error", logErr)
	}

	if tossErr != nil {
		s.logger.Error("billing-confirm: toss rejected payment", "error", tossErr)
		writeJSON(w, http.StatusPaymentRequired, map[string]string{"error": "payment_failed", "detail": tossErr.Error()})
		return
	}

	expiresAt := requestedAt.AddDate(0, 1, 0)
	if _, err := s.db.ExecContext(r.Context(), `
		UPDATE subscriptions SET plan = $1, status = 'active', started_at = $2, expires_at = $3, amount = $4
		WHERE id = $5`,
		string(plan), requestedAt, expiresAt, req.Amount, subscriptionID,
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
