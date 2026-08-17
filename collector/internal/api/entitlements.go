// entitlements.go — 기능 권한(유료 기능) 판정 단일 진입점(2026-08-16, 평가기준
// 맞춤 제안서). 유료 여부는 기존 구독 구조(subscriptions, company_profile_id
// 단위, effectivePlan = active && 미만료만 유효)를 그대로 재사용하고, 기능 키는
// billing.Feature 하나로 판정한다 — 플랜 이름을 핸들러/프론트에서 직접 비교하지
// 않는다. 프론트에는 GET /api/me 응답의 entitlements 맵으로 같은 판정 결과를
// 내려주지만, 실제 기능 API는 반드시 서버에서 다시 검사한다(requirePaidFeature).
package api

import (
	"context"
	"net/http"

	"biz-platform/collector/internal/billing"
)

// errorPaidFeatureRequired — 유료 기능 거부 응답의 machine-readable 코드.
// 프론트는 이 코드를 받으면 결제 안내 모달을 띄운다.
const errorPaidFeatureRequired = "paid_feature_required"

// canUseFeature — profileID(회사)의 effective 플랜이 feature를 쓸 수 있는가.
// 구독 행이 없으면 Free(=거부).
//
// 2026-08-18 Free 제안서 체험: proposal_draft_docx는 플랜 표(Free=false)를 그대로 두되, Free 회사에
// 평생 체험 1회(billing.FreeProposalTrialLifetime)가 남아 있으면 "새 초안을 만들 수 있는 상태"로
// 본다 — 프론트 entitlements 맵과 readiness/생성 게이트가 이 판정을 공유한다. 체험을 이미 썼으면
// 다시 false(결제 안내). 체험으로 만든 기존 초안의 조회/수정/DOCX는 소유 검사만 한다
// (requireOwnedDraft) — 플랜 강등 후에도 자기 초안은 계속 열 수 있다.
func (s *Server) canUseFeature(ctx context.Context, profileID string, feature billing.Feature) (bool, billing.Plan, error) {
	plan, err := s.effectivePlan(ctx, profileID)
	if err != nil {
		return false, "", err
	}
	if billing.PlanHasFeature(plan, feature) {
		return true, plan, nil
	}
	if feature == billing.FeatureProposalDraftDocx && plan == billing.PlanFree {
		used, err := s.countFeatureUsage(ctx, profileID, billing.UsageProposalDraft, usagePeriodLifetime)
		if err != nil {
			return false, plan, err
		}
		return used < billing.FreeProposalTrialLifetime, plan, nil
	}
	return false, plan, nil
}

// entitlementsFor — GET /api/me용. profileID가 비어 있으면(회사 미생성) 전부 false.
func (s *Server) entitlementsFor(ctx context.Context, profileID string) map[string]bool {
	out := make(map[string]bool, len(billing.AllFeatures))
	for _, f := range billing.AllFeatures {
		out[string(f)] = false
	}
	if profileID == "" {
		return out
	}
	for _, f := range billing.AllFeatures {
		allowed, _, err := s.canUseFeature(ctx, profileID, f)
		if err != nil {
			s.logger.Warn("entitlements: feature check failed; denying", "feature", string(f), "error", err)
			allowed = false
		}
		out[string(f)] = allowed
	}
	return out
}

// writePaidFeatureRequired — 403 + {error: paid_feature_required, feature}. 402도
// 가능하지만 기존 quota 거부(ai_analysis_quota_exceeded 등)가 403이라 통일한다.
func writePaidFeatureRequired(w http.ResponseWriter, feature billing.Feature) {
	writeJSON(w, http.StatusForbidden, map[string]any{
		"error":   errorPaidFeatureRequired,
		"feature": string(feature),
	})
}

// requirePaidFeature — 핸들러 공용 게이트. 인증 → 회사 프로필 → 유료 권한 순서로
// 검사하고, 통과하면 (userID, profile, true). 실패 시 응답은 이미 썼다.
// 회사가 없으면 company_profile_required(400) — 기존 회사 API 관례와 동일.
func (s *Server) requirePaidFeature(w http.ResponseWriter, r *http.Request, feature billing.Feature, logTag string) (string, *companyProfileDTO, bool) {
	userID, ok := s.currentUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return "", nil, false
	}
	profile, err := s.getCompanyProfile(r, userID)
	if err != nil {
		s.logger.Error(logTag+": profile lookup failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return "", nil, false
	}
	if profile == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "company_profile_required"})
		return "", nil, false
	}
	allowed, plan, err := s.canUseFeature(r.Context(), profile.ID, feature)
	if err != nil {
		s.logger.Error(logTag+": entitlement check failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return "", nil, false
	}
	if !allowed {
		s.logger.Info("paid gate denied", "feature", string(feature), "plan", string(plan), "userId", userID, "profileId", profile.ID, "handler", logTag)
		writePaidFeatureRequired(w, feature)
		return "", nil, false
	}
	return userID, profile, true
}
