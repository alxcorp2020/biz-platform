// saved_search_digest.go — "맞춤공고"(saved_searches) 알림 배치. 매일
// 09:00 KST(startBackgroundNotifications, sendRecommendationDigest와 같은
// 시각)에 실행되어, alert_enabled=true AND is_active=true인 저장 조건마다
// 새로 매칭되는 공고를 찾아 사용자별로 이메일 한 통(+인앱+웹푸시)으로
// 묶어 보낸다. is_active=false(복제 직후 기본값, 사용자가 확인 전까지)인
// 조건은 alert_enabled/reminder_enabled 설정과 무관하게 완전히 제외된다.
// 조건별 매칭 로직은 server.go의 handleListNotices WHERE절 확장과 같은
// 모양이지만, 여긴 HTTP 왕복 없이 직접 쿼리한다(페이지네이션/북마크/
// 변경감지 배지처럼 이 배치에 필요 없는 것들을 안 실어도 되므로).
package api

import (
	"biz-platform/collector/internal/billing"
	"context"
	"database/sql"
	"fmt"
	"html"
	"strings"
	"time"

	"github.com/lib/pq"
)

type savedSearchDigestRow struct {
	id, userID, name                      string
	noticeType, region, industry, orgName sql.NullString
	budgetMin, budgetMax                  sql.NullInt64
	include, exclude, recipientContactIDs pq.StringArray
}

// savedSearchRecipient — 2026-08-06, 기업프로필 "담당자 관리"를 맞춤공고로
// 통합하며 도입. contactID가 채워지면 company_contacts 한 명(선택된
// 수신자), nil이면 검색 소유자 계정으로의 폴백(userID만 채워짐) — 담당자를
// 하나도 안 골랐거나 고른 담당자 전원이 두 채널 다 꺼둔 경우 "알림 공백"을
// 막기 위한 안전장치다(resolveSavedSearchRecipients 참고).
type savedSearchRecipient struct {
	contactID, userID        *string
	name, email, phone       string
	emailEnabled, smsEnabled bool
}

// resolveSavedSearchRecipients turns a saved search's recipient_contact_ids
// into actual notifiable targets. Falls back to the search owner's login
// email when the explicit selection resolves to zero notifiable contacts —
// either because nothing was selected, or every selected contact has both
// channels toggled off — so changing "who gets notified" never silently
// drops a company down to zero recipients.
func (s *Server) resolveSavedSearchRecipients(ctx context.Context, userID string, contactIDs []string) ([]savedSearchRecipient, error) {
	var out []savedSearchRecipient
	if len(contactIDs) > 0 {
		rows, err := s.db.QueryContext(ctx, `
			SELECT id, name, email, phone, email_notifications_enabled, sms_notifications_enabled
			FROM company_contacts
			WHERE id = ANY($1) AND (email_notifications_enabled = true OR sms_notifications_enabled = true)`,
			pq.Array(contactIDs))
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id, name string
			var email, phone sql.NullString
			var emailEnabled, smsEnabled bool
			if err := rows.Scan(&id, &name, &email, &phone, &emailEnabled, &smsEnabled); err != nil {
				continue
			}
			cid := id
			out = append(out, savedSearchRecipient{
				contactID: &cid, name: name, email: email.String, phone: phone.String,
				emailEnabled: emailEnabled, smsEnabled: smsEnabled,
			})
		}
		closeErr := rows.Err()
		rows.Close()
		if closeErr != nil {
			return nil, closeErr
		}
	}
	if len(out) > 0 {
		return out, nil
	}
	var email string
	if err := s.db.QueryRowContext(ctx, `SELECT email FROM users WHERE id = $1`, userID).Scan(&email); err != nil {
		return nil, err
	}
	uid := userID
	return []savedSearchRecipient{{userID: &uid, name: "회원님", email: email, emailEnabled: true}}, nil
}

// fetchSavedSearchAlreadySentNoticeIDs — 다이제스트/리마인더 공용. dedup은
// 검색 소유자(user_id)가 아니라 실제 이메일/SMS를 받는 수신자(담당자 또는
// 폴백 계정) 기준이어야 한다 — 담당자 A는 이미 받은 공고를 담당자 B는
// 처음 볼 수 있다.
func (s *Server) fetchSavedSearchAlreadySentNoticeIDs(ctx context.Context, eventType, channel string, rec savedSearchRecipient) (map[string]bool, error) {
	var rows *sql.Rows
	var err error
	if rec.contactID != nil {
		rows, err = s.db.QueryContext(ctx, `
			SELECT notice_id FROM notification_log
			WHERE event_type = $1 AND channel = $2 AND status = 'sent' AND notice_id IS NOT NULL AND contact_id = $3`,
			eventType, channel, *rec.contactID)
	} else {
		rows, err = s.db.QueryContext(ctx, `
			SELECT notice_id FROM notification_log
			WHERE event_type = $1 AND channel = $2 AND status = 'sent' AND notice_id IS NOT NULL AND user_id = $3`,
			eventType, channel, *rec.userID)
	}
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

// logSavedSearchNoticeEmail — sendSavedSearchDigest/sendSavedSearchDeadlineReminders
// 공용 로깅. 다이제스트 관례대로 이메일 1통에 매칭 공고마다 notification_log
// 행을 하나씩 남긴다(fetchSavedSearchAlreadySentNoticeIDs가 notice_id 단위로
// dedup해야 해서).
func (s *Server) logSavedSearchNoticeEmail(ctx context.Context, eventType string, rec savedSearchRecipient, noticeIDs []string, subject, status string, errMsg sql.NullString) {
	for _, noticeID := range noticeIDs {
		if _, err := s.db.ExecContext(ctx, `
			INSERT INTO notification_log (event_type, channel, recipient_email, user_id, contact_id, notice_id, subject, status, error_message)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
			eventType, notifyChannelEmail, rec.email, rec.userID, rec.contactID, noticeID, subject, status, errMsg,
		); err != nil {
			s.logger.Error("notify: saved-search notice email log insert failed", "error", err)
		}
	}
}

// matchNoticesForSavedSearch runs one saved_searches row's condition against
// the current open notices — 필터 조합은 handleListNotices의 쿼리파라미터
// 확장과 정확히 같은 컬럼/연산자를 쓴다(둘 다 같은 저장 조건 개념을
// 표현하는 것이라 매칭 결과가 서로 달라지면 안 됨).
func (s *Server) matchNoticesForSavedSearch(ctx context.Context, sr savedSearchDigestRow) ([]digestNoticeRow, error) {
	args := []any{}
	argN := 0
	addArg := func(v any) string {
		argN++
		args = append(args, v)
		return "$" + itoa(argN)
	}

	query := `
		SELECT id, notice_type, title, organization_name, region, industry, budget_amount, application_end_at
		FROM notices n
		WHERE status NOT IN ('closed','cancelled')
		  AND (application_end_at IS NULL OR application_end_at >= CURRENT_DATE)`
	if sr.noticeType.Valid && sr.noticeType.String != "" {
		query += " AND n.notice_type = " + addArg(sr.noticeType.String)
	}
	if sr.region.Valid && sr.region.String != "" {
		query += " AND n.region = " + addArg(sr.region.String)
	}
	if sr.industry.Valid && sr.industry.String != "" {
		query += " AND n.industry = " + addArg(sr.industry.String)
	}
	if sr.orgName.Valid && sr.orgName.String != "" {
		query += " AND n.organization_name ILIKE " + addArg("%"+sr.orgName.String+"%")
	}
	if sr.budgetMin.Valid {
		query += " AND n.budget_amount >= " + addArg(sr.budgetMin.Int64)
	}
	if sr.budgetMax.Valid {
		query += " AND n.budget_amount <= " + addArg(sr.budgetMax.Int64)
	}
	if len(sr.include) > 0 {
		var orParts []string
		for _, kw := range sr.include {
			orParts = append(orParts, "n.title ILIKE "+addArg("%"+kw+"%"))
		}
		query += " AND (" + strings.Join(orParts, " OR ") + ")"
	}
	for _, kw := range sr.exclude {
		query += " AND n.title NOT ILIKE " + addArg("%"+kw+"%")
	}
	query += " LIMIT " + itoa(dashboardNoticeScanLimit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []digestNoticeRow
	for rows.Next() {
		var n digestNoticeRow
		if err := rows.Scan(&n.id, &n.noticeType, &n.title, &n.org, &n.region, &n.industry, &n.budget, &n.deadline); err != nil {
			continue
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// fetchSavedSearchDigestedNoticeIDs — recommendation_digest의
// fetchDigestedNoticeIDs와 동일 패턴(status='sent'만, 실패한 시도는 다시
// 시도 가능하게 재조회 대상에 남긴다).
func (s *Server) fetchSavedSearchDigestedNoticeIDs(ctx context.Context, userID string) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT notice_id FROM notification_log
		WHERE event_type = $1 AND user_id = $2 AND status = 'sent' AND notice_id IS NOT NULL`,
		notifyEventSavedSearchMatch, userID)
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

type savedSearchMatchGroup struct {
	searchName string
	notices    []digestNoticeRow
}

// savedSearchDigestSectionsHTML renders one email's worth of match-group
// sections — sendSavedSearchDigest 재사용을 위해 분리(수신자마다 이미
// 받은 공고가 달라 groups를 수신자별로 다시 필터링해야 하므로, HTML
// 조립만 별도 함수로 뗀다).
func savedSearchDigestSectionsHTML(appBaseURL string, groups []savedSearchMatchGroup) string {
	var sectionsHTML string
	for _, g := range groups {
		var itemsHTML string
		for _, n := range g.notices {
			itemsHTML += digestNoticeItemHTML(appBaseURL, n)
		}
		sectionsHTML += fmt.Sprintf(`
			<p style="margin:20px 0 8px;font-weight:700;color:#191f28;">"%s" 조건에 새로 매칭됨 (%d건)</p>
			<div>%s</div>`, html.EscapeString(g.searchName), len(g.notices), itemsHTML)
	}
	return sectionsHTML
}

// sendSavedSearchDigest sends, per user with at least one alert_enabled
// saved search, an in-app/push notification to the search owner and an
// email to each resolved recipient(recipient_contact_ids, or the owner's
// login email if that list is empty/all-channels-off — resolveSavedSearchRecipients)
// covering every newly-matched notice across that user's saved searches.
// 같은 공고가 저장 조건 여러 개에 동시에 걸려도 한 번만 알린다("왜 같은
// 공고가 두 번 오지" 혼란 방지) — 단, "새로 매칭됐는지"는 검색 소유자
// 기준으로 한 번 걸러낸 뒤(기존 그대로) 수신자별로 다시 한 번 걸러서
// 이미 그 사람에게 보낸 적 있는 공고는 또 안 보낸다(2026-08-06, 담당자별
// 개별 dedup — 담당자 A는 이미 받았어도 새로 추가된 담당자 B는 처음
// 볼 수 있다).
func (s *Server) sendSavedSearchDigest(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, user_id, name, notice_type, region, industry, organization_name, budget_min, budget_max,
		       keywords_include, keywords_exclude, recipient_contact_ids
		FROM saved_searches WHERE alert_enabled = true AND is_active = true`)
	if err != nil {
		return err
	}
	byUser := map[string][]savedSearchDigestRow{}
	for rows.Next() {
		var sr savedSearchDigestRow
		if err := rows.Scan(&sr.id, &sr.userID, &sr.name, &sr.noticeType, &sr.region, &sr.industry, &sr.orgName,
			&sr.budgetMin, &sr.budgetMax, &sr.include, &sr.exclude, &sr.recipientContactIDs); err != nil {
			continue
		}
		byUser[sr.userID] = append(byUser[sr.userID], sr)
	}
	closeErr := rows.Err()
	rows.Close()
	if closeErr != nil {
		return closeErr
	}
	if len(byUser) == 0 {
		return nil
	}

	companyInfoData, err := s.fetchCompanyInfo(ctx)
	if err != nil {
		s.logger.Error("notify: company info lookup failed for saved-search digest footer", "error", err)
	}
	footerHTML := digestFooterHTML(companyInfoData)
	dashboardLink := s.appBaseURL + "/#/me/saved-searches"

	for userID, userSearches := range byUser {
		digestedIDs, err := s.fetchSavedSearchDigestedNoticeIDs(ctx, userID)
		if err != nil {
			s.logger.Error("notify: saved-search digested-ids query failed", "error", err)
			continue
		}

		seen := map[string]bool{}
		var groups []savedSearchMatchGroup
		var allMatched []digestNoticeRow
		var titlesForInApp []string
		var recipientContactIDUnion []string
		unionSeen := map[string]bool{}
		for _, sr := range userSearches {
			for _, cid := range sr.recipientContactIDs {
				if !unionSeen[cid] {
					unionSeen[cid] = true
					recipientContactIDUnion = append(recipientContactIDUnion, cid)
				}
			}
			matched, err := s.matchNoticesForSavedSearch(ctx, sr)
			if err != nil {
				s.logger.Error("notify: saved-search match query failed", "saved_search_id", sr.id, "error", err)
				continue
			}
			var group []digestNoticeRow
			for _, n := range matched {
				if digestedIDs[n.id] || seen[n.id] {
					continue
				}
				seen[n.id] = true
				group = append(group, n)
				allMatched = append(allMatched, n)
				titlesForInApp = append(titlesForInApp, n.title)
			}
			if len(group) > 0 {
				groups = append(groups, savedSearchMatchGroup{searchName: sr.name, notices: group})
			}
		}
		if len(groups) == 0 {
			continue
		}

		inAppSubject := fmt.Sprintf("[맞춤공고] 오늘 매칭된 공고 %d건", len(allMatched))
		inAppBody := strings.Join(titlesForInApp, " · ")
		if len(titlesForInApp) > 3 {
			inAppBody = strings.Join(titlesForInApp[:3], " · ") + fmt.Sprintf(" 외 %d건", len(titlesForInApp)-3)
		}
		if err := s.insertDigestInAppNotification(ctx, userID, notifyEventSavedSearchMatch, inAppSubject, inAppBody); err != nil {
			s.logger.Error("notify: saved-search digest in-app notification insert failed", "error", err)
		}
		// 인앱/웹푸시는 담당자(company_contacts)가 아니라 로그인 계정에만
		// 낼 수 있어 — 검색 소유자 계정에는 이메일 수신자 설정과 무관하게
		// 항상 함께 보낸다(기존 동작 유지).
		s.sendPushToUser(ctx, userID, inAppSubject, inAppBody, "/#/me/saved-searches")

		recipients, err := s.resolveSavedSearchRecipients(ctx, userID, recipientContactIDUnion)
		if err != nil {
			s.logger.Error("notify: saved-search recipient resolution failed", "user_id", userID, "error", err)
			continue
		}

		emailAllowed := true
		if profileID, err := s.companyProfileIDForUser(ctx, userID); err == nil && profileID != "" {
			emailAllowed = s.checkEmailNotificationQuota(ctx, profileID)
		}

		for _, rec := range recipients {
			if !rec.emailEnabled || rec.email == "" {
				continue
			}
			alreadySent, err := s.fetchSavedSearchAlreadySentNoticeIDs(ctx, notifyEventSavedSearchMatch, notifyChannelEmail, rec)
			if err != nil {
				s.logger.Error("notify: saved-search per-recipient dedup query failed", "error", err)
				continue
			}
			var recGroups []savedSearchMatchGroup
			var recMatched []digestNoticeRow
			for _, g := range groups {
				var notices []digestNoticeRow
				for _, n := range g.notices {
					if !alreadySent[n.id] {
						notices = append(notices, n)
						recMatched = append(recMatched, n)
					}
				}
				if len(notices) > 0 {
					recGroups = append(recGroups, savedSearchMatchGroup{searchName: g.searchName, notices: notices})
				}
			}
			if len(recMatched) == 0 {
				continue
			}

			subject := fmt.Sprintf("[맞춤공고] 오늘 매칭된 공고 %d건", len(recMatched))
			body := fmt.Sprintf(`
				<p>안녕하세요!</p>
				<p>저장하신 맞춤공고 조건에 새로운 공고 <b>%d건</b>이 매칭되었습니다.</p>
				%s
				<p style="text-align:center;margin:28px 0;">
					<a href="%s" style="display:inline-block;padding:12px 28px;background-color:#3182f6;color:#ffffff;text-decoration:none;border-radius:6px;font-weight:700;font-size:15px;">맞춤공고 관리하기</a>
				</p>
				%s`,
				len(recMatched), savedSearchDigestSectionsHTML(s.appBaseURL, recGroups), html.EscapeString(dashboardLink), footerHTML,
			)

			var status string
			var errMsg sql.NullString
			if emailAllowed {
				sendErr := s.notify.Send(ctx, rec.email, subject, body)
				status = "sent"
				if sendErr != nil {
					status = "failed"
					errMsg = sql.NullString{String: sendErr.Error(), Valid: true}
					s.logger.Error("notify: saved-search digest send failed", "recipient", rec.email, "error", sendErr)
				}
			} else {
				status = "skipped_quota"
			}
			noticeIDs := make([]string, len(recMatched))
			for i, n := range recMatched {
				noticeIDs[i] = n.id
			}
			s.logSavedSearchNoticeEmail(ctx, notifyEventSavedSearchMatch, rec, noticeIDs, subject, status, errMsg)
		}
	}
	return nil
}

// companyProfileIDForUser — saved_searches는 user_id 단위지만, 이메일 발송
// 한도(Free 플랜 월간 알림성 이메일 개수)는 조직 단위로 집계되는 기존
// 체계를 그대로 따른다(checkEmailNotificationQuota가 profileID를 받음).
// 프로필이 없으면(이론상 온보딩 필수라 발생하지 않음) 빈 문자열을
// 반환하고, 호출부는 그 경우 한도 검사를 건너뛴다(fail open — 다른 알림
// 경로들의 "한도 오판보다 덜 보내는 게 더 나쁘다" 원칙과 동일).
func (s *Server) companyProfileIDForUser(ctx context.Context, userID string) (string, error) {
	var profileID string
	err := s.db.QueryRowContext(ctx, `SELECT company_profile_id FROM company_members WHERE user_id = $1`, userID).Scan(&profileID)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return profileID, err
}

// isDueOnOffset reports whether deadline falls exactly offsetDays from
// today, comparing calendar dates only(UTC 자정 기준 — formatDigestDeadline과
// 동일한 절삭 방식) — sendDeadlineReminders(파이프라인용)가 SQL의
// `CURRENT_DATE + offset`으로 하는 것과 같은 판단을 여기서는 Go 쪽에서
// 한다(matchNoticesForSavedSearch가 이미 열린 공고 전체를 가져온 뒤라
// 정확한 오프셋만 이 함수로 다시 거른다).
func isDueOnOffset(deadline time.Time, offsetDays int) bool {
	today := time.Now().UTC().Truncate(24 * time.Hour)
	target := today.AddDate(0, 0, offsetDays)
	day := deadline.UTC().Truncate(24 * time.Hour)
	return day.Equal(target)
}

// logSavedSearchNoticeSMS — logSavedSearchNoticeEmail의 SMS 버전. SMS는
// 이메일과 달리 여러 건을 한 통에 요약해서 "실제로는 한 번만" 보내므로
// (건당 과금이라 매칭 공고 수만큼 중복발송하면 안 됨), 발송은 호출부가
// 한 번만 하고 이 함수는 커버한 notice_id마다 로그 행만 남긴다.
func (s *Server) logSavedSearchNoticeSMS(ctx context.Context, eventType string, rec savedSearchRecipient, noticeIDs []string, msg, status string, errMsg sql.NullString) {
	for _, noticeID := range noticeIDs {
		if _, err := s.db.ExecContext(ctx, `
			INSERT INTO notification_log (event_type, channel, recipient_phone, user_id, contact_id, notice_id, subject, status, error_message)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
			eventType, notifyChannelSMS, rec.phone, rec.userID, rec.contactID, noticeID, msg, status, errMsg,
		); err != nil {
			s.logger.Error("notify: saved-search notice sms log insert failed", "error", err)
		}
	}
}

// sendSavedSearchDeadlineReminders — 2026-08-06, 맞춤공고 "제출마감
// 리마인더". reminder_enabled=true인 검색마다, 그 필터에 매칭되는 공고
// "전체"(파이프라인에 추가했는지 여부와 무관) 중 마감이 정확히
// offsetDays 남은 것들을 찾아 그 검색의 recipient_contact_ids(또는
// owner-fallback)에게 보낸다. 인앱/웹푸시는 검색 소유자 계정에 항상
// 나란히 보낸다(sendSavedSearchDigest와 동일 원칙 — 담당자는 로그인
// 계정이 아니라 이메일/SMS로만 알림받을 수 있다).
func (s *Server) sendSavedSearchDeadlineReminders(ctx context.Context, offsetDays int, eventType string) error {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, user_id, name, notice_type, region, industry, organization_name, budget_min, budget_max,
		       keywords_include, keywords_exclude, recipient_contact_ids
		FROM saved_searches WHERE reminder_enabled = true AND is_active = true AND $1 = ANY(reminder_days_before)`, offsetDays)
	if err != nil {
		return err
	}
	var searches []savedSearchDigestRow
	for rows.Next() {
		var sr savedSearchDigestRow
		if err := rows.Scan(&sr.id, &sr.userID, &sr.name, &sr.noticeType, &sr.region, &sr.industry, &sr.orgName,
			&sr.budgetMin, &sr.budgetMax, &sr.include, &sr.exclude, &sr.recipientContactIDs); err != nil {
			continue
		}
		searches = append(searches, sr)
	}
	closeErr := rows.Err()
	rows.Close()
	if closeErr != nil {
		return closeErr
	}
	if len(searches) == 0 {
		return nil
	}

	companyInfoData, err := s.fetchCompanyInfo(ctx)
	if err != nil {
		s.logger.Error("notify: company info lookup failed for saved-search reminder footer", "error", err)
	}
	footerHTML := digestFooterHTML(companyInfoData)
	dashboardLink := s.appBaseURL + "/#/me/saved-searches"

	for _, sr := range searches {
		matched, err := s.matchNoticesForSavedSearch(ctx, sr)
		if err != nil {
			s.logger.Error("notify: saved-search reminder match query failed", "saved_search_id", sr.id, "error", err)
			continue
		}
		var due []digestNoticeRow
		for _, n := range matched {
			if n.deadline.Valid && isDueOnOffset(n.deadline.Time, offsetDays) {
				due = append(due, n)
			}
		}
		if len(due) == 0 {
			continue
		}

		titles := make([]string, len(due))
		for i, n := range due {
			titles[i] = n.title
		}
		inAppTitle := fmt.Sprintf("[맞춤공고 마감임박 D-%d] \"%s\" 조건 %d건", offsetDays, sr.name, len(due))
		inAppBody := strings.Join(titles, " · ")
		if len(titles) > 3 {
			inAppBody = strings.Join(titles[:3], " · ") + fmt.Sprintf(" 외 %d건", len(titles)-3)
		}
		if err := s.insertDigestInAppNotification(ctx, sr.userID, eventType, inAppTitle, inAppBody); err != nil {
			s.logger.Error("notify: saved-search reminder in-app notification insert failed", "error", err)
		}
		s.sendPushToUser(ctx, sr.userID, inAppTitle, inAppBody, "/#/me/saved-searches")

		recipients, err := s.resolveSavedSearchRecipients(ctx, sr.userID, []string(sr.recipientContactIDs))
		if err != nil {
			s.logger.Error("notify: saved-search reminder recipient resolution failed", "saved_search_id", sr.id, "error", err)
			continue
		}

		emailAllowed, smsAllowed := true, false
		smsProfileID := ""
		if profileID, err := s.companyProfileIDForUser(ctx, sr.userID); err == nil && profileID != "" {
			emailAllowed = s.checkEmailNotificationQuota(ctx, profileID)
			smsAllowed = s.smsAllowedForPlan(ctx, profileID)
			smsProfileID = profileID
		}

		for _, rec := range recipients {
			if rec.emailEnabled && rec.email != "" {
				alreadySent, err := s.fetchSavedSearchAlreadySentNoticeIDs(ctx, eventType, notifyChannelEmail, rec)
				if err != nil {
					s.logger.Error("notify: saved-search reminder email dedup query failed", "error", err)
				} else {
					var dueForRec []digestNoticeRow
					for _, n := range due {
						if !alreadySent[n.id] {
							dueForRec = append(dueForRec, n)
						}
					}
					if len(dueForRec) > 0 {
						subject := fmt.Sprintf("[맞춤공고 마감임박 D-%d] \"%s\" 조건 %d건", offsetDays, sr.name, len(dueForRec))
						var itemsHTML string
						for _, n := range dueForRec {
							itemsHTML += digestNoticeItemHTML(s.appBaseURL, n)
						}
						body := fmt.Sprintf(`
							<p>저장하신 맞춤공고 "%s" 조건에 매칭되는 공고의 제출마감이 <b>D-%d</b> 남았습니다.</p>
							<div>%s</div>
							<p style="text-align:center;margin:28px 0;">
								<a href="%s" style="display:inline-block;padding:12px 28px;background-color:#3182f6;color:#ffffff;text-decoration:none;border-radius:6px;font-weight:700;font-size:15px;">맞춤공고 관리하기</a>
							</p>
							%s`,
							html.EscapeString(sr.name), offsetDays, itemsHTML, html.EscapeString(dashboardLink), footerHTML,
						)
						var status string
						var errMsg sql.NullString
						if emailAllowed {
							sendErr := s.notify.Send(ctx, rec.email, subject, body)
							status = "sent"
							if sendErr != nil {
								status = "failed"
								errMsg = sql.NullString{String: sendErr.Error(), Valid: true}
								s.logger.Error("notify: saved-search reminder email send failed", "recipient", rec.email, "error", sendErr)
							}
						} else {
							status = "skipped_quota"
						}
						ids := make([]string, len(dueForRec))
						for i, n := range dueForRec {
							ids[i] = n.id
						}
						s.logSavedSearchNoticeEmail(ctx, eventType, rec, ids, subject, status, errMsg)
					}
				}
			}
			if smsAllowed && rec.smsEnabled && rec.phone != "" && s.smsNotify != nil {
				alreadySentSMS, err := s.fetchSavedSearchAlreadySentNoticeIDs(ctx, eventType, notifyChannelSMS, rec)
				if err != nil {
					s.logger.Error("notify: saved-search reminder sms dedup query failed", "error", err)
					continue
				}
				var dueForRec []digestNoticeRow
				for _, n := range due {
					if !alreadySentSMS[n.id] {
						dueForRec = append(dueForRec, n)
					}
				}
				if len(dueForRec) == 0 {
					continue
				}
				var msg string
				if len(dueForRec) == 1 {
					msg = fmt.Sprintf("[맞춤공고 D-%d] %s 마감 D-%d일 남았습니다.", offsetDays, truncateForSMS(dueForRec[0].title, 30), offsetDays)
				} else {
					msg = fmt.Sprintf("[맞춤공고 D-%d] \"%s\" 조건 마감임박 공고 %d건, 앱에서 확인하세요.", offsetDays, truncateForSMS(sr.name, 15), len(dueForRec))
				}
				// 플랜 월 사용량 예약(2026-08-18): subject = 이벤트|맞춤공고|수신자|날짜라 같은 날 재실행은 1건.
				// 한도 초과면 provider를 부르지 않고 skipped_quota로만 기록.
				smsPeriod := usagePeriodMonth(time.Now())
				recContact := ""
				if rec.contactID != nil {
					recContact = *rec.contactID
				}
				smsSubject := eventType + "|" + sr.id + "|" + recContact + "|" + rec.phone + "|" + time.Now().In(kstZone).Format("2006-01-02")
				smsReserved := false
				if smsProfileID != "" {
					dec, qerr := s.reserveSMSUsage(ctx, smsProfileID, smsPeriod, smsSubject)
					if qerr != nil {
						s.logger.Error("notify: saved-search sms usage reserve failed", "error", qerr)
					} else if !dec.Allowed {
						ids := make([]string, len(dueForRec))
						for i, n := range dueForRec {
							ids[i] = n.id
						}
						s.logSavedSearchNoticeSMS(ctx, eventType, rec, ids, msg, "skipped_quota", sql.NullString{})
						continue
					} else {
						smsReserved = dec.NewlyCounted
					}
				}
				sendErr := s.smsNotify.Send(ctx, rec.phone, msg)
				status := "sent"
				var errMsg sql.NullString
				if sendErr != nil {
					status = "failed"
					errMsg = sql.NullString{String: sendErr.Error(), Valid: true}
					s.logger.Error("notify: saved-search reminder sms send failed", "recipient", rec.phone, "error", sendErr)
					if smsReserved {
						s.releaseFeatureUsage(ctx, smsProfileID, billing.UsageSMS, smsPeriod, smsSubject)
					}
				}
				ids := make([]string, len(dueForRec))
				for i, n := range dueForRec {
					ids[i] = n.id
				}
				s.logSavedSearchNoticeSMS(ctx, eventType, rec, ids, msg, status, errMsg)
			}
		}
	}
	return nil
}
