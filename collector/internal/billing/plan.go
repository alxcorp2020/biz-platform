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
//
// ---- 플랜 정책 확장(2026-08-18, Free/Basic/Pro 출시·Business 문의) ----
// 세 종류를 구분한다(혼동 금지):
//   - Entitlement(기능 허용/불가): features.go PlanHasFeature — 예: SMS Free 불가, 제안서 유료.
//   - Capacity(동시 보유 개수, 실제 row count 기준): MaxPipelineEntries, MaxTeamMembers,
//     MaxSavedSearches — 삭제하면 자리가 비어 다시 만들 수 있다. 월 사용량으로 세지 않는다.
//   - Consumption usage(기간 내 사용 횟수, feature_usage 테이블): MonthlyLimit(UsageFeature) —
//     participation_review(같은 달 같은 공고는 1건), proposal_draft(새 초안 생성 성공 시 1건),
//     business_registration_ocr(같은 파일은 1건), sms(발송 성공 1건). -1 = 무제한.
//
// 기존 MaxAIAnalysisPerMonth(회사서류 AI추출)와 Free 이메일 월 한도는 원천 테이블 count 방식
// 그대로 유지한다(회귀 방지) — feature_usage로 이전하지 않음.
type PlanInfo struct {
	Name                  string
	AmountKRW             int64
	MaxPipelineEntries    int
	MaxAIAnalysisPerMonth int
	MaxTeamMembers        int
	// MaxSavedSearches — 맞춤공고(saved_searches) 보유 개수 상한(회사 소속 사용자 합산, 온보딩
	// 자동생성분 포함). -1 무제한.
	MaxSavedSearches int
	// MaxParticipationReviewsPerMonth — 참여 가능 여부 확인(공고 상세 참여판정)을 볼 수 있는
	// "서로 다른 공고" 수/월. 같은 공고 재조회는 추가 소비 없음.
	MaxParticipationReviewsPerMonth int
	// MaxProposalDraftsPerMonth — 평가기준 맞춤 제안서 새 초안 생성 수/월. Free는 0이지만
	// 회사당 평생 1회 체험(FreeProposalTrialLifetime)이 별도로 있다.
	MaxProposalDraftsPerMonth int
	// MaxSMSPerMonth — 문자 알림 발송 성공 건수/월(OTP 제외). Free 0(발송 자체 차단 유지).
	MaxSMSPerMonth int
	// MaxBusinessRegistrationOCRPerMonth — 사업자등록증 분석(외부 모델 호출) 회사당 월 상한.
	// 원가 방어용 Fair Use. 같은 파일(해시)은 몇 번 올려도 1건.
	MaxBusinessRegistrationOCRPerMonth int
}

var Plans = map[Plan]PlanInfo{
	PlanFree: {Name: "Free", AmountKRW: 0, MaxPipelineEntries: 3, MaxAIAnalysisPerMonth: 0, MaxTeamMembers: 1,
		MaxSavedSearches: 1, MaxParticipationReviewsPerMonth: 3, MaxProposalDraftsPerMonth: 0, MaxSMSPerMonth: 0, MaxBusinessRegistrationOCRPerMonth: 5},
	PlanBasic: {Name: "Basic", AmountKRW: 19900, MaxPipelineEntries: -1, MaxAIAnalysisPerMonth: 5, MaxTeamMembers: 1,
		MaxSavedSearches: 5, MaxParticipationReviewsPerMonth: 30, MaxProposalDraftsPerMonth: 5, MaxSMSPerMonth: 10, MaxBusinessRegistrationOCRPerMonth: 20},
	PlanPro: {Name: "Pro", AmountKRW: 49000, MaxPipelineEntries: -1, MaxAIAnalysisPerMonth: 20, MaxTeamMembers: 1,
		MaxSavedSearches: 20, MaxParticipationReviewsPerMonth: 200, MaxProposalDraftsPerMonth: 30, MaxSMSPerMonth: 100, MaxBusinessRegistrationOCRPerMonth: 20},
	// Business — 이번 출시 판매 대상 아님("기업용 문의/준비 중"). 기존 사용 상태를 깨지 않도록
	// 사용량 한도는 무제한으로 두고, 팀 인원(3)·AI 한도(60)는 기존 값 유지.
	PlanBusiness: {Name: "Business", AmountKRW: 99000, MaxPipelineEntries: -1, MaxAIAnalysisPerMonth: 60, MaxTeamMembers: 3,
		MaxSavedSearches: -1, MaxParticipationReviewsPerMonth: -1, MaxProposalDraftsPerMonth: -1, MaxSMSPerMonth: -1, MaxBusinessRegistrationOCRPerMonth: 20},
}

// FreeProposalTrialLifetime — Free 회사가 평생 1회 체험할 수 있는 제안서 초안 수(period_key
// 'lifetime'). 체험으로 만든 초안은 이후에도 조회/수정/DOCX 다운로드가 가능하다.
const FreeProposalTrialLifetime = 1

// MonthlyLimit — 소비형 사용량 기능의 월 상한(-1 무제한, 0 불가).
func (p PlanInfo) MonthlyLimit(f UsageFeature) int {
	switch f {
	case UsageParticipationReview:
		return p.MaxParticipationReviewsPerMonth
	case UsageProposalDraft:
		return p.MaxProposalDraftsPerMonth
	case UsageSMS:
		return p.MaxSMSPerMonth
	case UsageBusinessRegistrationOCR:
		return p.MaxBusinessRegistrationOCRPerMonth
	}
	return 0
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
