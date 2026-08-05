// 프로필 완성도("AI 분석 커버리지") 요약. 2026-08-05 온보딩 재설계(1차) —
// 예전엔 "기본정보"가 company_profiles 행 존재 여부만으로 무조건 100%
// 였는데(지역/기업규모/업력/직원수/매출액을 개별적으로 채점 안 함), 이러면
// 온보딩 카드에서 이 필드들을 등록해도 커버리지 %가 전혀 안 올라 실시간
// %표시 기능 자체가 성립 안 됨. "기본정보"를 지역/기업규모/업력/직원수/
// 매출액 5개로 쪼개고 직접생산확인을 신규 추가해 총 12개 카테고리로
// 재설계했다.
//
// 2026-08-05 재설계(2차, 심플화) — 12개를 전부 동일 가중치(1/12)로 두면
// 필수 3개(지역/업종/기업규모)만 채워도 겨우 25%라 "필수만 성실히
// 채웠는데 50% 게이트에 막힌다"는 부당한 상황이 생겼다(사용자 확인). 그래서
// 지역/업종/기업규모(온보딩에서 건너뛸 수 없는 필수 3개) 각각에 20%(합
// 60%)를, 나머지 9개 선택 카테고리가 남은 40%를 균등분배(각 40/9%)받도록
// 가중평균으로 바꿨다 — 필수 3개만 채우면 선택 항목을 하나도 안 건드려도
// 항상 60%로 온보딩MinCompleteness(50%)를 넘는다. 이 재설계에 맞춰
// "업종"의 기존 부분점수 공식(선택한 업종 수/10)도 이진(하나라도 선택하면
// 100%)으로 바꿨다 — 안 그러면 업종을 1개만 선택한 흔한 경우(대부분의
// 중소기업) industry가 10%만 인정돼 필수 3개를 다 채워도 60%에
// 못미치는 모순이 생긴다.
package api

import (
	"context"
	"database/sql"
	"math"
	"net/http"

	"github.com/lib/pq"
)

// requiredCompletenessCategoryWeight — 온보딩 필수 3개(지역/업종/기업규모)
// 각각의 가중치(%). 3개 합 60%로, 이 3개만 채우면 선택 항목 없이도
// onboardingMinCompleteness(50%)를 항상 넘는다. index.html의
// ONBOARDING_REQUIRED_FIELD_KEYS가 가리키는 3개와 반드시 일치해야 한다.
const requiredCompletenessCategoryWeight = 20.0

// requiredCompletenessCategoryKeys / optionalCompletenessCategoryKeys —
// 위 가중치를 적용할 카테고리 키 목록. 나머지(선택) 카테고리는 남은
// 비중(100 - 20*3 = 40%)을 균등분배한다.
var requiredCompletenessCategoryKeys = []string{"region", "industry", "companySize"}
var optionalCompletenessCategoryKeys = []string{
	"businessAgeYears", "employeeCount", "revenueAmount",
	"licenses", "certifications", "financials", "trackRecords", "personnel", "directProduction",
}

// completenessConfidenceTables lists every table this feature averages
// confidence(A~D) over. 순서는 응답과 무관 — 그냥 반복 대상 목록.
var completenessConfidenceTables = []string{
	"company_licenses", "company_certifications", "company_financials",
	"company_track_records", "company_personnel",
}

type profileCompletenessCategory struct {
	Label        string `json:"label"`
	Completeness int    `json:"completeness"` // 0~100
}

type profileCompletenessResponse struct {
	Categories             map[string]profileCompletenessCategory `json:"categories"`
	OverallCompleteness    int                                    `json:"overallCompleteness"`
	MatchConfidence        int                                    `json:"matchConfidence"`
	NeedsVerificationCount int                                    `json:"needsVerificationCount"`
}

func (s *Server) handleGetProfileCompleteness(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.currentUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	profile, err := s.getCompanyProfile(r, userID)
	if err != nil {
		s.logger.Error("profile-completeness: profile lookup failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if profile == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "company_profile_required"})
		return
	}

	resp, err := s.computeProfileCompleteness(r.Context(), profile.ID)
	if err != nil {
		s.logger.Error("profile-completeness: compute failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// onboardingMinCompleteness — 2026-08-05 온보딩 재설계에서 확정한 차단
// 기준선(사용자 확인). 프론트도 같은 기준(50)으로 완료 버튼을 잠그지만,
// 그건 UX일 뿐 신뢰 경계가 아니다 — 실제 게이트는 여기, 서버가 최신
// 데이터로 다시 계산해서 확인한다.
const onboardingMinCompleteness = 50

// handleCompleteOnboarding — POST /api/me/onboarding/complete. 최종 확인
// 화면의 "이 정보로 분석을 시작합니다" 버튼이 호출한다. 클라이언트가
// "50% 넘었다"고 자체 판단한 걸 그대로 믿지 않고(다른 서버측 판단들과
// 같은 원칙 — 예: billing.go의 결제금액 재계산) 서버가 completeness를
// 다시 계산해 기준 미달이면 400으로 거부한다. 통과하면
// onboarding_completed_at을 찍어 route() 게이트가 더 이상 이 사용자를
// 온보딩 화면에 붙잡아두지 않게 한다.
func (s *Server) handleCompleteOnboarding(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.currentUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	profile, err := s.getCompanyProfile(r, userID)
	if err != nil {
		s.logger.Error("complete-onboarding: profile lookup failed", "error", err)
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

	completeness, err := s.computeProfileCompleteness(r.Context(), profile.ID)
	if err != nil {
		s.logger.Error("complete-onboarding: completeness compute failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if completeness.OverallCompleteness < onboardingMinCompleteness {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error":               "completeness_below_threshold",
			"overallCompleteness": completeness.OverallCompleteness,
			"threshold":           onboardingMinCompleteness,
		})
		return
	}

	if _, err := s.db.ExecContext(r.Context(),
		`UPDATE company_profiles SET onboarding_completed_at = now() WHERE id = $1 AND onboarding_completed_at IS NULL`,
		profile.ID,
	); err != nil {
		s.logger.Error("complete-onboarding: update failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"overallCompleteness": completeness.OverallCompleteness})
}

// completenessProfileFields — 새로 채점 대상이 된 5개 스칼라 필드+직접생산
// 확인. computeProfileCompleteness가 매번 company_profiles를 직접 조회해
// 가져온다(호출부마다 다른 타입(companyProfileDTO/companyScoringInput)을
// 들고 있어 공통 필드가 region/industry/companySize뿐이라, 매번 호출부를
// 고치는 대신 이 함수 안에서 필요한 필드를 스스로 가져오는 쪽을 택함).
type completenessProfileFields struct {
	Region               sql.NullString
	Industry             []string
	CompanySize          sql.NullString
	BusinessAgeYears     sql.NullFloat64
	EmployeeCount        sql.NullInt64
	RevenueAmount        sql.NullInt64
	DirectProductionCert bool
}

func (s *Server) fetchCompletenessProfileFields(ctx context.Context, profileID string) (completenessProfileFields, error) {
	var f completenessProfileFields
	var industry pq.StringArray
	err := s.db.QueryRowContext(ctx, `
		SELECT region, industry, company_size, business_age_years, employee_count,
		       revenue_amount, direct_production_cert
		FROM company_profiles WHERE id = $1`, profileID,
	).Scan(&f.Region, &industry, &f.CompanySize, &f.BusinessAgeYears, &f.EmployeeCount,
		&f.RevenueAmount, &f.DirectProductionCert)
	f.Industry = []string(industry)
	return f, err
}

// boolCompleteness — 새로 채점하는 5개 스칼라 필드+직접생산확인은 "값이
// 있으면 100%, 없으면 0%"인 단순 존재 체크다(면허/재무처럼 여러 건 등록할
// 수록 올라가는 개념이 아니라 필드 하나짜리라 부분점수 자체가 성립 안 함).
func boolCompleteness(filled bool) int {
	if filled {
		return 100
	}
	return 0
}

// computeProfileCompleteness is the pure-computation core of
// handleGetProfileCompleteness, factored out so growth_analytics.go's
// weekly/monthly snapshot (reports.go) can reuse the exact same formula —
// two separately-maintained completeness definitions would drift apart.
func (s *Server) computeProfileCompleteness(ctx context.Context, profileID string) (profileCompletenessResponse, error) {
	counts := map[string]struct{ total, ab int }{}
	for _, table := range completenessConfidenceTables {
		total, ab, err := s.countConfidenceBucket(ctx, table, profileID)
		if err != nil {
			return profileCompletenessResponse{}, err
		}
		counts[table] = struct{ total, ab int }{total, ab}
	}

	fields, err := s.fetchCompletenessProfileFields(ctx, profileID)
	if err != nil {
		return profileCompletenessResponse{}, err
	}

	// 업종은 부분점수(선택 수/10)가 아니라 이진 판정(하나라도 선택했으면
	// 100%)이다 — 위 패키지 주석 참고, 필수 3개 가중치 60% 보장에 필요.
	industryCompleteness := boolCompleteness(len(fields.Industry) > 0)
	licenseCompleteness := capCompleteness(counts["company_licenses"].total, 1)
	certCompleteness := capCompleteness(counts["company_certifications"].total, 1)
	financialCompleteness := capCompleteness(counts["company_financials"].total, 3)
	trackRecordCompleteness := capCompleteness(counts["company_track_records"].total, 3)
	personnelCompleteness := capCompleteness(counts["company_personnel"].total, 1)

	categories := map[string]profileCompletenessCategory{
		"region":           {Label: "지역", Completeness: boolCompleteness(fields.Region.Valid && fields.Region.String != "")},
		"companySize":      {Label: "기업 규모", Completeness: boolCompleteness(fields.CompanySize.Valid && fields.CompanySize.String != "")},
		"businessAgeYears": {Label: "업력", Completeness: boolCompleteness(fields.BusinessAgeYears.Valid)},
		"employeeCount":    {Label: "직원 수", Completeness: boolCompleteness(fields.EmployeeCount.Valid)},
		"revenueAmount":    {Label: "매출액", Completeness: boolCompleteness(fields.RevenueAmount.Valid)},
		"directProduction": {Label: "직접생산확인", Completeness: boolCompleteness(fields.DirectProductionCert)},
		"industry":         {Label: "업종", Completeness: industryCompleteness},
		"licenses":         {Label: "면허", Completeness: licenseCompleteness},
		"certifications":   {Label: "인증", Completeness: certCompleteness},
		"financials":       {Label: "재무", Completeness: financialCompleteness},
		"trackRecords":     {Label: "수행실적", Completeness: trackRecordCompleteness},
		"personnel":        {Label: "인력", Completeness: personnelCompleteness},
	}

	// overall — 12개 단순평균이 아니라 가중평균(위 패키지 주석 참고).
	// 필수 3개(20%씩, 합 60%) + 선택 9개(합 40%를 균등분배).
	optionalWeight := (100.0 - requiredCompletenessCategoryWeight*float64(len(requiredCompletenessCategoryKeys))) / float64(len(optionalCompletenessCategoryKeys))
	weighted := 0.0
	for _, key := range requiredCompletenessCategoryKeys {
		weighted += float64(categories[key].Completeness) * requiredCompletenessCategoryWeight / 100
	}
	for _, key := range optionalCompletenessCategoryKeys {
		weighted += float64(categories[key].Completeness) * optionalWeight / 100
	}
	overall := int(math.Round(weighted))

	totalItems, totalAB := 0, 0
	for _, c := range counts {
		totalItems += c.total
		totalAB += c.ab
	}
	matchConfidence := 0
	if totalItems > 0 {
		matchConfidence = totalAB * 100 / totalItems
	}
	needsVerificationCount := totalItems - totalAB // CHECK 제약상 confidence는 항상 A/B/C/D 중 하나

	return profileCompletenessResponse{
		Categories:             categories,
		OverallCompleteness:    overall,
		MatchConfidence:        matchConfidence,
		NeedsVerificationCount: needsVerificationCount,
	}, nil
}

// capCompleteness converts a raw count into a 0~100 percentage, capped once
// count reaches target(예: 면허 1개=100%, 재무 3개연도=100%) — 그 이상
// 등록해도 100%를 넘지 않는다.
func capCompleteness(count, target int) int {
	if count >= target {
		return 100
	}
	return count * 100 / target
}

// profileHasNoOptionalData reports whether a profile has zero rows across
// every optional category (면허/인증/재무/실적/인력) — used by
// handleGetNotice to flag a 3분 온보딩 minimal profile's participation
// judgement as "간이 판정" (region/industry/budget only, same as any other
// judgement — see scoring.go — but the user hasn't gone past the signup
// minimum yet, so the UI nudges them to fill in more).
func (s *Server) profileHasNoOptionalData(ctx context.Context, profileID string) (bool, error) {
	query := "SELECT NOT EXISTS (" + "SELECT 1 FROM " + completenessConfidenceTables[0] + " WHERE company_profile_id = $1"
	for _, table := range completenessConfidenceTables[1:] {
		query += " UNION ALL SELECT 1 FROM " + table + " WHERE company_profile_id = $1"
	}
	query += ")"
	var noData bool
	err := s.db.QueryRowContext(ctx, query, profileID).Scan(&noData)
	return noData, err
}

// countConfidenceBucket returns (total row count, count with confidence
// A or B) for a company profile's rows in the given table. table is always
// a literal from completenessConfidenceTables, never request input — same
// safe-interpolation pattern company_licenses.go's shared handler already uses.
func (s *Server) countConfidenceBucket(ctx context.Context, table, profileID string) (total, ab int, err error) {
	query := `SELECT count(*), count(*) FILTER (WHERE confidence IN ('A','B')) FROM ` + table + ` WHERE company_profile_id = $1`
	err = s.db.QueryRowContext(ctx, query, profileID).Scan(&total, &ab)
	return total, ab, err
}
