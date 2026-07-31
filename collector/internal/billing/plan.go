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
// MaxAIAnalysisPerMonth counts uploads to the 5 AI-extraction document
// endpoints (license/cert, financials, track-records, personnel,
// employee-verification) — each upload triggers one Claude call regardless
// of whether the user later confirms the extracted candidate, so counting
// uploads (company_documents rows) is the accurate usage signal. The
// notice-detail "AI 참여 분석" score is NOT AI/LLM-based (pure rule engine —
// see scoring.go) and is intentionally excluded from this quota.
type PlanInfo struct {
	Name                  string
	AmountKRW             int64
	MaxPipelineEntries    int
	MaxAIAnalysisPerMonth int
}

var Plans = map[Plan]PlanInfo{
	PlanFree:     {Name: "Free", AmountKRW: 0, MaxPipelineEntries: 3, MaxAIAnalysisPerMonth: 0},
	PlanBasic:    {Name: "Basic", AmountKRW: 19900, MaxPipelineEntries: -1, MaxAIAnalysisPerMonth: 5},
	PlanPro:      {Name: "Pro", AmountKRW: 49000, MaxPipelineEntries: -1, MaxAIAnalysisPerMonth: 20},
	PlanBusiness: {Name: "Business", AmountKRW: 99000, MaxPipelineEntries: -1, MaxAIAnalysisPerMonth: 60},
}

// PlanOrder lists plans low-to-high for stable UI rendering (map iteration
// order is not stable).
var PlanOrder = []Plan{PlanFree, PlanBasic, PlanPro, PlanBusiness}

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
