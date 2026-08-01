// billing_refund.go — 환불(전액/불가 둘 중 하나, 부분환불 없음)과 해지
// (구독취소, 환불과 별개)를 다룬다. 정책(사용자와 확정):
//   - "이번 결제 주기(started_at ~ 지금) 동안 AI 분석 사용 1건 이상 또는
//     파이프라인 신규 생성 1건 이상"이면 "사용함" — 조금이라도 사용했으면
//     환불 불가, 해지만 가능.
//   - 전혀 사용하지 않았으면 토스 결제취소 API를 실제로 호출해 전액환불,
//     구독은 즉시 Free로 강등(expires_at을 기다리지 않음).
//   - 해지는 결제가 관여하지 않는다 — expires_at까지는 정상 이용하고 그
//     이후 배치(ApplyScheduledCancellations)가 Free로 전환한다.
//     subscriptions.pending_plan(예약 다운그레이드 전용, 반드시 결제를
//     거쳐야만 설정됨)과는 별개 필드(cancel_at_period_end)를 쓴다 —
//     db/migrations/001_init.sql 주석 참고.
package api

import (
	"context"
	"database/sql"
	"net/http"
	"time"
)

const refundReasonText = "서비스 미사용으로 인한 전액환불"

// serviceUsageSummary — "이번 결제 주기 동안 서비스를 사용했는가"의 근거.
// AI 분석 사용은 company_documents(업로드 1건 = Claude 호출 1건, 이미
// countAIAnalysisThisMonth가 쓰는 것과 같은 테이블이지만 캘린더 월이 아니라
// 결제 주기 시작(subscriptions.started_at) 기준으로 센다는 점이 다르다.
type serviceUsageSummary struct {
	AIAnalysisCount      int `json:"aiAnalysisCount"`
	PipelineCreatedCount int `json:"pipelineCreatedCount"`
}

func (u serviceUsageSummary) hasUsage() bool {
	return u.AIAnalysisCount > 0 || u.PipelineCreatedCount > 0
}

func (s *Server) serviceUsageSince(ctx context.Context, profileID string, since time.Time) (serviceUsageSummary, error) {
	var u serviceUsageSummary
	err := s.db.QueryRowContext(ctx, `
		SELECT
			(SELECT count(*) FROM company_documents WHERE company_profile_id = $1 AND uploaded_at >= $2),
			(SELECT count(*) FROM notice_pipeline_entries WHERE company_profile_id = $1 AND created_at >= $2)`,
		profileID, since,
	).Scan(&u.AIAnalysisCount, &u.PipelineCreatedCount)
	return u, err
}

// activePaidSubscriptionRow is the row shape both refund and cancel-renewal
// need beyond what currentSubscription returns(그쪽은 subscription id와
// cancel_at_period_end를 안 돌려줌 — 이 둘은 환불/해지에서만 필요해서
// currentSubscription 시그니처를 넓히지 않고 여기서 직접 쿼리한다).
type activePaidSubscriptionRow struct {
	id                string
	plan              string
	status            string
	startedAt         time.Time
	expiresAt         *time.Time
	cancelAtPeriodEnd bool
}

// fetchActivePaidSubscription returns ok=false (no error) when the caller
// has nothing refundable/cancellable — no subscription row, or plan=free,
// or status isn't 'active'. Both handlers share this precondition.
func (s *Server) fetchActivePaidSubscription(ctx context.Context, profileID string) (row activePaidSubscriptionRow, ok bool, err error) {
	var started, expires sql.NullTime
	err = s.db.QueryRowContext(ctx, `
		SELECT id, plan, status, started_at, expires_at, cancel_at_period_end
		FROM subscriptions WHERE company_profile_id = $1`, profileID,
	).Scan(&row.id, &row.plan, &row.status, &started, &expires, &row.cancelAtPeriodEnd)
	if err == sql.ErrNoRows {
		return row, false, nil
	}
	if err != nil {
		return row, false, err
	}
	if row.plan == "free" || row.status != "active" || !started.Valid {
		return row, false, nil
	}
	row.startedAt = started.Time
	if expires.Valid {
		row.expiresAt = &expires.Time
	}
	return row, true, nil
}

// handleBillingRefundRequest — POST /api/billing/refund-request. owner-only
// (환불은 결제 주체만 요청 가능 — 다른 billing 변경 핸들러들과 동일 정책).
func (s *Server) handleBillingRefundRequest(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.currentUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	profile, err := s.getCompanyProfile(r, userID)
	if err != nil {
		s.logger.Error("billing-refund: profile lookup failed", "error", err)
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
	ctx := r.Context()

	sub, ok, err := s.fetchActivePaidSubscription(ctx, profile.ID)
	if err != nil {
		s.logger.Error("billing-refund: subscription lookup failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no_active_paid_subscription"})
		return
	}

	usage, err := s.serviceUsageSince(ctx, profile.ID, sub.startedAt)
	if err != nil {
		s.logger.Error("billing-refund: usage lookup failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if usage.hasUsage() {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error":   "already_used",
			"message": "이미 서비스를 이용하신 내역이 있어 환불이 불가합니다.",
			"usage":   usage,
		})
		return
	}

	var paymentLogID, paymentKey string
	err = s.db.QueryRowContext(ctx, `
		SELECT id, toss_payment_key FROM payment_log
		WHERE subscription_id = $1 AND status = '승인'
		ORDER BY approved_at DESC LIMIT 1`, sub.id,
	).Scan(&paymentLogID, &paymentKey)
	if err == sql.ErrNoRows {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no_payment_to_refund"})
		return
	}
	if err != nil {
		s.logger.Error("billing-refund: payment lookup failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}

	if s.toss == nil || !s.toss.Configured() {
		s.logger.Warn("billing-refund: TOSS_SECRET_KEY not configured, cannot call Toss")
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "payment_provider_not_configured"})
		return
	}
	if _, _, tossErr := s.toss.Cancel(ctx, paymentKey, refundReasonText); tossErr != nil {
		s.logger.Error("billing-refund: toss cancel failed", "paymentLogId", paymentLogID, "error", tossErr)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "toss_cancel_failed", "detail": tossErr.Error()})
		return
	}

	now := time.Now()
	if _, err := s.db.ExecContext(ctx, `
		UPDATE payment_log SET status = '환불', refund_reason = $1, refunded_at = $2, refund_processed_by = 'system_auto'
		WHERE id = $3`,
		refundReasonText, now, paymentLogID,
	); err != nil {
		// 토스 취소는 이미 성공했다 — DB 반영 실패를 조용히 삼키면 안 되고
		// 크게 로그를 남겨야 한다(운영에서 수동 정정 필요). 사용자에게는
		// 에러로 알리되, 토스 쪽 취소 자체는 이미 끝났다는 걸 상세 메시지에
		// 남긴다.
		s.logger.Error("billing-refund: payment_log update failed after toss cancel succeeded", "paymentLogId", paymentLogID, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed", "detail": "toss_cancelled_but_db_update_failed"})
		return
	}
	if _, err := s.db.ExecContext(ctx, `
		UPDATE subscriptions
		SET plan = 'free', status = 'active', started_at = $1, expires_at = NULL, amount = 0,
		    pending_plan = NULL, cancel_at_period_end = false, cancel_requested_at = NULL
		WHERE id = $2`,
		now, sub.id,
	); err != nil {
		s.logger.Error("billing-refund: subscription downgrade failed after toss cancel succeeded", "subscriptionId", sub.id, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed", "detail": "toss_cancelled_but_db_update_failed"})
		return
	}

	s.recordAuditLog(ctx, userID, "subscription_refunded", "subscription", sub.id, map[string]any{"paymentLogId": paymentLogID})
	writeJSON(w, http.StatusOK, map[string]string{"status": "refunded"})
}

// handleCancelRenewal — POST /api/billing/cancel-renewal ("해지 신청", 환불과
// 별개). expires_at까지는 그대로 이용하고, 그 이후 ApplyScheduledCancellations
// 가 Free로 전환한다. 이미 예약된 다운그레이드(pending_plan)가 있었다면
// 해지가 우선한다 — 둘 다 동시에 유효할 수 없으므로 같이 지운다.
func (s *Server) handleCancelRenewal(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.currentUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	profile, err := s.getCompanyProfile(r, userID)
	if err != nil {
		s.logger.Error("cancel-renewal: profile lookup failed", "error", err)
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
	ctx := r.Context()

	sub, ok, err := s.fetchActivePaidSubscription(ctx, profile.ID)
	if err != nil {
		s.logger.Error("cancel-renewal: subscription lookup failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no_active_paid_subscription"})
		return
	}

	if _, err := s.db.ExecContext(ctx, `
		UPDATE subscriptions
		SET cancel_at_period_end = true, cancel_requested_at = now(), pending_plan = NULL
		WHERE id = $1`, sub.id,
	); err != nil {
		s.logger.Error("cancel-renewal: update failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}

	s.recordAuditLog(ctx, userID, "subscription_cancel_requested", "subscription", sub.id, map[string]any{"effectiveAt": sub.expiresAt})
	writeJSON(w, http.StatusOK, map[string]any{"status": "cancel_scheduled", "effectiveAt": sub.expiresAt})
}

// handleResumeRenewal — POST /api/billing/resume-renewal. "해지 신청"을
// 마음이 바뀌어 되돌리는 경우 — 기존 handleCancelDowngrade(예약 다운그레이드
// 취소)와 같은 성격의 짝 기능이라 같이 추가했다.
func (s *Server) handleResumeRenewal(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.currentUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	profile, err := s.getCompanyProfile(r, userID)
	if err != nil {
		s.logger.Error("resume-renewal: profile lookup failed", "error", err)
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

	res, err := s.db.ExecContext(r.Context(), `
		UPDATE subscriptions SET cancel_at_period_end = false, cancel_requested_at = NULL
		WHERE company_profile_id = $1 AND cancel_at_period_end = true`,
		profile.ID,
	)
	if err != nil {
		s.logger.Error("resume-renewal: update failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no_pending_cancellation"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "resumed"})
}

// ApplyScheduledCancellations mirrors ApplyScheduledDowngrades' shape but for
// 해지: expires_at을 지난 cancel_at_period_end 구독을 Free로 전환한다.
// 다운그레이드 배치와 달리 "다음 만료일을 1개월 뒤로 다시 계산"하지
// 않는다 — Free는 결제 주기가 없다(expires_at을 NULL로 둔다).
func (s *Server) ApplyScheduledCancellations(ctx context.Context) (int, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id FROM subscriptions
		WHERE cancel_at_period_end = true AND status = 'active' AND expires_at <= now()`)
	if err != nil {
		return 0, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			continue
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()

	applied := 0
	for _, id := range ids {
		if _, err := s.db.ExecContext(ctx, `
			UPDATE subscriptions
			SET plan = 'free', status = 'active', started_at = now(), expires_at = NULL, amount = 0,
			    cancel_at_period_end = false, cancel_requested_at = NULL
			WHERE id = $1`, id,
		); err != nil {
			s.logger.Error("apply-scheduled-cancellation: update failed", "subscriptionId", id, "error", err)
			continue
		}
		applied++
	}
	return applied, nil
}

// handleRunScheduledCancellations manually fires ApplyScheduledCancellations
// on demand — same system_admin-only pattern as handleRunScheduledDowngrades.
func (s *Server) handleRunScheduledCancellations(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.currentUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	role, err := s.userRole(r.Context(), userID)
	if err != nil {
		s.logger.Error("run-scheduled-cancellations: role lookup failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if role != "system_admin" {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
		return
	}
	applied, err := s.ApplyScheduledCancellations(r.Context())
	if err != nil {
		s.logger.Error("run-scheduled-cancellations: batch failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"applied": applied})
}
