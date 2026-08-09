// participation_judgment.go — 유료화 1단계(2026-08-09): 참여판정 신뢰성 확장.
// 기존 3요소(지역/업종/기업규모) 판정을 그대로 재사용하면서, 면허·인증·직접생산
// 확인을 조건 단위 PASS/REVIEW/FAIL/UNKNOWN + HARD/SOFT로 이어붙여, 사용자가 "왜
// 참여 가능/확인 필요/어려움인지"를 시스템 근거만으로 이해하게 한다.
//
// 안전 원칙(가장 중요): 모르면 REVIEW/UNKNOWN. 거짓 PASS/FAIL보다 REVIEW가 낫다.
// AI 추정값을 근거 없이 HARD 판정에 쓰지 않는다 — 공식/구조화 데이터 + 확인 가능한
// 회사 데이터(company_licenses/certifications, direct_production_cert)만 쓴다.
//
// 이번 1차 범위: 지역/업종/기업규모/면허/인증/직접생산확인 6개만. 실적/재무/기술
// 인력/공동수급/업력은 REVIEW/후속 대상으로 남긴다(여기서 판정하지 않는다).
// DB 변경 없음 — 조회 시점 계산 결과라 영속화하지 않는다.
package api

import (
	"context"
	"strings"
)

const (
	condPASS    = "PASS"
	condREVIEW  = "REVIEW"
	condFAIL    = "FAIL"
	condUNKNOWN = "UNKNOWN"

	sevHARD = "HARD"
	sevSOFT = "SOFT"
)

// conditionResult — 참가조건 하나의 판정 결과 + 근거.
type conditionResult struct {
	ConditionType   string `json:"conditionType"`             // 지역/업종/기업규모/면허/인증/직접생산확인
	Result          string `json:"result"`                    // PASS/REVIEW/FAIL/UNKNOWN
	Severity        string `json:"severity"`                  // HARD/SOFT
	RequirementText string `json:"requirementText,omitempty"` // 공고 요구(근거)
	CompanyEvidence string `json:"companyEvidence,omitempty"` // 회사 데이터(근거)
	Reason          string `json:"reason"`                    // 사람이 읽는 판정 사유
}

// participationJudgment — 조건별 결과 + 종합 grade. 기존 participationScore와
// 별도 필드로 함께 내려간다(하위호환).
type participationJudgment struct {
	Grade        string            `json:"grade"`      // ready | needsReview | notRecommended
	GradeLabel   string            `json:"gradeLabel"` // 참여 가능 / 확인 필요 / 참여 어려움
	PassCount    int               `json:"passCount"`
	ReviewCount  int               `json:"reviewCount"`
	FailCount    int               `json:"failCount"`
	UnknownCount int               `json:"unknownCount"`
	Conditions   []conditionResult `json:"conditions"`
	Note         string            `json:"note,omitempty"`
}

// mapCategoryToCondition — 기존 3요소(categoryScore.Result)를 조건 결과로 변환.
func mapCategoryResult(result string) string {
	switch result {
	case "met":
		return condPASS
	case "not_met":
		return condFAIL
	case "needs_confirmation":
		return condREVIEW
	default: // insufficient_data 등 — 데이터 부족
		return condUNKNOWN
	}
}

// buildParticipationJudgment은 기존 3요소 판정(score)에 면허/인증/직접생산확인을
// 이어붙여 조건별 결과 + 종합 grade를 만든다. procurement 공고에서만 의미가 있다
// (지원사업은 판정 기준이 달라 호출부에서 제외).
func (s *Server) buildParticipationJudgment(ctx context.Context, versionID, profileID string, score *participationScore, requiredDocs []requiredDocumentItem) *participationJudgment {
	if score == nil || profileID == "" {
		return nil
	}
	j := &participationJudgment{}

	// 1) 기존 3요소(지역/업종/기업규모) — 전부 HARD.
	for _, c := range score.Categories {
		label := c.Category
		if label == "예산 규모" {
			label = "기업규모/예산"
		}
		j.Conditions = append(j.Conditions, conditionResult{
			ConditionType: label,
			Result:        mapCategoryResult(c.Result),
			Severity:      sevHARD,
			Reason:        c.Reason,
		})
	}

	// 2) 공고 요구 서류에서 면허/인증/직접생산 이름을 분류한다. 데이터 출처는
	//    required_documents.document_name(document_extraction.go가 추출) — g2b
	//    원문에 구조화된 "필수 면허" 필드가 없어서다(notice_license_match.go 참고).
	var licenseNames, certNames []string
	directProdRequired := false
	for _, d := range requiredDocs {
		n := strings.TrimSpace(d.DocumentName)
		if n == "" {
			continue
		}
		switch {
		case strings.Contains(n, "직접생산"): // "직접생산확인증명서" 먼저 걸러 인증과 안 겹치게
			directProdRequired = true
		case strings.Contains(n, "면허"):
			licenseNames = append(licenseNames, n)
		case strings.Contains(n, "인증서") || strings.Contains(n, "ISO"):
			certNames = append(certNames, n)
		}
	}

	// 3) 면허(HARD 중요도, 단 FAIL로 단정하지 않음): 요구 면허는 required_documents의
	//    "제출서류명"에서 감지하는데(g2b 원문에 구조화된 필수면허 필드가 없음),
	//    ① 제출서류명("면허증 사본" 등)이 회사 면허명("정보통신공사업 면허")과
	//    정확일치(matchLicenseCertByName은 TRIM 정확일치)하지 않아 실제 보유해도
	//    not-found가 나기 쉽고, ② 필수/조건부·OR/AND를 서류명만으로 구분할 수 없다.
	//    → 미보유/미확인/만료를 HARD FAIL로 단정하면 실제 참여 가능한 공고를 오탈락
	//    시킨다(FALSE HARD FAIL). 그래서 보유(정확일치)만 PASS, 나머지는 REVIEW로
	//    두어 "확실하지 않으면 확인 필요" 원칙을 지킨다(2026-08-09 HARD FAIL 안전 검증).
	if len(licenseNames) > 0 {
		var held, expired, unresolved []string
		for _, name := range licenseNames {
			st, ok := s.matchLicenseCertByName(ctx, profileID, name)
			switch {
			case ok && st == "보유":
				held = append(held, name)
			case ok && st == "갱신필요":
				expired = append(expired, name)
			default: // not-found(정확명 불일치 포함) / 발급필요 / 확인필요
				unresolved = append(unresolved, name)
			}
		}
		cond := conditionResult{ConditionType: "면허", Severity: sevHARD, RequirementText: strings.Join(licenseNames, ", ")}
		switch {
		case len(unresolved) == 0 && len(expired) == 0:
			cond.Result = condPASS
			cond.CompanyEvidence = strings.Join(held, ", ")
			cond.Reason = "보유하신 면허가 이 공고 요건과 일치합니다."
		case len(expired) > 0 && len(unresolved) == 0:
			cond.Result = condREVIEW
			cond.Reason = "요구 면허(" + strings.Join(expired, ", ") + ")가 만료 상태입니다 — 갱신 여부를 확인해주세요."
		default:
			// 서류명만으로는 필수성·OR/AND·정확 면허명을 확정할 수 없어 FAIL로 단정하지 않는다.
			cond.Result = condREVIEW
			cond.Reason = "이 공고가 요구하는 면허(" + strings.Join(unresolved, ", ") + ") 보유·충족 여부를 확인해주세요."
		}
		j.Conditions = append(j.Conditions, cond)
	}

	// 4) 인증(SOFT): 필수/우대 구분을 원문에서 신뢰성 있게 판별할 수 없으므로,
	//    미보유를 곧바로 FAIL로 두지 않는다 — 보유면 PASS, 아니면 REVIEW.
	if len(certNames) > 0 {
		allHeld := true
		var held []string
		for _, name := range certNames {
			st, ok := s.matchLicenseCertByName(ctx, profileID, name)
			if ok && st == "보유" {
				held = append(held, name)
			} else {
				allHeld = false
			}
		}
		cond := conditionResult{ConditionType: "인증", Severity: sevSOFT, RequirementText: strings.Join(certNames, ", ")}
		if allHeld {
			cond.Result = condPASS
			cond.CompanyEvidence = strings.Join(held, ", ")
			cond.Reason = "요구 인증을 보유하고 있습니다."
		} else {
			cond.Result = condREVIEW
			cond.Reason = "요구 인증의 보유 여부·필수 여부(필수/우대)를 확인해주세요."
		}
		j.Conditions = append(j.Conditions, cond)
	}

	// 5) 직접생산확인(HARD 중요도, 결과는 REVIEW): 프로필은 boolean(direct_production_cert)
	//    뿐이라 "이 공고가 요구하는 세부품명 충족"을 확정할 수 없다 → 절대 PASS/FAIL
	//    확정하지 않고 REVIEW로 남긴다(§6/§11).
	if directProdRequired {
		hasCert := s.companyHasDirectProductionCert(ctx, profileID)
		cond := conditionResult{ConditionType: "직접생산확인", Severity: sevHARD, Result: condREVIEW,
			RequirementText: "직접생산확인증명서 요구"}
		if hasCert {
			cond.CompanyEvidence = "직접생산확인 보유(체크)"
			cond.Reason = "직접생산확인 보유 정보는 있으나, 이 공고가 요구하는 세부 품명 충족 여부는 확인이 필요합니다."
		} else {
			cond.Reason = "이 공고는 직접생산확인을 요구합니다 — 보유·품명 충족 여부를 확인해주세요."
		}
		j.Conditions = append(j.Conditions, cond)
	}

	// 6) 안전장치: 공고문 분석이 아직 안 돼(required_documents 자체가 비어 있음)
	//    면허·인증 요건을 확인하지 못한 상태라면, 3요소만 보고 "참여 가능"으로
	//    확정하지 않는다 — UNKNOWN 조건을 넣어 최소 "확인 필요"가 되게 한다.
	if len(requiredDocs) == 0 {
		j.Conditions = append(j.Conditions, conditionResult{
			ConditionType: "필수 면허·자격", Result: condUNKNOWN, Severity: sevHARD,
			Reason: "공고문 분석이 아직 완료되지 않아 필수 면허·인증 요건을 확인하지 못했습니다.",
		})
	}

	finalizeJudgment(j)
	return j
}

// finalizeJudgment은 조건 목록에서 카운트와 종합 grade를 계산한다(순수 함수 —
// DB 불필요, 단위테스트 대상). grade 규칙(§2): HARD FAIL이 있으면 참여 어려움,
// 없고 REVIEW/UNKNOWN이 있으면 확인 필요, 전부 PASS면 참여 가능. SOFT FAIL은
// 전체 참여곤란을 만들지 않는다(현재 SOFT 조건은 FAIL 대신 REVIEW로만 나오지만,
// 방어적으로 규칙을 명시한다).
func finalizeJudgment(j *participationJudgment) {
	hardFail := false
	for _, c := range j.Conditions {
		switch c.Result {
		case condPASS:
			j.PassCount++
		case condREVIEW:
			j.ReviewCount++
		case condFAIL:
			j.FailCount++
			if c.Severity == sevHARD {
				hardFail = true
			}
		case condUNKNOWN:
			j.UnknownCount++
		}
	}
	switch {
	case hardFail:
		j.Grade, j.GradeLabel = "notRecommended", "참여 어려움"
	case j.ReviewCount > 0 || j.UnknownCount > 0:
		j.Grade, j.GradeLabel = "needsReview", "확인 필요"
	default:
		j.Grade, j.GradeLabel = "ready", "참여 가능"
	}
}

// companyHasDirectProductionCert — 프로필의 direct_production_cert(boolean).
func (s *Server) companyHasDirectProductionCert(ctx context.Context, profileID string) bool {
	var has bool
	if err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(direct_production_cert, false) FROM company_profiles WHERE id = $1`, profileID,
	).Scan(&has); err != nil {
		return false
	}
	return has
}
