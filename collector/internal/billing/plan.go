// Package billing holds plan definitions and the 토스페이먼츠 confirm API
// client for the OAuth-less, checkout-widget payment flow (CTO 평가 TOP10).
// Only one-off payments are implemented this round — recurring billing
// (빌링키 자동결제) needs separate Toss approval and is deferred.
package billing

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
)

type Plan string

const (
	PlanFree     Plan = "free"
	PlanBasic    Plan = "basic"
	PlanPro      Plan = "pro"
	PlanBusiness Plan = "business"
)

// PlanInfo.MaxPipelineEntries/MaxAIAnalysisPerMonth: -1 means unlimited.
// MaxAIAnalysisPerMonth counts uploads to the 6 AI-extraction document
// endpoints (license/cert, financials, track-records, personnel,
// intellectual-property, employee-verification) — each upload triggers one
// Claude call regardless of whether the user later confirms the extracted
// candidate, so counting uploads (company_documents rows) is the accurate
// usage signal. The notice-detail "AI 참여 분석" score is NOT AI/LLM-based
// (pure rule engine — see scoring.go) and is intentionally excluded from
// this quota.
//
// MaxTeamMembers — 팀 협업(company_members) 최대 인원(owner 포함).
// Business만 3명, 나머지는 본인 1명뿐(팀 기능 자체가 없음) — 예전엔
// company_team.go의 maxTeamMembers()에 상수로 하드코딩되어 있었다.
//
// 아래 값들은 관리자 화면(#/admin/plan-settings)에서 system_settings로
// 오버라이드 가능하다 — api.effectivePlanInfo(billing 패키지는 DB에 접근하지
// 않으므로 오버라이드 로직은 api 패키지 쪽에 있다)가 이 맵을 기본값/폴백으로
// 쓰고 그 위에 관리자 설정값을 얹는다. 이 맵 자체는 항상 "설정이 전혀 없을
// 때의 정적 기본값"으로 남는다.
type PlanInfo struct {
	Name                  string
	AmountKRW             int64
	MaxPipelineEntries    int
	MaxAIAnalysisPerMonth int
	MaxTeamMembers        int
}

var Plans = map[Plan]PlanInfo{
	PlanFree:     {Name: "Free", AmountKRW: 0, MaxPipelineEntries: 3, MaxAIAnalysisPerMonth: 0, MaxTeamMembers: 1},
	PlanBasic:    {Name: "Basic", AmountKRW: 19900, MaxPipelineEntries: -1, MaxAIAnalysisPerMonth: 5, MaxTeamMembers: 1},
	PlanPro:      {Name: "Pro", AmountKRW: 49000, MaxPipelineEntries: -1, MaxAIAnalysisPerMonth: 20, MaxTeamMembers: 1},
	PlanBusiness: {Name: "Business", AmountKRW: 99000, MaxPipelineEntries: -1, MaxAIAnalysisPerMonth: 60, MaxTeamMembers: 3},
}

// PlanOrder lists plans low-to-high for stable UI rendering (map iteration
// order is not stable).
var PlanOrder = []Plan{PlanFree, PlanBasic, PlanPro, PlanBusiness}

// PlanRank returns p's position in PlanOrder (0=Free .. 3=Business), or -1
// for an unknown plan. Used to tell upgrade from downgrade — see
// api/billing.go's handleBillingConfirm(즉시 업그레이드/예약 다운그레이드 정책).
func PlanRank(p Plan) int {
	for i, candidate := range PlanOrder {
		if candidate == p {
			return i
		}
	}
	return -1
}

// ParsePlan validates a plan string against the known plan set.
func ParsePlan(s string) (Plan, bool) {
	p := Plan(s)
	_, ok := Plans[p]
	return p, ok
}

// Purchasable reports whether a plan can go through checkout — Free has
// nothing to pay for.
func (p Plan) Purchasable() bool {
	return p == PlanBasic || p == PlanPro || p == PlanBusiness
}

// EncodeOrderID embeds the plan in the Toss order id itself (plan-<random
// hex>) so /api/billing/confirm can recover what was being purchased from
// the order id alone, without a separate DB lookup keyed by order id —
// subscriptions/payment_log have no order_id column in this schema, only
// company_profile_id, so this is the simplest way to round-trip "what was
// requested" through Toss's redirect. Toss requires 6~64 chars from
// [A-Za-z0-9-_]; "business-" (9) + 32 hex chars = 41, well within range.
func EncodeOrderID(plan Plan) (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate order id: %w", err)
	}
	return string(plan) + "-" + hex.EncodeToString(buf), nil
}

// DecodePlanFromOrderID recovers the plan from an order id produced by
// EncodeOrderID. Splits on the first '-' only, since plan names themselves
// never contain one.
func DecodePlanFromOrderID(orderID string) (Plan, bool) {
	prefix, _, found := strings.Cut(orderID, "-")
	if !found {
		return "", false
	}
	return ParsePlan(prefix)
}
