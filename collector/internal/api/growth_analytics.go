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
//   - Phase 7 2단계: 벤치마킹(전체 가입 회사 익명 집계 평균 대비 우리
//     회사 위치)과 낙찰이력(scsbid) 연동 준비 구조를 추가했다 — 아래
//     minBenchmarkCompanies/fetchIndustryAwardBenchmark 주석 참고.
package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"time"

	"github.com/lib/pq"
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

// minBenchmarkCompanies — 벤치마크 지표(비교 대상 회사 수)가 이 값
// 미만이면 평균을 보여주지 않는다. 회사 수가 적을 때 "평균"을 보여주면
// 사실상 특정 소수 회사(경쟁사)의 데이터를 역산할 수 있어 익명성이
// 깨진다 — 요구사항 확정 사항.
const minBenchmarkCompanies = 5

// benchmarkMetric — 지표 하나의 벤치마크 결과. Available=false면
// companyCount만 보여주고 averageValue는 비워서(프론트가 "비교 데이터
// 부족" 안내만 하도록) 익명성을 지킨다. ourValue는 비교 가능 여부와
// 무관하게 항상 채운다(우리 회사 자기 수치는 항상 보여줘도 무방).
type benchmarkMetric struct {
	Available    bool     `json:"available"`
	CompanyCount int      `json:"companyCount"`
	AverageValue *float64 `json:"averageValue,omitempty"`
	OurValue     *float64 `json:"ourValue,omitempty"`
}

type growthBenchmark struct {
	MinCompanyCount     int             `json:"minCompanyCount"`
	ProfileCompleteness benchmarkMetric `json:"profileCompleteness"`
	PipelineConversion  benchmarkMetric `json:"pipelineConversion"`
}

type growthAnalyticsResponse struct {
	Trend             []growthTrendPoint      `json:"trend"`
	GradeDistribution []gradeDistributionItem `json:"gradeDistribution"`
	ROI               growthROI               `json:"roi"`
	Benchmark         growthBenchmark         `json:"benchmark"`
	// IndustryAwardBenchmark — scsbid 연동 준비(아래 fetchIndustryAwardBenchmark
	// 주석 참고). 지금은 notice_award_history가 비어있어 항상 nil.
	IndustryAwardBenchmark *industryAwardBenchmark `json:"industryAwardBenchmark,omitempty"`
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

	completeness, err := s.computeProfileCompleteness(ctx, profile.ID)
	if err != nil {
		s.logger.Error("growth-analytics: completeness query failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	completenessBenchmark, err := s.fetchCompletenessBenchmark(ctx, profile.ID, completeness.OverallCompleteness)
	if err != nil {
		s.logger.Error("growth-analytics: completeness benchmark query failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	conversionBenchmark, err := s.fetchConversionBenchmark(ctx, profile.ID)
	if err != nil {
		s.logger.Error("growth-analytics: conversion benchmark query failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}

	// Phase 7 2단계: 낙찰이력(scsbid) 연동 준비 — notice_award_history가
	// 아직 비어있어(수집기 미승인) 지금은 항상 nil을 반환하고 응답에서
	// 빠진다(omitempty). 프론트는 이 필드를 아직 참조하지 않는다 —
	// 데이터가 채워지기 시작하면 자동으로 값이 실리므로, 그때 프론트만
	// 추가로 작업하면 된다.
	industryAwardBenchmark, err := s.fetchIndustryAwardBenchmark(ctx, profile.Industry)
	if err != nil {
		s.logger.Error("growth-analytics: industry award benchmark query failed", "error", err)
		// 이 필드는 부가 정보라 실패해도 나머지 응답은 그대로 반환한다.
	}

	writeJSON(w, http.StatusOK, growthAnalyticsResponse{
		Trend: trend, GradeDistribution: distribution, ROI: roi,
		Benchmark: growthBenchmark{
			MinCompanyCount:     minBenchmarkCompanies,
			ProfileCompleteness: completenessBenchmark,
			PipelineConversion:  conversionBenchmark,
		},
		IndustryAwardBenchmark: industryAwardBenchmark,
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
// 그대로 막대그래프 순서로 쓴다. 2026-08-07: joint_venture_review가
// Grade 자체에서 서브태그(JointVentureRecommended)로 분리되면서 4단계로
// 줄었다 — scoring.go의 gradeFromCategories 주석 참고.
var gradeDisplayOrder = []string{
	gradeRecommended, gradeConditional, gradeNeedsConfirmation, gradeNotRecommended,
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
		SELECT n.notice_type, n.region, n.industry, n.budget_amount, n.industry_restricted,
		       nv.enrichment_status,
		       COALESCE((SELECT array_agg(pr.region_name ORDER BY pr.sort_no) FROM notice_participation_regions pr WHERE pr.notice_version_id = nv.id), '{}')
		FROM notices n
		JOIN notice_versions nv ON nv.notice_id = n.id AND nv.version_number = n.current_version
		WHERE n.status NOT IN ('closed','cancelled')
		  AND (n.application_end_at IS NULL OR n.application_end_at >= CURRENT_DATE)
		LIMIT `+itoa(dashboardNoticeScanLimit))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	counts := map[string]int{}
	for rows.Next() {
		var noticeType string
		var noticeRegion, noticeIndustry, enrichStatus sql.NullString
		var budget sql.NullInt64
		var industryRestricted sql.NullBool
		var officialRegions pq.StringArray
		if err := rows.Scan(&noticeType, &noticeRegion, &noticeIndustry, &budget, &industryRestricted, &enrichStatus, &officialRegions); err != nil {
			continue
		}
		score := scoreNoticeForCompany(
			noticeScoringInput{NoticeType: noticeType, Region: noticeRegion, Industry: noticeIndustry, BudgetAmount: budget, IndustryRestricted: nullBoolPtr(industryRestricted),
				OfficialRegions: []string(officialRegions), RegionEnriched: regionEnrichedFromStatus(enrichStatus)},
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

// fetchCompletenessBenchmark averages every OTHER company's most recent
// reports.summary.profileCompletenessScore (회사당 최신 리포트 1건만 —
// DISTINCT ON) and compares it against ourScore(요청 시점 실시간 계산,
// handleGetGrowthAnalytics가 이미 구했음). 우리 회사 자신은 평균에서
// 제외한다 — 자기 자신과 비교하는 건 의미가 없다.
func (s *Server) fetchCompletenessBenchmark(ctx context.Context, profileID string, ourScore int) (benchmarkMetric, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT ON (company_profile_id) summary
		FROM reports
		WHERE company_profile_id != $1
		ORDER BY company_profile_id, period_start DESC`,
		profileID,
	)
	if err != nil {
		return benchmarkMetric{}, err
	}
	defer rows.Close()

	sum, count := 0, 0
	for rows.Next() {
		var summaryRaw []byte
		if err := rows.Scan(&summaryRaw); err != nil {
			continue
		}
		var summary reportSummary
		if err := json.Unmarshal(summaryRaw, &summary); err != nil {
			continue
		}
		sum += summary.ProfileCompletenessScore
		count++
	}
	if err := rows.Err(); err != nil {
		return benchmarkMetric{}, err
	}

	our := float64(ourScore)
	if count < minBenchmarkCompanies {
		return benchmarkMetric{CompanyCount: count, OurValue: &our}, nil
	}
	avg := float64(sum) / float64(count)
	return benchmarkMetric{Available: true, CompanyCount: count, AverageValue: &avg, OurValue: &our}, nil
}

// fetchConversionBenchmark computes each company's all-time "제출 전환율"
// (제출완료/낙찰/탈락까지 도달한 비율 — 아직 진행 중이거나 보류/제외로
// 끝난 건은 분자에서 뺀다) and averages every OTHER company's rate against
// ours. 파이프라인 엔트리가 하나도 없는 회사는 분모가 0이라 애초에 비율을
// 정의할 수 없어 평균 계산에서 제외한다(우리 회사가 그런 경우도 동일).
func (s *Server) fetchConversionBenchmark(ctx context.Context, profileID string) (benchmarkMetric, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT company_profile_id,
			count(*) AS total,
			count(*) FILTER (WHERE status IN ('제출완료','낙찰','탈락')) AS submitted
		FROM notice_pipeline_entries
		GROUP BY company_profile_id`,
	)
	if err != nil {
		return benchmarkMetric{}, err
	}
	defer rows.Close()

	var ourValue *float64
	sum, count := 0.0, 0
	for rows.Next() {
		var pid string
		var total, submitted int
		if err := rows.Scan(&pid, &total, &submitted); err != nil {
			continue
		}
		if total == 0 {
			continue
		}
		rate := float64(submitted) / float64(total)
		if pid == profileID {
			r := rate
			ourValue = &r
			continue
		}
		sum += rate
		count++
	}
	if err := rows.Err(); err != nil {
		return benchmarkMetric{}, err
	}

	if count < minBenchmarkCompanies {
		return benchmarkMetric{CompanyCount: count, OurValue: ourValue}, nil
	}
	avg := sum / float64(count)
	return benchmarkMetric{Available: true, CompanyCount: count, AverageValue: &avg, OurValue: ourValue}, nil
}

type industryAwardBenchmark struct {
	SampleCount      int      `json:"sampleCount"`
	AverageAwardRate *float64 `json:"averageAwardRate,omitempty"`
}

// fetchIndustryAwardBenchmark — Phase 7 2단계: 낙찰이력(scsbid) 연동
// 준비. scsbid 수집기가 아직 미승인이라 notice_award_history가 항상
// 비어있고, 이 함수는 지금은 항상 (nil, nil)을 반환한다 — 승인되면
// 수집기가 이 테이블을 채우기 시작하는 순간 자동으로 값이 채워진다.
//
// 회사 단위(우리 회사의 실제 낙찰률)가 아니라 업종 단위 평균으로 설계한
// 이유: company_profiles에 "회사명"(사업자등록증 상호) 컬럼이 아예 없다
// (업종/지역/규모 등 속성만 저장) — notice_award_history.winner_name과
// 매칭할 방법이 없다. 정확한 자사 낙찰률을 보여주려면 company_profiles에
// 회사명 컬럼을 먼저 추가해야 하는데, 이번 단계 범위 밖이라 남겨둔다.
// 대신 이미 양쪽에 다 있는 industry로 "우리 업종 평균 낙찰률"을 계산한다.
func (s *Server) fetchIndustryAwardBenchmark(ctx context.Context, industries []string) (*industryAwardBenchmark, error) {
	if len(industries) == 0 {
		return nil, nil
	}
	var count int
	var avgRate sql.NullFloat64
	if err := s.db.QueryRowContext(ctx, `
		SELECT count(*), avg(award_rate) FROM notice_award_history
		WHERE industry = ANY($1) AND award_rate IS NOT NULL`,
		pq.Array(industries),
	).Scan(&count, &avgRate); err != nil {
		return nil, err
	}
	if count == 0 {
		return nil, nil
	}
	result := &industryAwardBenchmark{SampleCount: count}
	if avgRate.Valid {
		result.AverageAwardRate = &avgRate.Float64
	}
	return result, nil
}
