// notifications.go — 이메일 알림 최소 버전(CTO 평가 TOP10 4번). 발송
// 대상 3개 이벤트를 하루 1회 배치로 처리한다: 파이프라인 제출마감
// D-3/D-1 리마인더, grade='recommended' 신규 공고 다이제스트. 담당자
// 상태변경 알림(3번째 이벤트)은 배치가 아니라 company_pipeline.go의
// PATCH 핸들러에서 상태가 실제로 바뀌는 순간 동기 트리거된다.
//
// 실발송은 notify.Client가 감싸고, 이 파일은 "누구에게 보낼지"만 결정한다
// — notification_log에 event_type+대상 조합으로 status='sent' 행이 이미
// 있으면 재조회 대상에서 제외해 중복발송을 막는다.
package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"html"
	"net/http"

	"github.com/lib/pq"
)

const (
	notifyEventDeadlineD3           = "deadline_d3"
	notifyEventDeadlineD1           = "deadline_d1"
	notifyEventRecommendationDigest = "recommendation_digest"
	notifyEventAssigneeStatusChange = "assignee_status_change"
)

// pipelineActiveForNotification: 종결된 건(제출완료/낙찰/탈락/보류/제외)은
// 마감이 임박해도 더 이상 챙길 필요가 없어 알림 대상에서 제외한다 —
// dashboard.go의 pipelineActiveStatuses와 동일한 판단.
var pipelineActiveForNotification = pipelineActiveStatuses

// RunDailyNotifications is the entry point cmd/apiserver calls on a daily
// ticker (see notify.NextDailyRun). Each sub-batch logs its own errors and
// keeps going — one failing batch shouldn't block the others.
func (s *Server) RunDailyNotifications(ctx context.Context) {
	if s.notify == nil || !s.notify.Configured() {
		s.logger.Warn("notify: RESEND_API_KEY not configured, skipping daily notification run")
		return
	}
	if err := s.sendDeadlineReminders(ctx, 3, notifyEventDeadlineD3); err != nil {
		s.logger.Error("notify: D-3 reminder batch failed", "error", err)
	}
	if err := s.sendDeadlineReminders(ctx, 1, notifyEventDeadlineD1); err != nil {
		s.logger.Error("notify: D-1 reminder batch failed", "error", err)
	}
	if err := s.sendRecommendationDigest(ctx); err != nil {
		s.logger.Error("notify: recommendation digest batch failed", "error", err)
	}
}

// sendDeadlineReminders emails every pipeline entry (not yet closed out)
// whose submission_deadline is exactly offsetDays from today, to the profile
// owner, skipping users who opted out and anything already logged as sent
// for this exact event_type — see notification_log.
func (s *Server) sendDeadlineReminders(ctx context.Context, offsetDays int, eventType string) error {
	rows, err := s.db.QueryContext(ctx, `
		SELECT pe.id, pe.notice_id, n.title, n.organization_name, pe.status, u.id, u.email
		FROM notice_pipeline_entries pe
		JOIN company_profiles cp ON cp.id = pe.company_profile_id
		JOIN users u ON u.id = cp.user_id
		JOIN notices n ON n.id = pe.notice_id
		WHERE pe.submission_deadline = CURRENT_DATE + ($1 * INTERVAL '1 day')
		  AND u.email_notifications_enabled = true
		  AND NOT EXISTS (
		      SELECT 1 FROM notification_log nl
		      WHERE nl.event_type = $2 AND nl.pipeline_entry_id = pe.id AND nl.status = 'sent'
		  )`, offsetDays, eventType)
	if err != nil {
		return err
	}

	type row struct {
		entryID, noticeID, title, status, userID, email string
		org                                              sql.NullString
	}
	var targets []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.entryID, &r.noticeID, &r.title, &r.org, &r.status, &r.userID, &r.email); err != nil {
			continue
		}
		if !pipelineActiveForNotification[r.status] {
			continue // 종결된 건은 마감이 와도 알릴 필요 없음
		}
		targets = append(targets, r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	for _, t := range targets {
		subject := fmt.Sprintf("[제출마감 D-%d] %s", offsetDays, t.title)
		body := fmt.Sprintf(
			"<p>제출마감이 D-%d 남은 참여 건이 있습니다.</p><p><b>%s</b></p><p>발주기관: %s</p>",
			offsetDays, html.EscapeString(t.title), html.EscapeString(t.org.String),
		)
		userID, entryID, noticeID := t.userID, t.entryID, t.noticeID
		s.sendNotificationEmail(ctx, eventType, t.email, &userID, &entryID, &noticeID, subject, body)
	}
	return nil
}

// sendRecommendationDigest sends, per company profile with notifications
// enabled, one email listing today's newly-recommended notices (grade ==
// gradeRecommended) that aren't already in that profile's pipeline and
// haven't been digested before. One notification_log row is written per
// (user, notice) pair even though they're bundled into a single email, so
// each notice is only ever digested once — same dedup granularity as the
// other two events.
func (s *Server) sendRecommendationDigest(ctx context.Context) error {
	profileRows, err := s.db.QueryContext(ctx, `
		SELECT cp.id, u.id, u.email, cp.region, cp.industry, cp.company_size
		FROM company_profiles cp
		JOIN users u ON u.id = cp.user_id
		WHERE u.email_notifications_enabled = true`)
	if err != nil {
		return err
	}
	type profileRow struct {
		profileID, userID, email string
		region, size             sql.NullString
		industry                 pq.StringArray
	}
	var profiles []profileRow
	for profileRows.Next() {
		var p profileRow
		if err := profileRows.Scan(&p.profileID, &p.userID, &p.email, &p.region, &p.industry, &p.size); err != nil {
			continue
		}
		profiles = append(profiles, p)
	}
	if err := profileRows.Err(); err != nil {
		profileRows.Close()
		return err
	}
	profileRows.Close()
	if len(profiles) == 0 {
		return nil
	}

	noticeRows, err := s.db.QueryContext(ctx, `
		SELECT id, title, organization_name, region, industry, budget_amount
		FROM notices
		WHERE status NOT IN ('closed','cancelled')
		  AND (application_end_at IS NULL OR application_end_at >= CURRENT_DATE)
		LIMIT `+itoa(dashboardNoticeScanLimit))
	if err != nil {
		return err
	}
	type noticeRow struct {
		id, title            string
		org, region, industry sql.NullString
		budget               sql.NullInt64
	}
	var notices []noticeRow
	for noticeRows.Next() {
		var n noticeRow
		if err := noticeRows.Scan(&n.id, &n.title, &n.org, &n.region, &n.industry, &n.budget); err != nil {
			continue
		}
		notices = append(notices, n)
	}
	if err := noticeRows.Err(); err != nil {
		noticeRows.Close()
		return err
	}
	noticeRows.Close()

	for _, p := range profiles {
		pipelinedIDs, err := s.fetchPipelinedNoticeIDs(ctx, p.profileID)
		if err != nil {
			s.logger.Error("notify: pipelined notice ids query failed", "error", err)
			continue
		}
		digestedIDs, err := s.fetchDigestedNoticeIDs(ctx, p.userID)
		if err != nil {
			s.logger.Error("notify: digested notice ids query failed", "error", err)
			continue
		}
		trackRecordMax, err := s.fetchTrackRecordMaxAmount(ctx, p.profileID)
		if err != nil {
			s.logger.Error("notify: track record max amount query failed", "error", err)
		}
		company := companyScoringInput{
			Region: p.region, Industry: []string(p.industry), Size: p.size,
			TrackRecordMaxAmount: trackRecordMax,
		}

		var matched []noticeRow
		for _, n := range notices {
			if pipelinedIDs[n.id] || digestedIDs[n.id] {
				continue
			}
			score := scoreNoticeForCompany(
				noticeScoringInput{Region: n.region, Industry: n.industry, BudgetAmount: n.budget}, company,
			)
			if score.Grade != gradeRecommended {
				continue
			}
			matched = append(matched, n)
		}
		if len(matched) == 0 {
			continue
		}

		subject := fmt.Sprintf("오늘의 추천 공고 %d건", len(matched))
		var itemsHTML string
		for _, n := range matched {
			itemsHTML += fmt.Sprintf("<li><b>%s</b> (%s)</li>", html.EscapeString(n.title), html.EscapeString(n.org.String))
		}
		body := fmt.Sprintf("<p>참여를 권장하는 신규 공고 %d건이 발견되었습니다.</p><ul>%s</ul>", len(matched), itemsHTML)

		sendErr := s.notify.Send(ctx, p.email, subject, body)
		status, errMsg := "sent", sql.NullString{}
		if sendErr != nil {
			status = "failed"
			errMsg = sql.NullString{String: sendErr.Error(), Valid: true}
			s.logger.Error("notify: digest send failed", "recipient", p.email, "error", sendErr)
		}
		for _, n := range matched {
			if _, logErr := s.db.ExecContext(ctx, `
				INSERT INTO notification_log (event_type, recipient_email, user_id, notice_id, subject, status, error_message)
				VALUES ($1,$2,$3,$4,$5,$6,$7)`,
				notifyEventRecommendationDigest, p.email, p.userID, n.id, subject, status, errMsg,
			); logErr != nil {
				s.logger.Error("notify: digest log insert failed", "error", logErr)
			}
		}
	}
	return nil
}

func (s *Server) fetchPipelinedNoticeIDs(ctx context.Context, profileID string) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT notice_id FROM notice_pipeline_entries WHERE company_profile_id = $1`, profileID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	ids := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			continue
		}
		ids[id] = true
	}
	return ids, rows.Err()
}

// fetchDigestedNoticeIDs returns notices already sent to this user in a past
// recommendation digest — status='sent' only, so a previously-failed attempt
// doesn't permanently block retrying.
func (s *Server) fetchDigestedNoticeIDs(ctx context.Context, userID string) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT notice_id FROM notification_log
		WHERE event_type = $1 AND user_id = $2 AND status = 'sent' AND notice_id IS NOT NULL`,
		notifyEventRecommendationDigest, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	ids := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			continue
		}
		ids[id] = true
	}
	return ids, rows.Err()
}

// notifyAssigneeStatusChange sends the third notification event: pipeline
// status changed and an assignee_email is on file. Called as a fire-and-forget
// goroutine from company_pipeline.go's PATCH handler — never blocks the HTTP
// response, and uses context.Background() since the request context would
// already be cancelled by the time the goroutine runs.
func (s *Server) notifyAssigneeStatusChange(ctx context.Context, email, pipelineEntryID, noticeID, noticeTitle, newStatus string) {
	subject := fmt.Sprintf("[상태변경] %s", noticeTitle)
	body := fmt.Sprintf(
		"<p><b>%s</b>의 참여 상태가 <b>%s</b>(으)로 변경되었습니다.</p>",
		html.EscapeString(noticeTitle), html.EscapeString(newStatus),
	)
	s.sendNotificationEmail(ctx, notifyEventAssigneeStatusChange, email, nil, &pipelineEntryID, &noticeID, subject, body)
}

// handleRunNotifications manually fires the daily notification batch
// (D-3/D-1 deadline reminders + recommendation digest) on demand — the only
// other trigger is the 09:00 KST ticker in cmd/apiserver. system_admin-only:
// this sends real email to real recipients, same as the scheduled run.
func (s *Server) handleRunNotifications(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.currentUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	role, err := s.userRole(r.Context(), userID)
	if err != nil {
		s.logger.Error("run-notifications: role lookup failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if role != "system_admin" {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
		return
	}
	s.RunDailyNotifications(r.Context())
	writeJSON(w, http.StatusOK, map[string]string{"status": "completed"})
}

// handleUpdateNotificationSettings toggles the caller's
// users.email_notifications_enabled — the "이메일 알림 전체 on/off" the
// user asked for. Deadline reminders and the recommendation digest both
// check this column; the assignee-status-change event does not (its
// recipient is an arbitrary assignee_email, not necessarily a system user).
func (s *Server) handleUpdateNotificationSettings(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.currentUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	var req struct {
		EmailNotificationsEnabled bool `json:"emailNotificationsEnabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_body"})
		return
	}

	if _, err := s.db.ExecContext(r.Context(),
		`UPDATE users SET email_notifications_enabled = $1 WHERE id = $2`,
		req.EmailNotificationsEnabled, userID,
	); err != nil {
		s.logger.Error("notification-settings: update failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]bool{"emailNotificationsEnabled": req.EmailNotificationsEnabled})
}

// sendNotificationEmail is the shared send+log path for the two
// single-recipient events (deadline reminders, assignee status change) — the
// digest event logs itself directly since it fans one email out to multiple
// notification_log rows (see sendRecommendationDigest).
func (s *Server) sendNotificationEmail(ctx context.Context, eventType, recipientEmail string, userID, pipelineEntryID, noticeID *string, subject, bodyHTML string) {
	if s.notify == nil {
		return
	}
	err := s.notify.Send(ctx, recipientEmail, subject, bodyHTML)
	status, errMsg := "sent", sql.NullString{}
	if err != nil {
		status = "failed"
		errMsg = sql.NullString{String: err.Error(), Valid: true}
		s.logger.Error("notify: send failed", "eventType", eventType, "recipient", recipientEmail, "error", err)
	}
	if _, logErr := s.db.ExecContext(ctx, `
		INSERT INTO notification_log (event_type, recipient_email, user_id, pipeline_entry_id, notice_id, subject, status, error_message)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		eventType, recipientEmail, userID, pipelineEntryID, noticeID, subject, status, errMsg,
	); logErr != nil {
		s.logger.Error("notify: log insert failed", "error", logErr)
	}
}
