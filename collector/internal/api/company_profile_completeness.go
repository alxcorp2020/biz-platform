// 프로필 완성도/신뢰도 요약. company_profiles 자체는 프로필 존재 여부만
// 게이트로 쓰고(기본정보는 존재하면 항상 100%), 나머지 6개 카테고리는
// 각각 등록된 데이터 양을 0~100%로 환산한다. "완성도"의 기준치(면허/인증
// 1개, 재무 3개연도, 실적 3건 = 100%)는 실제 규정이 아니라 사용자와
// 합의한 예시 cap이다 — 다른 곳의 smallBusinessBudgetCap류 휴리스틱과
// 같은 성격.
package api

import (
	"context"
	"net/http"
)

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
	OverallCompleteness    int                                     `json:"overallCompleteness"`
	MatchConfidence        int                                     `json:"matchConfidence"`
	NeedsVerificationCount int                                     `json:"needsVerificationCount"`
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

	resp, err := s.computeProfileCompleteness(r.Context(), profile.ID, profile.Industry)
	if err != nil {
		s.logger.Error("profile-completeness: compute failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// computeProfileCompleteness is the pure-computation core of
// handleGetProfileCompleteness, factored out so growth_analytics.go's
// weekly/monthly snapshot (reports.go) can reuse the exact same formula —
// two separately-maintained completeness definitions would drift apart.
func (s *Server) computeProfileCompleteness(ctx context.Context, profileID string, industry []string) (profileCompletenessResponse, error) {
	counts := map[string]struct{ total, ab int }{}
	for _, table := range completenessConfidenceTables {
		total, ab, err := s.countConfidenceBucket(ctx, table, profileID)
		if err != nil {
			return profileCompletenessResponse{}, err
		}
		counts[table] = struct{ total, ab int }{total, ab}
	}

	industryCompleteness := len(industry) * 100 / len(industryGroups)
	if industryCompleteness > 100 {
		industryCompleteness = 100
	}
	licenseCompleteness := capCompleteness(counts["company_licenses"].total, 1)
	certCompleteness := capCompleteness(counts["company_certifications"].total, 1)
	financialCompleteness := capCompleteness(counts["company_financials"].total, 3)
	trackRecordCompleteness := capCompleteness(counts["company_track_records"].total, 3)
	personnelCompleteness := capCompleteness(counts["company_personnel"].total, 1)

	categories := map[string]profileCompletenessCategory{
		"basic":          {Label: "기본정보", Completeness: 100},
		"industry":       {Label: "업종", Completeness: industryCompleteness},
		"licenses":       {Label: "면허", Completeness: licenseCompleteness},
		"certifications": {Label: "인증", Completeness: certCompleteness},
		"financials":     {Label: "재무", Completeness: financialCompleteness},
		"trackRecords":   {Label: "수행실적", Completeness: trackRecordCompleteness},
		"personnel":      {Label: "인력", Completeness: personnelCompleteness},
	}

	sum := 0
	for _, c := range categories {
		sum += c.Completeness
	}
	overall := sum / len(categories)

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
