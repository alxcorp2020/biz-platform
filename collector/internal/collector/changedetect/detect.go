// Package changedetect compares two NormalizedNotice snapshots field by
// field and classifies the importance of each change (spec 3단계 / 6.6).
package changedetect

import (
	"fmt"
	"time"

	"biz-platform/collector/internal/collector"
)

type FieldChange struct {
	Field      string
	OldValue   string
	NewValue   string
	Importance string // "critical" | "major" | "minor"
}

// criticalFields mirrors spec 6.6 "중요 변경 항목": 참가자격/신청기간/
// 입찰기간/지원금/사업예산/제출서류/평가기준/계약조건/첨부파일.
// At the structured-field level we approximate with these:
var criticalFields = map[string]bool{
	"application_end_at": true,
	"budget_amount":      true,
	"support_amount":     true,
	"status":             true,
}

// Compare returns the list of field-level differences between the current
// stored notice and the freshly normalized one. Returns nil if identical.
func Compare(prev, next collector.NormalizedNotice) []FieldChange {
	var changes []FieldChange

	cmp := func(field, oldV, newV string) {
		if oldV == newV {
			return
		}
		importance := "minor"
		if criticalFields[field] {
			importance = "critical"
		}
		changes = append(changes, FieldChange{Field: field, OldValue: oldV, NewValue: newV, Importance: importance})
	}

	cmp("title", prev.Title, next.Title)
	cmp("organization_name", prev.OrganizationName, next.OrganizationName)
	cmp("region", prev.Region, next.Region)
	cmp("industry", prev.Industry, next.Industry)
	cmp("status", prev.Status, next.Status)
	cmp("application_end_at", fmtTimePtr(prev.ApplicationEndAt), fmtTimePtr(next.ApplicationEndAt))
	cmp("application_start_at", fmtTimePtr(prev.ApplicationStartAt), fmtTimePtr(next.ApplicationStartAt))
	cmp("budget_amount", fmtAmount(prev.BudgetAmount), fmtAmount(next.BudgetAmount))
	cmp("support_amount", fmtAmount(prev.SupportAmount), fmtAmount(next.SupportAmount))

	return changes
}

// OverallChangeType maps the most severe field change to a notice_versions
// row's change_type value.
func OverallChangeType(changes []FieldChange) string {
	if len(changes) == 0 {
		return "minor_update"
	}
	for _, c := range changes {
		if c.Importance == "critical" {
			return "major_update"
		}
	}
	return "minor_update"
}

func fmtTimePtr(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.Format(time.RFC3339)
}

func fmtAmount(v *int64) string {
	if v == nil {
		return ""
	}
	return fmt.Sprintf("%d", *v)
}
