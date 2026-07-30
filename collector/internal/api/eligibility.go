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

	var profileID string
	var companyRegion, companySize sql.NullString
	var companyIndustry pq.StringArray
	err := s.db.QueryRowContext(ctx,
		`SELECT id, region, industry, company_size FROM company_profiles WHERE user_id = $1`, userID,
	).Scan(&profileID, &companyRegion, &companyIndustry, &companySize)
	if err == sql.ErrNoRows {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "company_profile_required"})
		return
	}
	if err != nil {
		s.logger.Error("evaluate: profile lookup failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}

	var noticeRegion, noticeIndustry sql.NullString
	var budgetAmount sql.NullInt64
	var currentVersion int
	err = s.db.QueryRowContext(ctx,
		`SELECT region, industry, budget_amount, current_version FROM notices WHERE id = $1`, noticeID,
	).Scan(&noticeRegion, &noticeIndustry, &budgetAmount, &currentVersion)
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

	regionItem, err := s.evaluateRegion(ctx, versionID, profileID, noticeRegion, companyRegion)
	if err != nil {
		s.logger.Error("evaluate: region check failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	industryItem, err := s.evaluateIndustry(ctx, versionID, profileID, noticeIndustry, []string(companyIndustry))
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

	items := []eligibilityItem{regionItem, industryItem, budgetItem}
	writeJSON(w, http.StatusOK, map[string]any{
		"noticeId":         noticeID,
		"companyProfileId": profileID,
		"overallResult":    overallResult(items),
		"items":            items,
		"disclaimer":       evalDisclaimer,
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

// scoreRegion is the pure decision logic behind evaluateRegion — no DB
// access, safe to call in a loop (dashboard scans hundreds of notices).
// scoreRegion의 세 번째 리턴값(dataGapSide)은 insufficient_data일 때 어느 쪽에
// 정보가 없는지 구분한다: "notice"는 공고 자체에 지역 데이터가 없는 경우(g2b
// 수집 데이터의 구조적 한계 — 사용자가 고칠 수 없음), "company"는 기업 프로필에
// 지역이 없는 경우(사용자가 "내 프로필"에서 채워 넣으면 해결됨). 이 구분이 없으면
// 프론트가 "회사 정보 보완하기"를 모든 자료 부족 상황에 잘못 안내하게 된다.
func scoreRegion(noticeRegion, companyRegion sql.NullString) (result, reason, dataGapSide string) {
	switch {
	case !noticeRegion.Valid || noticeRegion.String == "":
		return "insufficient_data", "공고에 지역 정보가 없어 지역 조건을 판정할 수 없습니다.", "notice"
	case noticeRegion.String == regionNationwide:
		return "met", "공고가 전국 대상이라 지역 제한이 없습니다.", ""
	case !companyRegion.Valid || companyRegion.String == "":
		return "insufficient_data", "기업 프로필에 지역 정보가 없어 판정할 수 없습니다.", "company"
	case noticeRegion.String == companyRegion.String:
		return "met", fmt.Sprintf("공고 지역(%s)과 기업 지역이 일치합니다.", noticeRegion.String), ""
	default:
		return "not_met", fmt.Sprintf("공고 지역(%s)이 기업 지역(%s)과 다릅니다.", noticeRegion.String, companyRegion.String), ""
	}
}

func (s *Server) evaluateRegion(ctx context.Context, versionID, profileID string, noticeRegion, companyRegion sql.NullString) (eligibilityItem, error) {
	result, reason, _ := scoreRegion(noticeRegion, companyRegion)

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
func scoreIndustry(noticeIndustry sql.NullString, companyGroups []string) (result, reason string) {
	noticeRaw := strings.TrimSpace(noticeIndustry.String)

	switch {
	case !noticeIndustry.Valid || noticeRaw == "":
		return "insufficient_data", "공고에 업종 정보가 없어 업종 조건을 판정할 수 없습니다."
	case len(companyGroups) == 0:
		return "insufficient_data", "기업 프로필에 업종 정보가 없어 판정할 수 없습니다."
	default:
		noticeGroup, known := industryRawToGroup[noticeRaw]
		switch {
		case !known:
			return "needs_confirmation", fmt.Sprintf(
				"공고 업종(%s)이 자동 분류 목록에 없는 새로운 값입니다. 기업이 선택한 업종(%s)과 "+
					"일치하는지 원문에서 직접 확인하세요.",
				noticeRaw, strings.Join(companyGroups, ", "))
		case containsString(companyGroups, noticeGroup):
			return "met", fmt.Sprintf("공고 업종(%s, %s 분류)이 기업이 선택한 업종과 일치합니다.", noticeRaw, noticeGroup)
		default:
			return "needs_confirmation", fmt.Sprintf(
				"공고 업종(%s, %s 분류)이 기업이 선택한 업종(%s)과 다릅니다. 업종 분류가 정확한 표준 "+
					"산업분류가 아니므로 실제로는 겹칠 수 있으니 원문에서 직접 확인하세요.",
				noticeRaw, noticeGroup, strings.Join(companyGroups, ", "))
		}
	}
}

func (s *Server) evaluateIndustry(ctx context.Context, versionID, profileID string, noticeIndustry sql.NullString, companyGroups []string) (eligibilityItem, error) {
	noticeRaw := strings.TrimSpace(noticeIndustry.String)
	result, reason := scoreIndustry(noticeIndustry, companyGroups)

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
func scoreBudgetSize(budgetAmount sql.NullInt64, companySize sql.NullString) (result, reason string) {
	switch {
	case !companySize.Valid || companySize.String == "" || !budgetAmount.Valid:
		return "insufficient_data", "기업 규모 또는 공고 예산 정보가 없어 예산 규모 조건을 판정할 수 없습니다."
	case companySize.String == smallBusinessSize && budgetAmount.Int64 >= smallBusinessBudgetCap:
		return "needs_confirmation", fmt.Sprintf(
			"기업 규모가 %s인데 공고 예산(%d원)이 %d원 이상으로 큽니다. "+
				"실제 참가자격 규정을 확인하지 않은 예시 기준이니, 공고문에서 참가자격을 직접 확인하세요.",
			smallBusinessSize, budgetAmount.Int64, smallBusinessBudgetCap)
	default:
		return "met", "예산 규모 관련 확인된 제한 사항이 없습니다."
	}
}

func (s *Server) evaluateBudgetSize(ctx context.Context, versionID, profileID string, budgetAmount sql.NullInt64, companySize sql.NullString) (eligibilityItem, error) {
	result, reason := scoreBudgetSize(budgetAmount, companySize)

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
