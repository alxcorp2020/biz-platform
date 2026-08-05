// saved_search_digest.go — "맞춤공고"(saved_searches) 알림 배치. 매일
// 09:00 KST(startBackgroundNotifications, sendRecommendationDigest와 같은
// 시각)에 실행되어, alert_enabled=true인 저장 조건마다 새로 매칭되는
// 공고를 찾아 사용자별로 이메일 한 통(+인앱+웹푸시)으로 묶어 보낸다.
// 조건별 매칭 로직은 server.go의 handleListNotices WHERE절 확장과 같은
// 모양이지만, 여긴 HTTP 왕복 없이 직접 쿼리한다(페이지네이션/북마크/
// 변경감지 배지처럼 이 배치에 필요 없는 것들을 안 실어도 되므로).
package api

import (
	"context"
	"database/sql"
	"fmt"
	"html"
	"strings"

	"github.com/lib/pq"
)

type savedSearchDigestRow struct {
	id, userID, name                      string
	noticeType, region, industry, orgName sql.NullString
	budgetMin, budgetMax                  sql.NullInt64
	include, exclude                      pq.StringArray
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

// sendSavedSearchDigest sends, per user with at least one alert_enabled
// saved search, one digest email covering every newly-matched notice across
// all of that user's saved searches — 같은 공고가 저장 조건 여러 개에
// 동시에 걸려도 한 번만 알린다(사용자 입장에서 "왜 같은 공고가 두 번
// 오지" 혼란 방지).
func (s *Server) sendSavedSearchDigest(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, user_id, name, notice_type, region, industry, organization_name, budget_min, budget_max,
		       keywords_include, keywords_exclude
		FROM saved_searches WHERE alert_enabled = true`)
	if err != nil {
		return err
	}
	byUser := map[string][]savedSearchDigestRow{}
	for rows.Next() {
		var sr savedSearchDigestRow
		if err := rows.Scan(&sr.id, &sr.userID, &sr.name, &sr.noticeType, &sr.region, &sr.industry, &sr.orgName,
			&sr.budgetMin, &sr.budgetMax, &sr.include, &sr.exclude); err != nil {
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

	for userID, userSearches := range byUser {
		var email string
		if err := s.db.QueryRowContext(ctx, `SELECT email FROM users WHERE id = $1`, userID).Scan(&email); err != nil {
			s.logger.Error("notify: saved-search digest recipient lookup failed", "user_id", userID, "error", err)
			continue
		}

		digestedIDs, err := s.fetchSavedSearchDigestedNoticeIDs(ctx, userID)
		if err != nil {
			s.logger.Error("notify: saved-search digested-ids query failed", "error", err)
			continue
		}

		seen := map[string]bool{}
		type matchGroup struct {
			searchName string
			notices    []digestNoticeRow
		}
		var groups []matchGroup
		var allMatched []digestNoticeRow
		var titlesForInApp []string
		for _, sr := range userSearches {
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
				groups = append(groups, matchGroup{searchName: sr.name, notices: group})
			}
		}
		if len(groups) == 0 {
			continue
		}

		subject := fmt.Sprintf("[맞춤공고] 오늘 매칭된 공고 %d건", len(allMatched))
		var sectionsHTML string
		for _, g := range groups {
			var itemsHTML string
			for _, n := range g.notices {
				itemsHTML += digestNoticeItemHTML(s.appBaseURL, n)
			}
			sectionsHTML += fmt.Sprintf(`
				<p style="margin:20px 0 8px;font-weight:700;color:#191f28;">"%s" 조건에 새로 매칭됨 (%d건)</p>
				<div>%s</div>`, html.EscapeString(g.searchName), len(g.notices), itemsHTML)
		}
		dashboardLink := s.appBaseURL + "/#/me/saved-searches"
		body := fmt.Sprintf(`
			<p>안녕하세요!</p>
			<p>저장하신 맞춤공고 조건에 새로운 공고 <b>%d건</b>이 매칭되었습니다.</p>
			%s
			<p style="text-align:center;margin:28px 0;">
				<a href="%s" style="display:inline-block;padding:12px 28px;background-color:#3182f6;color:#ffffff;text-decoration:none;border-radius:6px;font-weight:700;font-size:15px;">맞춤공고 관리하기</a>
			</p>
			%s`,
			len(allMatched), sectionsHTML, html.EscapeString(dashboardLink), footerHTML,
		)

		inAppBody := strings.Join(titlesForInApp, " · ")
		if len(titlesForInApp) > 3 {
			inAppBody = strings.Join(titlesForInApp[:3], " · ") + fmt.Sprintf(" 외 %d건", len(titlesForInApp)-3)
		}
		if err := s.insertDigestInAppNotification(ctx, userID, notifyEventSavedSearchMatch, subject, inAppBody); err != nil {
			s.logger.Error("notify: saved-search digest in-app notification insert failed", "error", err)
		}
		s.sendPushToUser(ctx, userID, subject, inAppBody, "/#/me/saved-searches")

		var status string
		var errMsg sql.NullString
		emailAllowed := true
		if profileID, err := s.companyProfileIDForUser(ctx, userID); err == nil && profileID != "" {
			emailAllowed = s.checkEmailNotificationQuota(ctx, profileID)
		}
		if emailAllowed {
			sendErr := s.notify.Send(ctx, email, subject, body)
			status = "sent"
			if sendErr != nil {
				status = "failed"
				errMsg = sql.NullString{String: sendErr.Error(), Valid: true}
				s.logger.Error("notify: saved-search digest send failed", "recipient", email, "error", sendErr)
			}
		} else {
			status = "skipped_quota"
		}
		for _, n := range allMatched {
			if _, logErr := s.db.ExecContext(ctx, `
				INSERT INTO notification_log (event_type, channel, recipient_email, user_id, notice_id, subject, status, error_message)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
				notifyEventSavedSearchMatch, notifyChannelEmail, email, userID, n.id, subject, status, errMsg,
			); logErr != nil {
				s.logger.Error("notify: saved-search digest log insert failed", "error", logErr)
			}
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
