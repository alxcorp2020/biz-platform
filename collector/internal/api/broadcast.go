// broadcast.go — 관리자 CMS 4번(회원 알림 메시지). 새 발송 인프라를 만들지
// 않고 기존 3채널(이메일 notify.Client, 인앱 in_app_notifications, 웹푸시
// push_notifications.go)을 그대로 재사용한다 — sendRecommendationDigest
// (notifications.go)가 이미 "여러 회원에게 3채널 반복 발송"을 하고 있어
// 그 구조를 그대로 따른다.
//
// "지금 발송" 버튼을 누르면 HTTP 핸들러 안에서 대상 전원에게 동기로 즉시
// 보낸다 — 이 서비스 규모(수십~수백 명, admin.go 주석의 "운영 규모" 전제와
// 동일)에서는 별도 배치/큐가 필요 없다는 판단. 수만 명 단위로 커지면 비동기
// 처리로 바꿔야 한다.
package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"html"
	"net/http"
	"strings"
	"time"

	"github.com/lib/pq"

	"biz-platform/collector/internal/billing"
)

const notifyEventAdminBroadcast = "admin_broadcast"

var validBroadcastChannels = map[string]bool{"email": true, "in_app": true, "push": true}

type broadcastRecipient struct {
	userID string
	email  string
}

// resolveBroadcastRecipients — admin.go의 computePlanDistribution과 동일한
// JOIN(users ↔ company_members ↔ subscriptions)으로 (user_id, email, 실효
// 플랜)을 뽑는다. targetPlan이 빈 문자열이면 전체 회원, 아니면 그 플랜인
// 사람만 남긴다.
func (s *Server) resolveBroadcastRecipients(ctx context.Context, targetPlan string) ([]broadcastRecipient, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT u.id, u.email, sub.plan, sub.status, sub.expires_at
		FROM users u
		LEFT JOIN company_members cm ON cm.user_id = u.id
		LEFT JOIN subscriptions sub ON sub.company_profile_id = cm.company_profile_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []broadcastRecipient
	for rows.Next() {
		var id, email string
		var planStr, statusStr sql.NullString
		var expiresAt sql.NullTime
		if err := rows.Scan(&id, &email, &planStr, &statusStr, &expiresAt); err != nil {
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
		if targetPlan != "" && string(plan) != targetPlan {
			continue
		}
		out = append(out, broadcastRecipient{userID: id, email: email})
	}
	return out, rows.Err()
}

type broadcastRequest struct {
	Title      string   `json:"title"`
	Content    string   `json:"content"`
	TargetPlan string   `json:"targetPlan"` // "" = 전체 회원
	Channels   []string `json:"channels"`   // email/in_app/push 중 1개 이상
}

// handleCreateBroadcast — POST /api/admin/broadcasts. 대상 계산 → 채널별
// 즉시 발송(실패해도 이 함수는 멈추지 않음 — 한 사람 발송 실패가 전체를
// 막으면 안 됨, 각 헬퍼가 이미 자체적으로 실패를 로깅한다) → 발송 이력 1행
// 기록. 이메일은 Free 플랜 월간 한도(checkEmailNotificationQuota)를 거치지
// 않는다 — 관리자가 직접 발송을 트리거하는 공지성 메시지는 팀 초대/비밀번호
// 재설정과 같은 "운영성" 성격에 가깝다고 판단(사용자가 명시한 한도 대상
// 목록(마감리마인더/상태변경/추천다이제스트/주간·월간 리포트)에 방송이
// 없다는 점 참고).
func (s *Server) handleCreateBroadcast(w http.ResponseWriter, r *http.Request) {
	adminUserID, ok := s.requireSystemAdmin(w, r)
	if !ok {
		return
	}

	var req broadcastRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_body"})
		return
	}
	req.Title = strings.TrimSpace(req.Title)
	req.Content = strings.TrimSpace(req.Content)
	if req.Title == "" || req.Content == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "title_and_content_required"})
		return
	}
	if len(req.Channels) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "channel_required"})
		return
	}
	sendEmail, sendInApp, sendPush := false, false, false
	for _, c := range req.Channels {
		if !validBroadcastChannels[c] {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_channel"})
			return
		}
		switch c {
		case "email":
			sendEmail = true
		case "in_app":
			sendInApp = true
		case "push":
			sendPush = true
		}
	}
	if req.TargetPlan != "" {
		if _, known := billing.Plans[billing.Plan(req.TargetPlan)]; !known {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_target_plan"})
			return
		}
	}

	ctx := r.Context()
	recipients, err := s.resolveBroadcastRecipients(ctx, req.TargetPlan)
	if err != nil {
		s.logger.Error("broadcast: resolve recipients failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}

	bodyHTML := "<p>" + strings.ReplaceAll(html.EscapeString(req.Content), "\n", "<br>") + "</p>"
	for _, rcpt := range recipients {
		if sendInApp {
			if err := s.insertInAppNotification(ctx, nil, &rcpt.userID, notifyEventAdminBroadcast, req.Title, req.Content, nil, nil); err != nil {
				s.logger.Error("broadcast: in-app insert failed", "userID", rcpt.userID, "error", err)
			}
		}
		if sendPush {
			s.sendPushToUser(ctx, rcpt.userID, req.Title, req.Content, "/#/announcements")
		}
		if sendEmail {
			uid := rcpt.userID
			s.sendNotificationEmail(ctx, notifyEventAdminBroadcast, rcpt.email, &uid, nil, nil, nil, req.Title, bodyHTML)
		}
	}

	var id string
	err = s.db.QueryRowContext(ctx, `
		INSERT INTO broadcast_messages (title, content, target_plan, channels, recipient_count, created_by)
		VALUES ($1,$2,$3,$4,$5,$6) RETURNING id`,
		req.Title, req.Content, sql.NullString{String: req.TargetPlan, Valid: req.TargetPlan != ""},
		pq.Array(req.Channels), len(recipients), adminUserID,
	).Scan(&id)
	if err != nil {
		s.logger.Error("broadcast: insert history failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"id": id, "recipientCount": len(recipients)})
}

type broadcastHistoryItem struct {
	ID             string    `json:"id"`
	Title          string    `json:"title"`
	Content        string    `json:"content"`
	TargetPlan     *string   `json:"targetPlan"`
	Channels       []string  `json:"channels"`
	RecipientCount int       `json:"recipientCount"`
	CreatedByEmail string    `json:"createdByEmail"`
	CreatedAt      time.Time `json:"createdAt"`
}

func (s *Server) handleListBroadcasts(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireSystemAdmin(w, r); !ok {
		return
	}
	rows, err := s.db.QueryContext(r.Context(), `
		SELECT bm.id, bm.title, bm.content, bm.target_plan, bm.channels, bm.recipient_count, u.email, bm.created_at
		FROM broadcast_messages bm
		JOIN users u ON u.id = bm.created_by
		ORDER BY bm.created_at DESC`)
	if err != nil {
		s.logger.Error("list-broadcasts: query failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	defer rows.Close()

	items := []broadcastHistoryItem{}
	for rows.Next() {
		var it broadcastHistoryItem
		var targetPlan sql.NullString
		if err := rows.Scan(&it.ID, &it.Title, &it.Content, &targetPlan, pq.Array(&it.Channels), &it.RecipientCount, &it.CreatedByEmail, &it.CreatedAt); err != nil {
			s.logger.Error("list-broadcasts: scan failed", "error", err)
			continue
		}
		it.TargetPlan = nullStringPtr(targetPlan)
		items = append(items, it)
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}
