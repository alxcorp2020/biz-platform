// billing_refund.go — 환불(전액/불가 둘 중 하나, 부분환불 없음)과 해지
// (구독취소, 환불과 별개)를 다룬다. 정책(사용자와 확정):
//   - "이번 결제 주기(started_at ~ 지금) 동안 AI 분석 사용 1건 이상 또는
//     파이프라인 신규 생성 1건 이상"이면 "사용함" — 조금이라도 사용했으면
//     환불 불가, 해지만 가능.
//   - 전혀 사용하지 않았으면 토스 결제취소 API를 실제로 호출해 전액환불,
//     구독은 이 조직의 payment_log 중 지금도 유효한 결제가 있으면 그중
//     가장 높은 플랜으로, 없으면 Free로 강등한다(expires_at을 기다리지
//     않음) — bestValidPriorPayment 참고.
//   - 해지는 결제가 관여하지 않는다 — expires_at까지는 정상 이용하고 그
//     이후 배치(ApplyScheduledCancellations)가 Free로 전환한다.
//     subscriptions.pending_plan(예약 다운그레이드 전용, 반드시 결제를
//     거쳐야만 설정됨)과는 별개 필드(cancel_at_period_end)를 쓴다 —
//     db/migrations/001_init.sql 주석 참고.
//
// 🚨 2026-08-06 상위 플랜 환불 사고: Basic 결제로 유효기간이 남아있는
// 상태에서 Pro로 즉시업그레이드한 뒤 Pro만 환불했더니, 환불 로직이
// 무조건 plan='free'로 덮어써서 아직 유효했던 Basic까지 통째로 사라진
// 실사고가 있었다(운영 데이터 수동 조치로 우선 복구, 이 파일이 그 근본
// 수정). subscriptions가 프로필당 한 행뿐이라 즉시업그레이드마다
// plan/expires_at을 덮어쓰는 구조라서, "지금 진짜 유효한 최고 플랜"은
// 그때그때 payment_log를 다시 계산해야 안전하다(bestValidPriorPayment) —
// previous_plan 스냅샷 컬럼(billing.go의 handleBillingConfirm이 즉시적용
// 때마다 씀) 하나만 보면 두 단계 이상 겹친 결제를 놓칠 수 있어, 실제
// 환불 판단에는 쓰지 않고 지원팀용 감사 정보로만 남긴다.
//
// ## 업그레이드/다운그레이드/환불/해지 겹침 시나리오 — "환불 시 복귀 대상"
//
//	시나리오                                          | 환불 시 복귀 대상            | 비고
//	--------------------------------------------------|------------------------------|------------------------------------------
//	Basic 유효 중 Pro 업그레이드 → Pro 환불            | Basic (원래 만료일까지)      | 사고가 실제로 난 케이스. bestValidPriorPayment가
//	                                                   |                              | payment_log의 Basic 승인건(미환불, 아직 유효)을 찾아 복귀.
//	Basic 유효 중 Pro 업그레이드 → Pro 해지(구독취소)  | Free (expires_at 도달 후)    | 해지는 환불(결제 취소)이 아니라 "다 쓰고 정상 만료"라
//	                                                   |                              | 이 로직 대상이 아님 — ApplyScheduledCancellations는
//	                                                   |                              | 그대로 Free로 전환(의도적, 변경 안 함). Pro의 expires_at은
//	                                                   |                              | 통상 Basic의 expires_at보다 나중이라 실무적으로도 그때는
//	                                                   |                              | Basic이 이미 만료된 경우가 대부분.
//	예약된 다운그레이드(pending_plan)가 있는 상태에서  | 현재 활성 플랜(예: Pro)만    | 환불 대상 결제 조회를 sub.plan(지금 활성 플랜) 기준
//	Pro 환불                                          | 환불 — pending_plan으로 선결제| order_id로 필터링해서, 다운그레이드용 선결제 건과
//	                                                   | 해둔 하위 플랜 결제는 안 건드림| 헷갈리지 않도록 수정(2026-08-06, 같이 발견한 버그).
//	                                                   |                              | 이후 bestValidPriorPayment가 그 선결제 건을 발견하면
//	                                                   |                              | (아직 유효하므로) 그 플랜으로 즉시 복귀 — pending_plan은
//	                                                   |                              | 자연스럽게 NULL로 정리됨(더 기다릴 이유가 없어짐).
//	Free에서 바로 Basic 결제 → 환불(회귀 확인용)       | Free                         | payment_log에 남는 유효 결제가 없으므로 기존과 동일하게
//	                                                   |                              | Free로. 기본 케이스 회귀 없음을 확인하는 용도.
package api

import (
	"context"
	"database/sql"
	"net/http"
	"time"

	"biz-platform/collector/internal/billing"
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

	// toss_order_id는 "{plan}-{hex}" 형식(billing.EncodeOrderID)이라 LIKE로
	// 지금 활성 플랜(sub.plan)의 결제만 정확히 골라낸다 — plan 필터 없이
	// "가장 최근 승인건"만 찾으면, 다운그레이드 예약(pending_plan) 때문에
	// 그보다 나중에 결제된 하위 플랜 선결제 건을 잘못 집어 엉뚱한 결제를
	// 환불하게 되는 사고가 날 수 있다(2026-08-06 상위 플랜 환불 사고
	// 조사 중 함께 발견).
	var paymentLogID, paymentKey string
	err = s.db.QueryRowContext(ctx, `
		SELECT id, toss_payment_key FROM payment_log
		WHERE subscription_id = $1 AND status = '승인' AND toss_order_id LIKE $2
		ORDER BY approved_at DESC LIMIT 1`, sub.id, sub.plan+"-%",
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
	// 2026-08-06 상위 플랜 환불 사고 이후 재설계: 무조건 Free로 내리지
	// 않고, 이 조직의 payment_log 중 "지금도 유효한"(승인시각+1개월이 아직
	// 안 지난) 승인·미환불 결제가 남아있으면 그중 가장 높은 플랜으로
	// 복귀한다. subscriptions가 프로필당 한 행뿐이라 즉시업그레이드마다
	// 이전 plan/expires_at을 덮어써버리는 구조라서, "진짜 지금 유효한
	// 최고 플랜"은 previous_plan 스냅샷 한 칸이 아니라 payment_log 전체를
	// 매번 다시 계산해야 안전하다(두 단계 이상 겹친 경우까지 커버).
	// 시나리오별 정확한 동작은 파일 상단 표 참고.
	fallback, foundFallback, fbErr := s.bestValidPriorPayment(ctx, sub.id, now)
	if fbErr != nil {
		// 폴백 대상 조회 자체가 실패했다고 이미 성공한 토스 취소를 되돌릴
		// 순 없다 — 안전한 쪽(Free)으로 내리고 크게 로그를 남긴다.
		s.logger.Error("billing-refund: fallback plan lookup failed, downgrading to free", "subscriptionId", sub.id, "error", fbErr)
	}

	restoredPlan := "free"
	if foundFallback {
		if _, err := s.db.ExecContext(ctx, `
			UPDATE subscriptions
			SET plan = $1, status = 'active', started_at = $2, expires_at = $3, amount = $4,
			    pending_plan = NULL, cancel_at_period_end = false, cancel_requested_at = NULL,
			    previous_plan = NULL, previous_plan_expires_at = NULL
			WHERE id = $5`,
			string(fallback.plan), fallback.approvedAt, fallback.approvedAt.AddDate(0, 1, 0), fallback.amount, sub.id,
		); err != nil {
			s.logger.Error("billing-refund: subscription fallback-restore failed after toss cancel succeeded", "subscriptionId", sub.id, "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed", "detail": "toss_cancelled_but_db_update_failed"})
			return
		}
		restoredPlan = string(fallback.plan)
	} else {
		if _, err := s.db.ExecContext(ctx, `
			UPDATE subscriptions
			SET plan = 'free', status = 'active', started_at = $1, expires_at = NULL, amount = 0,
			    pending_plan = NULL, cancel_at_period_end = false, cancel_requested_at = NULL,
			    previous_plan = NULL, previous_plan_expires_at = NULL
			WHERE id = $2`,
			now, sub.id,
		); err != nil {
			s.logger.Error("billing-refund: subscription downgrade failed after toss cancel succeeded", "subscriptionId", sub.id, "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed", "detail": "toss_cancelled_but_db_update_failed"})
			return
		}
	}

	s.recordAuditLog(ctx, userID, "subscription_refunded", "subscription", sub.id, map[string]any{"paymentLogId": paymentLogID, "restoredPlan": restoredPlan})
	writeJSON(w, http.StatusOK, map[string]string{"status": "refunded", "restoredPlan": restoredPlan})
}

// paymentLogValidPayment — bestValidPriorPayment이 찾아낸, "지금도 유효한"
// 승인 결제 한 건의 최소 정보.
type paymentLogValidPayment struct {
	plan       billing.Plan
	amount     int64
	approvedAt time.Time
}

// bestValidPriorPayment — subscriptionID의 payment_log 중 status='승인'이고
// approved_at + 1개월이 아직 지나지 않은(=지금도 유효한) 결제들 중 가장
// 등급이 높은 플랜을 돌려준다. 동일 등급이 여럿이면 더 최근에 승인된
// 것(=만료일이 더 늦은 것)을 고른다. 호출 시점 기준으로 이미 환불 처리한
// payment_log 행은 status가 '환불'로 바뀐 뒤이므로 자동으로 제외된다 —
// 별도 예외 처리가 필요 없다.
func (s *Server) bestValidPriorPayment(ctx context.Context, subscriptionID string, now time.Time) (best paymentLogValidPayment, found bool, err error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT toss_order_id, amount, approved_at FROM payment_log
		WHERE subscription_id = $1 AND status = '승인' AND approved_at IS NOT NULL`,
		subscriptionID)
	if err != nil {
		return best, false, err
	}
	defer rows.Close()

	for rows.Next() {
		var orderID string
		var amount int64
		var approvedAt time.Time
		if err := rows.Scan(&orderID, &amount, &approvedAt); err != nil {
			continue
		}
		plan, ok := billing.DecodePlanFromOrderID(orderID)
		if !ok || !plan.Purchasable() {
			continue
		}
		if !approvedAt.AddDate(0, 1, 0).After(now) {
			continue // 이미 만료된 결제 — 복귀 대상 아님
		}
		candidate := paymentLogValidPayment{plan: plan, amount: amount, approvedAt: approvedAt}
		if !found ||
			billing.PlanRank(candidate.plan) > billing.PlanRank(best.plan) ||
			(billing.PlanRank(candidate.plan) == billing.PlanRank(best.plan) && candidate.approvedAt.After(best.approvedAt)) {
			best = candidate
			found = true
		}
	}
	return best, found, rows.Err()
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
			    cancel_at_period_end = false, cancel_requested_at = NULL,
			    previous_plan = NULL, previous_plan_expires_at = NULL
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
