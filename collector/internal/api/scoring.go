// scoring.go — 순수(비영속) 참여 가능성 스코어링. eligibility.go의
// scoreRegion/scoreIndustry/scoreBudgetSize를 조합해 공고 하나에 대한
// "참여 추천 여부"를 계산한다. DB 접근이 전혀 없어 대시보드가 공고
// 수백 건을 스캔할 때 반복 호출해도 저렴하다 — 감사이력이 필요한
// 사용자 명시적 평가(POST /api/notices/{id}/evaluate)는 기존처럼
// eligibility.go의 evaluate* 함수(영속)를 그대로 쓴다.
package api

import "database/sql"

type noticeScoringInput struct {
	// NoticeType: "procurement"(기본값, 빈 문자열도 이걸로 취급 — 기존
	// 호출부와 하위호환) | "support_program". 지원사업이면 아래
	// scoreNoticeForCompany가 3-카테고리 자동판정을 건너뛰고
	// supportProgramScore()로 대체한다.
	NoticeType   string
	Region       sql.NullString
	Industry     sql.NullString
	BudgetAmount sql.NullInt64
}

type companyScoringInput struct {
	Region   sql.NullString
	Industry []string
	Size     sql.NullString
	// TrackRecordMaxAmount is MAX(contract_amount) across company_track_records
	// for this company profile, fetched once per request (not per notice —
	// same "build company struct once outside the notice loop" pattern
	// dashboard.go already uses for Region/Industry/Size) and reused as a
	// coarse capacity signal for the 공동수급 검토(joint-venture-review) grade.
	TrackRecordMaxAmount sql.NullInt64
}

type categoryScore struct {
	Category string `json:"category"`
	Result   string `json:"result"`
	Reason   string `json:"reason"`
	// DataGapSide는 Result가 insufficient_data일 때만 채워진다: "notice"는
	// 공고 자체에 데이터가 없는 경우(사용자가 고칠 수 없음 — g2b 원본 데이터의
	// 구조적 한계), "company"는 기업 프로필 정보 부족(사용자가 "내 프로필"에서
	// 채우면 해결됨). 프론트가 "회사 정보 보완하기" 링크를 정확히 그 경우에만
	// 보여주고, notice 쪽은 "지역 정보 없음" 같은 별도 문구로 구분해서 보여준다.
	DataGapSide string `json:"dataGapSide,omitempty"`
}

// participationScore.Bucket is one of "ready" | "needs_review" | "not_recommended"
// (기존 3버킷 — 하위호환을 위해 그대로 유지). Grade는 5단계 종합판정
// (아래 grade* 상수) — 기획서 세분화 요청으로 추가된 필드이며 Bucket을
// 대체하지 않는다. MetCount/TotalCount back the "N개 조건 중 M개 충족" 문구 —
// 근거 없는 확률 대신 항상 이 비율을 함께 보여준다(스펙 논의에서 확정한 원칙).
type participationScore struct {
	Bucket      string          `json:"bucket"`
	Grade       string          `json:"grade"`
	GradeReason string          `json:"gradeReason,omitempty"`
	MetCount    int             `json:"metCount"`
	TotalCount  int             `json:"totalCount"`
	Categories  []categoryScore `json:"categories"`
}

// noticeTypeSupportProgram matches notices.notice_type/collector.NormalizedNotice.NoticeType's
// "support_program" value (bizinfo 등 지원사업 수집기).
const noticeTypeSupportProgram = "support_program"

// supportProgramReviewReason — 지원사업은 procurement 전용 판정 기준(예산
// 규모별 참가자격 상한, 실적 규모 대비 공동수급 검토)이 적용되지 않고,
// 지원분야 분류 체계도 g2b의 조달 업종 분류와 완전히 다르다(무엇을
// 파는 공고인지가 아니라 어떤 "종류의 지원"인지라 회사의 업종과 애초에
// 비교 대상이 아님 — bizinfo.go 패키지 주석 참고). 강제로 3개 카테고리
// (지역/업종/예산)를 자동판정하면 근거 없는 판정이 되므로, procurement과
// 달리 항상 "확인 필요"로 정직하게 처리한다 — 업종 불일치로 오판해
// 실제로는 맞는 지원사업을 놓치는 것보다 안전하다.
const supportProgramReviewReason = "지원사업은 입찰공고와 판정 기준이 달라 아직 자동 판정하지 않습니다. 공고 원문에서 지원대상·지원분야를 직접 확인해주세요."

func supportProgramScore() participationScore {
	categories := []categoryScore{
		{Category: "종합판정", Result: "needs_confirmation", Reason: supportProgramReviewReason},
	}
	return participationScore{
		Bucket:     "needs_review",
		Grade:      gradeNeedsConfirmation,
		MetCount:   0,
		TotalCount: 1,
		Categories: categories,
	}
}

func scoreNoticeForCompany(notice noticeScoringInput, company companyScoringInput) participationScore {
	if notice.NoticeType == noticeTypeSupportProgram {
		return supportProgramScore()
	}
	regionResult, regionReason, regionGapSide := scoreRegion(notice.Region, company.Region)
	industryResult, industryReason, industryGapSide := scoreIndustry(notice.Industry, company.Industry)
	budgetResult, budgetReason, budgetGapSide := scoreBudgetSize(notice.BudgetAmount, company.Size)

	categories := []categoryScore{
		{Category: "지역", Result: regionResult, Reason: regionReason, DataGapSide: regionGapSide},
		{Category: "업종", Result: industryResult, Reason: industryReason, DataGapSide: industryGapSide},
		{Category: "예산 규모", Result: budgetResult, Reason: budgetReason, DataGapSide: budgetGapSide},
	}

	metCount := 0
	for _, c := range categories {
		if c.Result == "met" {
			metCount++
		}
	}

	grade, gradeReason := gradeFromCategories(categories, trackRecordThin(notice.BudgetAmount, company.TrackRecordMaxAmount))

	return participationScore{
		Bucket:      bucketFromCategories(categories),
		Grade:       grade,
		GradeReason: gradeReason,
		MetCount:    metCount,
		TotalCount:  len(categories),
		Categories:  categories,
	}
}

// bucketFromCategories: 하나라도 not_met이면 참여 비추천, 전부 met이면
// 즉시 참여 가능, 그 외(needs_confirmation/insufficient_data 섞임)는
// 확인 필요로 묶는다 — 사용자와 논의해 확정한 3버킷 분류.
func bucketFromCategories(categories []categoryScore) string {
	allMet := true
	for _, c := range categories {
		if c.Result == "not_met" {
			return "not_recommended"
		}
		if c.Result != "met" {
			allMet = false
		}
	}
	if allMet {
		return "ready"
	}
	return "needs_review"
}

const (
	gradeRecommended        = "recommended"          // 참여 권장: 모든 필수조건 충족
	gradeConditional        = "conditional"          // 조건부 참여 가능: 애매하지만 보완 여지 있음
	gradeJointVentureReview = "joint_venture_review" // 공동수급 검토: 실적 규모 부족 후보(휴리스틱)
	gradeNeedsConfirmation  = "needs_confirmation"   // 확인 필요: 공고/회사 데이터 자체가 불완전
	gradeNotRecommended     = "not_recommended"      // 참여 곤란: 핵심 필수조건 명확히 미충족
)

// trackRecordThinBudgetRatio: 회사의 최대 계약실적이 공고 예산의 이 비율
// 미만이면 "실적 규모가 부족해 보인다"는 휴리스틱 신호로 본다. 실제
// 참가자격의 실적 요건(문서에서 추출되지 않음 — analyzer가 category='general'
// 로만 저장하고 수치 파싱을 하지 않는다)과는 무관한 참고용 추정치이며,
// gradeReason에 그 사실을 명시한다.
const trackRecordThinBudgetRatio = 0.5

func trackRecordThin(budgetAmount, trackRecordMaxAmount sql.NullInt64) bool {
	if !budgetAmount.Valid || budgetAmount.Int64 <= 0 {
		return false // 공고 예산 자체를 모르면 실적 규모 비교 불가 — 부족하다고 단정하지 않음
	}
	if !trackRecordMaxAmount.Valid {
		return true // 등록된 실적이 아예 없음
	}
	return float64(trackRecordMaxAmount.Int64) < float64(budgetAmount.Int64)*trackRecordThinBudgetRatio
}

// gradeFromCategories computes the 5-tier overall grade on top of the same
// per-category results bucketFromCategories already uses, plus the
// track-record-scale heuristic. Priority order (worst-to-best signal wins):
// 지역/업종/예산 중 하나라도 not_met이면 실적과 무관하게 참여 곤란으로
// 본다 — 공동수급 검토는 사용자 지정 범위대로 "실적 부족" 케이스에만
// 후보로 표시한다.
func gradeFromCategories(categories []categoryScore, thin bool) (grade, reason string) {
	hasNotMet := false
	hasInsufficientData := false
	hasNeedsConfirmation := false
	for _, c := range categories {
		switch c.Result {
		case "not_met":
			hasNotMet = true
		case "insufficient_data":
			hasInsufficientData = true
		case "needs_confirmation":
			hasNeedsConfirmation = true
		}
	}

	switch {
	case hasNotMet:
		return gradeNotRecommended, ""
	case hasInsufficientData:
		return gradeNeedsConfirmation, ""
	case thin:
		return gradeJointVentureReview,
			"회사의 최근 수행실적 규모가 이 공고 예산 대비 작아 보입니다. 공동수급(협력사와 함께 참여)을 " +
				"검토해보세요 — 실제 참가자격의 실적 요건과는 별개의 참고용 추정입니다."
	case hasNeedsConfirmation:
		return gradeConditional, ""
	default:
		return gradeRecommended, ""
	}
}
