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
	"database/sql"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// buildJudgmentForNotice — 공고 하나에 대한 참여판정을 만든다. 공고 상세
// (handleGetNotice)와 '동일한' 입력(같은 notices 행의 지역/업종/예산/업종제한 +
// scoreNoticeForCompany + 현재버전 required_documents)으로 buildParticipationJudgment를
// 재사용하므로, 같은 notice/company면 상세와 파이프라인 상세의 judgment가 반드시
// 일치한다. procurement가 아니면 nil. DB 변경 없음(조회 시점 계산).
func (s *Server) buildJudgmentForNotice(ctx context.Context, noticeID, profileID string, company companyScoringInput) *participationJudgment {
	var noticeType string
	var region, industry sql.NullString
	var budget sql.NullInt64
	var industryRestricted sql.NullBool
	var currentVersion int
	err := s.db.QueryRowContext(ctx,
		`SELECT notice_type, region, industry, budget_amount, industry_restricted, current_version
		 FROM notices WHERE id = $1`, noticeID,
	).Scan(&noticeType, &region, &industry, &budget, &industryRestricted, &currentVersion)
	if err != nil || noticeType != "procurement" {
		return nil
	}
	versionID, err := s.currentVersionID(ctx, noticeID, currentVersion)
	if err != nil {
		return nil
	}
	auths, err := s.regionAuthoritiesByVersions(ctx, []string{versionID})
	if err != nil {
		return nil
	}
	score := scoreNoticeForCompany(noticeScoringInput{
		NoticeType: noticeType, Region: region, Industry: industry, BudgetAmount: budget,
		IndustryRestricted: nullBoolPtr(industryRestricted),
		OfficialRegions:    auths[versionID].OfficialRegions, RegionEnriched: auths[versionID].Enriched,
	}, company)
	reqDocs, err := s.listRequiredDocuments(ctx, versionID, profileID)
	if err != nil {
		reqDocs = nil
	}
	return s.buildParticipationJudgment(ctx, versionID, profileID, &score, reqDocs)
}

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
	ConditionType   string             `json:"conditionType"`             // 지역/업종/기업규모/면허/인증/직접생산확인
	Result          string             `json:"result"`                    // PASS/REVIEW/FAIL/UNKNOWN
	Severity        string             `json:"severity"`                  // HARD/SOFT
	RequirementText string             `json:"requirementText,omitempty"` // 공고 요구(근거)
	CompanyEvidence string             `json:"companyEvidence,omitempty"` // 회사 데이터(근거)
	Reason          string             `json:"reason"`                    // 사람이 읽는 판정 사유
	Question        *conditionQuestion `json:"question,omitempty"`        // 사용자 질문형 해소(면허/인증 REVIEW만)
}

// conditionQuestion — Human-in-the-loop(2026-08-10): REVIEW로 남은 면허/인증 조건을
// 사용자가 짧은 Yes/No 답변으로 해소할 수 있을 때만 채운다. Targets는 "아직 회사
// 정보에 답이 없는"(company_licenses/certifications에 해당 이름의 행이 아예 없는)
// 요구명 목록 — 이미 답한 항목은 넣지 않아 같은 질문을 반복하지 않는다(§9). 답변은
// 프론트가 기존 POST /api/me/licenses|certifications로 회사 프로필에 저장하므로(§5
// 재사용), 새 저장 구조가 필요 없다. Category는 저장 시 그대로 쓴다(면허/인증).
type conditionQuestion struct {
	Kind     string   `json:"kind"`     // license | certification
	Category string   `json:"category"` // 면허 | 인증 (company_licenses.category)
	Targets  []string `json:"targets"`  // 미답변 요구명(정확일치 저장키)
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
// [Canonical eligibility core — STEP 1, 2026-08-14] buildParticipationJudgment은 신규 참가자격
// 판정의 단일 진입점이다. 회사 측은 구조화 테이블(company_licenses/certifications,
// direct_production_cert)을, 공고 측은 required_documents(+지역 enrichment)를 근거로 조건별
// PASS/REVIEW/UNKNOWN을 산출한다(NOT_HELD와 UNKNOWN을 구분, 만료는 갱신필요→REVIEW).
// 신규 판정 로직은 여기(또는 여기가 부르는 헬퍼)에만 추가하고, scoreNoticeForCompany/
// eligibility.go에 판정 로직을 중복 구현하지 말 것.
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

	// 2) 공고 요구 면허/인증/직접생산 이름을 Requirement Resolver로 정규화해 얻는다(STEP 1,
	//    notice_requirements.go). Resolver가 required_documents + eligibility_conditions +
	//    (허용면허)license_limits를 근거 보존·중복 제거해 합친다. 단 판정 소비는
	//    judgmentConsumableRequirements로 required_documents/eligibility_conditions만 쓴다 —
	//    license_limits(허용/OR 범위)를 개별 HARD 면허요건으로 오판하지 않도록 기존 판정 동작을
	//    보존한다. versionID=="" (대시보드 추천 카드)면 Resolver는 requiredDocs만 정규화하므로
	//    결과가 기존과 동일하다(회귀 없음).
	var licenseNames, certNames []string
	var trackRecordReqs []noticeRequirement
	directProdRequired := false
	for _, req := range judgmentConsumableRequirements(s.resolveNoticeRequirements(ctx, versionID, requiredDocs)) {
		switch req.Type {
		case reqTypeLicense:
			licenseNames = append(licenseNames, req.DisplayName)
		case reqTypeCertification:
			certNames = append(certNames, req.DisplayName)
		case reqTypeDirectProduction:
			directProdRequired = true
		case reqTypeTrackRecord:
			trackRecordReqs = append(trackRecordReqs, req)
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
		var held, expired, unresolved, askable []string
		for _, name := range licenseNames {
			st, ok := s.matchLicenseCertByName(ctx, profileID, name)
			switch {
			case ok && st == "보유":
				held = append(held, name)
			case ok && st == "갱신필요":
				expired = append(expired, name)
			default: // not-found(정확명 불일치 포함) / 발급필요 / 확인필요
				unresolved = append(unresolved, name)
				if !ok { // 회사 정보에 이 이름의 행이 아예 없음 = 아직 미답변 → 질문 가능(§9)
					askable = append(askable, name)
				}
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
		if cond.Result == condREVIEW && len(askable) > 0 {
			cond.Question = &conditionQuestion{Kind: "license", Category: "면허", Targets: askable}
		}
		j.Conditions = append(j.Conditions, cond)
	}

	// 4) 인증(SOFT): 필수/우대 구분을 원문에서 신뢰성 있게 판별할 수 없으므로,
	//    미보유를 곧바로 FAIL로 두지 않는다 — 보유면 PASS, 아니면 REVIEW.
	if len(certNames) > 0 {
		allHeld := true
		var held, askable []string
		for _, name := range certNames {
			st, ok := s.matchLicenseCertByName(ctx, profileID, name)
			if ok && st == "보유" {
				held = append(held, name)
			} else {
				allHeld = false
				if !ok { // 회사 정보에 이 인증명 행이 아예 없음 = 미답변 → 질문 가능(§9)
					askable = append(askable, name)
				}
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
			if len(askable) > 0 {
				cond.Question = &conditionQuestion{Kind: "certification", Category: "인증", Targets: askable}
			}
		}
		j.Conditions = append(j.Conditions, cond)
	}

	// 5) 직접생산확인(HARD 중요도, 결과는 REVIEW): 세부 품명 충족까지는 확정할 수
	//    없어 절대 PASS/FAIL 확정하지 않고 REVIEW로 남긴다(§6/§11). 단 보유상태는
	//    3-상태(보유/미보유/확인되지않음)로 구분해, 미확인일 때만 사용자에게 질문을
	//    띄운다(보유·미보유는 이미 답한 것이므로 재질문하지 않음 — §7 재사용).
	if directProdRequired {
		status := s.companyDirectProductionStatus(ctx, profileID)
		cond := conditionResult{ConditionType: "직접생산확인", Severity: sevHARD, Result: condREVIEW,
			RequirementText: "직접생산확인증명서 요구"}
		switch status {
		case "보유":
			cond.CompanyEvidence = "직접생산확인 보유(등록된 회사정보)"
			cond.Reason = "직접생산확인 보유 정보는 있으나, 이 공고가 요구하는 세부 품명 충족 여부는 확인이 필요합니다."
		case "미보유":
			cond.Reason = "회사정보에 직접생산확인 미보유로 등록되어 있습니다 — 이 공고 참여에는 직접생산확인이 필요합니다."
		default: // 확인되지않음 — 아직 답하지 않음 → 질문 가능(§9)
			cond.Reason = "이 공고는 직접생산확인을 요구합니다 — 보유 여부를 확인해주세요."
			cond.Question = &conditionQuestion{Kind: "directProduction", Category: "직접생산", Targets: []string{"직접생산확인증명서"}}
		}
		j.Conditions = append(j.Conditions, cond)
	}

	// 5b) 수행실적(HARD 중요도, 결과는 REVIEW): 공고가 요구하는 실적(금액·기간·분야)을
	//     회사 등록 실적(company_track_records)과 대조한다. 단 ① 공고측 정량 임계치는
	//     대부분 원문에만 있고 구조화가 희박하며, ② 회사 실적도 사용자 입력(신뢰도 낮음)
	//     이라 "충족"을 자동 PASS로 단정하지 않는다(over-PASS 방지) — 항상 REVIEW로 두고
	//     대조 결과(충족/금액부족/기간부족/정보부족)를 사유로만 제시한다. FAIL도 하지
	//     않는다(면허와 동일한 FALSE-FAIL 안전 원칙).
	if len(trackRecordReqs) > 0 {
		j.Conditions = append(j.Conditions, s.evaluateTrackRecordRequirement(ctx, profileID, trackRecordReqs))
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

// recommendationJudgment — 대시보드 추천 카드용 경량 요약(전체 conditions는 안
// 싣는다). 상세와 동일한 buildParticipationJudgment 결과에서 grade + 짧은 이유
// 최대 2개만 뽑는다(판정 로직을 새로 만들지 않는다).
type recommendationJudgment struct {
	Grade      string   `json:"grade"`      // ready | needsReview | notRecommended
	GradeLabel string   `json:"gradeLabel"` // 참여 가능 / 확인 필요 / 참여 어려움
	Reasons    []string `json:"reasons,omitempty"`
	// Questions — 홈 HERO 인라인 REVIEW 질문형 해소용(2026-08-10). 상세와 동일한
	// conditionResult.Question(면허/인증 중 아직 미답변 요구명)을 그대로 실어, 홈에서도
	// 같은 질문/답변/저장 규칙을 재사용한다(새 판정·저장 로직 없음). 없으면 생략.
	Questions []recommendationQuestion `json:"questions,omitempty"`
}

// recommendationQuestion — recommendationJudgment에 싣는 홈 질문 1건. conditionResult
// 의 ConditionType/Reason + conditionQuestion(Kind/Category/Targets)을 그대로 옮긴 것.
type recommendationQuestion struct {
	ConditionType string   `json:"conditionType"` // 면허 | 인증
	Reason        string   `json:"reason"`        // 사람이 읽는 확인 사유(상세와 동일)
	Kind          string   `json:"kind"`          // license | certification (저장 엔드포인트)
	Category      string   `json:"category"`      // 면허 | 인증 (저장 payload)
	Targets       []string `json:"targets"`       // 미답변 요구명(정확일치 저장키)
}

// recommendationJudgmentFrom은 판정 결과에서 카드용 요약을 만든다. 이유는
// FAIL > REVIEW > UNKNOWN 순으로 최대 2개(§5). 질문형 해소가 가능한 조건(면허/인증
// 의 Question)은 홈에서 인라인으로 답할 수 있게 그대로 옮긴다(상세와 동일 데이터).
func recommendationJudgmentFrom(j *participationJudgment) *recommendationJudgment {
	if j == nil {
		return nil
	}
	r := &recommendationJudgment{Grade: j.Grade, GradeLabel: j.GradeLabel}
	add := func(want string) {
		for _, c := range j.Conditions {
			if len(r.Reasons) >= 2 {
				return
			}
			if c.Result == want {
				r.Reasons = append(r.Reasons, shortConditionReason(c))
			}
		}
	}
	add(condFAIL)
	add(condREVIEW)
	add(condUNKNOWN)
	for _, c := range j.Conditions {
		if c.Question != nil && len(c.Question.Targets) > 0 {
			r.Questions = append(r.Questions, recommendationQuestion{
				ConditionType: c.ConditionType, Reason: c.Reason,
				Kind: c.Question.Kind, Category: c.Question.Category, Targets: c.Question.Targets,
			})
		}
	}
	return r
}

// shortConditionReason — 카드에 넣을 짧은 사유 라벨(1행).
func shortConditionReason(c conditionResult) string {
	switch c.ConditionType {
	case "면허":
		if c.Result == condFAIL {
			return "면허 조건 미충족"
		}
		return "면허 확인 필요"
	case "인증":
		return "인증 확인 필요"
	case "직접생산확인":
		return "직접생산 세부품명 확인"
	case "필수 면허·자격":
		return "공고문 분석 대기"
	default: // 지역/업종/기업규모
		if c.Result == condFAIL {
			return c.ConditionType + " 조건 미충족"
		}
		if c.Result == condUNKNOWN {
			return c.ConditionType + " 정보 부족"
		}
		return c.ConditionType + " 확인 필요"
	}
}

// companyDirectProductionStatus — 직접생산확인 보유상태를 3-상태로 돌려준다
// (STEP 2-B). 우선순위: 새 direct_production_status 컬럼(사용자가 참여검토에서
// 답한 값)이 있으면 그대로, 없으면(NULL) legacy boolean으로 폴백한다.
// 폴백 매핑: true→"보유", false→"확인되지않음". ⚠️ legacy false는 미보유와
// 미확인이 섞여 있으므로(온보딩은 보유만 true 저장) false를 "미보유"로
// 단정하지 않는다 — 미확인으로 두어야 재질문/재사용이 안전하다(§4).
func (s *Server) companyDirectProductionStatus(ctx context.Context, profileID string) string {
	var status sql.NullString
	var legacy bool
	if err := s.db.QueryRowContext(ctx,
		`SELECT direct_production_status, COALESCE(direct_production_cert, false) FROM company_profiles WHERE id = $1`, profileID,
	).Scan(&status, &legacy); err != nil {
		return "확인되지않음"
	}
	if status.Valid && strings.TrimSpace(status.String) != "" {
		return status.String
	}
	if legacy {
		return "보유"
	}
	return "확인되지않음"
}

var recentYearsRe = regexp.MustCompile(`최근\s*(\d+)\s*년`)

// trackRecordAmountThreshold — 실적 요구 목록에서 정량 금액 임계치(원 단위)를 뽑는다.
// eligibility_conditions의 threshold_value(text)+unit을 원 단위로 환산해 가장 큰 값(가장
// 엄격한 요구)을 취한다. 금액형 근거가 하나도 없으면 (0,false).
func trackRecordAmountThreshold(reqs []noticeRequirement) (int64, bool) {
	var best int64
	found := false
	for _, r := range reqs {
		raw := strings.TrimSpace(r.ThresholdValue)
		if raw == "" {
			continue
		}
		// 건수형(unit='건', STEP 2-C-1)은 금액이 아니다 — 환산 로직에 절대 넣지 않는다
		// (threshold_value=1, unit=건을 1원으로 오해하면 거짓 금액요구가 생긴다).
		if strings.Contains(r.Unit, "건") {
			continue
		}
		// 숫자만 남기고(콤마·통화기호 제거) 파싱.
		var b strings.Builder
		for _, ch := range raw {
			if (ch >= '0' && ch <= '9') || ch == '.' {
				b.WriteRune(ch)
			}
		}
		num, err := strconv.ParseFloat(b.String(), 64)
		if err != nil || num <= 0 {
			continue
		}
		// 단위는 구조화된 unit 컬럼만 신뢰한다. source_text(자유문)로 단위를 추론하면
		// threshold_value가 이미 원 단위인데 원문의 "억"을 잡아 이중 환산되는 버그가
		// 난다(E2E에서 확인). unit이 비면 원 단위로 간주(factor 1).
		factor := 1.0
		switch {
		case strings.Contains(r.Unit, "억"):
			factor = 1e8
		case strings.Contains(r.Unit, "천만"):
			factor = 1e7
		case strings.Contains(r.Unit, "백만"):
			factor = 1e6
		case strings.Contains(r.Unit, "천원"):
			factor = 1e3
		case strings.Contains(r.Unit, "만"): // "만원"(백만/천만은 위에서 이미 처리)
			factor = 1e4
		}
		won := int64(num * factor)
		if won > best {
			best = won
		}
		found = true
	}
	return best, found
}

// trackRecordCountThreshold — 건수형 실적 요구(unit='건', STEP 2-C-1)의 최소 건수를
// 뽑는다(없으면 0,false). 여러 개면 가장 엄격한(큰) 값.
func trackRecordCountThreshold(reqs []noticeRequirement) (int, bool) {
	best, found := 0, false
	for _, r := range reqs {
		if !strings.Contains(r.Unit, "건") {
			continue
		}
		if n, err := strconv.Atoi(strings.TrimSpace(r.ThresholdValue)); err == nil && n > 0 {
			if n > best {
				best = n
			}
			found = true
		}
	}
	return best, found
}

// trackRecordRecentYears — 실적 요구에서 "최근 N년" 기간 창을 뽑는다(없으면 0,false).
func trackRecordRecentYears(reqs []noticeRequirement) (int, bool) {
	for _, r := range reqs {
		for _, txt := range []string{r.DisplayName, r.SourceText} {
			if m := recentYearsRe.FindStringSubmatch(txt); m != nil {
				if y, err := strconv.Atoi(m[1]); err == nil && y > 0 {
					return y, true
				}
			}
		}
	}
	return 0, false
}

// evaluateTrackRecordRequirement — 공고 실적 요구를 회사 등록 실적과 대조한다.
// 결과는 항상 REVIEW(over-PASS/FALSE-FAIL 방지) — 대조 결과를 4가지 사유
// (충족추정/금액부족/기간부족/정보부족)로만 구분해 사람이 최종 확인하게 한다.
// 회사 실적이 없으면 "정보부족"으로 회사정보 실적 등록을 안내한다(기존 회사정보
// 실적 등록 UI 재사용 — 새 저장/업로드 파이프라인을 만들지 않는다).
func (s *Server) evaluateTrackRecordRequirement(ctx context.Context, profileID string, reqs []noticeRequirement) conditionResult {
	reqText := "수행실적 요구"
	if len(reqs) > 0 && strings.TrimSpace(reqs[0].DisplayName) != "" {
		reqText = reqs[0].DisplayName
	}
	cond := conditionResult{ConditionType: "수행실적", Severity: sevHARD, Result: condREVIEW, RequirementText: reqText}

	amountReq, hasAmount := trackRecordAmountThreshold(reqs)
	yearsReq, hasYears := trackRecordRecentYears(reqs)
	countReq, hasCount := trackRecordCountThreshold(reqs) // 건수형(STEP 2-C-1)

	rows, err := s.db.QueryContext(ctx, `
		SELECT COALESCE(project_name,''), contract_amount, COALESCE(period_end, contract_date), industry_field, COALESCE(is_completed,false)
		FROM company_track_records WHERE company_profile_id = $1`, profileID)
	if err != nil {
		cond.Reason = "이 공고는 수행실적을 요구합니다 — 회사 실적 정보를 확인해주세요."
		return cond
	}
	defer rows.Close()

	type trec struct {
		name     string
		amount   sql.NullInt64
		refDate  sql.NullTime
		field    sql.NullString
		complete bool
	}
	var recs []trec
	for rows.Next() {
		var t trec
		if rows.Scan(&t.name, &t.amount, &t.refDate, &t.field, &t.complete) == nil {
			recs = append(recs, t)
		}
	}

	if len(recs) == 0 {
		// 정보부족: 회사 실적이 아직 없음 → 회사정보 실적 등록으로 안내(자동 대조 예정).
		cond.Reason = "이 공고는 수행실적을 요구합니다 — 회사정보에 완료한 사업 실적을 등록하면 금액·기간을 자동으로 대조해 드립니다."
		return cond
	}

	// 대조: 요구 금액을 만족하는 실적 중 요구 기간(최근 N년) 내인 것이 하나라도 있으면 충족추정.
	cutoff := time.Now().AddDate(-yearsReq, 0, 0)
	var maxAmount int64
	anyAmount := false
	amountOK := !hasAmount // 금액 요구 없으면 금액은 통과로 간주
	periodOK := !hasYears  // 기간 요구 없으면 기간은 통과로 간주
	var evidences []string
	for _, t := range recs {
		if t.amount.Valid {
			anyAmount = true
			if t.amount.Int64 > maxAmount {
				maxAmount = t.amount.Int64
			}
		}
		amtPass := !hasAmount || (t.amount.Valid && t.amount.Int64 >= amountReq)
		perPass := !hasYears || (t.refDate.Valid && !t.refDate.Time.Before(cutoff))
		if amtPass {
			amountOK = true
		}
		if perPass {
			periodOK = true
		}
		// 근거 표기(최대 2건).
		if len(evidences) < 2 && strings.TrimSpace(t.name) != "" {
			ev := t.name
			if t.amount.Valid {
				ev += fmt.Sprintf("(%s원)", formatWonComma(t.amount.Int64))
			}
			evidences = append(evidences, ev)
		}
	}
	if len(evidences) > 0 {
		cond.CompanyEvidence = strings.Join(evidences, ", ")
	}

	// 건수형(STEP 2-C-1): 요구 건수 대비 등록 실적 건수. 분야 유사성은 자동 판정하지
	// 않으므로(semantic 매칭 없음, §5) 충족돼 보여도 결과는 항상 REVIEW 유지.
	countOK := !hasCount || len(recs) >= countReq

	switch {
	case hasAmount && anyAmount && maxAmount < amountReq:
		// 금액부족: 등록된 실적 최대 금액이 요구액에 못 미침(확정 아님 — 미등록 실적 가능).
		cond.Reason = fmt.Sprintf("등록된 실적의 최대 금액(%s원)이 요구 금액(%s원)에 못 미칩니다 — 추가 실적이 있으면 등록해 확인해주세요.", formatWonComma(maxAmount), formatWonComma(amountReq))
	case hasYears && !periodOK:
		// 기간부족: 요구 기간(최근 N년) 내 실적을 확인하지 못함.
		cond.Reason = fmt.Sprintf("요구 기간(최근 %d년) 내 실적을 확인하지 못했습니다 — 해당 기간 실적이 있으면 등록해 확인해주세요.", yearsReq)
	case hasCount && !countOK:
		// 건수부족: 등록된 실적 건수가 요구 건수에 못 미침(확정 아님 — 미등록 실적 가능).
		cond.Reason = fmt.Sprintf("등록된 실적 건수(%d건)가 요구 건수(%d건 이상)에 못 미칩니다 — 추가 실적이 있으면 등록해 확인해주세요.", len(recs), countReq)
	case hasCount && countOK && !hasAmount && !hasYears:
		// 건수 충족추정: 요구 분야와의 유사성은 자동 판정하지 않으므로 최종 확인 안내.
		cond.Reason = fmt.Sprintf("요구 건수(%d건 이상)를 충족하는 것으로 보입니다 — 공고가 요구하는 분야와의 유사성을 최종 확인해주세요.", countReq)
	case amountOK && periodOK && countOK:
		// 충족추정: 금액·기간을 만족하는 실적이 있음(그래도 세부 유사성은 최종 확인 필요).
		cond.Reason = "요구 실적을 충족하는 것으로 보입니다 — 공고의 유사성·세부 기준을 최종 확인해주세요."
	default:
		// 정보 일부 부족(예: 금액 요구가 있으나 등록 실적에 금액 정보가 없음).
		cond.Reason = "수행실적이 등록돼 있으나 금액·기간 충족 여부를 확정할 수 없습니다 — 세부 정보를 확인해주세요."
	}
	return cond
}

// formatWonComma — 원 단위 정수를 천단위 콤마 문자열로.
func formatWonComma(v int64) string {
	s := strconv.FormatInt(v, 10)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	n := len(s)
	if n <= 3 {
		if neg {
			return "-" + s
		}
		return s
	}
	var b strings.Builder
	pre := n % 3
	if pre > 0 {
		b.WriteString(s[:pre])
		if n > pre {
			b.WriteByte(',')
		}
	}
	for i := pre; i < n; i += 3 {
		b.WriteString(s[i : i+3])
		if i+3 < n {
			b.WriteByte(',')
		}
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}
