package api

import (
	"database/sql"
	"testing"
)

func ns(v string) sql.NullString {
	if v == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: v, Valid: true}
}

// TestScoreRegionAuthoritative — 공식 참가가능지역을 authoritative source로 쓰는 지역 판정을
// STEP 5(6개 유형) + STEP 6(false PASS/REVIEW) 기준으로 검증한다. 순수 함수라 DB 불필요.
func TestScoreRegionAuthoritative(t *testing.T) {
	cases := []struct {
		name       string
		auth       regionAuthority
		inferred   string // notices.region(추론값)
		company    string
		wantResult string
		wantGap    string
	}{
		// (A) 공식 참가가능지역 = 부산, 회사 부산 → met
		{"A_official_busan_match", regionAuthority{OfficialRegions: []string{"부산광역시"}, Enriched: true}, "전국", "부산광역시", "met", ""},
		// (B) 공식 = 강원, 회사 강원 → met
		{"B_official_gangwon_match", regionAuthority{OfficialRegions: []string{"강원특별자치도"}, Enriched: true}, "전국", "강원특별자치도", "met", ""},
		// (C) 공식 = 경상남도 함양군(시군), 회사 = 경상남도(도) → 좁아서 REVIEW(단정 금지)
		{"C_official_narrower_review", regionAuthority{OfficialRegions: []string{"경상남도 함양군"}, Enriched: true}, "전국", "경상남도", "needs_confirmation", ""},
		// (C') 회사가 그 시군 소재(회사 더 좁음) → met
		{"C2_company_in_province", regionAuthority{OfficialRegions: []string{"경상남도"}, Enriched: true}, "전국", "경상남도 함양군", "met", ""},
		// (D) 공식 복수, 회사가 그 중 하나 → met
		{"D_official_multi_match", regionAuthority{OfficialRegions: []string{"대전광역시", "세종특별자치시", "충청북도"}, Enriched: true}, "전국", "세종특별자치시", "met", ""},
		// (D') 공식 복수인데 회사가 전혀 무관 → not_met (STEP6: false PASS 아님)
		{"D2_official_multi_mismatch", regionAuthority{OfficialRegions: []string{"대전광역시", "세종특별자치시"}, Enriched: true}, "전국", "서울특별시", "not_met", ""},
		// (E) enrichment 완료 + 제한지역 없음 → 전국 확정 met (STEP6: 실제 전국은 REVIEW로 안 내림)
		{"E_enriched_nationwide", regionAuthority{OfficialRegions: nil, Enriched: true}, "전국", "부산광역시", "met", ""},
		// (F) enrichment 미실행 + 추론값 '전국' → met 단정 금지 → insufficient_data (핵심 false PASS 제거)
		{"F_unenriched_inferred_nationwide", regionAuthority{OfficialRegions: nil, Enriched: false}, "전국", "부산광역시", "insufficient_data", "notice"},
		// (F') enrichment 미실행 + 추론값도 없음 → notice-gap
		{"F2_unenriched_no_region", regionAuthority{OfficialRegions: nil, Enriched: false}, "", "부산광역시", "insufficient_data", "notice"},
		// 공식 지역 있는데 회사 지역 미기재 → company-gap
		{"official_but_no_company", regionAuthority{OfficialRegions: []string{"부산광역시"}, Enriched: true}, "전국", "", "insufficient_data", "company"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			result, _, gap := scoreRegion(c.auth, ns(c.inferred), ns(c.company))
			if result != c.wantResult {
				t.Errorf("result=%q, want %q", result, c.wantResult)
			}
			if gap != c.wantGap {
				t.Errorf("dataGapSide=%q, want %q", gap, c.wantGap)
			}
		})
	}
}

// TestScoreRegionNoFalsePassFromInference — STEP6 핵심 회귀: 과거엔 notices.region='전국'만으로
// 지역 met(PASS)를 만들었다. 이제 enrichment 미실행이면 절대 met가 아니어야 한다.
func TestScoreRegionNoFalsePassFromInference(t *testing.T) {
	for _, inferred := range []string{"전국", "부산광역시", "경상남도"} {
		result, _, _ := scoreRegion(regionAuthority{Enriched: false}, ns(inferred), ns("서울특별시"))
		if result == "met" {
			t.Errorf("추론 region=%q(enrichment 미실행)만으로 met가 나오면 안 됨(false PASS)", inferred)
		}
	}
}

func TestMatchCompanyRegion(t *testing.T) {
	cases := []struct {
		official []string
		company  string
		want     string
	}{
		{[]string{"부산광역시"}, "부산광역시", regionMatchFull},
		{[]string{"경상남도"}, "경상남도 함양군", regionMatchFull},        // 회사가 허용 도 안의 시군
		{[]string{"경상남도 함양군"}, "경상남도", regionMatchProvince},    // 허용이 더 좁음 → REVIEW
		{[]string{"부산광역시"}, "서울특별시", regionMatchNone},           // 무관
		{[]string{"대전광역시", "세종특별자치시"}, "세종특별자치시", regionMatchFull},
	}
	for _, c := range cases {
		if got := matchCompanyRegion(c.official, c.company); got != c.want {
			t.Errorf("matchCompanyRegion(%v, %q)=%q, want %q", c.official, c.company, got, c.want)
		}
	}
}
