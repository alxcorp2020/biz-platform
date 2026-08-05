// inapp_notifications.go — 인앱 알림함(Phase 5). notification_log(이메일/
// SMS 발송 이력, 채널별로 행이 갈라짐)와는 별개 테이블(in_app_notifications)
// 을 쓴다 — 화면에 보여줄 목적이라 이벤트 1건당 1행만 쌓고 읽음/안읽음
// 상태를 갖는다. 발송 이벤트가 실제로 발생하는 자리(notifications.go의
// sendDeadlineReminders/notifyAssigneeStatusChange/sendRecommendationDigest)
// 에서 insertXxxInAppNotification 헬퍼를 그대로 호출해 채운다.
package api

import (
	"context"
	"database/sql"
	"net/http"
	"time"
)

// insertInAppNotification is the low-level insert shared by all 3 event
// paths below. company_profile_id/user_id 중 정확히 하나만 채워야 한다
// (DB CHECK 제약과 동일한 규칙).
func (s *Server) insertInAppNotification(ctx context.Context, profileID, userID *string, eventType, title, body string, pipelineEntryID, noticeID *string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO in_app_notifications (company_profile_id, user_id, event_type, title, body, pipeline_entry_id, notice_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		profileID, userID, eventType, title, body, pipelineEntryID, noticeID,
	)
	return err
}

// insertEntryScopedInAppNotification covers the two org-scoped events
// (deadline reminders/assignee status change) — dedup key는 (event_type,
// pipeline_entry_id, title)이다. title까지 넣은 이유: 담당자 상태변경은
// 같은 파이프라인 건이 여러 번 상태가 바뀔 수 있고(참여검토→서류준비→
// 제출완료 등) 그때마다 다른 알림이어야 하는데, event_type 상수 자체는
// "assignee_status_change" 하나로 고정이라 title(전환 내용)까지 넣지
// 않으면 첫 상태변경 이후로는 계속 무시된다. 마감 리마인더는 D-7/D-3/D-1이
// 애초에 서로 다른 event_type이라 title을 더해도 기존 동작과 같다.
func (s *Server) insertEntryScopedInAppNotification(ctx context.Context, profileID, eventType, pipelineEntryID, noticeID, title, body string) error {
	var exists bool
	err := s.db.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM in_app_notifications
			WHERE event_type = $1 AND pipeline_entry_id = $2 AND title = $3
		)`, eventType, pipelineEntryID, title).Scan(&exists)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	return s.insertInAppNotification(ctx, &profileID, nil, eventType, title, body, &pipelineEntryID, &noticeID)
}

// insertDigestInAppNotification covers user-scoped daily digest events
// (recommendation_digest, saved_search_match) — 둘 다 하루에 여러 건이
// 아니라 "오늘의 매칭 N건" 한 알림으로 묶이므로, dedup 기준도 (event_type,
// user_id, 오늘 날짜)다. eventType을 파라미터로 받는다 — 2026-08-06 이전엔
// notifyEventRecommendationDigest로 고정돼 있었는데, saved_search_match가
// 이 헬퍼를 그대로 재사용하면서 라벨이 잘못 찍히는 버그가 있어(실제
// 로컬 검증 중 발견) 일반화했다.
func (s *Server) insertDigestInAppNotification(ctx context.Context, userID, eventType, title, body string) error {
	var exists bool
	err := s.db.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM in_app_notifications
			WHERE event_type = $1 AND user_id = $2 AND created_at::date = CURRENT_DATE
		)`, eventType, userID).Scan(&exists)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	return s.insertInAppNotification(ctx, nil, &userID, eventType, title, body, nil, nil)
}

type inAppNotificationItem struct {
	ID              string    `json:"id"`
	EventType       string    `json:"eventType"`
	Title           string    `json:"title"`
	Body            string    `json:"body"`
	PipelineEntryID *string   `json:"pipelineEntryId,omitempty"`
	NoticeID        *string   `json:"noticeId,omitempty"`
	Read            bool      `json:"read"`
	CreatedAt       time.Time `json:"createdAt"`
}

// handleListInAppNotifications — GET /api/me/notifications. 조직 단위
// 이벤트(company_profile_id로 채워진 행)는 같은 회사 팀원 전체에게,
// 회원 단위 이벤트(user_id)는 받은 사람 본인에게만 보인다 — 로그인
// 계정 하나가 두 조건 중 뭐든 해당하면 다 보이므로 OR로 합친다.
func (s *Server) handleListInAppNotifications(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.currentUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	ctx := r.Context()

	profile, err := s.getCompanyProfile(r, userID)
	if err != nil {
		s.logger.Error("list-notifications: profile lookup failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}

	q := r.URL.Query()
	offset := parseListingIntParam(q.Get("offset"), 0)
	limit := parseListingIntParam(q.Get("limit"), 20)
	if limit <= 0 || limit > 50 {
		limit = 20
	}
	unreadOnly := q.Get("unreadOnly") == "true"

	var profileID sql.NullString
	if profile != nil {
		profileID = sql.NullString{String: profile.ID, Valid: true}
	}

	query := `
		SELECT id, event_type, title, body, pipeline_entry_id, notice_id, read_at, created_at, COUNT(*) OVER() AS total_count
		FROM in_app_notifications
		WHERE (company_profile_id = $1 OR user_id = $2)`
	args := []any{profileID, userID}
	if unreadOnly {
		query += " AND read_at IS NULL"
	}
	query += " ORDER BY created_at DESC LIMIT $3 OFFSET $4"
	args = append(args, limit, offset)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		s.logger.Error("list-notifications: query failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	defer rows.Close()

	items := []inAppNotificationItem{}
	total := 0
	for rows.Next() {
		var it inAppNotificationItem
		var pipelineEntryID, noticeID sql.NullString
		var readAt sql.NullTime
		if err := rows.Scan(&it.ID, &it.EventType, &it.Title, &it.Body, &pipelineEntryID, &noticeID, &readAt, &it.CreatedAt, &total); err != nil {
			s.logger.Error("list-notifications: scan failed", "error", err)
			continue
		}
		if pipelineEntryID.Valid {
			it.PipelineEntryID = &pipelineEntryID.String
		}
		if noticeID.Valid {
			it.NoticeID = &noticeID.String
		}
		it.Read = readAt.Valid
		items = append(items, it)
	}
	if err := rows.Err(); err != nil {
		s.logger.Error("list-notifications: rows iteration failed", "error", err)
	}

	var unreadCount int
	if err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM in_app_notifications
		WHERE (company_profile_id = $1 OR user_id = $2) AND read_at IS NULL`,
		profileID, userID,
	).Scan(&unreadCount); err != nil {
		s.logger.Error("list-notifications: unread count query failed", "error", err)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"items": items, "total": total, "unreadCount": unreadCount,
		"offset": offset, "limit": limit, "hasMore": offset+len(items) < total,
	})
}

// handleUnreadNotificationCount — GET /api/me/notifications/unread-count.
// 사이드바/탭바 배지용 가벼운 엔드포인트(목록 전체를 안 불러온다).
func (s *Server) handleUnreadNotificationCount(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.currentUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	profile, err := s.getCompanyProfile(r, userID)
	if err != nil {
		s.logger.Error("unread-count: profile lookup failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	var profileID sql.NullString
	if profile != nil {
		profileID = sql.NullString{String: profile.ID, Valid: true}
	}
	var count int
	if err := s.db.QueryRowContext(r.Context(), `
		SELECT COUNT(*) FROM in_app_notifications
		WHERE (company_profile_id = $1 OR user_id = $2) AND read_at IS NULL`,
		profileID, userID,
	).Scan(&count); err != nil {
		s.logger.Error("unread-count: query failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"count": count})
}

// handleMarkNotificationRead — POST /api/me/notifications/{id}/read. 소유권
// 확인은 목록 조회와 같은 (company_profile_id/user_id) 조건으로 — 남의
// 알림을 id만 알면 읽음 처리할 수 있으면 안 되므로 WHERE절에 그대로 건다.
func (s *Server) handleMarkNotificationRead(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.currentUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	id := r.PathValue("id")
	profile, err := s.getCompanyProfile(r, userID)
	if err != nil {
		s.logger.Error("mark-notification-read: profile lookup failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	var profileID sql.NullString
	if profile != nil {
		profileID = sql.NullString{String: profile.ID, Valid: true}
	}
	res, err := s.db.ExecContext(r.Context(), `
		UPDATE in_app_notifications SET read_at = now()
		WHERE id = $1 AND read_at IS NULL AND (company_profile_id = $2 OR user_id = $3)`,
		id, profileID, userID)
	if err != nil {
		s.logger.Error("mark-notification-read: update failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		// 이미 읽음 상태였거나(정상), 존재하지 않거나 남의 알림(둘 다 굳이
		// 구분해 알려줄 필요 없음 — 프론트는 낙관적으로 읽음 처리하고 끝).
		writeJSON(w, http.StatusOK, map[string]string{"status": "no_change"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "read"})
}

// handleMarkAllNotificationsRead — POST /api/me/notifications/read-all.
func (s *Server) handleMarkAllNotificationsRead(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.currentUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	profile, err := s.getCompanyProfile(r, userID)
	if err != nil {
		s.logger.Error("mark-all-notifications-read: profile lookup failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	var profileID sql.NullString
	if profile != nil {
		profileID = sql.NullString{String: profile.ID, Valid: true}
	}
	if _, err := s.db.ExecContext(r.Context(), `
		UPDATE in_app_notifications SET read_at = now()
		WHERE read_at IS NULL AND (company_profile_id = $1 OR user_id = $2)`,
		profileID, userID); err != nil {
		s.logger.Error("mark-all-notifications-read: update failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
