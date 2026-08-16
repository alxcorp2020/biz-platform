package billing

import "testing"

// 플랜→기능 매핑: Free는 유료 기능 없음, 유료 3플랜은 제안서 DOCX 가능,
// 알 수 없는 플랜/기능은 false(fail-closed).
func TestPlanHasFeature(t *testing.T) {
	if PlanHasFeature(PlanFree, FeatureProposalDraftDocx) {
		t.Fatal("free must not have proposal_draft_docx")
	}
	for _, p := range []Plan{PlanBasic, PlanPro, PlanBusiness} {
		if !PlanHasFeature(p, FeatureProposalDraftDocx) {
			t.Fatalf("%s must have proposal_draft_docx", p)
		}
	}
	if PlanHasFeature(Plan("unknown"), FeatureProposalDraftDocx) {
		t.Fatal("unknown plan must be denied")
	}
	if PlanHasFeature(PlanBusiness, Feature("no_such_feature")) {
		t.Fatal("unknown feature must be denied")
	}
	if len(AllFeatures) == 0 || AllFeatures[0] != FeatureProposalDraftDocx {
		t.Fatalf("AllFeatures must list proposal_draft_docx: %v", AllFeatures)
	}
}
