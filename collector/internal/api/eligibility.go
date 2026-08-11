// 기업 적합성 규칙 엔진 1차 버전 (스펙 5단계 착수분).
//
// 문서 분석(4단계)이 아직 없어 eligibility_conditions는 비어 있다. 이 파일은
// 공고의 구조화 필드(region/industry/budget_amount)만으로 지역·업종·예산
// 규모 3가지를 판정하고, 판정마다 그 근거가 되는 eligibility_conditions
// 행을 자동 생성해 eligibility_evaluations의 NOT NULL 외래키를 충족시킨다.
// 문서 분석이 붙으면 실제 파싱된 조건이 이 자동 생성 행을 대체하게 된다.
//
// 원칙(스펙): 이 결과는 참가 가능 여부를 "확정"하지 않는다 — 애매하거나
// 데이터가 없으면 반드시 insufficient_data/needs_confirmation으로 표시하고,
// 응답에는 항상 원문 확인을 요구하는 disclaimer를 포함한다.
package api

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"strings"

	"github.com/lib/pq"
)

const (
	ruleEngineVersion = "structured-fields-v1"
	regionNationwide  = "전국"
	smallBusinessSize = "소기업"
	// smallBusinessBudgetCap: 소기업이 참여하기엔 예산 규모가 크다고 볼 수
	// 있는 예시 기준선(10억 원). 실제 참가자격 규정은 아직 모델링하지
	// 않았으므로 확정 판정이 아니라 "확인 필요" 플래그로만 쓴다.
	smallBusinessBudgetCap = int64(1_000_000_000)

	evalDisclaimer = "이 결과는 공고의 구조화 항목만으로 산출한 1차 참고용 판정이며, 확정 판정이 아닙니다. " +
		"반드시 공식 공고문 원문을 확인하세요."
)

// industryRawToGroup maps every distinct notices.industry value observed in
// real g2b data (2026-07-29 조회, 35종) to a broad selectable category. g2b's
// industry field is free text, not a standard classification — this mapping
// exists so a company can multi-select a handful of broad categories instead
// of every raw string, and so eligibility matching can OR across the group's
// raw values instead of requiring an exact string match (겸업 반영: 소상공인은
// 보통 업종을 2개 이상 등록한다).
//
// Keys are trimmed — real g2b rows have inconsistent leading/trailing spaces
// (e.g. " 폐기물 처리 "), so notice industry values are trimmed before lookup.
var industryRawToGroup = map[string]string{
	"SW 및 시스템 개발": "ICT/SW",
	"시스템 운영환경 구축": "ICT/SW",
	"DB구축 및 자료입력": "ICT/SW",
	"디지털콘텐츠 개발":   "ICT/SW",
	"ICT사업 컨설팅":   "ICT/SW",
	"통신서비스":       "ICT/SW",

	"학술연구서비스":        "연구/조사/컨설팅",
	"시장 및 여론조사":      "연구/조사/컨설팅",
	"문화재 조사/발굴 및 수리": "연구/조사/컨설팅",
	"기술시험,검사 및 분석":   "연구/조사/컨설팅",

	"설계": "설계/감리/CM",
	"감리": "설계/감리/CM",
	"CM": "설계/감리/CM",
	"측량": "설계/감리/CM",

	"행사 기획 및 대행":   "행사/홍보/미디어",
	"매체제작":         "행사/홍보/미디어",
	"홍보 및 마케팅":     "행사/홍보/미디어",
	"전시관 및 홍보관 설치": "행사/홍보/미디어",
	"디자인":          "행사/홍보/미디어",

	"시설물관리, 청소 등": "시설관리/유지보수",
	"운영 및 유지관리":   "시설관리/유지보수",
	"수리":          "시설관리/유지보수",
	"임대":          "시설관리/유지보수",

	"폐기물 처리":  "환경/폐기물",
	"폐기물 재활용": "환경/폐기물",

	"운송서비스": "생활서비스",
	"여행서비스": "생활서비스",
	"숙박서비스": "생활서비스",
	"음식서비스": "생활서비스",
	"보건서비스": "생활서비스",

	"보험서비스":  "전문서비스",
	"회계서비스":  "전문서비스",
	"사업장 위탁": "전문서비스",

	"교육서비스": "교육",

	"기타": "기타",
}

// industryGroupToRaws — industryRawToGroup의 역인덱스(그룹 → raw값 목록).
// 검색 필터(handleListNotices)가 그룹 업종으로 들어올 때 쓴다: notices.industry에는
// raw값만 저장되고 그룹명은 결코 저장되지 않으므로 "n.industry = 그룹명" 정확일치는
// 항상 0건이었다(맞춤공고 "결과 보기"의 업종 필터가 무력화되던 원인). 이 역맵으로
// 그룹을 raw값 집합으로 확장해 ANY 매칭한다(2026-08-08 Phase 0).
var industryGroupToRaws = func() map[string][]string {
	m := map[string][]string{}
	for raw, group := range industryRawToGroup {
		m[group] = append(m[group], raw)
	}
	return m
}()

// industryGroups is the fixed, ordered list of selectable multi-select
// options (order matters for a stable UI — map iteration order does not).
var industryGroups = []string{
	"ICT/SW",
	"연구/조사/컨설팅",
	"설계/감리/CM",
	"행사/홍보/미디어",
	"시설관리/유지보수",
	"환경/폐기물",
	"생활서비스",
	"전문서비스",
	"교육",
	"기타",
}

func isKnownIndustryGroup(group string) bool {
	for _, g := range industryGroups {
		if g == group {
			return true
		}
	}
	return false
}

type eligibilityItem struct {
	Category string `json:"category"`
	Result   string `json:"result"`
	Reason   string `json:"reason"`
}

func (s *Server) handleEvaluateNotice(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.currentUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	ctx := r.Context()
	noticeID := r.PathValue("id")

	profile, err := s.getCompanyProfile(r, userID)
	if err != nil {
		s.logger.Error("evaluate: profile lookup failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if profile == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "company_profile_required"})
		return
	}
	profileID := profile.ID
	var companyRegion, companySize sql.NullString
	if profile.Region != nil {
		companyRegion = sql.NullString{String: *profile.Region, Valid: true}
	}
	if profile.CompanySize != nil {
		companySize = sql.NullString{String: *profile.CompanySize, Valid: true}
	}
	companyIndustry := pq.StringArray(profile.Industry)

	var noticeType string
	var noticeRegion, noticeIndustry sql.NullString
	var budgetAmount sql.NullInt64
	var currentVersion int
	var industryRestricted sql.NullBool
	err = s.db.QueryRowContext(ctx,
		`SELECT notice_type, region, industry, budget_amount, current_version, industry_restricted FROM notices WHERE id = $1`, noticeID,
	).Scan(&noticeType, &noticeRegion, &noticeIndustry, &budgetAmount, &currentVersion, &industryRestricted)
	if err == sql.ErrNoRows {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "notice_not_found"})
		return
	}
	if err != nil {
		s.logger.Error("evaluate: notice lookup failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}

	var versionID string
	err = s.db.QueryRowContext(ctx,
		`SELECT id FROM notice_versions WHERE notice_id = $1 AND version_number = $2`,
		noticeID, currentVersion,
	).Scan(&versionID)
	if err != nil {
		s.logger.Error("evaluate: current version lookup failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}

	var items []eligibilityItem
	var grade string
	var jvRecommended bool
	var jvReason string
	if noticeType == noticeTypeSupportProgram {
		// 지원사업은 procurement 3-카테고리(지역/업종/예산) 자동판정이
		// 성립하지 않는다 — scoring.go의 supportProgramScore 주석 참고.
		// 이 영속(evaluate) 경로도 같은 원칙을 따른다.
		item, err := s.evaluateSupportProgram(ctx, versionID, profileID)
		if err != nil {
			s.logger.Error("evaluate: support program check failed", "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
			return
		}
		items = []eligibilityItem{item}
		grade = gradeNeedsConfirmation
	} else {
		regionItem, err := s.evaluateRegion(ctx, versionID, profileID, noticeRegion, companyRegion)
		if err != nil {
			s.logger.Error("evaluate: region check failed", "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
			return
		}
		industryItem, err := s.evaluateIndustry(ctx, versionID, profileID, noticeIndustry, []string(companyIndustry), nullBoolPtr(industryRestricted))
		if err != nil {
			s.logger.Error("evaluate: industry check failed", "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
			return
		}
		budgetItem, err := s.evaluateBudgetSize(ctx, versionID, profileID, budgetAmount, companySize)
		if err != nil {
			s.logger.Error("evaluate: budget size check failed", "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
			return
		}

		items = []eligibilityItem{regionItem, industryItem, budgetItem}

		trackRecordMax, err := s.fetchTrackRecordMaxAmount(ctx, profileID)
		if err != nil {
			s.logger.Error("evaluate: track record max amount query failed", "error", err)
		}
		categories := make([]categoryScore, len(items))
		for i, it := range items {
			categories[i] = categoryScore{Category: it.Category, Result: it.Result, Reason: it.Reason}
		}
		grade = gradeFromCategories(categories)
		jvRecommended, jvReason = computeJointVentureSignal(budgetAmount, trackRecordMax)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"noticeId":                noticeID,
		"companyProfileId":        profileID,
		"overallResult":           overallResult(items),
		"grade":                   grade,
		"jointVentureRecommended": jvRecommended,
		"jointVentureReason":      jvReason,
		"items":                   items,
		"disclaimer":              evalDisclaimer,
	})
}

// overallResult picks the worst-case outcome across items: any not_met wins,
// otherwise any needs_confirmation/insufficient_data, otherwise met.
func overallResult(items []eligibilityItem) string {
	hasNeedsConfirmation := false
	for _, it := range items {
		if it.Result == "not_met" {
			return "not_met"
		}
		if it.Result == "needs_confirmation" || it.Result == "insufficient_data" {
			hasNeedsConfirmation = true
		}
	}
	if hasNeedsConfirmation {
		return "needs_confirmation"
	}
	return "met"
}

// regionAuthority — 공고의 "지역 제한" 권위 데이터(2026-08-11 authoritative source 전환).
// 공식 참가가능지역 API(getBidPblancListInfoPrtcptPsblRgn) 결과가 authoritative이고,
// notices.region(수집기가 bidPrtcptLmtYn Y/N로 추론한 값)은 신뢰할 수 없어 판정 단정에 쓰지
// 않는다 — 공식 데이터가 없을 때 "지역 정보 자체가 없음(notice-gap)"과 구분하는 용도로만 남긴다.
//
//	OfficialRegions: 공식 참가가능지역 목록(비어있으면 없음)
//	Enriched:        공식 지역 enrichment가 확정 실행됨(notice_versions.enrichment_status ∈ {completed, not_found})
//	                 — false면 미실행/불명확(error·NULL)이라 "전국"으로 단정하지 않는다.
type regionAuthority struct {
	OfficialRegions []string
	Enriched        bool
}

// regionEnrichedFromStatus — enrichment_status가 지역 판정을 확정할 수 있는 상태인지.
// completed(지역 또는 면허 조회됨)/not_found(제한 없음 확정) = 확정 실행. error·NULL = 미실행/불명확.
func regionEnrichedFromStatus(status sql.NullString) bool {
	return status.Valid && (status.String == "completed" || status.String == "not_found")
}

const (
	regionMatchFull     = "full"     // 회사 지역이 허용지역에 확실히 포함 → met
	regionMatchProvince = "province" // 같은 도지만 허용지역이 더 좁음(시군) → REVIEW(단정 금지)
	regionMatchNone     = "none"     // 지역 무관 → not_met
)

// scoreRegion — 공식 참가가능지역을 authoritative source로 지역 조건을 판정한다(2026-08-11).
//
//	(1) 공식 참가가능지역이 있으면 → 회사 지역과 대조(포함=met, 같은 도 좁은지역=REVIEW, 무관=not_met)
//	(2) enrichment 완료 + 제한지역 없음 → 전국 확정(met)
//	(3) enrichment 미실행/불명확 → 추론값(notices.region)만으로 전국 PASS 단정 금지 → insufficient_data
//
// inferredRegion(notices.region)은 (3)에서 "공고에 지역 정보 자체가 없음(notice-gap)"과 구분하는
// 데만 쓴다. dataGapSide: "notice"=공고 쪽 데이터 공백(사용자가 못 고침), "company"=기업 프로필 공백.
func scoreRegion(auth regionAuthority, inferredRegion, companyRegion sql.NullString) (result, reason, dataGapSide string) {
	if len(auth.OfficialRegions) > 0 {
		if !companyRegion.Valid || strings.TrimSpace(companyRegion.String) == "" {
			return "insufficient_data", "기업 프로필에 지역 정보가 없어 지역 조건을 판정할 수 없습니다.", "company"
		}
		switch matchCompanyRegion(auth.OfficialRegions, companyRegion.String) {
		case regionMatchFull:
			return "met", fmt.Sprintf("공고 참가가능지역에 기업 지역(%s)이 포함됩니다.", strings.TrimSpace(companyRegion.String)), ""
		case regionMatchProvince:
			return "needs_confirmation", fmt.Sprintf("공고 참가가능지역(%s)이 기업 지역(%s)보다 좁아 실제 소재지 확인이 필요합니다.", strings.Join(auth.OfficialRegions, ", "), strings.TrimSpace(companyRegion.String)), ""
		default:
			return "not_met", fmt.Sprintf("공고 참가가능지역(%s)에 기업 지역(%s)이 포함되지 않습니다.", strings.Join(auth.OfficialRegions, ", "), strings.TrimSpace(companyRegion.String)), ""
		}
	}
	if auth.Enriched {
		return "met", "공고에 참가가능지역 제한이 없어 전국 대상입니다(공식 확인).", ""
	}
	// (3) 공식 enrichment 미실행 — 추론값만으로 전국 PASS를 단정하지 않는다.
	if !inferredRegion.Valid || inferredRegion.String == "" {
		return "insufficient_data", "공고에 지역 정보가 없어 지역 조건을 판정할 수 없습니다.", "notice"
	}
	return "insufficient_data", "공식 참가가능지역 정보가 아직 확인되지 않아 지역 조건을 단정할 수 없습니다. 원문 공고를 확인해주세요.", "notice"
}

// matchCompanyRegion — 회사 지역이 공식 참가가능지역 목록에 부합하는지(full/province/none).
// 공식 지역은 시도("경상남도","부산광역시") 또는 시군("경상남도 함양군") 단위가 섞여 온다.
//   - 정확 일치 또는 회사가 허용 도 안의 더 좁은 지역(회사="경남 함양군", 허용="경상남도") → full(met)
//   - 같은 도지만 허용지역이 더 좁은 시군(회사="경상남도", 허용="경상남도 함양군") → province(REVIEW):
//     회사가 그 시군 소재인지 알 수 없어 단정하지 않는다(모르면 REVIEW 원칙)
//   - 도 자체가 겹치지 않음 → none(not_met)
func matchCompanyRegion(official []string, company string) string {
	c := strings.TrimSpace(company)
	if c == "" {
		return regionMatchNone
	}
	cProv := regionProvinceToken(c)
	provinceOverlap := false
	for _, o := range official {
		o = strings.TrimSpace(o)
		if o == "" {
			continue
		}
		if o == c || strings.HasPrefix(c, o+" ") {
			return regionMatchFull
		}
		if strings.HasPrefix(o, c+" ") || regionProvinceToken(o) == cProv {
			provinceOverlap = true
		}
	}
	if provinceOverlap {
		return regionMatchProvince
	}
	return regionMatchNone
}

// regionProvinceToken — 지역 문자열의 시도(첫 공백 이전) 부분만 반환.
func regionProvinceToken(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, ' '); i >= 0 {
		return s[:i]
	}
	return s
}

// regionAuthoritiesByVersions — 여러 현재버전의 공식 참가가능지역 + enrichment 실행여부를
// 한 번에 로드한다(공고별 개별 조회 N+1 방지 — dashboard/추천 등 루프 호출부용).
func (s *Server) regionAuthoritiesByVersions(ctx context.Context, versionIDs []string) (map[string]regionAuthority, error) {
	out := make(map[string]regionAuthority, len(versionIDs))
	if len(versionIDs) == 0 {
		return out, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT nv.id, nv.enrichment_status,
		       COALESCE(array_agg(pr.region_name ORDER BY pr.sort_no) FILTER (WHERE pr.region_name IS NOT NULL), '{}')
		FROM notice_versions nv
		LEFT JOIN notice_participation_regions pr ON pr.notice_version_id = nv.id
		WHERE nv.id = ANY($1)
		GROUP BY nv.id, nv.enrichment_status`, pq.Array(versionIDs))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var status sql.NullString
		var regions pq.StringArray
		if err := rows.Scan(&id, &status, &regions); err != nil {
			return nil, err
		}
		out[id] = regionAuthority{
			OfficialRegions: []string(regions),
			Enriched:        regionEnrichedFromStatus(status),
		}
	}
	return out, rows.Err()
}

// regionAuthoritiesByNoticeIDs — 여러 공고(현재버전)의 지역 권위 데이터를 공고ID 키로 로드.
// notice_versions를 조인하지 않는 스캔(대시보드 추천 등)이 공고ID만으로 쓸 수 있게 한다.
func (s *Server) regionAuthoritiesByNoticeIDs(ctx context.Context, noticeIDs []string) (map[string]regionAuthority, error) {
	out := make(map[string]regionAuthority, len(noticeIDs))
	if len(noticeIDs) == 0 {
		return out, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT n.id, nv.enrichment_status,
		       COALESCE(array_agg(pr.region_name ORDER BY pr.sort_no) FILTER (WHERE pr.region_name IS NOT NULL), '{}')
		FROM notices n
		JOIN notice_versions nv ON nv.notice_id = n.id AND nv.version_number = n.current_version
		LEFT JOIN notice_participation_regions pr ON pr.notice_version_id = nv.id
		WHERE n.id = ANY($1)
		GROUP BY n.id, nv.enrichment_status`, pq.Array(noticeIDs))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var status sql.NullString
		var regions pq.StringArray
		if err := rows.Scan(&id, &status, &regions); err != nil {
			return nil, err
		}
		out[id] = regionAuthority{
			OfficialRegions: []string(regions),
			Enriched:        regionEnrichedFromStatus(status),
		}
	}
	return out, rows.Err()
}

func (s *Server) evaluateRegion(ctx context.Context, versionID, profileID string, noticeRegion, companyRegion sql.NullString) (eligibilityItem, error) {
	auths, err := s.regionAuthoritiesByVersions(ctx, []string{versionID})
	if err != nil {
		return eligibilityItem{}, err
	}
	result, reason, _ := scoreRegion(auths[versionID], noticeRegion, companyRegion)

	conditionID, err := s.findOrCreateAutoCondition(ctx, versionID,
		"지역", "auto:region", "eq", nsOrEmpty(noticeRegion),
		"공고 API 구조화 필드(region) 자동 추출 — 문서 미분석, 1차 참고용")
	if err != nil {
		return eligibilityItem{}, err
	}
	if err := s.recordEvaluation(ctx, profileID, versionID, conditionID, result, reason); err != nil {
		return eligibilityItem{}, err
	}
	return eligibilityItem{Category: "지역", Result: result, Reason: reason}, nil
}

// scoreIndustry is the pure decision logic behind evaluateIndustry — OR-matches:
// a company can select multiple broad industry groups (겸업 반영, e.g.
// 사업자등록증에 마케팅업 + 통신판매업 둘 다 등록된 경우), so it's "met" the
// moment the notice's industry group is any one of them — not "met" only if
// every selected group matches.
// scoreIndustry의 세 번째 반환값(dataGapSide)은 scoreRegion과 같은 원칙:
// "notice"는 공고 자체에 업종 정보가 없는 경우(사용자가 고칠 수 없음),
// "company"는 기업 프로필에 업종 정보가 없는 경우(사용자가 "내 프로필"에서
// 채우면 해결됨). insufficient_data가 아닌 경우(met/needs_confirmation)엔
// 항상 빈 문자열.
//
// "met"(=참여 권장의 근거)은 오직 "공고 업종이 회사가 선택한 업종과 실제로
// 일치"할 때만 준다(2026-08-08 개선). 이전엔 업종 제한 없는 공고(indstrytyLmtYn=N,
// 열린 공고의 ~25%)를 무조건 met으로 봤는데, "참가 가능"과 "추천할 만큼 관련 있음"은
// 다르다 — 음식점이 건설·IT 용역에 참가 자체는 가능해도 추천은 아니다. 그 자동충족이
// 지역(전국)·예산만 맞으면 업종 무관하게 "참여 권장"을 남발해 추천공고가 무관하게
// 넘치는 원인이었다. 이제 제한 없는 공고는 일치가 없으면 "확인 필요(참여 가능하나
// 관련 낮음)"로 내려 추천에서 빠지되, not_met(참여 곤란)까진 아니므로 검색·판정엔
// 그대로 노출된다.
// "기타"는 조달 카테고리에 안 맞는 업종이 떨어지는 미분류 버킷이라 관련성 신호로
// 보지 않는다("기타"↔"기타" 우연 일치를 met에서 제외).
func scoreIndustry(noticeIndustry sql.NullString, companyGroups []string, industryRestricted *bool) (result, reason, dataGapSide string) {
	noticeRaw := strings.TrimSpace(noticeIndustry.String)

	// 데이터 부족은 어떤 규칙보다 먼저 판정한다(제한 유무와 무관).
	switch {
	case !noticeIndustry.Valid || noticeRaw == "":
		return "insufficient_data", "공고에 업종 정보가 없어 업종 조건을 판정할 수 없습니다.", "notice"
	case len(companyGroups) == 0:
		return "insufficient_data", "기업 프로필에 업종 정보가 없어 판정할 수 없습니다.", "company"
	}

	// Phase 2b — 회사가 선택한 업종을 "조달청 중분류 집합"으로 전개해 공고
	// 중분류(noticeRaw)와 직접 비교한다. 신규 값(조달청 중분류명)은 그대로 쓰고,
	// 레거시 10그룹명(마이그레이션 2c 이전 기존 회사)은 그 그룹의 중분류들로
	// 전개한다 — 전개 결과가 기존 그룹 매칭과 동치라 하위호환이 유지된다.
	// 단, "기타"는 미분류라 실제 일치로 치지 않는다.
	if noticeRaw != "기타" {
		effective := expandCompanyIndustries(companyGroups)
		if effective[noticeRaw] {
			return "met", fmt.Sprintf("공고 업종(%s)이 기업이 선택한 업종과 일치합니다.", noticeRaw), ""
		}
	}

	// 실제 일치가 아님. 제한 유무로 안내 문구만 달리하되, 어느 쪽도 "참여 권장"까진
	// 올리지 않는다(추천은 실제 업종 일치일 때만).
	if industryRestricted != nil && !*industryRestricted {
		return "needs_confirmation", fmt.Sprintf(
			"이 공고는 업종 제한이 없어 참여 자체는 가능하지만, 공고 업종(%s)이 기업이 선택한 업종(%s)과 "+
				"직접 관련은 낮습니다. 관련 있다고 판단되면 원문에서 직접 확인하세요.",
			noticeRaw, strings.Join(companyGroups, ", ")), ""
	}
	restrictNote := ""
	if industryRestricted != nil && *industryRestricted {
		restrictNote = "이 공고는 업종 제한이 있습니다. "
	}
	return "needs_confirmation", fmt.Sprintf(
		"%s공고 업종(%s)이 기업이 선택한 업종(%s)에 없습니다. 조달청 분류가 참가자격과 정확히 "+
			"같진 않아 실제로는 겹칠 수 있으니 원문에서 직접 확인하세요.",
		restrictNote, noticeRaw, strings.Join(companyGroups, ", ")), ""
}

// expandCompanyIndustries — 회사가 선택한 업종 값들을 조달청 "중분류 이름 집합"으로
// 전개한다. 값이 레거시 10그룹명이면(industryGroupToRaws에 키로 존재) 그 그룹의
// 중분류들로 펼치고, 아니면(조달청 중분류명 = 신규 선택) 그대로 넣는다. 이렇게
// 하면 마이그레이션(2c) 전 기존 그룹 값과 신규 중분류 값을 한 로직으로 매칭한다.
func expandCompanyIndustries(values []string) map[string]bool {
	out := map[string]bool{}
	for _, v := range values {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		if raws, isLegacyGroup := industryGroupToRaws[v]; isLegacyGroup {
			for _, r := range raws {
				out[strings.TrimSpace(r)] = true
			}
		} else {
			out[v] = true
		}
	}
	return out
}

// nullBoolPtr — 스캔한 sql.NullBool을 *bool로(NULL이면 nil). 업종제한 등
// nullable BOOLEAN 컬럼을 noticeScoringInput.IndustryRestricted로 옮길 때 쓴다.
func nullBoolPtr(nb sql.NullBool) *bool {
	if nb.Valid {
		b := nb.Bool
		return &b
	}
	return nil
}

func (s *Server) evaluateIndustry(ctx context.Context, versionID, profileID string, noticeIndustry sql.NullString, companyGroups []string, industryRestricted *bool) (eligibilityItem, error) {
	noticeRaw := strings.TrimSpace(noticeIndustry.String)
	result, reason, _ := scoreIndustry(noticeIndustry, companyGroups, industryRestricted)

	conditionID, err := s.findOrCreateAutoCondition(ctx, versionID,
		"업종", "auto:industry", "eq", noticeRaw,
		"공고 API 구조화 필드(industry) 자동 추출 — 문서 미분석, 1차 참고용")
	if err != nil {
		return eligibilityItem{}, err
	}
	if err := s.recordEvaluation(ctx, profileID, versionID, conditionID, result, reason); err != nil {
		return eligibilityItem{}, err
	}
	return eligibilityItem{Category: "업종", Result: result, Reason: reason}, nil
}

func containsString(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

// scoreBudgetSize is the pure decision logic behind evaluateBudgetSize.
// 세 번째 반환값(dataGapSide)은 scoreIndustry/scoreRegion과 같은 원칙 —
// 예전엔 "예산 없음"과 "기업 규모 없음"을 한 case로 합쳐놔서 어느 쪽
// 데이터가 빈 건지 구분이 아예 불가능했다. budgetAmount는 공고쪽(notice),
// companySize는 회사쪽(company) 데이터라 순서대로 나눠 확인한다.
func scoreBudgetSize(budgetAmount sql.NullInt64, companySize sql.NullString) (result, reason, dataGapSide string) {
	switch {
	case !budgetAmount.Valid:
		return "insufficient_data", "공고에 예산 정보가 없어 예산 규모 조건을 판정할 수 없습니다.", "notice"
	case !companySize.Valid || companySize.String == "":
		return "insufficient_data", "기업 프로필에 기업 규모 정보가 없어 판정할 수 없습니다.", "company"
	case companySize.String == smallBusinessSize && budgetAmount.Int64 >= smallBusinessBudgetCap:
		return "needs_confirmation", fmt.Sprintf(
			"기업 규모가 %s인데 공고 예산(%d원)이 %d원 이상으로 큽니다. "+
				"실제 참가자격 규정을 확인하지 않은 예시 기준이니, 공고문에서 참가자격을 직접 확인하세요.",
			smallBusinessSize, budgetAmount.Int64, smallBusinessBudgetCap), ""
	default:
		return "met", "예산 규모 관련 확인된 제한 사항이 없습니다.", ""
	}
}

func (s *Server) evaluateBudgetSize(ctx context.Context, versionID, profileID string, budgetAmount sql.NullInt64, companySize sql.NullString) (eligibilityItem, error) {
	result, reason, _ := scoreBudgetSize(budgetAmount, companySize)

	conditionID, err := s.findOrCreateAutoCondition(ctx, versionID,
		"예산규모", "auto:budget_size", "gte", fmt.Sprintf("%d", smallBusinessBudgetCap),
		"공고 API 구조화 필드(budget_amount) + 기업 규모 자동 대조 — 문서 미분석, 1차 참고용")
	if err != nil {
		return eligibilityItem{}, err
	}
	if err := s.recordEvaluation(ctx, profileID, versionID, conditionID, result, reason); err != nil {
		return eligibilityItem{}, err
	}
	return eligibilityItem{Category: "예산 규모", Result: result, Reason: reason}, nil
}

// evaluateSupportProgram persists a single "종합판정" condition/evaluation
// for notice_type='support_program' — see scoring.go's supportProgramScore
// doc comment for why a single always-needs_confirmation category replaces
// procurement's 3-category(지역/업종/예산) automated 판정 for this notice type.
func (s *Server) evaluateSupportProgram(ctx context.Context, versionID, profileID string) (eligibilityItem, error) {
	conditionID, err := s.findOrCreateAutoCondition(ctx, versionID,
		"종합판정", "auto:support_program", "n/a", "",
		"지원사업은 procurement 전용 자동판정 기준(예산규모/업종분류)이 적용되지 않아 항상 확인 필요로 처리")
	if err != nil {
		return eligibilityItem{}, err
	}
	if err := s.recordEvaluation(ctx, profileID, versionID, conditionID, "needs_confirmation", supportProgramReviewReason); err != nil {
		return eligibilityItem{}, err
	}
	return eligibilityItem{Category: "종합판정", Result: "needs_confirmation", Reason: supportProgramReviewReason}, nil
}

// findOrCreateAutoCondition reuses the auto-generated eligibility_conditions
// row for this notice version + check (keyed by condition_name) instead of
// inserting a fresh one on every evaluate call.
func (s *Server) findOrCreateAutoCondition(ctx context.Context, versionID, category, conditionName, operator, thresholdValue, sourceText string) (string, error) {
	var id string
	err := s.db.QueryRowContext(ctx,
		`SELECT id FROM eligibility_conditions WHERE notice_version_id = $1 AND condition_name = $2`,
		versionID, conditionName,
	).Scan(&id)
	if err == nil {
		return id, nil
	}
	if err != sql.ErrNoRows {
		return "", err
	}

	err = s.db.QueryRowContext(ctx, `
		INSERT INTO eligibility_conditions
			(notice_version_id, category, condition_name, operator, threshold_value, is_required, source_text, confidence, review_status)
		VALUES ($1,$2,$3,$4,$5,false,$6,0.50,'pending')
		RETURNING id`,
		versionID, category, conditionName, operator, thresholdValue, sourceText,
	).Scan(&id)
	return id, err
}

func (s *Server) recordEvaluation(ctx context.Context, profileID, versionID, conditionID, result, reason string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO eligibility_evaluations
			(company_profile_id, notice_version_id, condition_id, result, reason, rule_engine_version)
		VALUES ($1,$2,$3,$4,$5,$6)`,
		profileID, versionID, conditionID, result, reason, ruleEngineVersion)
	return err
}

func nsOrEmpty(n sql.NullString) string {
	if !n.Valid {
		return ""
	}
	return n.String
}
