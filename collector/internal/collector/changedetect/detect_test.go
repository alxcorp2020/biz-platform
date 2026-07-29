package changedetect

import (
	"testing"
	"time"

	"biz-platform/collector/internal/collector"
)

func amount(v int64) *int64 { return &v }
func date(s string) *time.Time {
	t, _ := time.Parse("2006-01-02", s)
	return &t
}

func TestCompare_NoChanges(t *testing.T) {
	n := collector.NormalizedNotice{Title: "A", Status: "open", ApplicationEndAt: date("2026-08-01")}
	changes := Compare(n, n)
	if len(changes) != 0 {
		t.Fatalf("expected no changes for identical notices, got %d", len(changes))
	}
}

func TestCompare_DeadlineChangeIsCritical(t *testing.T) {
	prev := collector.NormalizedNotice{Title: "A", ApplicationEndAt: date("2026-08-01")}
	next := collector.NormalizedNotice{Title: "A", ApplicationEndAt: date("2026-08-10")}

	changes := Compare(prev, next)
	if len(changes) != 1 {
		t.Fatalf("expected exactly 1 change, got %d", len(changes))
	}
	if changes[0].Field != "application_end_at" || changes[0].Importance != "critical" {
		t.Fatalf("expected critical application_end_at change, got %+v", changes[0])
	}
}

func TestCompare_TitleChangeIsMinor(t *testing.T) {
	prev := collector.NormalizedNotice{Title: "A"}
	next := collector.NormalizedNotice{Title: "A (수정)"}

	changes := Compare(prev, next)
	if len(changes) != 1 || changes[0].Importance != "minor" {
		t.Fatalf("expected 1 minor change, got %+v", changes)
	}
}

func TestCompare_BudgetChangeIsCritical(t *testing.T) {
	prev := collector.NormalizedNotice{BudgetAmount: amount(10000000)}
	next := collector.NormalizedNotice{BudgetAmount: amount(15000000)}

	changes := Compare(prev, next)
	if len(changes) != 1 || changes[0].Importance != "critical" {
		t.Fatalf("expected critical budget change, got %+v", changes)
	}
}

func TestOverallChangeType(t *testing.T) {
	if got := OverallChangeType(nil); got != "minor_update" {
		t.Fatalf("expected minor_update for no changes, got %s", got)
	}
	critical := []FieldChange{{Field: "budget_amount", Importance: "critical"}}
	if got := OverallChangeType(critical); got != "major_update" {
		t.Fatalf("expected major_update when a critical field changed, got %s", got)
	}
}
