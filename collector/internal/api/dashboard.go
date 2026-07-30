// dashboard.go — GET /api/dashboard. "AI 비서" 1단계의 새 첫 화면 데이터:
// 로그인한 사용자의 회사 프로필 기준으로 활성 공고를 스캔해 참여 가능성
// 버킷별로 집계하고, 즉시 참여 가능한 공고 중 마감 임박순 추천 목록을
// 만든다. scoring.go의 scoreNoticeForCompany는 DB에 아무것도 쓰지 않아
// 이렇게 공고 수백 건을 매 요청마다 스캔해도 eligibility_evaluations가
// 불어나지 않는다.
package api

import (
	"context"
	"database/sql"
	"net/http"
	"sort"
	"time"

	"github.com/lib/pq"
)

// dashboardRecommendationLimit caps how many "즉시 참여 가능" notices the
// dashboard returns — enough for a first screen, not a full list (그건
// #/notices에서 확인).
const dashboardRecommendationLimit = 10

// dashboardNoticeScanLimit is a safety cap on how many active notices get
// scored per dashboard request. 1차 버전이라 페이지네이션/캐싱은 없다 —
// 활성 공고가 이 한도를 넘어서면 그때 다시 손본다.
const dashboardNoticeScanLimit = 500

type dashboardRecommendation struct {
	NoticeID         string     `json:"noticeId"`
	Title            string     `json:"title"`
	OrganizationName string     `json:"organizationName"`
	ApplicationEndAt *time.Time `json:"applicationEndAt"`
	MetCount         int        `json:"metCount"`
	TotalCount       int        `json:"totalCount"`
	IsBookmarked     bool       `json:"isBookmarked"`
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.currentUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	ctx := r.Context()

	var region, size sql.NullString
	var industryArr pq.StringArray
	err := s.db.QueryRowContext(ctx,
		`SELECT region, industry, company_size FROM company_profiles WHERE user_id = $1`, userID,
	).Scan(&region, &industryArr, &size)
	if err == sql.ErrNoRows {
		// 프로필이 없는 것은 에러가 아니다 — 프론트가 온보딩 화면으로 분기한다.
		writeJSON(w, http.StatusOK, map[string]any{"hasProfile": false})
		return
	}
	if err != nil {
		s.logger.Error("dashboard: profile lookup failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	company := companyScoringInput{Region: region, Industry: []string(industryArr), Size: size}

	bookmarkedIDs, err := s.fetchBookmarkedNoticeIDs(ctx, userID)
	if err != nil {
		s.logger.Error("dashboard: bookmarked notice ids query failed", "error", err)
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT id, title, organization_name, region, industry, budget_amount, application_end_at
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
	defer rows.Close()

	var readyCount, needsReviewCount, notRecommendedCount, totalScanned int
	recommendations := []dashboardRecommendation{}
	for rows.Next() {
		var id, title string
		var org, noticeRegion, noticeIndustry sql.NullString
		var budget sql.NullInt64
		var deadline sql.NullTime
		if err := rows.Scan(&id, &title, &org, &noticeRegion, &noticeIndustry, &budget, &deadline); err != nil {
			continue
		}
		totalScanned++

		score := scoreNoticeForCompany(
			noticeScoringInput{Region: noticeRegion, Industry: noticeIndustry, BudgetAmount: budget},
			company,
		)
		switch score.Bucket {
		case "ready":
			readyCount++
			rec := dashboardRecommendation{
				NoticeID: id, Title: title, OrganizationName: org.String,
				MetCount: score.MetCount, TotalCount: score.TotalCount,
				IsBookmarked: bookmarkedIDs[id],
			}
			if deadline.Valid {
				rec.ApplicationEndAt = &deadline.Time
			}
			recommendations = append(recommendations, rec)
		case "needs_review":
			needsReviewCount++
		default:
			notRecommendedCount++
		}
	}
	if err := rows.Err(); err != nil {
		s.logger.Error("dashboard: notices scan failed", "error", err)
	}

	sort.Slice(recommendations, func(i, j int) bool {
		a, b := recommendations[i].ApplicationEndAt, recommendations[j].ApplicationEndAt
		if a == nil {
			return false
		}
		if b == nil {
			return true
		}
		return a.Before(*b)
	})
	if len(recommendations) > dashboardRecommendationLimit {
		recommendations = recommendations[:dashboardRecommendationLimit]
	}

	changedTodayCount, err := s.countNoticesChangedToday(ctx)
	if err != nil {
		s.logger.Error("dashboard: changed-today query failed", "error", err)
	}
	closingThisWeekCount, err := s.countNoticesClosingThisWeek(ctx)
	if err != nil {
		s.logger.Error("dashboard: closing-this-week query failed", "error", err)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"hasProfile":           true,
		"readyCount":           readyCount,
		"needsReviewCount":     needsReviewCount,
		"notRecommendedCount":  notRecommendedCount,
		"changedTodayCount":    changedTodayCount,
		"closingThisWeekCount": closingThisWeekCount,
		"totalScanned":         totalScanned,
		"recommendations":      recommendations,
	})
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

func (s *Server) countNoticesChangedToday(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `
		SELECT count(DISTINCT notice_id) FROM notice_changes
		WHERE created_at >= CURRENT_DATE`).Scan(&n)
	return n, err
}

func (s *Server) countNoticesClosingThisWeek(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `
		SELECT count(*) FROM notices
		WHERE application_end_at >= CURRENT_DATE
		  AND application_end_at < CURRENT_DATE + INTERVAL '7 days'
		  AND status NOT IN ('closed','cancelled')`).Scan(&n)
	return n, err
}
