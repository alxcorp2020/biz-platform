// scoring.go — 순수(비영속) 참여 가능성 스코어링. eligibility.go의
// scoreRegion/scoreIndustry/scoreBudgetSize를 조합해 공고 하나에 대한
// "참여 추천 여부"를 계산한다. DB 접근이 전혀 없어 대시보드가 공고
// 수백 건을 스캔할 때 반복 호출해도 저렴하다 — 감사이력이 필요한
// 사용자 명시적 평가(POST /api/notices/{id}/evaluate)는 기존처럼
// eligibility.go의 evaluate* 함수(영속)를 그대로 쓴다.
package api

import (
	"database/sql"
	"fmt"
	"strconv"
	"strings"
)

type noticeScoringInput struct {
	// NoticeType: "procurement"(기본값, 빈 문자열도 이걸로 취급 — 기존
	// 호출부와 하위호환) | "support_program". 지원사업이면 아래
	// scoreNoticeForCompany가 3-카테고리 자동판정을 건너뛰고
	// supportProgramScore()로 대체한다.
	NoticeType   string
	Region       sql.NullString
	Industry     sql.NullString
	BudgetAmount sql.NullInt64
	// IndustryRestricted — g2b indstrytyLmtYn(업종제한 여부). nil이면 미상(비-g2b
	// 소스 등)이라 기존 그룹 매칭으로 폴백. 채우는 건 각 로드 지점의 선택이므로
	// (옵셔널) 지정 안 하면 자연히 nil = 기존 동작.
	IndustryRestricted *bool
	// OfficialRegions/RegionEnriched — 공식 참가가능지역 authoritative 데이터(2026-08-11).
	// 각 호출부가 regionAuthoritiesByNoticeIDs/ByVersions로 배치 로드해 채운다. 지정 안 하면
	// (nil/false) = enrichment 미실행으로 취급 → 추론 region만으로 전국 PASS를 만들지 않는다.
	OfficialRegions []string
	RegionEnriched  bool
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
// (기존 3버킷 — 하위호환을 위해 그대로 유지). Grade는 4단계 종합판정
// (아래 grade* 상수) — 지역/업종/예산 규모 3요소만으로 계산되는 "통과/
// 불통과"의 핵심 기준이다. MetCount/TotalCount back the "N개 조건 중 M개
// 충족" 문구 — 근거 없는 확률 대신 항상 이 비율을 함께 보여준다(스펙
// 논의에서 확정한 원칙).
//
// JointVentureRecommended/JointVentureReason — 2026-08-07, 실적 규모
// 신뢰도 보조지표(판정엔진 확장 1단계). 과거엔 "실적 규모가 얕음"이
// gradeJointVentureReview라는 별도 등급으로 Grade 자체를 대체했는데,
// "메인 등급(참여권장/조건보완/참여어려움)은 절대 안 바뀌고 같은 등급
// 안에서 신뢰도만 세분화한다"는 새 원칙으로 Grade와 완전히 독립된
// 필드로 분리했다(사용자 확인 — 대시보드 "오늘 할 일"/추천공고 다이제스트
// 이메일/자동화 통계/리포트 4곳에서 이 등급 공고들이 이제 "참여권장"으로
// 카운트되는 실제 동작 변화를 인지하고 승인함). computeJointVentureSignal이
// 채운다 — 데이터(공고 예산 또는 회사 실적)가 없으면 절대 추측하지 않고
// false/안내문구로 남긴다.
type participationScore struct {
	Bucket                  string          `json:"bucket"`
	Grade                   string          `json:"grade"`
	GradeReason             string          `json:"gradeReason,omitempty"`
	JointVentureRecommended bool            `json:"jointVentureRecommended,omitempty"`
	JointVentureReason      string          `json:"jointVentureReason,omitempty"`
	MetCount                int             `json:"metCount"`
	TotalCount              int             `json:"totalCount"`
	Categories              []categoryScore `json:"categories"`
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
	regionResult, regionReason, regionGapSide := scoreRegion(
		regionAuthority{OfficialRegions: notice.OfficialRegions, Enriched: notice.RegionEnriched},
		notice.Region, company.Region)
	industryResult, industryReason, industryGapSide := scoreIndustry(notice.Industry, company.Industry, notice.IndustryRestricted)
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

	grade := gradeFromCategories(categories)
	jvRecommended, jvReason := computeJointVentureSignal(notice.BudgetAmount, company.TrackRecordMaxAmount)

	return participationScore{
		Bucket:                  bucketFromCategories(categories),
		Grade:                   grade,
		JointVentureRecommended: jvRecommended,
		JointVentureReason:      jvReason,
		MetCount:                metCount,
		TotalCount:              len(categories),
		Categories:              categories,
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
	gradeRecommended = "recommended"        // 참여 권장: 모든 필수조건 충족
	gradeConditional = "conditional"        // 조건부 참여 가능: 애매하지만 보완 여지 있음
	// gradeJointVentureReview("joint_venture_review")는 2026-08-07 폐지 —
	// 실적 규모 신호는 이제 Grade를 대체하지 않고 JointVentureRecommended
	// 서브태그로 독립됐다(participationScore 주석 참고). 과거 값이라
	// GRADE_LABELS/GRADE_BADGE_TONE(프론트) 매핑에는 하위호환용으로 남아있음.
	gradeNeedsConfirmation = "needs_confirmation" // 확인 필요: 공고/회사 데이터 자체가 불완전
	gradeNotRecommended    = "not_recommended"    // 참여 곤란: 핵심 필수조건 명확히 미충족
)

// trackRecordThinBudgetRatio: 회사의 최대 계약실적이 공고 예산의 이 비율
// 미만이면 "실적 규모가 부족해 보인다"는 신호로 보고 공동수급 검토
// 서브태그를 켠다. 실제 참가자격의 실적 요건(문서에서 추출되지 않음 —
// analyzer가 category='general'로만 저장하고 수치 파싱을 하지 않는다)과는
// 무관한 참고용 추정치이며, JointVentureReason에 그 사실을 명시한다.
const trackRecordThinBudgetRatio = 0.5

// formatKRWAmount — "12345678" -> "12,345,678". 실적/예산 금액은 항상
// 0 이상이라 음수 부호 케이스는 다루지 않는다.
func formatKRWAmount(n int64) string {
	s := strconv.FormatInt(n, 10)
	if len(s) <= 3 {
		return s
	}
	var parts []string
	for len(s) > 3 {
		parts = append([]string{s[len(s)-3:]}, parts...)
		s = s[:len(s)-3]
	}
	parts = append([]string{s}, parts...)
	return strings.Join(parts, ",")
}

// computeJointVentureSignal — 실적 규모 vs 공고 예산 실측 비교(판정엔진
// 확장 1단계, 2026-08-07). 과거엔 "회사의 최근 수행실적 규모가 이 공고
// 예산 대비 작아 보입니다"라는 고정 문구를 항상 내보냈는데, 실제
// company_track_records 데이터로 정확한 금액·비율을 계산해서 대체한다.
//
// 데이터가 없으면 절대 추측하지 않는다(원칙 재확인): 공고 예산 자체를
// 모르면 비교가 성립하지 않아 빈 값을 반환하고, 회사 실적이 아예
// 등록되지 않았으면(예전처럼 "얕다"고 단정하지 않고) 등록을 유도하는
// 중립 안내만 반환한다 — recommended=false, reason=등록 유도 문구.
func computeJointVentureSignal(budgetAmount, trackRecordMaxAmount sql.NullInt64) (recommended bool, reason string) {
	if !budgetAmount.Valid || budgetAmount.Int64 <= 0 {
		return false, ""
	}
	if !trackRecordMaxAmount.Valid {
		return false, "실적을 등록하면 더 정확한 분석이 가능합니다."
	}
	ratio := float64(trackRecordMaxAmount.Int64) / float64(budgetAmount.Int64)
	pct := int(ratio*100 + 0.5)
	comment := fmt.Sprintf("최근 실적 %s원, 이 공고 예산의 %d%% 수준입니다.", formatKRWAmount(trackRecordMaxAmount.Int64), pct)
	if ratio < trackRecordThinBudgetRatio {
		return true, comment + " 공동수급(협력사와 함께 참여)을 검토해보세요 — 실제 참가자격의 실적 요건과는 별개의 참고용 추정입니다."
	}
	return false, comment
}

// gradeFromCategories computes the 4-tier overall grade purely from 지역/
// 업종/예산 규모 3요소(scoreRegion/scoreIndustry/scoreBudgetSize) — 이
// 함수는 2026-08-07부로 실적 규모 신호를 전혀 반영하지 않는다(과거엔
// "실적 규모가 얕음"이면 이 등급 자체를 joint_venture_review로 대체했지만,
// "메인 등급은 안 바뀌고 서브태그만 추가"라는 새 원칙으로 완전히
// 분리했다 — computeJointVentureSignal 참고).
func gradeFromCategories(categories []categoryScore) string {
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
		return gradeNotRecommended
	case hasInsufficientData:
		return gradeNeedsConfirmation
	case hasNeedsConfirmation:
		return gradeConditional
	default:
		return gradeRecommended
	}
}
