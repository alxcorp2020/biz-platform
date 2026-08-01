package billing

import "testing"

func TestPlanRank_Ordering(t *testing.T) {
	if !(PlanRank(PlanFree) < PlanRank(PlanBasic) &&
		PlanRank(PlanBasic) < PlanRank(PlanPro) &&
		PlanRank(PlanPro) < PlanRank(PlanBusiness)) {
		t.Fatalf("expected Free < Basic < Pro < Business, got %d,%d,%d,%d",
			PlanRank(PlanFree), PlanRank(PlanBasic), PlanRank(PlanPro), PlanRank(PlanBusiness))
	}
}

func TestPlanRank_Unknown(t *testing.T) {
	if got := PlanRank(Plan("nonsense")); got != -1 {
		t.Errorf("PlanRank(unknown) = %d, want -1", got)
	}
}
