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
	"strconv"
	"strings"
	"time"

	"github.com/lib/pq"

	"biz-platform/collector/internal/billing"
	"biz-platform/collector/internal/collector/changedetect"
)

const (
	notifyEventDeadlineD7           = "deadline_d7"
	notifyEventDeadlineD3           = "deadline_d3"
	notifyEventDeadlineD1           = "deadline_d1"
	notifyEventRecommendationDigest = "recommendation_digest"
	notifyEventAssigneeStatusChange = "assignee_status_change"
	notifyEventNoticeCorrected      = "notice_corrected"

	notifyChannelEmail = "email"
	notifyChannelSMS   = "sms"
)

// pipelineActiveForNotification: 종결된 건(제출완료/낙찰/탈락/보류/제외)은
// 마감이 임박해도 더 이상 챙길 필요가 없어 알림 대상에서 제외한다 —
// dashboard.go의 pipelineActiveStatuses와 동일한 판단.
var pipelineActiveForNotification = pipelineActiveStatuses

// smsAllowedForPlan — Phase 6: SMS는 유료 플랜 전용(Free는 이메일+웹푸시만).
// 담당자 알림 설정 UI는 그대로 두고(company_contacts.smsEnabled 토글 자체는
// 안 막음) 발송 직전에만 거른다 — Free 사용자가 SMS 토글을 켜놨거나
// 다운그레이드 후에도 남아있어도 실제 비용(Aligo 건당 과금)이 발생하지
// 않게 하는 안전장치. 조회 실패 시에도 안전하게(비용 발생 방지 우선) 막는다.
func (s *Server) smsAllowedForPlan(ctx context.Context, profileID string) bool {
	plan, err := s.effectivePlan(ctx, profileID)
	if err != nil {
		s.logger.Error("notify: effective plan lookup failed for SMS gate", "error", err)
		return false
	}
	return plan != billing.PlanFree
}

// notificationEmailEventTypes — Free 플랜 월간 이메일 한도에 실제로
// 포함되는 이벤트 종류(요구사항 확정: "알림성 이메일만"). 팀 초대
// (team_invite/team_invite_accepted) 같은 필수/운영성 이메일은 이 목록에
// 넣지 않는다 — 그 발송 경로들은 애초에 checkEmailNotificationQuota를
// 거치지 않으므로(company_team.go 참고) 한도와 무관하게 항상 나간다.
var notificationEmailEventTypes = []string{
	notifyEventDeadlineD7, notifyEventDeadlineD3, notifyEventDeadlineD1,
	notifyEventAssigneeStatusChange, notifyEventRecommendationDigest,
	notifyEventWeeklyReport, notifyEventMonthlyReport,
}

// checkEmailNotificationQuota — Free 플랜 월간 알림성 이메일 한도(기본
// 20건, 관리자가 system_settings.free_plan_email_limit로 조절 가능 —
// system_settings.go 참고). 유료 플랜은 무제한. 한도 조회/집계 실패
// 시에는 막지 않는다(fail open) — SMS 게이트(smsAllowedForPlan)와 반대
// 판단인 이유: SMS는 건당 실비용이 있어 실수로 더 보내면 손해지만,
// 이메일은 한도 오판으로 "덜 보내는" 쪽이 사용자 경험을 더 해친다.
func (s *Server) checkEmailNotificationQuota(ctx context.Context, profileID string) bool {
	plan, err := s.effectivePlan(ctx, profileID)
	if err != nil {
		s.logger.Error("notify: effective plan lookup failed for email quota gate", "error", err)
		return true
	}
	if plan != billing.PlanFree {
		return true
	}
	limit, err := s.getSystemSettingInt(ctx, freePlanEmailLimitSettingKey, defaultFreePlanEmailLimit)
	if err != nil {
		s.logger.Error("notify: email limit setting lookup failed", "error", err)
		return true
	}
	if limit < 0 {
		return true // 관리자가 음수를 넣으면 무제한으로 취급(확장 여지)
	}
	count, err := s.countNotificationEmailsThisMonth(ctx, profileID)
	if err != nil {
		s.logger.Error("notify: email quota count query failed", "error", err)
		return true
	}
	return count < limit
}

// countNotificationEmailsThisMonth counts this calendar month's successfully
// sent notification-emails for one org — notification_log엔 company_profile_id가
// 직접 없어(담당자 단위 이벤트는 contact_id, 회원 단위 이벤트는 user_id로만
// 남음, notification_log 테이블 주석 참고) 양쪽 경로를 LEFT JOIN으로 모두
// 확인한다. checkAIAnalysisQuota(billing.go)와 동일하게 "이번 달 1일부터"
// 롤링 집계라 별도 리셋 배치가 필요 없다 — 매월 1일이 지나면 자연히 0부터
// 다시 센다.
// countNotificationEmailsThisMonth — recommendation_digest는 다이제스트
// 이메일 1통에 매칭된 공고마다 notification_log 행을 하나씩 남긴다
// (fetchDigestedNoticeIDs가 "그 공고를 이미 다이제스트했는지"를 notice_id
// 단위로 추적해야 해서 — 이 로깅 구조 자체는 바꾸지 않는다). 그래서 단순히
// count(*)를 하면 다이제스트 한 통을 여러 통으로 잘못 세게 된다 —
// recommendation_digest만 (수신자, 제목, 날짜) 기준으로 묶어서 "실제
// 보낸 이메일 수"로 정규화하고, 나머지 이벤트(행 1개 = 이메일 1통이 이미
// 성립)는 그대로 센다.
func (s *Server) countNotificationEmailsThisMonth(ctx context.Context, profileID string) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `
		SELECT count(DISTINCT
			CASE WHEN nl.event_type = $1
				THEN nl.recipient_email || '|' || nl.subject || '|' || nl.created_at::date::text
				ELSE nl.id::text
			END
		)
		FROM notification_log nl
		LEFT JOIN company_contacts cc ON cc.id = nl.contact_id
		LEFT JOIN company_members cm ON cm.user_id = nl.user_id
		WHERE nl.channel = 'email' AND nl.status = 'sent'
		  AND nl.created_at >= date_trunc('month', now())
		  AND nl.event_type = ANY($2)
		  AND (cc.company_profile_id = $3 OR cm.company_profile_id = $3)`,
		notifyEventRecommendationDigest, pq.Array(notificationEmailEventTypes), profileID,
	).Scan(&count)
	return count, err
}

// logSkippedEmailNotification records that an email was intentionally
// skipped (not a failure) so the admin dashboard can show "이번달 한도
// 초과로 스킵된 발송 건수"(admin.go의 handleAdminDashboard 참고) — 아무
// 흔적도 안 남기면 운영자가 "왜 이 회사만 이메일이 안 갔지"를 알 방법이
// 없다.
func (s *Server) logSkippedEmailNotification(ctx context.Context, eventType, recipientEmail string, userID, contactID, pipelineEntryID, noticeID *string, subject string) {
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO notification_log (event_type, channel, recipient_email, user_id, contact_id, pipeline_entry_id, notice_id, subject, status)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,'skipped_quota')`,
		eventType, notifyChannelEmail, recipientEmail, userID, contactID, pipelineEntryID, noticeID, subject,
	); err != nil {
		s.logger.Error("notify: skipped-quota log insert failed", "error", err)
	}
}

// RunDailyNotifications is the entry point cmd/apiserver calls on a daily
// ticker (see notify.NextDailyRun). Each sub-batch logs its own errors and
// keeps going — one failing batch shouldn't block the others.
func (s *Server) RunDailyNotifications(ctx context.Context) {
	emailReady := s.notify != nil && s.notify.Configured()
	smsReady := s.smsNotify != nil && s.smsNotify.Configured()
	if !emailReady && !smsReady {
		s.logger.Warn("notify: RESEND_API_KEY/ALIGO_API_KEY 모두 설정되지 않아 이메일/SMS는 건너뜁니다(인앱 알림함은 그대로 채워짐)")
	}
	// 이메일/SMS가 둘 다 꺼져 있어도 마감 리마인더/상태변경 배치는 계속
	// 돈다 — 인앱 알림함(Phase 5)은 발송 채널과 무관하게 항상 채워야
	// 하고, sendNotificationEmail/SMS는 클라이언트가 nil이면 각자 알아서
	// 조용히 스킵한다(기존 관례).
	if err := s.sendDeadlineReminders(ctx, 7, notifyEventDeadlineD7); err != nil {
		s.logger.Error("notify: D-7 reminder batch failed", "error", err)
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

// sendDeadlineReminders notifies every pipeline entry (not yet closed out)
// whose submission_deadline is exactly offsetDays from today AND whose org
// opted into that offset (company_profiles.notification_days_before —
// "제출마감 알림 시점" 다중선택). 수신자는 이제 조직(company_members/
// company_profiles) 단위가 아니라 담당자(company_contacts) 단위다 — 이
// 파이프라인의 회사에 등록된 담당자 중 채널별 토글이 켜진 사람 전부에게
// 각자 보낸다(담당자별 개별 설정 재설계, notifications.go 상단 참고).
// 이메일/SMS는 서로 독립적으로 중복발송 여부를 판단한다(contact_id +
// channel로 dedup).
func (s *Server) sendDeadlineReminders(ctx context.Context, offsetDays int, eventType string) error {
	rows, err := s.db.QueryContext(ctx, `
		SELECT pe.id, pe.notice_id, n.title, n.organization_name, pe.status, pe.company_profile_id
		FROM notice_pipeline_entries pe
		JOIN company_profiles cp ON cp.id = pe.company_profile_id
		JOIN notices n ON n.id = pe.notice_id
		WHERE pe.submission_deadline = CURRENT_DATE + ($1 * INTERVAL '1 day')
		  AND $1 = ANY(cp.notification_days_before)`, offsetDays)
	if err != nil {
		return err
	}

	type row struct {
		entryID, noticeID, title, status, profileID string
		org                                         sql.NullString
	}
	var targets []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.entryID, &r.noticeID, &r.title, &r.org, &r.status, &r.profileID); err != nil {
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

		// 인앱 알림함은 이메일/SMS 채널 토글과 무관하게 항상 남는다 — 로그인
		// 자체가 "받아볼 채널"이라 담당자별 email/sms 설정을 따로 타지 않는다.
		inAppTitle := fmt.Sprintf("제출마감 D-%d · %s", offsetDays, t.title)
		inAppBody := fmt.Sprintf("발주기관: %s", t.org.String)
		if err := s.insertEntryScopedInAppNotification(ctx, t.profileID, eventType, entryID, noticeID, inAppTitle, inAppBody); err != nil {
			s.logger.Error("notify: in-app notification insert failed", "error", err)
		}
		// Phase 6: 웹 푸시도 인앱 알림함과 동일한 대상(조직 전체)에 나란히
		// 보낸다 — VAPID 미설정이면 sendPushToProfileMembers가 조용히 스킵.
		s.sendPushToProfileMembers(ctx, t.profileID, inAppTitle, inAppBody, "/#/pipeline/"+entryID)

		contacts, err := s.fetchNotifiableContacts(ctx, t.profileID, eventType, entryID)
		if err != nil {
			s.logger.Error("notify: contact lookup failed", "error", err)
			continue
		}
		// 대상 전부가 같은 회사(t.profileID)라 타깃당 한 번만 조회 — 담당자마다
		// 반복 조회하지 않는다.
		smsAllowed := s.smsAllowedForPlan(ctx, t.profileID)
		emailAllowed := s.checkEmailNotificationQuota(ctx, t.profileID)
		for _, c := range contacts {
			contactID := c.id
			if c.emailEnabled && c.email != "" && !c.emailAlreadySent {
				subject := fmt.Sprintf("[제출마감 D-%d] %s", offsetDays, t.title)
				if emailAllowed {
					body := fmt.Sprintf(
						"<p>제출마감이 D-%d 남은 참여 건이 있습니다.</p><p><b>%s</b></p><p>발주기관: %s</p>",
						offsetDays, html.EscapeString(t.title), html.EscapeString(t.org.String),
					)
					s.sendNotificationEmail(ctx, eventType, c.email, nil, &contactID, &entryID, &noticeID, subject, body)
				} else {
					s.logSkippedEmailNotification(ctx, eventType, c.email, nil, &contactID, &entryID, &noticeID, subject)
				}
			}
			if smsAllowed && c.smsEnabled && c.phone != "" && !c.smsAlreadySent {
				msg := fmt.Sprintf("[제출마감 D-%d] %s 제출마감이 D-%d일 남았습니다.", offsetDays, truncateForSMS(t.title, 25), offsetDays)
				s.sendNotificationSMS(ctx, eventType, c.phone, nil, &contactID, &entryID, &noticeID, msg)
			}
		}
	}
	return nil
}

type memberNotifyTarget struct {
	userID, email string
	alreadySent   bool
}

// contactNotifyTarget — company_contacts 한 명 + 이 (event_type,
// pipeline_entry_id) 조합에 대해 채널별로 이미 발송했는지. 마감 리마인더/
// 담당자 상태변경 알림 둘 다 이 헬퍼를 공유한다.
type contactNotifyTarget struct {
	id, name, email, phone           string
	emailEnabled, smsEnabled         bool
	emailAlreadySent, smsAlreadySent bool
}

// fetchNotifiableContacts lists every contact of an org along with whether
// *that specific contact* already has a 'sent' row (per channel) for this
// exact (event_type, pipeline_entry_id) — email/sms dedup은 contact_id
// 기준으로 서로 독립적이다(한쪽 채널만 먼저 성공해도 다른 채널이 막히면
// 안 됨, 기존 이메일/SMS 독립 dedup 원칙과 동일). 두 채널 다 꺼둔 담당자는
// 결과에서 제외한다(이메일/전화번호가 비어 있으면 어차피 호출부에서
// 걸러진다).
func (s *Server) fetchNotifiableContacts(ctx context.Context, profileID, eventType, pipelineEntryID string) ([]contactNotifyTarget, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT cc.id, cc.name, cc.email, cc.phone, cc.email_notifications_enabled, cc.sms_notifications_enabled,
		       EXISTS (
		           SELECT 1 FROM notification_log nl
		           WHERE nl.event_type = $2 AND nl.pipeline_entry_id = $3
		             AND nl.channel = 'email' AND nl.status = 'sent' AND nl.contact_id = cc.id
		       ) AS email_already_sent,
		       EXISTS (
		           SELECT 1 FROM notification_log nl
		           WHERE nl.event_type = $2 AND nl.pipeline_entry_id = $3
		             AND nl.channel = 'sms' AND nl.status = 'sent' AND nl.contact_id = cc.id
		       ) AS sms_already_sent
		FROM company_contacts cc
		WHERE cc.company_profile_id = $1
		  AND (cc.email_notifications_enabled = true OR cc.sms_notifications_enabled = true)`,
		profileID, eventType, pipelineEntryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []contactNotifyTarget
	for rows.Next() {
		var c contactNotifyTarget
		var email, phone sql.NullString
		if err := rows.Scan(&c.id, &c.name, &email, &phone, &c.emailEnabled, &c.smsEnabled,
			&c.emailAlreadySent, &c.smsAlreadySent); err != nil {
			continue
		}
		c.email = email.String
		c.phone = phone.String
		out = append(out, c)
	}
	return out, rows.Err()
}

// digestNoticeRow — 다이제스트 후보 공고 한 건. sendRecommendationDigest의
// 스캔 쿼리 스캔 대상이자 digestNoticeItemHTML의 인자 타입이라(패키지
// 레벨 함수 파라미터로 쓰이려면 지역 타입일 수 없어) 패키지 스코프로 둔다.
type digestNoticeRow struct {
	id, noticeType, title string
	org, region, industry sql.NullString
	budget                sql.NullInt64
	deadline              sql.NullTime
}

// digestOrDash returns "-" for an unset nullable string — 다이제스트
// 이메일 본문에서 발주기관/업종처럼 값이 없을 수 있는 항목을 빈칸 대신
// 일관되게 표시하기 위함(company_info 푸터의 "값 없으면 줄 자체를
// 숨김" 원칙과는 다르다 — 여긴 공고 목록의 한 항목이라 줄을 통째로
// 없애면 표 형태가 깨지므로 "-"로 표시만 한다).
func digestOrDash(v sql.NullString) string {
	if !v.Valid || strings.TrimSpace(v.String) == "" {
		return "-"
	}
	return html.EscapeString(v.String)
}

// formatDigestDeadline renders a deadline as "2026-08-20 (D-15)" — 오늘
// 이후면 D-N, 오늘이면 "D-day", 이미 지났으면(이론상 sendRecommendationDigest의
// 쿼리가 이미 마감 지난 공고를 걸러내지만 방어적으로) "마감"으로 표시한다.
// 날짜만 비교해야 하므로(시:분:초 영향 배제) 둘 다 UTC 자정 기준으로 자른다.
func formatDigestDeadline(deadline time.Time) string {
	today := time.Now().UTC().Truncate(24 * time.Hour)
	day := deadline.UTC().Truncate(24 * time.Hour)
	diff := int(day.Sub(today).Hours() / 24)
	dateStr := deadline.Format("2006-01-02")
	switch {
	case diff > 0:
		return fmt.Sprintf("%s (D-%d)", dateStr, diff)
	case diff == 0:
		return fmt.Sprintf("%s (D-day)", dateStr)
	default:
		return fmt.Sprintf("%s (마감)", dateStr)
	}
}

// formatWonAmount adds thousands separators for Korean won display(예:
// 1234567 → "1,234,567") — 백엔드에 기존 금액 포맷 헬퍼가 전혀 없다(전부
// 프론트 toLocaleString 의존, 이메일은 서버에서 완성된 HTML을 보내야 해서
// 새로 만든다). 음수는 이 도메인(공고 예산)에 나타나지 않아 별도 처리 없음.
func formatWonAmount(n int64) string {
	s := strconv.FormatInt(n, 10)
	if len(s) <= 3 {
		return s
	}
	var parts []string
	for len(s) > 3 {
		parts = append([]string{s[len(s)-3:]}, parts...)
		s = s[:len(s)-3]
	}
	parts = append([]string{s}, parts...)
	return strings.Join(parts, ",")
}

// digestNoticeItemHTML renders one notice block in the digest email:
// 공고명(제목=상세 딥링크)/발주기관/업종/마감일(D-day)/예산. appBaseURL은
// s.appBaseURL 그대로 — ⚠️나중에 네이티브 앱이 나오면 이 링크를
// Universal Link(iOS)/App Link(Android)로 전환해야 한다(sendRecommendationDigest의
// dashboardLink 주석과 동일한 이유).
func digestNoticeItemHTML(appBaseURL string, n digestNoticeRow) string {
	link := appBaseURL + "/#/notices/" + n.id
	budget := "-"
	if n.budget.Valid {
		budget = formatWonAmount(n.budget.Int64) + "원"
	}
	deadline := "-"
	if n.deadline.Valid {
		deadline = formatDigestDeadline(n.deadline.Time)
	}
	return fmt.Sprintf(`
		<div style="border:1px solid #e5e8eb;border-radius:8px;padding:16px;margin-bottom:12px;">
			<p style="margin:0 0 8px;"><a href="%s" style="color:#191f28;font-size:16px;font-weight:700;text-decoration:none;">%s</a></p>
			<p style="margin:0 0 4px;color:#4e5968;font-size:14px;">발주기관: %s</p>
			<p style="margin:0 0 4px;color:#4e5968;font-size:14px;">업종: %s</p>
			<p style="margin:0 0 4px;color:#4e5968;font-size:14px;">마감일: %s</p>
			<p style="margin:0;color:#4e5968;font-size:14px;">예산: %s</p>
		</div>`,
		html.EscapeString(link), html.EscapeString(n.title), digestOrDash(n.org), digestOrDash(n.industry), html.EscapeString(deadline), budget,
	)
}

// digestFooterHTML builds the digest email's footer from company_info(운영사
// 정보 싱글턴) — renderLandingCompanyInfo(index.html, 랜딩페이지 푸터)가
// 쓰는 "값 없으면 그 줄만 숨김" 관례를 서버 사이드(이메일 HTML)에서
// 그대로 재현한다. 아무 필드도 없으면 빈 문자열을 반환해 <div> 자체가
// 안 보이게 한다.
func digestFooterHTML(info companyInfo) string {
	var lines []string
	if strings.TrimSpace(info.BrandName) != "" {
		lines = append(lines, fmt.Sprintf(`<p style="margin:0 0 4px;font-weight:700;">%s</p>`, html.EscapeString(info.BrandName)))
	}
	if info.ContactEmail != nil && strings.TrimSpace(*info.ContactEmail) != "" {
		lines = append(lines, fmt.Sprintf(`<p style="margin:0 0 4px;">%s</p>`, html.EscapeString(*info.ContactEmail)))
	}
	var bizParts []string
	if info.CompanyName != nil && strings.TrimSpace(*info.CompanyName) != "" {
		bizParts = append(bizParts, "상호: "+html.EscapeString(*info.CompanyName))
	}
	if info.BusinessRegistrationNumber != nil && strings.TrimSpace(*info.BusinessRegistrationNumber) != "" {
		bizParts = append(bizParts, "사업자 등록번호: "+html.EscapeString(*info.BusinessRegistrationNumber))
	}
	if info.RepresentativeName != nil && strings.TrimSpace(*info.RepresentativeName) != "" {
		bizParts = append(bizParts, "대표: "+html.EscapeString(*info.RepresentativeName))
	}
	if len(bizParts) > 0 {
		lines = append(lines, fmt.Sprintf(`<p style="margin:0 0 4px;">%s</p>`, strings.Join(bizParts, " | ")))
	}
	if info.MailOrderRegistrationNumber != nil && strings.TrimSpace(*info.MailOrderRegistrationNumber) != "" {
		lines = append(lines, fmt.Sprintf(`<p style="margin:0 0 4px;">통신판매업 신고번호: %s</p>`, html.EscapeString(*info.MailOrderRegistrationNumber)))
	}
	if info.Address != nil && strings.TrimSpace(*info.Address) != "" {
		lines = append(lines, fmt.Sprintf(`<p style="margin:0;">%s</p>`, html.EscapeString(*info.Address)))
	}
	if len(lines) == 0 {
		return ""
	}
	return fmt.Sprintf(`<div style="margin-top:32px;padding-top:16px;border-top:1px solid #e5e8eb;color:#8b95a1;font-size:12px;">%s</div>`, strings.Join(lines, ""))
}

// sendRecommendationDigest sends, per organization with notifications
// enabled, one email per member listing today's newly-recommended notices
// (grade == gradeRecommended) that aren't already in that org's pipeline.
// 후보 공고 계산(지역/업종/규모 매칭)은 조직 단위로 한 번만 하고 — 이 셋은
// company_profiles의 조직 속성이라 멤버마다 다르지 않다 — "이미 다이제스트
// 받은 적 있는지"만 멤버별로 따로 걸러서 각자에게 보낸다(한 멤버가 이미
// 본 공고를 다른 신규 멤버는 여전히 처음 보는 것일 수 있으므로).
func (s *Server) sendRecommendationDigest(ctx context.Context) error {
	// company_info(운영사 정보, 싱글턴)는 발송 대상과 무관하게 항상 같은
	// 값이라 루프 밖에서 한 번만 조회해 이메일 하단 푸터 HTML을 미리
	// 만들어둔다. 조회 자체가 실패해도 다이제스트 발송을 막을 이유는
	// 없으니(운영사 정보 없이도 공고 알림 기능은 정상 동작해야 함)
	// 그 경우엔 빈 companyInfo로 계속 진행 — digestFooterHTML이 값이 없는
	// 필드는 알아서 줄 자체를 생략한다.
	companyInfoData, err := s.fetchCompanyInfo(ctx)
	if err != nil {
		s.logger.Error("notify: company info lookup failed for digest footer", "error", err)
	}
	footerHTML := digestFooterHTML(companyInfoData)

	profileRows, err := s.db.QueryContext(ctx, `
		SELECT cp.id, cp.company_name, cp.region, cp.industry, cp.company_size
		FROM company_profiles cp
		WHERE cp.email_notifications_enabled = true`)
	if err != nil {
		return err
	}
	type profileRow struct {
		profileID    string
		companyName  sql.NullString
		region, size sql.NullString
		industry     pq.StringArray
	}
	var profiles []profileRow
	for profileRows.Next() {
		var p profileRow
		if err := profileRows.Scan(&p.profileID, &p.companyName, &p.region, &p.industry, &p.size); err != nil {
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
		SELECT id, notice_type, title, organization_name, region, industry, budget_amount, application_end_at
		FROM notices
		WHERE status NOT IN ('closed','cancelled')
		  AND (application_end_at IS NULL OR application_end_at >= CURRENT_DATE)
		LIMIT `+itoa(dashboardNoticeScanLimit))
	if err != nil {
		return err
	}
	var notices []digestNoticeRow
	for noticeRows.Next() {
		var n digestNoticeRow
		if err := noticeRows.Scan(&n.id, &n.noticeType, &n.title, &n.org, &n.region, &n.industry, &n.budget, &n.deadline); err != nil {
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
		var orgRecommended []digestNoticeRow
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
			var matched []digestNoticeRow
			for _, n := range orgRecommended {
				if !digestedIDs[n.id] {
					matched = append(matched, n)
				}
			}
			if len(matched) == 0 {
				continue
			}

			// 제목: "[공고 공유] {대표 공고명}"(+2건 이상이면 "외 N건") —
			// notice_share.go(담당자에게 전달 버튼)가 이미 쓰는 "[공고 공유]"
			// 접두사와 동일한 관례를 다이제스트에도 맞춘다.
			subject := fmt.Sprintf("[공고 공유] %s", matched[0].title)
			if len(matched) > 1 {
				subject += fmt.Sprintf(" 외 %d건", len(matched)-1)
			}
			var itemsHTML string
			titlesForInApp := make([]string, 0, len(matched))
			for _, n := range matched {
				itemsHTML += digestNoticeItemHTML(s.appBaseURL, n)
				titlesForInApp = append(titlesForInApp, n.title)
			}

			greeting := "회원님"
			if p.companyName.Valid && strings.TrimSpace(p.companyName.String) != "" {
				greeting = html.EscapeString(p.companyName.String) + "의 회원님"
			}
			// dashboardLink — 지금은 순수 웹 URL이다. ⚠️나중에 네이티브 앱이
			// 나오면 이 링크(그리고 아래 digestNoticeItemHTML의 공고별
			// 링크)를 Universal Link(iOS)/App Link(Android) 방식으로 바꿔서,
			// 앱이 설치돼있으면 자동으로 앱이 열리게 전환해야 한다 — 지금은
			// 앱 자체가 없어 이번 범위에서는 웹 URL만 발송한다. 2026-08-05:
			// "#/"가 이제 비로그인 전용 랜딩페이지로 고정돼 회원용 진입점을
			// "#/dashboard"로 명시했다(로그인 상태면 "#/"도 결국 리다이렉트
			// 되지만, 이메일 클라이언트의 링크 래핑을 거치며 해시가 씹히는
			// 사고를 줄이려 처음부터 최종 목적지를 지정).
			dashboardLink := s.appBaseURL + "/#/dashboard"
			body := fmt.Sprintf(`
				<p>안녕하세요. %s!</p>
				<p>회원님께 맞는 공고가 <b>%d건</b> 발생하였습니다.</p>
				<div style="margin:20px 0;">%s</div>
				<p style="color:#5b6472;font-size:13px;">검토 및 공고 사업의 상태와 캘린더를 조정해서 관리하시면 더욱 좋습니다.</p>
				<p style="text-align:center;margin:28px 0;">
					<a href="%s" style="display:inline-block;padding:12px 28px;background-color:#3182f6;color:#ffffff;text-decoration:none;border-radius:6px;font-weight:700;font-size:15px;">나의 공공사업 보러가기</a>
				</p>
				%s`,
				greeting, len(matched), itemsHTML, html.EscapeString(dashboardLink), footerHTML,
			)

			inAppBody := strings.Join(titlesForInApp, " · ")
			if len(titlesForInApp) > 3 {
				inAppBody = strings.Join(titlesForInApp[:3], " · ") + fmt.Sprintf(" 외 %d건", len(titlesForInApp)-3)
			}
			if err := s.insertDigestInAppNotification(ctx, m.userID, subject, inAppBody); err != nil {
				s.logger.Error("notify: digest in-app notification insert failed", "error", err)
			}
			// Phase 6: 다이제스트는 이미 회원 단위(m.userID)라 sendPushToUser를 직접 쓴다.
			s.sendPushToUser(ctx, m.userID, subject, inAppBody, "/#/notices")

			var status string
			var errMsg sql.NullString
			if s.checkEmailNotificationQuota(ctx, p.profileID) {
				sendErr := s.notify.Send(ctx, m.email, subject, body)
				status = "sent"
				if sendErr != nil {
					status = "failed"
					errMsg = sql.NullString{String: sendErr.Error(), Valid: true}
					s.logger.Error("notify: digest send failed", "recipient", m.email, "error", sendErr)
				}
			} else {
				status = "skipped_quota"
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
// status changed. Called as a fire-and-forget goroutine from
// company_pipeline.go's PATCH handler — never blocks the HTTP response, and
// uses context.Background() since the request context would already be
// cancelled by the time the goroutine runs. 수신자는 더 이상 파이프라인
// 엔트리 자체의 assignee_email/assignee_phone 한 명이 아니라, 이 회사에
// 등록된 담당자(company_contacts) 중 채널별 알림이 켜진 사람 전부다
// (담당자별 개별 설정 재설계) — fetchNotifiableContacts를 그대로 재사용한다.
func (s *Server) notifyAssigneeStatusChange(ctx context.Context, profileID, pipelineEntryID, noticeID, noticeTitle, oldStatus, newStatus string) {
	inAppTitle := fmt.Sprintf("상태변경: %s → %s", oldStatus, newStatus)
	if err := s.insertEntryScopedInAppNotification(ctx, profileID, notifyEventAssigneeStatusChange, pipelineEntryID, noticeID, inAppTitle, noticeTitle); err != nil {
		s.logger.Error("notify: status-change in-app notification insert failed", "error", err)
	}
	// Phase 6: 인앱 알림함과 동일한 대상(조직 전체)에 웹 푸시도 나란히 보낸다.
	s.sendPushToProfileMembers(ctx, profileID, inAppTitle, noticeTitle, "/#/pipeline/"+pipelineEntryID)

	contacts, err := s.fetchNotifiableContacts(ctx, profileID, notifyEventAssigneeStatusChange, pipelineEntryID)
	if err != nil {
		s.logger.Error("notify: assignee-status-change contact lookup failed", "error", err)
		return
	}
	smsAllowed := s.smsAllowedForPlan(ctx, profileID)
	emailAllowed := s.checkEmailNotificationQuota(ctx, profileID)
	for _, c := range contacts {
		contactID := c.id
		if c.emailEnabled && c.email != "" && !c.emailAlreadySent {
			subject := fmt.Sprintf("[상태변경] %s", noticeTitle)
			if emailAllowed {
				body := fmt.Sprintf(
					"<p><b>%s</b>의 참여 상태가 <b>%s</b>(으)로 변경되었습니다.</p>",
					html.EscapeString(noticeTitle), html.EscapeString(newStatus),
				)
				s.sendNotificationEmail(ctx, notifyEventAssigneeStatusChange, c.email, nil, &contactID, &pipelineEntryID, &noticeID, subject, body)
			} else {
				s.logSkippedEmailNotification(ctx, notifyEventAssigneeStatusChange, c.email, nil, &contactID, &pipelineEntryID, &noticeID, subject)
			}
		}
		if smsAllowed && c.smsEnabled && c.phone != "" && !c.smsAlreadySent {
			msg := fmt.Sprintf("[상태변경] %s %s(으)로 변경", truncateForSMS(noticeTitle, 25), newStatus)
			s.sendNotificationSMS(ctx, notifyEventAssigneeStatusChange, c.phone, nil, &contactID, &pipelineEntryID, &noticeID, msg)
		}
	}
}

// noticeChangeFieldLabelsKo — changedetect가 비교하는 필드명(DB 컬럼명
// 그대로)을 정정 알림 문구용 한글로 매핑. index.html의
// NOTICE_CHANGE_FIELD_LABELS와 개념은 같지만 백엔드/프론트가 서로 다른
// 런타임이라 별도로 유지한다 — 하나를 바꾸면 다른 쪽도 확인할 것.
var noticeChangeFieldLabelsKo = map[string]string{
	"title":                 "공고명",
	"organization_name":     "발주기관",
	"department_name":       "수요기관",
	"region":                "지역",
	"industry":              "업종",
	"status":                "공고상태",
	"application_start_at":  "입찰서 제출 시작",
	"application_end_at":    "투찰마감일",
	"budget_amount":         "예산",
	"support_amount":        "지원금액",
}

func noticeChangeFieldLabel(field string) string {
	if label, ok := noticeChangeFieldLabelsKo[field]; ok {
		return label
	}
	return field
}

// NotifyNoticeChanged — 2026-08-06, "정정된 관심공고" 즉시 알림. 수집
// 파이프라인(internal/collector/runner.Runner)이 changedetect로 실제
// 변경을 감지하고 저장까지 마친 직후 호출하는 콜백이다(runner.OnChangesRecorded,
// cmd/apiserver/main.go에서 연결) — runner 패키지는 api 패키지를 몰라도
// 되도록, api가 runner의 함수 타입 필드에 이 메서드를 꽂아 넣는 방향으로
// 의존한다. major_update(낙찰하한율/마감일/예산/상태처럼 changedetect가
// "중요"로 분류한 필드)일 때만 발송한다 — 제목 오타 수정 같은 minor 변경까지
// 매번 알리면 알림 피로만 커진다는 판단(사용자 확정, 즉시vs모아받기
// 선택지와 야간 quiet-hours는 이번 범위 밖 — P1로 남김).
func (s *Server) NotifyNoticeChanged(ctx context.Context, noticeID, changeType string, changes []changedetect.FieldChange) {
	if changeType != "major_update" || len(changes) == 0 {
		return
	}

	var noticeTitle string
	if err := s.db.QueryRowContext(ctx, `SELECT title FROM notices WHERE id = $1`, noticeID).Scan(&noticeTitle); err != nil {
		s.logger.Error("notify notice changed: title lookup failed", "notice_id", noticeID, "error", err)
		return
	}

	// 활성 파이프라인(검토전/참여검토/승인대기/준비중)에 이 공고를 담아둔
	// 조직에게만 보낸다 — dashboard.go의 pipelineActiveStatuses/activeStatusList와
	// 동일한 "종결된 건은 더 안 챙겨도 됨" 판단(notifyAssigneeStatusChange의
	// pipelineActiveForNotification 원칙과 같음).
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, company_profile_id FROM notice_pipeline_entries
		WHERE notice_id = $1 AND status = ANY($2)`,
		noticeID, pq.Array(activeStatusList()))
	if err != nil {
		s.logger.Error("notify notice changed: pipeline entry lookup failed", "notice_id", noticeID, "error", err)
		return
	}
	type target struct{ entryID, profileID string }
	var targets []target
	for rows.Next() {
		var t target
		if err := rows.Scan(&t.entryID, &t.profileID); err != nil {
			continue
		}
		targets = append(targets, t)
	}
	closeErr := rows.Err()
	rows.Close()
	if closeErr != nil {
		s.logger.Error("notify notice changed: pipeline entry scan failed", "error", closeErr)
	}
	if len(targets) == 0 {
		return
	}

	labels := make([]string, 0, len(changes))
	for _, c := range changes {
		labels = append(labels, noticeChangeFieldLabel(c.Field))
	}
	changedSummary := strings.Join(labels, ", ")

	for _, t := range targets {
		s.notifyNoticeCorrected(ctx, t.profileID, t.entryID, noticeID, noticeTitle, changedSummary)
	}
}

// notifyNoticeCorrected sends the notice_corrected event to one pipeline
// entry's org — notifyAssigneeStatusChange와 완전히 같은 채널 구성(인앱+
// 웹푸시+담당자 이메일/SMS, fetchNotifiableContacts 재사용)이라 그 함수를
// 그대로 본떴다.
func (s *Server) notifyNoticeCorrected(ctx context.Context, profileID, pipelineEntryID, noticeID, noticeTitle, changedSummary string) {
	inAppTitle := fmt.Sprintf("공고 정정: %s", noticeTitle)
	inAppBody := "변경된 항목: " + changedSummary
	if err := s.insertEntryScopedInAppNotification(ctx, profileID, notifyEventNoticeCorrected, pipelineEntryID, noticeID, inAppTitle, inAppBody); err != nil {
		s.logger.Error("notify: notice-corrected in-app notification insert failed", "error", err)
	}
	s.sendPushToProfileMembers(ctx, profileID, inAppTitle, inAppBody, "/#/pipeline/"+pipelineEntryID)

	contacts, err := s.fetchNotifiableContacts(ctx, profileID, notifyEventNoticeCorrected, pipelineEntryID)
	if err != nil {
		s.logger.Error("notify: notice-corrected contact lookup failed", "error", err)
		return
	}
	smsAllowed := s.smsAllowedForPlan(ctx, profileID)
	emailAllowed := s.checkEmailNotificationQuota(ctx, profileID)
	for _, c := range contacts {
		contactID := c.id
		if c.emailEnabled && c.email != "" && !c.emailAlreadySent {
			subject := fmt.Sprintf("[공고 정정] %s", noticeTitle)
			if emailAllowed {
				body := fmt.Sprintf(
					"<p>참여 검토 중인 <b>%s</b> 공고의 내용이 변경되었습니다.</p><p>변경된 항목: %s</p><p>원문을 다시 확인해주세요.</p>",
					html.EscapeString(noticeTitle), html.EscapeString(changedSummary),
				)
				s.sendNotificationEmail(ctx, notifyEventNoticeCorrected, c.email, nil, &contactID, &pipelineEntryID, &noticeID, subject, body)
			} else {
				s.logSkippedEmailNotification(ctx, notifyEventNoticeCorrected, c.email, nil, &contactID, &pipelineEntryID, &noticeID, subject)
			}
		}
		if smsAllowed && c.smsEnabled && c.phone != "" && !c.smsAlreadySent {
			msg := fmt.Sprintf("[공고 정정] %s 내용 변경(%s)", truncateForSMS(noticeTitle, 20), changedSummary)
			s.sendNotificationSMS(ctx, notifyEventNoticeCorrected, c.phone, nil, &contactID, &pipelineEntryID, &noticeID, msg)
		}
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

// notificationDaysBeforeAllowed — 실제로 배치가 도는 오프셋(D-7/D-3/D-1)
// 만 선택 가능하다(notifyEventDeadlineD7/D3/D1 3개뿐). 여기 없는 값을
// 넣어봤자 어떤 배치도 그 값을 체크하지 않아 조용히 무시되는 설정이 될
// 뿐이라, 애초에 저장을 막는다.
var notificationDaysBeforeAllowed = map[int]bool{7: true, 3: true, 1: true}

// handleUpdateNotificationSettings — 조직 단위 알림 설정. 마감 리마인더/
// 담당자 상태변경 알림의 수신자는 이제 company_contacts(담당자 개별
// 설정)로 옮겨갔으므로(notifications.go 상단 주석 참고) 여기서는 더 이상
// phoneNumber/smsNotificationsEnabled를 받지 않는다 — company_profiles의
// 해당 컬럼은 과거 값 보존을 위해 남아있을 뿐 어떤 발송 로직도 더는
// 읽지 않는다. emailNotificationsEnabled는 추천 공고 다이제스트 전용으로
// 남는다. notificationDaysBefore가 신규 필드(제출마감 리마인더 D-N 선택).
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
	if rawVal, present := raw["notificationDaysBefore"]; present {
		var days []int
		if err := json.Unmarshal(rawVal, &days); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_body"})
			return
		}
		seen := map[int]bool{}
		for _, d := range days {
			if !notificationDaysBeforeAllowed[d] {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_notification_days_before"})
				return
			}
			seen[d] = true // 중복 입력 방지(체크박스 UI라 실제로는 안 생기지만 방어적으로)
		}
		deduped := make([]int, 0, len(seen))
		for d := range seen {
			deduped = append(deduped, d)
		}
		addSet("notification_days_before", pq.Array(deduped))
		resp["notificationDaysBefore"] = deduped
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
// notification_log rows (see sendRecommendationDigest). userID is set only
// for the digest's sibling call paths(현재는 없음 — 자리만 남겨둠 호환용이
// 아니라 그냥 "누가 보낸 채널이냐"의 두 갈래 중 하나); contactID is set for
// the two contact-based events(deadline reminders/assignee status change).
// 정확히 하나만 채워지는 게 정상이지만 강제하지는 않는다(추천 다이제스트가
// 나중에 이 헬퍼를 재사용하게 되면 그때는 userID만 채워질 수 있음).
func (s *Server) sendNotificationEmail(ctx context.Context, eventType, recipientEmail string, userID, contactID, pipelineEntryID, noticeID *string, subject, bodyHTML string) {
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
		INSERT INTO notification_log (event_type, channel, recipient_email, user_id, contact_id, pipeline_entry_id, notice_id, subject, status, error_message)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		eventType, notifyChannelEmail, recipientEmail, userID, contactID, pipelineEntryID, noticeID, subject, status, errMsg,
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
func (s *Server) sendNotificationSMS(ctx context.Context, eventType, recipientPhone string, userID, contactID, pipelineEntryID, noticeID *string, msg string) {
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
		INSERT INTO notification_log (event_type, channel, recipient_phone, user_id, contact_id, pipeline_entry_id, notice_id, subject, status, error_message)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		eventType, notifyChannelSMS, recipientPhone, userID, contactID, pipelineEntryID, noticeID, msg, status, errMsg,
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
