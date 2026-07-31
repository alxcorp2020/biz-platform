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

// effectivePlan is the single choke point every feature-limit check
// (pipeline count, AI quota) goes through — only an 'active' subscription
// that hasn't passed its expires_at counts; pending/cancelled/expired all
// fall back to Free. handleGetSubscription shows the raw persisted status
// instead (a user should see "결제 대기중"/"만료됨" as what it is, not have
// it silently collapsed to Free).
func (s *Server) effectivePlan(ctx context.Context, profileID string) (billing.Plan, error) {
	plan, status, _, expiresAt, _, err := s.currentSubscription(ctx, profileID)
	if err != nil {
		return "", err
	}
	if status != "active" {
		return billing.PlanFree, nil
	}
	if expiresAt != nil && expiresAt.Before(time.Now()) {
		return billing.PlanFree, nil
	}
	return plan, nil
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
	var count int
	if err := s.db.QueryRowContext(ctx, `
		SELECT count(*) FROM company_documents
		WHERE company_profile_id = $1 AND uploaded_at >= date_trunc('month', now())`,
		profileID,
	).Scan(&count); err != nil {
		return false, max, err
	}
	return count < max, max, nil
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

	writeJSON(w, http.StatusOK, map[string]any{
		"plan":                  string(plan),
		"planName":              info.Name,
		"status":                status,
		"startedAt":             startedAt,
		"expiresAt":             expiresAt,
		"amount":                amount,
		"maxPipelineEntries":    info.MaxPipelineEntries,
		"maxAIAnalysisPerMonth": info.MaxAIAnalysisPerMonth,
	})
}

type billingCheckoutRequest struct {
	Plan string `json:"plan"`
}

// handleBillingCheckout generates the order identity Toss's requestPayment()
// needs (orderId/orderName/amount) and upserts a status='pending' row on
// subscriptions so payment_log has something to attach to no matter how the
// confirm attempt turns out (see the schema comment in 001_init.sql). The
// amount always comes from the server-side plan table — never trust a
// client-supplied price.
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

	if _, err := s.db.ExecContext(r.Context(), `
		INSERT INTO subscriptions (company_profile_id, plan, status, amount)
		VALUES ($1, $2, 'pending', $3)
		ON CONFLICT (company_profile_id) DO UPDATE SET plan = $2, status = 'pending', amount = $3`,
		profile.ID, string(plan), info.AmountKRW,
	); err != nil {
		s.logger.Error("billing-checkout: pending subscription upsert failed", "error", err)
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
