// growth_analytics.go — "성장분석" 화면. 3가지를 각자 다른 방식으로 조합한다:
//   - 완성도/파이프라인 성과 추이: 새 스냅샷 메커니즘을 만들지 않고
//     reports 테이블(reports.go의 주간/월간 배치가 이미 채움)을 시간순
//     으로 재구성한다 — 이 배치가 이번에 profileCompletenessScore를
//     같이 저장하도록 확장됐다(reports.go 참고).
//   - AI 참여판정 등급 분포: 현재 열려있는 공고 전체를 지금 시점 기준
//     으로 다시 채점해 등급별로 묶는다(히스토리 아님, 라이브 스냅샷).
//   - ROI(낙찰금액 대비 비용): notice_pipeline_entries.awarded_amount
//     (사용자가 낙찰 시 직접 입력) 합계와 payment_log의 실제 승인
//     결제 합계의 비율. notices.budget_amount(공고 예산)를 쓰지 않는
//     이유는 그건 "우리가 받은 돈"이 아니라 "발주기관이 배정한
//     예산"이라 실제 낙찰금액과 다를 수 있기 때문 — 부정확한 ROI보다
//     "아직 입력 안 됨(0건)"이 낫다는 판단.
package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"time"
)

type growthTrendPoint struct {
	PeriodType               string                  `json:"periodType"`
	PeriodStart              string                  `json:"periodStart"`
	PeriodEnd                string                  `json:"periodEnd"`
	ProfileCompletenessScore int                     `json:"profileCompletenessScore"`
	PipelineStartedCount     int                     `json:"pipelineStartedCount"`
	PipelineCompletedCount   int                     `json:"pipelineCompletedCount"`
	PipelineClosedCount      int                     `json:"pipelineClosedCount"`
	GradeDistribution        []gradeDistributionItem `json:"gradeDistribution,omitempty"`
}

type gradeDistributionItem struct {
	Grade string `json:"grade"`
	Count int    `json:"count"`
}

type growthROI struct {
	TotalAwardedAmount int64    `json:"totalAwardedAmount"`
	TotalPaidAmount    int64    `json:"totalPaidAmount"`
	AwardedCount       int      `json:"awardedCount"`
	Ratio              *float64 `json:"ratio"` // TotalAwardedAmount / TotalPaidAmount, null이면 결제 이력이 없어 계산 불가
}

type growthAnalyticsResponse struct {
	Trend             []growthTrendPoint      `json:"trend"`
	GradeDistribution []gradeDistributionItem `json:"gradeDistribution"`
	ROI               growthROI               `json:"roi"`
}

// handleGetGrowthAnalytics — GET /api/growth-analytics. 읽기 전용이라
// owner/member 둘 다 조회 가능(handleGetSubscription과 같은 접근 범위).
func (s *Server) handleGetGrowthAnalytics(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.currentUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	profile, err := s.getCompanyProfile(r, userID)
	if err != nil {
		s.logger.Error("growth-analytics: profile lookup failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if profile == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "company_profile_required"})
		return
	}
	ctx := r.Context()

	trend, err := s.fetchGrowthTrend(ctx, profile.ID)
	if err != nil {
		s.logger.Error("growth-analytics: trend query failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	distribution, err := s.fetchGradeDistribution(ctx, profile)
	if err != nil {
		s.logger.Error("growth-analytics: grade distribution query failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	roi, err := s.fetchROI(ctx, profile.ID)
	if err != nil {
		s.logger.Error("growth-analytics: ROI query failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}

	writeJSON(w, http.StatusOK, growthAnalyticsResponse{
		Trend: trend, GradeDistribution: distribution, ROI: roi,
	})
}

func (s *Server) fetchGrowthTrend(ctx context.Context, profileID string) ([]growthTrendPoint, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT period_type, period_start, period_end, summary
		FROM reports WHERE company_profile_id = $1
		ORDER BY period_start ASC`, profileID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	points := []growthTrendPoint{}
	for rows.Next() {
		var periodType string
		var periodStart, periodEnd time.Time
		var summaryRaw []byte
		if err := rows.Scan(&periodType, &periodStart, &periodEnd, &summaryRaw); err != nil {
			continue
		}
		var summary reportSummary
		if err := json.Unmarshal(summaryRaw, &summary); err != nil {
			continue
		}
		points = append(points, growthTrendPoint{
			PeriodType:               periodType,
			PeriodStart:              periodStart.Format("2006-01-02"),
			PeriodEnd:                periodEnd.Format("2006-01-02"),
			ProfileCompletenessScore: summary.ProfileCompletenessScore,
			PipelineStartedCount:     summary.PipelineStartedCount,
			PipelineCompletedCount:   summary.PipelineCompletedCount,
			PipelineClosedCount:      summary.PipelineClosedCount,
			GradeDistribution:        summary.GradeDistribution,
		})
	}
	return points, rows.Err()
}

// gradeDisplayOrder — 응답 배열 순서(좋은 등급 → 나쁜 등급), 프론트가
// 그대로 막대그래프 순서로 쓴다.
var gradeDisplayOrder = []string{
	gradeRecommended, gradeConditional, gradeJointVentureReview, gradeNeedsConfirmation, gradeNotRecommended,
}

// fetchGradeDistribution — 현재 시점 라이브 스냅샷(성장분석 화면의
// "지금 기준" 도넛차트용). company를 profile에서 새로 만들어야 하므로
// track record 조회가 한 번 더 필요하다 — reports.go의 스냅샷 저장
// 경로(gradeDistributionForCompany를 직접 호출)는 이미 company를
// 갖고 있어 이 조회를 중복하지 않는다.
func (s *Server) fetchGradeDistribution(ctx context.Context, profile *companyProfileDTO) ([]gradeDistributionItem, error) {
	var region, size sql.NullString
	if profile.Region != nil {
		region = sql.NullString{String: *profile.Region, Valid: true}
	}
	if profile.CompanySize != nil {
		size = sql.NullString{String: *profile.CompanySize, Valid: true}
	}
	trackRecordMax, err := s.fetchTrackRecordMaxAmount(ctx, profile.ID)
	if err != nil {
		return nil, err
	}
	company := companyScoringInput{
		Region: region, Industry: profile.Industry, Size: size, TrackRecordMaxAmount: trackRecordMax,
	}
	return s.gradeDistributionForCompany(ctx, company)
}

// gradeDistributionForCompany — 현재 열려있는 공고 전체를 주어진 회사
// 프로필 기준으로 다시 채점해 등급별로 묶는다. fetchGradeDistribution
// (라이브 조회)과 reports.go의 computeReportSummary(주간/월간 스냅샷
// 저장) 둘 다 이 함수를 공유한다 — 후자는 그 시점의 등급분포를
// reports.summary에 함께 저장해 성장분석의 "AI등급분포 추이" 차트가
// 매번 재계산이 아니라 실제 시계열이 되게 한다(growthTrendPoint 참고).
func (s *Server) gradeDistributionForCompany(ctx context.Context, company companyScoringInput) ([]gradeDistributionItem, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT notice_type, region, industry, budget_amount FROM notices
		WHERE status NOT IN ('closed','cancelled')
		  AND (application_end_at IS NULL OR application_end_at >= CURRENT_DATE)
		LIMIT `+itoa(dashboardNoticeScanLimit))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	counts := map[string]int{}
	for rows.Next() {
		var noticeType string
		var noticeRegion, noticeIndustry sql.NullString
		var budget sql.NullInt64
		if err := rows.Scan(&noticeType, &noticeRegion, &noticeIndustry, &budget); err != nil {
			continue
		}
		score := scoreNoticeForCompany(
			noticeScoringInput{NoticeType: noticeType, Region: noticeRegion, Industry: noticeIndustry, BudgetAmount: budget},
			company,
		)
		counts[score.Grade]++
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	items := make([]gradeDistributionItem, 0, len(gradeDisplayOrder))
	for _, g := range gradeDisplayOrder {
		items = append(items, gradeDistributionItem{Grade: g, Count: counts[g]})
	}
	return items, nil
}

func (s *Server) fetchROI(ctx context.Context, profileID string) (growthROI, error) {
	var roi growthROI
	var awarded sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(awarded_amount), 0), count(*) FILTER (WHERE awarded_amount IS NOT NULL)
		FROM notice_pipeline_entries WHERE company_profile_id = $1 AND status = '낙찰'`,
		profileID,
	).Scan(&awarded, &roi.AwardedCount); err != nil {
		return roi, err
	}
	roi.TotalAwardedAmount = awarded.Int64

	var paid sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(pl.amount), 0) FROM payment_log pl
		JOIN subscriptions sub ON sub.id = pl.subscription_id
		WHERE sub.company_profile_id = $1 AND pl.status = '승인'`,
		profileID,
	).Scan(&paid); err != nil {
		return roi, err
	}
	roi.TotalPaidAmount = paid.Int64
	if roi.TotalPaidAmount > 0 {
		ratio := float64(roi.TotalAwardedAmount) / float64(roi.TotalPaidAmount)
		roi.Ratio = &ratio
	}
	return roi, nil
}
