// push_notifications.go — Phase 6(웹 푸시/PWA). 구독 단위는 "회원"(로그인
// 계정)이다 — company_contacts(담당자)와는 별개 모델이다(요구사항 확정
// 사항, project_phase6_app_requirements 메모리 참고). 대상 이벤트는 Phase 5
// 인앱 알림함과 동일한 3종(마감 리마인더/상태변경/추천 다이제스트) —
// notifications.go의 각 발송 지점에서 insertInAppNotification 옆에
// sendPushToUser/sendPushToProfileMembers를 나란히 호출한다.
package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	webpush "github.com/SherClockHolmes/webpush-go"
)

type pushSubscribeRequest struct {
	Endpoint string `json:"endpoint"`
	Keys     struct {
		P256dh string `json:"p256dh"`
		Auth   string `json:"auth"`
	} `json:"keys"`
}

// handleGetPushPublicKey — GET /api/push/vapid-public-key. 프론트가
// PushManager.subscribe()에 넘길 applicationServerKey를 여기서 받는다.
func (s *Server) handleGetPushPublicKey(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.currentUserID(r); !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	if s.vapidPublicKey == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "push_not_configured"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"publicKey": s.vapidPublicKey})
}

// handleSubscribePush — POST /api/me/push-subscriptions. endpoint UNIQUE +
// ON CONFLICT DO UPDATE로, 같은 기기가 로그아웃 후 다른 계정으로 다시
// 구독하면 소유자가 자연스럽게 바뀐다(migrate.go의 ensurePushSubscriptionsTable
// 주석 참고).
func (s *Server) handleSubscribePush(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.currentUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	var req pushSubscribeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Endpoint == "" || req.Keys.P256dh == "" || req.Keys.Auth == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_body"})
		return
	}
	if _, err := s.db.ExecContext(r.Context(), `
		INSERT INTO push_subscriptions (user_id, endpoint, p256dh_key, auth_key)
		VALUES ($1,$2,$3,$4)
		ON CONFLICT (endpoint) DO UPDATE SET user_id = EXCLUDED.user_id, p256dh_key = EXCLUDED.p256dh_key, auth_key = EXCLUDED.auth_key`,
		userID, req.Endpoint, req.Keys.P256dh, req.Keys.Auth,
	); err != nil {
		s.logger.Error("subscribe-push: insert failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "subscribed"})
}

// handleUnsubscribePush — DELETE /api/me/push-subscriptions. user_id까지
// 조건에 걸어 남의 구독을 endpoint만 알아도 지울 수 없게 한다.
func (s *Server) handleUnsubscribePush(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.currentUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	var req struct {
		Endpoint string `json:"endpoint"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Endpoint == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_body"})
		return
	}
	if _, err := s.db.ExecContext(r.Context(),
		`DELETE FROM push_subscriptions WHERE endpoint = $1 AND user_id = $2`, req.Endpoint, userID,
	); err != nil {
		s.logger.Error("unsubscribe-push: delete failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "unsubscribed"})
}

type pushPayload struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	URL   string `json:"url"`
}

type pushSubscriptionRow struct {
	id, endpoint, p256dh, auth string
}

// sendPushToUser sends a web push notification to every device the given
// user has subscribed on. Best-effort — 실패해도 다른 채널(이메일/SMS/
// 인앱)에는 영향 없이 로그만 남긴다(다른 채널과 동일 원칙). 410 Gone/404
// Not Found(브라우저가 구독을 이미 폐기했다는 표준 신호)를 받으면 해당
// 구독을 자동으로 정리한다 — 그대로 두면 매번 실패만 반복된다.
func (s *Server) sendPushToUser(ctx context.Context, userID, title, body, url string) {
	if s.vapidPrivateKey == "" || s.vapidPublicKey == "" {
		return
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, endpoint, p256dh_key, auth_key FROM push_subscriptions WHERE user_id = $1`, userID)
	if err != nil {
		s.logger.Error("push: subscription query failed", "error", err)
		return
	}
	var subs []pushSubscriptionRow
	for rows.Next() {
		var sc pushSubscriptionRow
		if err := rows.Scan(&sc.id, &sc.endpoint, &sc.p256dh, &sc.auth); err != nil {
			continue
		}
		subs = append(subs, sc)
	}
	rows.Close()
	if len(subs) == 0 {
		return
	}

	payload, err := json.Marshal(pushPayload{Title: title, Body: body, URL: url})
	if err != nil {
		s.logger.Error("push: payload marshal failed", "error", err)
		return
	}

	for _, sc := range subs {
		s.sendPushToSubscription(ctx, sc, payload, userID)
	}
}

// sendPushToSubscription — statusCode/err를 돌려주는 이유는 오직
// handleAdminTestPush(수동 진단용, 결과를 그 자리에서 보여줘야 함) 때문이다.
// 나머지 프로덕션 발송 경로(sendPushToUser)는 기존과 동일하게 반환값을
// 버리고 "베스트에포트 — 실패해도 로그만" 원칙을 그대로 유지한다.
func (s *Server) sendPushToSubscription(ctx context.Context, sc pushSubscriptionRow, payload []byte, userID string) (statusCode int, err error) {
	resp, sendErr := webpush.SendNotificationWithContext(ctx, payload, &webpush.Subscription{
		Endpoint: sc.endpoint,
		Keys:     webpush.Keys{P256dh: sc.p256dh, Auth: sc.auth},
	}, &webpush.Options{
		Subscriber:      s.vapidSubject,
		VAPIDPublicKey:  s.vapidPublicKey,
		VAPIDPrivateKey: s.vapidPrivateKey,
		TTL:             86400,
	})
	if sendErr != nil {
		s.logger.Error("push: send failed", "userId", userID, "error", sendErr)
		return 0, sendErr
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusGone || resp.StatusCode == http.StatusNotFound {
		s.logger.Info("push: subscription expired, cleaning up", "userId", userID, "status", resp.StatusCode)
		if _, err := s.db.ExecContext(ctx, `DELETE FROM push_subscriptions WHERE id = $1`, sc.id); err != nil {
			s.logger.Error("push: cleanup expired subscription failed", "error", err)
		}
		return resp.StatusCode, fmt.Errorf("subscription expired (status %d) — 자동 정리됨", resp.StatusCode)
	}
	if resp.StatusCode >= 300 {
		s.logger.Warn("push: non-2xx response", "userId", userID, "status", resp.StatusCode)
		return resp.StatusCode, fmt.Errorf("push service returned status %d", resp.StatusCode)
	}
	s.logger.Info("push: sent", "userId", userID, "status", resp.StatusCode)
	return resp.StatusCode, nil
}

// handleAdminTestPush — POST /api/admin/push/test. system_admin 전용 수동
// 진단 도구(2026-08-06, "#/notifications 알림 받기 배너가 안 보인다" 신고
// 조사 중 추가) — 이메일로 대상 계정을 지정하면 그 계정의 등록된 기기
// 전부에 실제 웹 푸시를 즉시 보내고, 기기별 성공/실패를 그 자리에서
// 응답으로 보여준다. sendPushToUser(프로덕션 발송 경로)는 실패해도 로그만
// 남기고 조용히 넘어가는 게 원칙이지만, 이건 "지금 이 계정에 진짜 푸시가
// 가는지" 확인이 목적이라 결과를 숨기면 안 된다 — 그래서 별도 함수로
// 분리했다.
func (s *Server) handleAdminTestPush(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.currentUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	role, err := s.userRole(r.Context(), userID)
	if err != nil {
		s.logger.Error("admin-test-push: role lookup failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if role != "system_admin" {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
		return
	}

	var req struct {
		Email string `json:"email"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Email) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_body"})
		return
	}

	if s.vapidPrivateKey == "" || s.vapidPublicKey == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "push_not_configured"})
		return
	}

	var targetUserID string
	err = s.db.QueryRowContext(r.Context(), `SELECT id FROM users WHERE email = $1`, strings.TrimSpace(strings.ToLower(req.Email))).Scan(&targetUserID)
	if err == sql.ErrNoRows {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "user_not_found"})
		return
	}
	if err != nil {
		s.logger.Error("admin-test-push: user lookup failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}

	rows, err := s.db.QueryContext(r.Context(), `SELECT id, endpoint, p256dh_key, auth_key FROM push_subscriptions WHERE user_id = $1`, targetUserID)
	if err != nil {
		s.logger.Error("admin-test-push: subscription query failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	var subs []pushSubscriptionRow
	for rows.Next() {
		var sc pushSubscriptionRow
		if err := rows.Scan(&sc.id, &sc.endpoint, &sc.p256dh, &sc.auth); err != nil {
			continue
		}
		subs = append(subs, sc)
	}
	rows.Close()
	if len(subs) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no_subscriptions", "detail": "이 계정에 등록된 푸시 구독이 없습니다"})
		return
	}

	payload, err := json.Marshal(pushPayload{
		Title: "테스트 알림",
		Body:  "관리자가 발송한 테스트 웹 푸시입니다. 이게 보이면 정상 수신 중입니다.",
		URL:   "/#/notifications",
	})
	if err != nil {
		s.logger.Error("admin-test-push: payload marshal failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal_error"})
		return
	}

	type result struct {
		Endpoint   string `json:"endpoint"` // 앞 40자만 — 전체 endpoint는 사실상 그 기기의 식별용 시크릿이라 로그/응답에 그대로 노출하지 않는다
		Success    bool   `json:"success"`
		StatusCode int    `json:"statusCode"`
		Error      string `json:"error,omitempty"`
	}
	maskEndpoint := func(endpoint string) string {
		if len(endpoint) <= 40 {
			return endpoint
		}
		return endpoint[:40] + "…"
	}
	results := make([]result, 0, len(subs))
	for _, sc := range subs {
		statusCode, sendErr := s.sendPushToSubscription(r.Context(), sc, payload, targetUserID)
		res := result{Endpoint: maskEndpoint(sc.endpoint), StatusCode: statusCode, Success: sendErr == nil}
		if sendErr != nil {
			res.Error = sendErr.Error()
		}
		results = append(results, res)
	}
	writeJSON(w, http.StatusOK, map[string]any{"targetEmail": req.Email, "results": results})
}

// sendPushToProfileMembers fans out to every member of the org — 마감
// 리마인더/상태변경처럼 특정 회원이 아니라 조직 전체가 대상인 이벤트용
// (추천 다이제스트는 이미 회원 단위라 sendPushToUser를 직접 쓴다).
func (s *Server) sendPushToProfileMembers(ctx context.Context, profileID, title, body, url string) {
	if s.vapidPrivateKey == "" || s.vapidPublicKey == "" {
		return
	}
	rows, err := s.db.QueryContext(ctx, `SELECT user_id FROM company_members WHERE company_profile_id = $1`, profileID)
	if err != nil {
		s.logger.Error("push: member query failed", "error", err)
		return
	}
	var userIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			continue
		}
		userIDs = append(userIDs, id)
	}
	rows.Close()
	for _, uid := range userIDs {
		s.sendPushToUser(ctx, uid, title, body, url)
	}
}
