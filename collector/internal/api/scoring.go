// scoring.go — 순수(비영속) 참여 가능성 스코어링. eligibility.go의
// scoreRegion/scoreIndustry/scoreBudgetSize를 조합해 공고 하나에 대한
// "참여 추천 여부"를 계산한다. DB 접근이 전혀 없어 대시보드가 공고
// 수백 건을 스캔할 때 반복 호출해도 저렴하다 — 감사이력이 필요한
// 사용자 명시적 평가(POST /api/notices/{id}/evaluate)는 기존처럼
// eligibility.go의 evaluate* 함수(영속)를 그대로 쓴다.
package api

import "database/sql"

type noticeScoringInput struct {
	Region       sql.NullString
	Industry     sql.NullString
	BudgetAmount sql.NullInt64
}

type companyScoringInput struct {
	Region   sql.NullString
	Industry []string
	Size     sql.NullString
}

type categoryScore struct {
	Category string `json:"category"`
	Result   string `json:"result"`
	Reason   string `json:"reason"`
}

// participationScore.Bucket is one of "ready" | "needs_review" | "not_recommended".
// MetCount/TotalCount back the "N개 조건 중 M개 충족" 문구 — 근거 없는
// 확률 대신 항상 이 비율을 함께 보여준다(스펙 논의에서 확정한 원칙).
type participationScore struct {
	Bucket     string          `json:"bucket"`
	MetCount   int             `json:"metCount"`
	TotalCount int             `json:"totalCount"`
	Categories []categoryScore `json:"categories"`
}

func scoreNoticeForCompany(notice noticeScoringInput, company companyScoringInput) participationScore {
	regionResult, regionReason := scoreRegion(notice.Region, company.Region)
	industryResult, industryReason := scoreIndustry(notice.Industry, company.Industry)
	budgetResult, budgetReason := scoreBudgetSize(notice.BudgetAmount, company.Size)

	categories := []categoryScore{
		{Category: "지역", Result: regionResult, Reason: regionReason},
		{Category: "업종", Result: industryResult, Reason: industryReason},
		{Category: "예산 규모", Result: budgetResult, Reason: budgetReason},
	}

	metCount := 0
	for _, c := range categories {
		if c.Result == "met" {
			metCount++
		}
	}

	return participationScore{
		Bucket:     bucketFromCategories(categories),
		MetCount:   metCount,
		TotalCount: len(categories),
		Categories: categories,
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
