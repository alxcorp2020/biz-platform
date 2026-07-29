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
)

const (
	ruleEngineVersion   = "structured-fields-v1"
	regionNationwide    = "전국"
	smallBusinessSize   = "소기업"
	// smallBusinessBudgetCap: 소기업이 참여하기엔 예산 규모가 크다고 볼 수
	// 있는 예시 기준선(10억 원). 실제 참가자격 규정은 아직 모델링하지
	// 않았으므로 확정 판정이 아니라 "확인 필요" 플래그로만 쓴다.
	smallBusinessBudgetCap = int64(1_000_000_000)

	evalDisclaimer = "이 결과는 공고의 구조화 항목만으로 산출한 1차 참고용 판정이며, 확정 판정이 아닙니다. " +
		"반드시 공식 공고문 원문을 확인하세요."
)

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
	var companyRegion, companyIndustry, companySize sql.NullString
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
	industryItem, err := s.evaluateIndustry(ctx, versionID, profileID, noticeIndustry, companyIndustry)
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

func (s *Server) evaluateRegion(ctx context.Context, versionID, profileID string, noticeRegion, companyRegion sql.NullString) (eligibilityItem, error) {
	var result, reason string
	switch {
	case !noticeRegion.Valid || noticeRegion.String == "":
		result = "insufficient_data"
		reason = "공고에 지역 정보가 없어 지역 조건을 판정할 수 없습니다."
	case noticeRegion.String == regionNationwide:
		result = "met"
		reason = "공고가 전국 대상이라 지역 제한이 없습니다."
	case !companyRegion.Valid || companyRegion.String == "":
		result = "insufficient_data"
		reason = "기업 프로필에 지역 정보가 없어 판정할 수 없습니다."
	case noticeRegion.String == companyRegion.String:
		result = "met"
		reason = fmt.Sprintf("공고 지역(%s)과 기업 지역이 일치합니다.", noticeRegion.String)
	default:
		result = "not_met"
		reason = fmt.Sprintf("공고 지역(%s)이 기업 지역(%s)과 다릅니다.", noticeRegion.String, companyRegion.String)
	}

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

func (s *Server) evaluateIndustry(ctx context.Context, versionID, profileID string, noticeIndustry, companyIndustry sql.NullString) (eligibilityItem, error) {
	var result, reason string
	switch {
	case !noticeIndustry.Valid || noticeIndustry.String == "":
		result = "insufficient_data"
		reason = "공고에 업종 정보가 없어 업종 조건을 판정할 수 없습니다."
	case !companyIndustry.Valid || companyIndustry.String == "":
		result = "insufficient_data"
		reason = "기업 프로필에 업종 정보가 없어 판정할 수 없습니다."
	case noticeIndustry.String == companyIndustry.String:
		result = "met"
		reason = fmt.Sprintf("공고 업종(%s)과 기업 업종이 일치합니다.", noticeIndustry.String)
	default:
		result = "needs_confirmation"
		reason = fmt.Sprintf(
			"공고 업종(%s)과 기업 업종(%s)이 문자열상 다릅니다. 표준 업종 분류체계 매핑이 아직 없어 "+
				"실제로는 유사/동일 업종일 수 있으니 원문에서 직접 확인하세요.",
			noticeIndustry.String, companyIndustry.String)
	}

	conditionID, err := s.findOrCreateAutoCondition(ctx, versionID,
		"업종", "auto:industry", "eq", nsOrEmpty(noticeIndustry),
		"공고 API 구조화 필드(industry) 자동 추출 — 문서 미분석, 1차 참고용")
	if err != nil {
		return eligibilityItem{}, err
	}
	if err := s.recordEvaluation(ctx, profileID, versionID, conditionID, result, reason); err != nil {
		return eligibilityItem{}, err
	}
	return eligibilityItem{Category: "업종", Result: result, Reason: reason}, nil
}

func (s *Server) evaluateBudgetSize(ctx context.Context, versionID, profileID string, budgetAmount sql.NullInt64, companySize sql.NullString) (eligibilityItem, error) {
	var result, reason string
	switch {
	case !companySize.Valid || companySize.String == "" || !budgetAmount.Valid:
		result = "insufficient_data"
		reason = "기업 규모 또는 공고 예산 정보가 없어 예산 규모 조건을 판정할 수 없습니다."
	case companySize.String == smallBusinessSize && budgetAmount.Int64 >= smallBusinessBudgetCap:
		result = "needs_confirmation"
		reason = fmt.Sprintf(
			"기업 규모가 %s인데 공고 예산(%d원)이 %d원 이상으로 큽니다. "+
				"실제 참가자격 규정을 확인하지 않은 예시 기준이니, 공고문에서 참가자격을 직접 확인하세요.",
			smallBusinessSize, budgetAmount.Int64, smallBusinessBudgetCap)
	default:
		result = "met"
		reason = "예산 규모 관련 확인된 제한 사항이 없습니다."
	}

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
