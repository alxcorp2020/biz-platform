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
	"strings"

	"github.com/lib/pq"
)

const (
	notifyEventDeadlineD3           = "deadline_d3"
	notifyEventDeadlineD1           = "deadline_d1"
	notifyEventRecommendationDigest = "recommendation_digest"
	notifyEventAssigneeStatusChange = "assignee_status_change"

	notifyChannelEmail = "email"
	notifyChannelSMS   = "sms"
)

// pipelineActiveForNotification: 종결된 건(제출완료/낙찰/탈락/보류/제외)은
// 마감이 임박해도 더 이상 챙길 필요가 없어 알림 대상에서 제외한다 —
// dashboard.go의 pipelineActiveStatuses와 동일한 판단.
var pipelineActiveForNotification = pipelineActiveStatuses

// RunDailyNotifications is the entry point cmd/apiserver calls on a daily
// ticker (see notify.NextDailyRun). Each sub-batch logs its own errors and
// keeps going — one failing batch shouldn't block the others.
func (s *Server) RunDailyNotifications(ctx context.Context) {
	emailReady := s.notify != nil && s.notify.Configured()
	smsReady := s.smsNotify != nil && s.smsNotify.Configured()
	if !emailReady && !smsReady {
		s.logger.Warn("notify: RESEND_API_KEY/ALIGO_API_KEY 모두 설정되지 않아 알림 배치를 건너뜁니다")
		return
	}
	if err := s.sendDeadlineReminders(ctx, 3, notifyEventDeadlineD3); err != nil {
		s.logger.Error("notify: D-3 reminder batch failed", "error", err)
	}
	if err := s.sendDeadlineReminders(ctx, 1, notifyEventDeadlineD1); err != nil {
		s.logger.Error("notify: D-1 reminder batch failed", "error", err)
	}
	// 추천공고 다이제스트는 이메일 전용으로 유지한다(판단 근거): 매칭
	// 건수가 0~N건으로 가변적이라 SMS 90바이트 예산 안에 의미 있게 요약할
	// 방법이 없다 — 제목 나열을 자르면 "어떤 공고인지"가 사라지고,
	// 건수만 보내면 실질 정보가 없다. 짧은 단일 이벤트인 마감 리마인더/
	// 담당자 상태변경과 달리 이 이벤트만 원천적으로 SMS에 맞지 않는다.
	if emailReady {
		if err := s.sendRecommendationDigest(ctx); err != nil {
			s.logger.Error("notify: recommendation digest batch failed", "error", err)
		}
	}
}

// sendDeadlineReminders notifies (email and/or SMS) every pipeline entry
// (not yet closed out) whose submission_deadline is exactly offsetDays from
// today. 팀기능: 알림 on/off와 전화번호는 이제 조직(company_profiles) 단위
// 설정이다 — SMS는 조직에 등록된 전화번호 하나로 한 번만 보내고(멤버 수만큼
// 중복 발송하지 않음), 이메일은 조직에 속한 모든 멤버 각자에게 보낸다(각자
// 자기 이메일로 확인해야 하므로) — 그래서 이메일 발송 대상은 파이프라인
// 엔트리별로 company_members를 다시 조회해 펼친다. 이메일/SMS는 서로
// 독립적으로 중복발송 여부를 판단한다(한쪽 채널이 이미 성공해도 다른
// 채널은 별도로 시도) — 이메일은 (event_type, pipeline_entry_id, channel,
// user_id)로, SMS는 조직 단위라 user_id 없이 (event_type,
// pipeline_entry_id, channel)로 dedup한다.
func (s *Server) sendDeadlineReminders(ctx context.Context, offsetDays int, eventType string) error {
	rows, err := s.db.QueryContext(ctx, `
		SELECT pe.id, pe.notice_id, n.title, n.organization_name, pe.status, pe.company_profile_id,
		       cp.email_notifications_enabled, cp.phone_number, cp.sms_notifications_enabled,
		       EXISTS (
		           SELECT 1 FROM notification_log nl
		           WHERE nl.event_type = $2 AND nl.pipeline_entry_id = pe.id
		             AND nl.channel = 'sms' AND nl.status = 'sent'
		       ) AS sms_already_sent
		FROM notice_pipeline_entries pe
		JOIN company_profiles cp ON cp.id = pe.company_profile_id
		JOIN notices n ON n.id = pe.notice_id
		WHERE pe.submission_deadline = CURRENT_DATE + ($1 * INTERVAL '1 day')
		  AND (cp.email_notifications_enabled = true
		       OR (cp.sms_notifications_enabled = true AND cp.phone_number IS NOT NULL AND cp.phone_number != ''))
		`, offsetDays, eventType)
	if err != nil {
		return err
	}

	type row struct {
		entryID, noticeID, title, status, profileID string
		org                                         sql.NullString
		emailEnabled                                bool
		phone                                       sql.NullString
		smsEnabled                                  bool
		smsAlreadySent                              bool
	}
	var targets []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.entryID, &r.noticeID, &r.title, &r.org, &r.status, &r.profileID,
			&r.emailEnabled, &r.phone, &r.smsEnabled, &r.smsAlreadySent); err != nil {
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
		entryID, noticeID := t.entryID, t.noticeID
		if t.emailEnabled {
			members, err := s.fetchCompanyMembersForNotification(ctx, t.profileID, eventType, notifyChannelEmail, entryID)
			if err != nil {
				s.logger.Error("notify: member lookup failed", "error", err)
			}
			for _, m := range members {
				if m.alreadySent {
					continue
				}
				userID := m.userID
				subject := fmt.Sprintf("[제출마감 D-%d] %s", offsetDays, t.title)
				body := fmt.Sprintf(
					"<p>제출마감이 D-%d 남은 참여 건이 있습니다.</p><p><b>%s</b></p><p>발주기관: %s</p>",
					offsetDays, html.EscapeString(t.title), html.EscapeString(t.org.String),
				)
				s.sendNotificationEmail(ctx, eventType, m.email, &userID, &entryID, &noticeID, subject, body)
			}
		}
		if t.smsEnabled && t.phone.Valid && t.phone.String != "" && !t.smsAlreadySent {
			msg := fmt.Sprintf("[제출마감 D-%d] %s 제출마감이 D-%d일 남았습니다.", offsetDays, truncateForSMS(t.title, 25), offsetDays)
			s.sendNotificationSMS(ctx, eventType, t.phone.String, nil, &entryID, &noticeID, msg)
		}
	}
	return nil
}

type memberNotifyTarget struct {
	userID, email string
	alreadySent   bool
}

// fetchCompanyMembersForNotification lists every member of an org along with
// whether *that specific member* already has a 'sent' row for this exact
// (event_type, pipeline_entry_id, channel) — email dedup is per-member
// (unlike SMS, which is per-org since there's only one shared phone number).
func (s *Server) fetchCompanyMembersForNotification(ctx context.Context, profileID, eventType, channel, pipelineEntryID string) ([]memberNotifyTarget, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT cm.user_id, u.email,
		       EXISTS (
		           SELECT 1 FROM notification_log nl
		           WHERE nl.event_type = $2 AND nl.pipeline_entry_id = $3
		             AND nl.channel = $4 AND nl.status = 'sent' AND nl.user_id = cm.user_id
		       ) AS already_sent
		FROM company_members cm
		JOIN users u ON u.id = cm.user_id
		WHERE cm.company_profile_id = $1`, profileID, eventType, pipelineEntryID, channel)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []memberNotifyTarget
	for rows.Next() {
		var m memberNotifyTarget
		if err := rows.Scan(&m.userID, &m.email, &m.alreadySent); err != nil {
			continue
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// sendRecommendationDigest sends, per organization with notifications
// enabled, one email per member listing today's newly-recommended notices
// (grade == gradeRecommended) that aren't already in that org's pipeline.
// 후보 공고 계산(지역/업종/규모 매칭)은 조직 단위로 한 번만 하고 — 이 셋은
// company_profiles의 조직 속성이라 멤버마다 다르지 않다 — "이미 다이제스트
// 받은 적 있는지"만 멤버별로 따로 걸러서 각자에게 보낸다(한 멤버가 이미
// 본 공고를 다른 신규 멤버는 여전히 처음 보는 것일 수 있으므로).
func (s *Server) sendRecommendationDigest(ctx context.Context) error {
	profileRows, err := s.db.QueryContext(ctx, `
		SELECT cp.id, cp.region, cp.industry, cp.company_size
		FROM company_profiles cp
		WHERE cp.email_notifications_enabled = true`)
	if err != nil {
		return err
	}
	type profileRow struct {
		profileID    string
		region, size sql.NullString
		industry     pq.StringArray
	}
	var profiles []profileRow
	for profileRows.Next() {
		var p profileRow
		if err := profileRows.Scan(&p.profileID, &p.region, &p.industry, &p.size); err != nil {
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
		SELECT id, notice_type, title, organization_name, region, industry, budget_amount
		FROM notices
		WHERE status NOT IN ('closed','cancelled')
		  AND (application_end_at IS NULL OR application_end_at >= CURRENT_DATE)
		LIMIT `+itoa(dashboardNoticeScanLimit))
	if err != nil {
		return err
	}
	type noticeRow struct {
		id, noticeType, title string
		org, region, industry sql.NullString
		budget                sql.NullInt64
	}
	var notices []noticeRow
	for noticeRows.Next() {
		var n noticeRow
		if err := noticeRows.Scan(&n.id, &n.noticeType, &n.title, &n.org, &n.region, &n.industry, &n.budget); err != nil {
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
		trackRecordMax, err := s.fetchTrackRecordMaxAmount(ctx, p.profileID)
		if err != nil {
			s.logger.Error("notify: track record max amount query failed", "error", err)
		}
		company := companyScoringInput{
			Region: p.region, Industry: []string(p.industry), Size: p.size,
			TrackRecordMaxAmount: trackRecordMax,
		}

		// 조직 속성(지역/업종/규모)만으로 판정하는 등급 계산은 멤버와
		// 무관하게 한 번만 — pipelinedIDs만 여기서 걸러내고, "이미
		// 다이제스트 받았는지"는 멤버별로 아래에서 따로 거른다.
		var orgRecommended []noticeRow
		for _, n := range notices {
			if pipelinedIDs[n.id] {
				continue
			}
			score := scoreNoticeForCompany(
				noticeScoringInput{NoticeType: n.noticeType, Region: n.region, Industry: n.industry, BudgetAmount: n.budget}, company,
			)
			if score.Grade != gradeRecommended {
				continue
			}
			orgRecommended = append(orgRecommended, n)
		}
		if len(orgRecommended) == 0 {
			continue
		}

		members, err := s.fetchCompanyMemberEmails(ctx, p.profileID)
		if err != nil {
			s.logger.Error("notify: member lookup failed", "error", err)
			continue
		}
		for _, m := range members {
			digestedIDs, err := s.fetchDigestedNoticeIDs(ctx, m.userID)
			if err != nil {
				s.logger.Error("notify: digested notice ids query failed", "error", err)
				continue
			}
			var matched []noticeRow
			for _, n := range orgRecommended {
				if !digestedIDs[n.id] {
					matched = append(matched, n)
				}
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

			sendErr := s.notify.Send(ctx, m.email, subject, body)
			status, errMsg := "sent", sql.NullString{}
			if sendErr != nil {
				status = "failed"
				errMsg = sql.NullString{String: sendErr.Error(), Valid: true}
				s.logger.Error("notify: digest send failed", "recipient", m.email, "error", sendErr)
			}
			for _, n := range matched {
				if _, logErr := s.db.ExecContext(ctx, `
					INSERT INTO notification_log (event_type, channel, recipient_email, user_id, notice_id, subject, status, error_message)
					VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
					notifyEventRecommendationDigest, notifyChannelEmail, m.email, m.userID, n.id, subject, status, errMsg,
				); logErr != nil {
					s.logger.Error("notify: digest log insert failed", "error", logErr)
				}
			}
		}
	}
	return nil
}

// fetchCompanyMemberEmails lists every member's (user_id, email) for an org
// — used by both sendRecommendationDigest and (indirectly, via
// fetchCompanyMembersForNotification) sendDeadlineReminders.
func (s *Server) fetchCompanyMemberEmails(ctx context.Context, profileID string) ([]memberNotifyTarget, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT cm.user_id, u.email FROM company_members cm
		JOIN users u ON u.id = cm.user_id
		WHERE cm.company_profile_id = $1`, profileID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []memberNotifyTarget
	for rows.Next() {
		var m memberNotifyTarget
		if err := rows.Scan(&m.userID, &m.email); err != nil {
			continue
		}
		out = append(out, m)
	}
	return out, rows.Err()
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
// status changed and an assignee_email/assignee_phone is on file. Called as a
// fire-and-forget goroutine from company_pipeline.go's PATCH handler — never
// blocks the HTTP response, and uses context.Background() since the request
// context would already be cancelled by the time the goroutine runs. Either
// email or phone may be empty (담당자가 둘 중 하나만 등록했을 수 있음) — this
// event never checks users.email_notifications_enabled/sms_notifications_enabled
// because the recipient is an arbitrary assignee_email/assignee_phone, not
// necessarily a system user; the pipeline owner filling in that contact info
// is itself the opt-in.
func (s *Server) notifyAssigneeStatusChange(ctx context.Context, email, phone, pipelineEntryID, noticeID, noticeTitle, newStatus string) {
	if email != "" {
		subject := fmt.Sprintf("[상태변경] %s", noticeTitle)
		body := fmt.Sprintf(
			"<p><b>%s</b>의 참여 상태가 <b>%s</b>(으)로 변경되었습니다.</p>",
			html.EscapeString(noticeTitle), html.EscapeString(newStatus),
		)
		s.sendNotificationEmail(ctx, notifyEventAssigneeStatusChange, email, nil, &pipelineEntryID, &noticeID, subject, body)
	}
	if phone != "" {
		msg := fmt.Sprintf("[상태변경] %s %s(으)로 변경", truncateForSMS(noticeTitle, 25), newStatus)
		s.sendNotificationSMS(ctx, notifyEventAssigneeStatusChange, phone, nil, &pipelineEntryID, &noticeID, msg)
	}
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

// handleUpdateNotificationSettings toggles the organization's email/SMS
// notification preferences — 팀기능 스펙("알림설정은 조직 단위로 공유")에
// 따라 users가 아니라 company_profiles를 갱신하고, owner만 바꿀 수 있다
// (member는 "파이프라인 조회+참여만"). "이메일 알림 전체 on/off"에 이어
// SMS on/off + 전화번호를 같은 엔드포인트에서 부분 업데이트로 처리한다
// (company_pipeline.go의 PATCH 핸들러와 같은 "raw map + present 체크"
// 패턴 — 프론트가 필드 일부만 보내도 나머지 설정을 건드리지 않음).
// Deadline reminders and the recommendation digest both check
// email_notifications_enabled/sms_notifications_enabled; the
// assignee-status-change event checks neither (its recipient is an
// arbitrary assignee_email/assignee_phone, not necessarily a system user).
func (s *Server) handleUpdateNotificationSettings(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.currentUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	profile, err := s.getCompanyProfile(r, userID)
	if err != nil {
		s.logger.Error("notification-settings: profile lookup failed", "error", err)
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

	var raw map[string]json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_body"})
		return
	}

	sets := []string{}
	args := []any{}
	addSet := func(column string, value any) {
		args = append(args, value)
		sets = append(sets, fmt.Sprintf("%s = $%d", column, len(args)))
	}
	resp := map[string]any{}

	if rawVal, present := raw["emailNotificationsEnabled"]; present {
		var enabled bool
		if err := json.Unmarshal(rawVal, &enabled); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_body"})
			return
		}
		addSet("email_notifications_enabled", enabled)
		resp["emailNotificationsEnabled"] = enabled
	}
	if rawVal, present := raw["smsNotificationsEnabled"]; present {
		var enabled bool
		if err := json.Unmarshal(rawVal, &enabled); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_body"})
			return
		}
		addSet("sms_notifications_enabled", enabled)
		resp["smsNotificationsEnabled"] = enabled
	}
	if rawVal, present := raw["phoneNumber"]; present {
		var phone string
		if err := json.Unmarshal(rawVal, &phone); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_body"})
			return
		}
		phone = strings.TrimSpace(phone)
		if phone == "" {
			addSet("phone_number", nil)
			resp["phoneNumber"] = nil
		} else {
			addSet("phone_number", phone)
			resp["phoneNumber"] = phone
		}
	}
	if len(sets) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no_fields_to_update"})
		return
	}

	args = append(args, profile.ID)
	query := "UPDATE company_profiles SET " + strings.Join(sets, ", ") + fmt.Sprintf(" WHERE id = $%d", len(args))
	if _, err := s.db.ExecContext(r.Context(), query, args...); err != nil {
		s.logger.Error("notification-settings: update failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}

	writeJSON(w, http.StatusOK, resp)
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
		INSERT INTO notification_log (event_type, channel, recipient_email, user_id, pipeline_entry_id, notice_id, subject, status, error_message)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		eventType, notifyChannelEmail, recipientEmail, userID, pipelineEntryID, noticeID, subject, status, errMsg,
	); logErr != nil {
		s.logger.Error("notify: log insert failed", "error", logErr)
	}
}

// sendNotificationSMS is sendNotificationEmail's SMS counterpart — same
// send+log pattern, channel='sms', recipient_phone instead of recipient_email.
// notification_log.subject엔 SMS 제목 개념이 없어 실제 발송한 메시지 본문을
// 그대로 담는다 — 이메일 쪽도 본문 전체(HTML)는 로그에 남기지 않고 subject만
// 남기는 것과 같은 수준의 감사 기록(전체 본문 아카이브가 목적이 아니라
// 발송 성공/실패 이력 추적이 목적)이라 SMS는 메시지 자체가 그 역할을 한다.
// s.smsNotify가 nil이 아니기만 하면(키 미설정이어도) Send를 그대로 호출해
// "not configured" 에러가 status='failed'로 로그에 남는다 — 실제 발송키
// 없이도 발송대상 조회 로직이 끝까지 도는지 검증할 수 있는 이유(이메일과
// 동일한 관례).
func (s *Server) sendNotificationSMS(ctx context.Context, eventType, recipientPhone string, userID, pipelineEntryID, noticeID *string, msg string) {
	if s.smsNotify == nil {
		return
	}
	err := s.smsNotify.Send(ctx, recipientPhone, msg)
	status, errMsg := "sent", sql.NullString{}
	if err != nil {
		status = "failed"
		errMsg = sql.NullString{String: err.Error(), Valid: true}
		s.logger.Error("notify: sms send failed", "eventType", eventType, "recipient", recipientPhone, "error", err)
	}
	if _, logErr := s.db.ExecContext(ctx, `
		INSERT INTO notification_log (event_type, channel, recipient_phone, user_id, pipeline_entry_id, notice_id, subject, status, error_message)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		eventType, notifyChannelSMS, recipientPhone, userID, pipelineEntryID, noticeID, msg, status, errMsg,
	); logErr != nil {
		s.logger.Error("notify: sms log insert failed", "error", logErr)
	}
}

// truncateForSMS shortens a title so SMS messages stay within Aligo's SMS
// byte budget (~90바이트, 한글 기준 약 40~45자 — 자세한 계산 대신 넉넉한
// 안전 마진으로 rune 개수를 제한). rune 단위로 잘라야 멀티바이트 한글
// 문자가 중간에 깨지지 않는다(byte 슬라이싱 금지).
func truncateForSMS(s string, maxRunes int) string {
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return string(runes[:maxRunes]) + "…"
}
