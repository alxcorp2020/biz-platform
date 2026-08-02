// reports.go — 주간/월간 자동 리포트. cmd/apiserver의 기존 09:00 KST
// 배치 인프라(startBackgroundNotifications)에 얹어, 매주 월요일엔 지난
// 한 주, 매월 1일엔 지난 한 달을 요약해 reports 테이블에 저장하고 조직
// 멤버 전원에게 이메일로 보낸다. "화면에서도 리포트 목록 조회 가능"은
// 새 최상위 메뉴가 아니라 #/me/subscription(구독 관리) 화면에 팀 관리와
// 같은 방식으로 임베드했다 — 사이드바/탭바가 이미 5개로 고정된 기존
// 설계 제약을 유지하기 위함(company_team.go의 renderTeamSection과 같은
// 선례).
//
// 히스토리 테이블로 남아있지 않은 지표는 있는 신호로 근사한다(각 항목
// 근거는 아래 주석 참고):
//   - "신규 추천공고"는 grade가 시계열로 저장되지 않아, 기간 내
//     first_collected_at인 공고 중 "리포트 생성 시점 기준"으로
//     recommended 채점되는 것으로 근사한다.
//   - "참여검토 시작/완료/종료"는 notice_pipeline_entries.created_at
//     (시작)과 decided_at(완료·종료 — 상태가 실제로 바뀐 시점, PATCH
//     핸들러가 상태 변경 때마다 갱신)을 그대로 쓴다 — 근사가 아니라
//     정확한 신호.
//   - "마감임박/경과"는 기간 집계가 아니라 리포트 생성 시점 스냅샷이다
//     (dashboard.go와 동일한 7일 기준) — 기간 중 며칠 동안 임박
//     상태였는지 추적할 방법이 없어 "지금 기준" 값을 보여준다.
package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"time"

	"github.com/lib/pq"

	"biz-platform/collector/internal/billing"
)

const (
	notifyEventWeeklyReport  = "weekly_report"
	notifyEventMonthlyReport = "monthly_report"
)

type reportTeamActivity struct {
	AssigneeName   string `json:"assigneeName"`
	ProcessedCount int    `json:"processedCount"`
}

type reportSummary struct {
	NewRecommendedCount    int                  `json:"newRecommendedCount"`
	PipelineStartedCount   int                  `json:"pipelineStartedCount"`
	PipelineCompletedCount int                  `json:"pipelineCompletedCount"`
	PipelineClosedCount    int                  `json:"pipelineClosedCount"`
	DeadlineSoonCount      int                  `json:"deadlineSoonCount"`
	DeadlinePassedCount    int                  `json:"deadlinePassedCount"`
	AIAnalysisUsedCount    int                  `json:"aiAnalysisUsedCount"`
	TeamActivity           []reportTeamActivity `json:"teamActivity,omitempty"`
	// ProfileCompletenessScore — 리포트 생성 시점 스냅샷(company_profile_
	// completeness.go의 computeProfileCompleteness와 같은 공식). growth_
	// analytics.go가 이 필드를 시간순으로 모아 "완성도 추이"를 그린다 —
	// 이 필드가 추가되기 전 리포트는 0으로 남아있다(추이 그래프에서
	// 자연히 구간 시작점으로 보임, 별도 백필 없음).
	ProfileCompletenessScore int `json:"profileCompletenessScore"`
	// GradeDistribution — Phase 7 고도화: AI 참여판정 등급분포도 리포트
	// 생성 시점에 함께 스냅샷으로 저장한다(growth_analytics.go의
	// gradeDistributionForCompany 재사용). 이전엔 성장분석 화면이 요청할
	// 때마다 "지금 기준"으로 재계산해 시계열이 아니었는데, 이제 이
	// 필드를 시간순으로 모으면 진짜 등급분포 추이가 된다 — 이 필드가
	// 추가되기 전 리포트는 nil로 남는다(추이 그래프에서 그 구간만 빈
	// 값으로 표시됨, 별도 백필 없음).
	GradeDistribution []gradeDistributionItem `json:"gradeDistribution,omitempty"`
}

type reportItem struct {
	ID          string        `json:"id"`
	PeriodType  string        `json:"periodType"`
	PeriodStart string        `json:"periodStart"`
	PeriodEnd   string        `json:"periodEnd"`
	Summary     reportSummary `json:"summary"`
	GeneratedAt time.Time     `json:"generatedAt"`
}

// RunScheduledReports checks whether now (already localized to the desired
// timezone — cmd/apiserver passes Asia/Seoul time) is a report-generation
// day and, if so, generates+emails that period's report for every
// company_profiles row. Idempotent via reports' UNIQUE(company_profile_id,
// period_type, period_start) — a re-run on the same day (e.g. process
// restart) skips profiles that already have that period's report.
func (s *Server) RunScheduledReports(ctx context.Context, now time.Time) (weeklyGenerated, monthlyGenerated int, err error) {
	if now.Weekday() == time.Monday {
		periodStart := dateOnly(now.AddDate(0, 0, -7))
		periodEnd := periodStart.AddDate(0, 0, 6) // 그 주 일요일
		n, err := s.generateReportsForAllProfiles(ctx, "weekly", periodStart, periodEnd, now)
		if err != nil {
			return weeklyGenerated, monthlyGenerated, fmt.Errorf("weekly reports: %w", err)
		}
		weeklyGenerated = n
	}
	if now.Day() == 1 {
		periodStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location()).AddDate(0, -1, 0)
		periodEnd := periodStart.AddDate(0, 1, 0).AddDate(0, 0, -1) // 지난달 말일
		n, err := s.generateReportsForAllProfiles(ctx, "monthly", periodStart, periodEnd, now)
		if err != nil {
			return weeklyGenerated, monthlyGenerated, fmt.Errorf("monthly reports: %w", err)
		}
		monthlyGenerated = n
	}
	return weeklyGenerated, monthlyGenerated, nil
}

func dateOnly(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
}

// generateReportsForAllProfiles loops every company_profiles row (every
// organization gets a report regardless of email_notifications_enabled —
// that flag only gates the email step, not generation, since "화면에서
// 리포트 목록 조회"는 이메일 설정과 무관해야 한다) and returns how many
// NEW report rows were created (used for the daily-ticker log line).
func (s *Server) generateReportsForAllProfiles(ctx context.Context, periodType string, periodStart, periodEnd, now time.Time) (int, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, region, industry, company_size, email_notifications_enabled FROM company_profiles`)
	if err != nil {
		return 0, err
	}
	type profileRow struct {
		id           string
		region, size sql.NullString
		industry     pq.StringArray
		emailEnabled bool
	}
	var profiles []profileRow
	for rows.Next() {
		var p profileRow
		if err := rows.Scan(&p.id, &p.region, &p.industry, &p.size, &p.emailEnabled); err != nil {
			continue
		}
		profiles = append(profiles, p)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()

	created := 0
	for _, p := range profiles {
		trackRecordMax, err := s.fetchTrackRecordMaxAmount(ctx, p.id)
		if err != nil {
			s.logger.Error("report: track record max amount query failed", "profileId", p.id, "error", err)
		}
		company := companyScoringInput{
			Region: p.region, Industry: []string(p.industry), Size: p.size,
			TrackRecordMaxAmount: trackRecordMax,
		}
		plan, err := s.effectivePlan(ctx, p.id)
		if err != nil {
			s.logger.Error("report: plan lookup failed", "profileId", p.id, "error", err)
		}

		summary, err := s.computeReportSummary(ctx, p.id, company, periodStart, periodEnd, now, plan)
		if err != nil {
			s.logger.Error("report: compute summary failed", "profileId", p.id, "error", err)
			continue
		}

		isNew, err := s.insertReport(ctx, p.id, periodType, periodStart, periodEnd, summary)
		if err != nil {
			s.logger.Error("report: insert failed", "profileId", p.id, "error", err)
			continue
		}
		if !isNew {
			continue // 이미 이 기간 리포트가 있음(배치 재실행/재기동) — 중복 생성/재발송 안 함
		}
		created++

		if p.emailEnabled && s.notify != nil && s.notify.Configured() {
			s.sendReportEmail(ctx, p.id, periodType, periodStart, periodEnd, summary)
		}
	}
	return created, nil
}

// computeReportSummary runs each metric's aggregate query independently —
// one failing query shouldn't blank out the rest of the report, so errors
// are logged and that field is just left at its zero value.
func (s *Server) computeReportSummary(
	ctx context.Context, profileID string, company companyScoringInput,
	periodStart, periodEnd, now time.Time, plan billing.Plan,
) (*reportSummary, error) {
	var summary reportSummary
	periodEndExclusive := periodEnd.AddDate(0, 0, 1)

	// 신규 추천공고: 기간 내 수집된 공고 중 리포트 생성 시점 기준 recommended.
	noticeRows, err := s.db.QueryContext(ctx, `
		SELECT notice_type, region, industry, budget_amount FROM notices
		WHERE first_collected_at >= $1 AND first_collected_at < $2`,
		periodStart, periodEndExclusive)
	if err != nil {
		s.logger.Error("report: new-recommended notices query failed", "error", err)
	} else {
		for noticeRows.Next() {
			var noticeType string
			var region, industry sql.NullString
			var budget sql.NullInt64
			if err := noticeRows.Scan(&noticeType, &region, &industry, &budget); err != nil {
				continue
			}
			score := scoreNoticeForCompany(noticeScoringInput{NoticeType: noticeType, Region: region, Industry: industry, BudgetAmount: budget}, company)
			if score.Grade == gradeRecommended {
				summary.NewRecommendedCount++
			}
		}
		noticeRows.Close()
	}

	// 참여검토 시작/완료/종료
	if err := s.db.QueryRowContext(ctx, `
		SELECT
			count(*) FILTER (WHERE created_at >= $2 AND created_at < $3),
			count(*) FILTER (WHERE status = '제출완료' AND decided_at >= $2 AND decided_at < $3),
			count(*) FILTER (WHERE status IN ('낙찰','탈락','보류','제외') AND decided_at >= $2 AND decided_at < $3)
		FROM notice_pipeline_entries WHERE company_profile_id = $1`,
		profileID, periodStart, periodEndExclusive,
	).Scan(&summary.PipelineStartedCount, &summary.PipelineCompletedCount, &summary.PipelineClosedCount); err != nil {
		s.logger.Error("report: pipeline started/completed/closed query failed", "error", err)
	}

	// 마감임박/경과 — 리포트 생성 시점 스냅샷(dashboard.go와 동일 7일 기준).
	closeSoonCutoff := dateOnly(now).AddDate(0, 0, dashboardPriorityCloseSoonDays)
	if err := s.db.QueryRowContext(ctx, `
		SELECT
			count(*) FILTER (WHERE status IN ('검토전','참여검토','승인대기','준비중')
				AND submission_deadline IS NOT NULL AND submission_deadline >= $2 AND submission_deadline < $3),
			count(*) FILTER (WHERE status IN ('검토전','참여검토','승인대기','준비중')
				AND submission_deadline IS NOT NULL AND submission_deadline < $2)
		FROM notice_pipeline_entries WHERE company_profile_id = $1`,
		profileID, dateOnly(now), closeSoonCutoff,
	).Scan(&summary.DeadlineSoonCount, &summary.DeadlinePassedCount); err != nil {
		s.logger.Error("report: deadline soon/passed query failed", "error", err)
	}

	// AI 분석 사용량
	if err := s.db.QueryRowContext(ctx, `
		SELECT count(*) FROM company_documents
		WHERE company_profile_id = $1 AND uploaded_at >= $2 AND uploaded_at < $3`,
		profileID, periodStart, periodEndExclusive,
	).Scan(&summary.AIAnalysisUsedCount); err != nil {
		s.logger.Error("report: AI analysis usage query failed", "error", err)
	}

	// 팀 활동 요약: Business 플랜(팀 최대 3명)에서만 의미가 있다 — 그 외
	// 플랜은 본인 1명뿐이라 "담당자별" 집계가 무의미하다.
	if plan == billing.PlanBusiness {
		teamRows, err := s.db.QueryContext(ctx, `
			SELECT assignee_name, count(*) FROM notice_pipeline_entries
			WHERE company_profile_id = $1 AND assignee_name IS NOT NULL
			  AND decided_at >= $2 AND decided_at < $3
			GROUP BY assignee_name ORDER BY count(*) DESC`,
			profileID, periodStart, periodEndExclusive)
		if err != nil {
			s.logger.Error("report: team activity query failed", "error", err)
		} else {
			for teamRows.Next() {
				var t reportTeamActivity
				if err := teamRows.Scan(&t.AssigneeName, &t.ProcessedCount); err != nil {
					continue
				}
				summary.TeamActivity = append(summary.TeamActivity, t)
			}
			teamRows.Close()
		}
	}

	completeness, err := s.computeProfileCompleteness(ctx, profileID, company.Industry)
	if err != nil {
		s.logger.Error("report: profile completeness query failed", "error", err)
	} else {
		summary.ProfileCompletenessScore = completeness.OverallCompleteness
	}

	gradeDist, err := s.gradeDistributionForCompany(ctx, company)
	if err != nil {
		s.logger.Error("report: grade distribution query failed", "error", err)
	} else {
		summary.GradeDistribution = gradeDist
	}

	return &summary, nil
}

// insertReport writes the report row, returning created=false (not an
// error) if this period's report already exists for this profile.
func (s *Server) insertReport(ctx context.Context, profileID, periodType string, periodStart, periodEnd time.Time, summary *reportSummary) (created bool, err error) {
	summaryJSON, err := json.Marshal(summary)
	if err != nil {
		return false, err
	}
	var id string
	err = s.db.QueryRowContext(ctx, `
		INSERT INTO reports (company_profile_id, period_type, period_start, period_end, summary)
		VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (company_profile_id, period_type, period_start) DO NOTHING
		RETURNING id`,
		profileID, periodType, periodStart, periodEnd, summaryJSON,
	).Scan(&id)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// sendReportEmail fans out to every org member (fetchCompanyMemberEmails,
// notifications.go) — same per-member pattern as sendRecommendationDigest.
// Phase 5 2단계: 인앱 알림함에도 1건 남긴다(원래 이메일만 가고 인앱은
// 빠져 있어서 이메일을 안 열어보면 리포트가 나온 사실 자체를 놓쳤다).
// 이 함수 자체가 insertReport의 ON CONFLICT(같은 기간 중복 방지)로 이미
// 기간당 한 번만 호출되도록 보장되므로, 인앱 쪽에 별도 dedup 로직을
// 추가할 필요는 없다 — 회사 단위 이벤트라 멤버별 반복 삽입도 안 한다.
func (s *Server) sendReportEmail(ctx context.Context, profileID, periodType string, periodStart, periodEnd time.Time, summary *reportSummary) {
	subject := reportEmailSubject(periodType, periodStart, periodEnd)
	body := reportEmailHTML(subject, summary, s.appBaseURL+"/#/me/subscription")
	eventType := notifyEventWeeklyReport
	if periodType == "monthly" {
		eventType = notifyEventMonthlyReport
	}

	inAppBody := fmt.Sprintf("신규 추천 %d건 · 참여검토 시작 %d건 · 마감임박 %d건",
		summary.NewRecommendedCount, summary.PipelineStartedCount, summary.DeadlineSoonCount)
	if err := s.insertInAppNotification(ctx, &profileID, nil, eventType, subject, inAppBody, nil, nil); err != nil {
		s.logger.Error("report: in-app notification insert failed", "error", err)
	}

	members, err := s.fetchCompanyMemberEmails(ctx, profileID)
	if err != nil {
		s.logger.Error("report: member lookup failed", "error", err)
		return
	}
	if len(members) == 0 {
		return
	}

	for _, m := range members {
		sendErr := s.notify.Send(ctx, m.email, subject, body)
		status, errMsg := "sent", sql.NullString{}
		if sendErr != nil {
			status = "failed"
			errMsg = sql.NullString{String: sendErr.Error(), Valid: true}
			s.logger.Error("report: send failed", "recipient", m.email, "error", sendErr)
		}
		if _, logErr := s.db.ExecContext(ctx, `
			INSERT INTO notification_log (event_type, channel, recipient_email, user_id, subject, status, error_message)
			VALUES ($1,$2,$3,$4,$5,$6,$7)`,
			eventType, notifyChannelEmail, m.email, m.userID, subject, status, errMsg,
		); logErr != nil {
			s.logger.Error("report: log insert failed", "error", logErr)
		}
	}
}

func reportEmailSubject(periodType string, periodStart, periodEnd time.Time) string {
	label := "주간"
	if periodType == "monthly" {
		label = "월간"
	}
	return fmt.Sprintf("%s 리포트 (%s ~ %s)", label, periodStart.Format("2006-01-02"), periodEnd.Format("2006-01-02"))
}

func reportEmailHTML(subject string, summary *reportSummary, reportURL string) string {
	teamHTML := ""
	if len(summary.TeamActivity) > 0 {
		var rows string
		for _, t := range summary.TeamActivity {
			rows += fmt.Sprintf("<li>%s: %d건</li>", html.EscapeString(t.AssigneeName), t.ProcessedCount)
		}
		teamHTML = fmt.Sprintf("<h3>담당자별 처리 건수</h3><ul>%s</ul>", rows)
	}
	return fmt.Sprintf(`
		<h2>%s</h2>
		<ul>
			<li>신규 추천 공고: %d건</li>
			<li>참여검토 시작: %d건</li>
			<li>제출완료: %d건</li>
			<li>종료(낙찰/탈락/보류/제외): %d건</li>
			<li>마감임박: %d건</li>
			<li>마감경과: %d건</li>
			<li>AI 분석 사용: %d건</li>
		</ul>
		%s
		<p><a href="%s">전체 리포트 보기 →</a></p>
	`, html.EscapeString(subject), summary.NewRecommendedCount, summary.PipelineStartedCount,
		summary.PipelineCompletedCount, summary.PipelineClosedCount, summary.DeadlineSoonCount,
		summary.DeadlinePassedCount, summary.AIAnalysisUsedCount, teamHTML, reportURL)
}

// handleListReports — GET /api/reports. Readable by both roles(owner/member),
// same as handleGetSubscription — 리포트는 읽기 전용 정보.
func (s *Server) handleListReports(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.currentUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	profile, err := s.getCompanyProfile(r, userID)
	if err != nil {
		s.logger.Error("list-reports: profile lookup failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if profile == nil {
		writeJSON(w, http.StatusOK, map[string]any{"items": []reportItem{}})
		return
	}

	rows, err := s.db.QueryContext(r.Context(), `
		SELECT id, period_type, period_start, period_end, summary, generated_at
		FROM reports WHERE company_profile_id = $1
		ORDER BY period_start DESC`, profile.ID)
	if err != nil {
		s.logger.Error("list-reports: query failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	defer rows.Close()

	items := []reportItem{}
	for rows.Next() {
		var it reportItem
		var periodStart, periodEnd time.Time
		var summaryRaw []byte
		if err := rows.Scan(&it.ID, &it.PeriodType, &periodStart, &periodEnd, &summaryRaw, &it.GeneratedAt); err != nil {
			s.logger.Error("list-reports: scan failed", "error", err)
			continue
		}
		it.PeriodStart = periodStart.Format("2006-01-02")
		it.PeriodEnd = periodEnd.Format("2006-01-02")
		if err := json.Unmarshal(summaryRaw, &it.Summary); err != nil {
			s.logger.Error("list-reports: summary unmarshal failed", "error", err)
		}
		items = append(items, it)
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}
