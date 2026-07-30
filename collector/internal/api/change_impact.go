// 공고 변경 → 우리 회사 영향 자동 설명. notice_changes(changedetect가
// 감지한 필드 변경)와 scoreNoticeForCompany(scoring.go의 순수 참여
// 가능성 스코어링)를 조합한다. AI 호출 없이 규칙 기반 템플릿으로만
// 문장을 만든다.
//
// notice_versions에는 region/industry/budget_amount 스냅샷이 없고
// notices(단일 행)만 현재 상태를 들고 있어, "변경 전" 상태는 현재값을
// notice_changes.old_value로 되돌려 재구성한다. 같은 필드가 연속으로
// 여러 번 바뀌었으면 오래된 변경 항목은 이 방식으로는 부정확해질 수
// 있으므로, 이 기능은 의도적으로 "최신 변경"(to_version_id가 현재
// 버전인 것)에만 적용한다 — 그 경우 재구성이 항상 정확하다.
package api

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/lib/pq"
)

// scoringRelevantFields: scoreNoticeForCompany가 실제로 반영하는 필드만
// 여기 포함한다. 제목/마감일/상태 등 다른 필드 변경은 현재 스코어링
// 로직상 참여 가능성 버킷에 영향이 없으므로 영향 분석을 만들지 않는다.
var scoringRelevantFields = []string{"region", "industry", "budget_amount"}

type changeImpactField struct {
	Field   string `json:"field"`
	Summary string `json:"summary"`
}

type changeImpact struct {
	Before        participationScore  `json:"before"`
	After         participationScore  `json:"after"`
	ChangedFields []changeImpactField `json:"changedFields"`
}

// computeLatestChangeImpact returns nil, nil when no scoring-relevant field
// changed in the transition ending at currentVersionID — the caller should
// simply omit the section in that case.
func (s *Server) computeLatestChangeImpact(
	ctx context.Context, currentVersionID string,
	current noticeScoringInput, company companyScoringInput, after participationScore,
) (*changeImpact, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT changed_field, old_value, new_value
		FROM notice_changes
		WHERE to_version_id = $1 AND changed_field = ANY($2)`,
		currentVersionID, pq.Array(scoringRelevantFields),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	before := current
	fields := []changeImpactField{}
	for rows.Next() {
		var field string
		var oldValue, newValue sql.NullString
		if err := rows.Scan(&field, &oldValue, &newValue); err != nil {
			return nil, err
		}

		var intro string
		switch field {
		case "region":
			before.Region = oldValue
			intro = describeRegionChange(oldValue.String, newValue.String)
		case "industry":
			before.Industry = oldValue
			intro = "업종 조건이 변경되었습니다."
		case "budget_amount":
			if v, ok := parseInt64OrEmpty(oldValue.String); ok {
				before.BudgetAmount = sql.NullInt64{Int64: v, Valid: true}
			} else {
				before.BudgetAmount = sql.NullInt64{}
			}
			intro = describeBudgetChange(oldValue.String, newValue.String)
		default:
			continue
		}
		fields = append(fields, changeImpactField{Field: field, Summary: intro + " " + reasonForCategory(after, field)})
	}
	if len(fields) == 0 {
		return nil, nil
	}

	beforeScore := scoreNoticeForCompany(before, company)
	return &changeImpact{Before: beforeScore, After: after, ChangedFields: fields}, nil
}

// reasonForCategory finds the Reason text scoreNoticeForCompany already
// generated for this field's category in the "after"(current) result —
// reused as-is instead of writing new phrasing, so the explanation stays
// consistent with the wording already shown elsewhere (참가자격 요건 등).
func reasonForCategory(score participationScore, field string) string {
	category := map[string]string{"region": "지역", "industry": "업종", "budget_amount": "예산 규모"}[field]
	for _, c := range score.Categories {
		if c.Category == category {
			return c.Reason
		}
	}
	return ""
}

func describeRegionChange(oldValue, newValue string) string {
	oldOpen := oldValue == "" || oldValue == regionNationwide
	newOpen := newValue == "" || newValue == regionNationwide
	switch {
	case oldOpen && !newOpen:
		return "지역제한이 추가되었습니다."
	case !oldOpen && newOpen:
		return "지역제한이 완화되었습니다."
	default:
		return "지역 조건이 변경되었습니다."
	}
}

func describeBudgetChange(oldValue, newValue string) string {
	oldAmount, oldOK := parseInt64OrEmpty(oldValue)
	newAmount, newOK := parseInt64OrEmpty(newValue)
	if !oldOK || !newOK {
		return "예산 규모 정보가 변경되었습니다."
	}
	if newAmount > oldAmount {
		return "예산 규모가 증가했습니다."
	}
	if newAmount < oldAmount {
		return "예산 규모가 감소했습니다."
	}
	return "예산 규모 정보가 변경되었습니다."
}

func parseInt64OrEmpty(s string) (int64, bool) {
	if s == "" {
		return 0, false
	}
	var v int64
	if _, err := fmt.Sscanf(s, "%d", &v); err != nil {
		return 0, false
	}
	return v, true
}
