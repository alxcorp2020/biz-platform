package billing

// Feature — 플랜에 묶인 "기능 권한" 키. 플랜 이름(free/basic/pro/business)을
// UI/API 여러 곳에서 직접 비교하지 않고, 기능 하나당 키 하나로 판정한다
// (2026-08-16, 평가기준 맞춤 제안서). 유료 플랜 구성이 바뀌면 이 파일의
// 매핑만 고치면 되고, 호출부(api.canUseFeature / 프론트 entitlements)는
// 그대로다. 사용량 quota(월 N건)는 별도 상품정책 승인 전이라 여기 없다 —
// "유료 여부"만 판정한다.
type Feature string

const (
	// FeatureProposalDraftDocx — 평가기준 맞춤 제안서(수정 가능한 DOCX 초안).
	// 유료 플랜(Basic 이상) 전용. 사용자 화면 문구는 "유료 서비스"로만 안내하고
	// 플랜 이름/가격은 결제 화면(#/billing/plans)이 기존 billing 데이터로 보여준다.
	FeatureProposalDraftDocx Feature = "proposal_draft_docx"
)

// planFeatures — 플랜별 사용 가능 기능. Free는 어떤 유료 기능도 없다(빈 집합).
// 새 유료 기능은 여기에만 추가한다.
var planFeatures = map[Plan]map[Feature]bool{
	PlanFree:     {},
	PlanBasic:    {FeatureProposalDraftDocx: true},
	PlanPro:      {FeatureProposalDraftDocx: true},
	PlanBusiness: {FeatureProposalDraftDocx: true},
}

// UsageFeature — 소비형 사용량(feature_usage 테이블)의 기능 키. Entitlement(Feature)와
// 별개다: 예를 들어 제안서는 Entitlement(proposal_draft_docx)로 "쓸 수 있는가"를, UsageProposalDraft로
// "이번 달 몇 개 만들었는가"를 각각 본다.
type UsageFeature string

const (
	UsageParticipationReview     UsageFeature = "participation_review"
	UsageProposalDraft           UsageFeature = "proposal_draft"
	UsageBusinessRegistrationOCR UsageFeature = "business_registration_ocr"
	UsageSMS                     UsageFeature = "sms"
)

// AllUsageFeatures — /api/me usage 맵 순서 고정용.
var AllUsageFeatures = []UsageFeature{UsageParticipationReview, UsageProposalDraft, UsageBusinessRegistrationOCR, UsageSMS}

// AllFeatures — 프론트에 내려주는 entitlements 맵을 안정적으로 만들기 위한
// 전체 기능 키 목록(순서 고정).
var AllFeatures = []Feature{FeatureProposalDraftDocx}

// PlanHasFeature — 주어진 (effective) 플랜이 feature를 쓸 수 있는가.
// 알 수 없는 플랜/기능은 false(fail-closed).
func PlanHasFeature(p Plan, f Feature) bool {
	fs, ok := planFeatures[p]
	if !ok {
		return false
	}
	return fs[f]
}
