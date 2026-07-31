// dashboard.go — GET /api/dashboard. 홈 화면 "오늘 할 일" 데이터: 진행 중
// 파이프라인(notice_pipeline_entries) 중 상태 미정/서류 미비인 것과, 아직
// 파이프라인에 없는 신규 추천 공고(grade='recommended', scoring.go의
// scoreNoticeForCompany 재사용 — DB에 아무것도 안 씀)를 합쳐 마감일순으로
// 보여준다. 이 응답을 쓰는 화면은 홈 하나뿐이라 예전 3버킷 집계
// (readyCount 등)는 완전히 대체한다(하위호환 유지 대상 아님).
package api

import (
	"context"
	"database/sql"
	"net/http"
	"sort"
	"time"

	"github.com/lib/pq"

	"biz-platform/collector/internal/billing"
)

// dashboardNoticeScanLimit is a safety cap on how many active notices get
// scored per dashboard request (기존과 동일한 이유 — 1차 버전, 페이지네이션 없음).
const dashboardNoticeScanLimit = 500

// dashboardPriorityCloseSoonDays: 이 기간 내 마감이면 "마감임박"으로 집계.
const dashboardPriorityCloseSoonDays = 7

// pipelineActivePipelineStatuses: "종결"되지 않아 여전히 챙겨야 하는
// 파이프라인 상태 — 우선 업무 리스트/서류 카운트는 이 상태들만 대상으로 한다.
var pipelineActiveStatuses = map[string]bool{
	"검토전": true, "참여검토": true, "승인대기": true, "준비중": true,
}

// pipelineUndecidedStatuses: "상태가 아직 정해지지 않은" 단계 — 우선
// 업무 리스트 포함 조건의 절반(나머지 절반은 서류 미비 여부).
var pipelineUndecidedStatuses = map[string]bool{
	"검토전": true, "참여검토": true, "승인대기": true,
}

type dashboardPriorityItem struct {
	Kind             string     `json:"kind"` // "pipeline" | "recommendation"
	NoticeID         string     `json:"noticeId"`
	PipelineEntryID  *string    `json:"pipelineEntryId,omitempty"`
	Title            string     `json:"title"`
	OrganizationName string     `json:"organizationName"`
	Status           *string    `json:"status,omitempty"` // pipeline 항목만
	ApplicationEndAt *time.Time `json:"applicationEndAt"`
	IsBookmarked     bool       `json:"isBookmarked"`
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.currentUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	ctx := r.Context()

	profile, err := s.getCompanyProfile(r, userID)
	if err != nil {
		s.logger.Error("dashboard: profile lookup failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if profile == nil {
		// 프로필이 없는 것은 에러가 아니다 — 프론트가 온보딩 화면으로 분기한다.
		writeJSON(w, http.StatusOK, map[string]any{"hasProfile": false})
		return
	}
	profileID := profile.ID
	var region, size sql.NullString
	if profile.Region != nil {
		region = sql.NullString{String: *profile.Region, Valid: true}
	}
	if profile.CompanySize != nil {
		size = sql.NullString{String: *profile.CompanySize, Valid: true}
	}
	industryArr := pq.StringArray(profile.Industry)
	trackRecordMax, err := s.fetchTrackRecordMaxAmount(ctx, profileID)
	if err != nil {
		s.logger.Error("dashboard: track record max amount query failed", "error", err)
	}
	company := companyScoringInput{
		Region: region, Industry: []string(industryArr), Size: size,
		TrackRecordMaxAmount: trackRecordMax,
	}

	bookmarkedIDs, err := s.fetchBookmarkedNoticeIDs(ctx, userID)
	if err != nil {
		s.logger.Error("dashboard: bookmarked notice ids query failed", "error", err)
	}

	pipelineRows, err := s.db.QueryContext(ctx, `
		SELECT pe.id, pe.notice_id, n.title, n.organization_name, pe.status,
		       pe.assignee_name, pe.submission_deadline
		FROM notice_pipeline_entries pe
		JOIN notices n ON n.id = pe.notice_id
		WHERE pe.company_profile_id = $1`, profileID)
	if err != nil {
		s.logger.Error("dashboard: pipeline entries query failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	type pipelineRow struct {
		id, noticeID, title, status string
		organizationName            sql.NullString
		assigneeName                sql.NullString
		deadline                    sql.NullTime
	}
	var pipelineEntries []pipelineRow
	pipelinedNoticeIDs := map[string]bool{}
	for pipelineRows.Next() {
		var pr pipelineRow
		if err := pipelineRows.Scan(&pr.id, &pr.noticeID, &pr.title, &pr.organizationName,
			&pr.status, &pr.assigneeName, &pr.deadline); err != nil {
			continue
		}
		pipelineEntries = append(pipelineEntries, pr)
		pipelinedNoticeIDs[pr.noticeID] = true
	}
	pipelineRows.Close()
	if err := pipelineRows.Err(); err != nil {
		s.logger.Error("dashboard: pipeline entries scan failed", "error", err)
	}

	incompleteDocCounts, err := s.fetchIncompleteChecklistCounts(ctx, profileID)
	if err != nil {
		s.logger.Error("dashboard: checklist counts query failed", "error", err)
	}

	reviewPendingCount, unassignedCount, needsDocumentCount, deadlineSoonCount := 0, 0, 0, 0
	priorityItems := []dashboardPriorityItem{}
	closeSoonCutoff := time.Now().AddDate(0, 0, dashboardPriorityCloseSoonDays)

	for _, pr := range pipelineEntries {
		if pipelineUndecidedStatuses[pr.status] {
			reviewPendingCount++
		}
		incomplete := incompleteDocCounts[pr.id]
		if pipelineActiveStatuses[pr.status] {
			needsDocumentCount += incomplete
			if pr.assigneeName.String == "" {
				unassignedCount++
			}
			// 마감임박 요약 카드는 파이프라인 항목만 센다(추천 공고 제외) —
			// 4개 요약 카드 전부 "#/pipeline?filter=..."로 들어갔을 때 보이는
			// 목록과 숫자가 정확히 일치해야 하는데, 아직 파이프라인에 넣지도
			// 않은 추천 공고는 그 목록에 나타날 수 없다. "오늘의 우선 업무"
			// 리스트(추천 포함)는 이 집계와 별개로 그대로 유지한다.
			if pr.deadline.Valid && pr.deadline.Time.Before(closeSoonCutoff) {
				deadlineSoonCount++
			}
		}
		if !pipelineActiveStatuses[pr.status] || (!pipelineUndecidedStatuses[pr.status] && incomplete == 0) {
			continue // 우선 업무 대상 아님: 종결됐거나, 상태도 정해지고 서류도 다 갖춰짐
		}
		entryID := pr.id
		status := pr.status
		item := dashboardPriorityItem{
			Kind: "pipeline", NoticeID: pr.noticeID, PipelineEntryID: &entryID,
			Title: pr.title, OrganizationName: pr.organizationName.String, Status: &status,
			IsBookmarked: bookmarkedIDs[pr.noticeID],
		}
		if pr.deadline.Valid {
			item.ApplicationEndAt = &pr.deadline.Time
		}
		priorityItems = append(priorityItems, item)
	}

	noticeRows, err := s.db.QueryContext(ctx, `
		SELECT id, notice_type, title, organization_name, region, industry, budget_amount, application_end_at
		FROM notices
		WHERE status NOT IN ('closed','cancelled')
		  AND (application_end_at IS NULL OR application_end_at >= CURRENT_DATE)
		ORDER BY application_end_at ASC NULLS LAST
		LIMIT `+itoa(dashboardNoticeScanLimit))
	if err != nil {
		s.logger.Error("dashboard: notices query failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	defer noticeRows.Close()

	for noticeRows.Next() {
		var id, title, noticeType string
		var org, noticeRegion, noticeIndustry sql.NullString
		var budget sql.NullInt64
		var deadline sql.NullTime
		if err := noticeRows.Scan(&id, &noticeType, &title, &org, &noticeRegion, &noticeIndustry, &budget, &deadline); err != nil {
			continue
		}
		if pipelinedNoticeIDs[id] {
			continue // 이미 파이프라인에 있음 — 위에서 이미 처리됨
		}
		score := scoreNoticeForCompany(
			noticeScoringInput{NoticeType: noticeType, Region: noticeRegion, Industry: noticeIndustry, BudgetAmount: budget},
			company,
		)
		if score.Grade != gradeRecommended {
			continue
		}
		item := dashboardPriorityItem{
			Kind: "recommendation", NoticeID: id, Title: title, OrganizationName: org.String,
			IsBookmarked: bookmarkedIDs[id],
		}
		if deadline.Valid {
			item.ApplicationEndAt = &deadline.Time
		}
		priorityItems = append(priorityItems, item)
	}
	if err := noticeRows.Err(); err != nil {
		s.logger.Error("dashboard: notices scan failed", "error", err)
	}

	sort.Slice(priorityItems, func(i, j int) bool {
		a, b := priorityItems[i].ApplicationEndAt, priorityItems[j].ApplicationEndAt
		if a == nil {
			return false
		}
		if b == nil {
			return true
		}
		return a.Before(*b)
	})

	plan, err := s.effectivePlan(ctx, profileID)
	if err != nil {
		s.logger.Error("dashboard: plan lookup failed", "error", err)
	}
	aiLimit := billing.Plans[plan].MaxAIAnalysisPerMonth
	aiUsed, err := s.countAIAnalysisThisMonth(ctx, profileID)
	if err != nil {
		s.logger.Error("dashboard: AI usage count failed", "error", err)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"hasProfile": true,
		"summary": map[string]int{
			"reviewPendingCount":  reviewPendingCount,
			"deadlineSoonCount":   deadlineSoonCount,
			"needsDocumentCount":  needsDocumentCount,
			"unassignedCount":     unassignedCount,
			"aiAnalysisUsedCount": aiUsed,
			"aiAnalysisLimit":     aiLimit, // -1 = 무제한, 0 = 이 플랜에서 이용 불가(Free)
		},
		"priorityItems": priorityItems,
	})
}

// fetchIncompleteChecklistCounts returns, per pipeline entry id, how many
// checklist items are NOT status='보유' — used both for the
// needsDocumentCount summary tile and for deciding which pipeline entries
// belong in "오늘의 우선 업무" (서류 미비인 것).
func (s *Server) fetchIncompleteChecklistCounts(ctx context.Context, profileID string) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT pe.id, count(*) FILTER (WHERE ci.status != '보유')
		FROM notice_pipeline_entries pe
		LEFT JOIN pipeline_checklist_items ci ON ci.pipeline_entry_id = pe.id
		WHERE pe.company_profile_id = $1
		GROUP BY pe.id`, profileID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	counts := map[string]int{}
	for rows.Next() {
		var id string
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			continue
		}
		counts[id] = n
	}
	return counts, rows.Err()
}

// fetchBookmarkedNoticeIDs는 이미 최대 500건을 스캔하는 메인 공고 쿼리에
// notice_bookmarks LEFT JOIN을 더 얹기보다, 사용자의 북마크 집합을 한 번만
// 조회해 맵으로 들고 있다가 추천 목록 생성 시 O(1)로 체크한다.
func (s *Server) fetchBookmarkedNoticeIDs(ctx context.Context, userID string) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT notice_id FROM notice_bookmarks WHERE user_id = $1`, userID)
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
