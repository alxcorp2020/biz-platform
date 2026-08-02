// plan_settings.go — 플랜별 한도/가격을 관리자 화면(#/admin/plan-settings)에서
// 재배포 없이 조정 가능하게 한다. billing.Plans(billing/plan.go)는 계속
// "설정이 전혀 없을 때의 정적 기본값/폴백"으로 남고, 이 파일이 그 위에
// system_settings 오버라이드를 얹는다 — free_plan_email_limit과 동일하게
// 캐시 없이 매번 DB에서 직접 읽는다(system_settings.go 참고).
package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"time"

	"biz-platform/collector/internal/billing"
)

// planOverrideField — 플랜의 한 필드(파이프라인 한도/AI분석 한도/가격/팀원
// 한도)를 system_settings 키(settingKey)와 관리자 API의 JSON 필드명(jsonKey)에
// 연결한다. minValue는 저장을 거부할 하한값 — 한도류는 -1(무제한)까지
// 허용하고, 가격은 음수가 의미 없어 0부터 허용한다.
type planOverrideField struct {
	settingKey string
	jsonKey    string
	minValue   int
}

// planOverrides — 플랜마다 어떤 필드가 관리자 화면에서 오버라이드
// 가능한지. nil인 필드는 그 플랜에서 조정 불가(관리자 화면에 입력란 자체가
// 안 뜨고, effectivePlanInfo도 항상 billing.Plans의 하드코딩 값을 그대로
// 씀) — Free의 가격은 정의상 항상 0, 유료 3개 플랜의 파이프라인은 무제한이
// 의도된 설계, Business 외 플랜은 팀 기능 자체가 없어 팀원 한도 조정이
// 의미 없다(이번 요청 범위에서 명시적으로 제외한 부분).
type planOverrides struct {
	pipeline    *planOverrideField
	aiAnalysis  *planOverrideField
	price       *planOverrideField
	memberLimit *planOverrideField
}

var planOverridesByPlan = map[billing.Plan]planOverrides{
	billing.PlanFree: {
		pipeline:   &planOverrideField{settingKey: "free_pipeline_limit", jsonKey: "freePipelineLimit", minValue: -1},
		aiAnalysis: &planOverrideField{settingKey: "free_ai_analysis_limit", jsonKey: "freeAiAnalysisLimit", minValue: -1},
	},
	billing.PlanBasic: {
		aiAnalysis: &planOverrideField{settingKey: "basic_ai_analysis_limit", jsonKey: "basicAiAnalysisLimit", minValue: -1},
		price:      &planOverrideField{settingKey: "basic_price_krw", jsonKey: "basicPriceKrw", minValue: 0},
	},
	billing.PlanPro: {
		aiAnalysis: &planOverrideField{settingKey: "pro_ai_analysis_limit", jsonKey: "proAiAnalysisLimit", minValue: -1},
		price:      &planOverrideField{settingKey: "pro_price_krw", jsonKey: "proPriceKrw", minValue: 0},
	},
	billing.PlanBusiness: {
		aiAnalysis:  &planOverrideField{settingKey: "business_ai_analysis_limit", jsonKey: "businessAiAnalysisLimit", minValue: -1},
		price:       &planOverrideField{settingKey: "business_price_krw", jsonKey: "businessPriceKrw", minValue: 0},
		memberLimit: &planOverrideField{settingKey: "business_member_limit", jsonKey: "businessMemberLimit", minValue: -1},
	},
}

// effectivePlanInfo returns billing.Plans[plan] with any admin-configured
// system_settings override applied. 결제/한도체크/가격표시 등 billing.Plans[plan]을
// 직접 읽던 모든 지점이 이 함수를 거치도록 바꿨다(billing.go/billing_ai_usage.go/
// dashboard.go/company_team.go) — Name 필드만 읽는 곳(admin.go의 플랜명 표시 등)은
// 어차피 안 바뀌는 값이라 그대로 billing.Plans[plan].Name을 직접 쓴다(불필요한
// DB 조회 방지).
func (s *Server) effectivePlanInfo(ctx context.Context, plan billing.Plan) billing.PlanInfo {
	info := billing.Plans[plan]
	ov := planOverridesByPlan[plan]
	if ov.pipeline != nil {
		v, _ := s.getSystemSettingInt(ctx, ov.pipeline.settingKey, info.MaxPipelineEntries)
		info.MaxPipelineEntries = v
	}
	if ov.aiAnalysis != nil {
		v, _ := s.getSystemSettingInt(ctx, ov.aiAnalysis.settingKey, info.MaxAIAnalysisPerMonth)
		info.MaxAIAnalysisPerMonth = v
	}
	if ov.price != nil {
		v, _ := s.getSystemSettingInt(ctx, ov.price.settingKey, int(info.AmountKRW))
		info.AmountKRW = int64(v)
	}
	if ov.memberLimit != nil {
		v, _ := s.getSystemSettingInt(ctx, ov.memberLimit.settingKey, info.MaxTeamMembers)
		info.MaxTeamMembers = v
	}
	return info
}

type planOverrideEntry struct {
	settingKey   string
	jsonKey      string
	defaultValue int
	minValue     int
}

// allPlanOverrideEntries flattens planOverridesByPlan(4개 플랜) into every
// individually adjustable (setting key, json field, 기본값, 최소값) — 관리자
// GET/PUT 핸들러가 이 목록 하나로 조회/검증/저장을 전부 처리한다. defaultValue는
// billing.Plans에서 그대로 가져온다 — system_settings에 값이 아직 없을 때
// (예: 이 기능이 생기기 전 DB) 이 기능이 생기기 전과 동일하게 동작하도록.
func allPlanOverrideEntries() []planOverrideEntry {
	var out []planOverrideEntry
	for _, plan := range billing.PlanOrder {
		info := billing.Plans[plan]
		ov := planOverridesByPlan[plan]
		if ov.pipeline != nil {
			out = append(out, planOverrideEntry{ov.pipeline.settingKey, ov.pipeline.jsonKey, info.MaxPipelineEntries, ov.pipeline.minValue})
		}
		if ov.aiAnalysis != nil {
			out = append(out, planOverrideEntry{ov.aiAnalysis.settingKey, ov.aiAnalysis.jsonKey, info.MaxAIAnalysisPerMonth, ov.aiAnalysis.minValue})
		}
		if ov.price != nil {
			out = append(out, planOverrideEntry{ov.price.settingKey, ov.price.jsonKey, int(info.AmountKRW), ov.price.minValue})
		}
		if ov.memberLimit != nil {
			out = append(out, planOverrideEntry{ov.memberLimit.settingKey, ov.memberLimit.jsonKey, info.MaxTeamMembers, ov.memberLimit.minValue})
		}
	}
	return out
}

// effectiveAIAnalysisLimit — 이번달 AI 분석 한도(개별 회원 한도조정,
// #/admin/members/{id} 화면의 "이번달 한도 임시조정" 반영). company_profiles.
// custom_ai_analysis_limit_month가 이번 달('YYYY-MM' 문자열 비교, DATE 타입
// 대신 이 형식을 쓴 이유는 ensureCompanyProfileCustomAILimitColumns 주석
// 참고)과 정확히 일치할 때만 커스텀 값을 쓰고, 그 외(설정 없음/지난 달
// 값이 남아있음)엔 플랜 기본값으로 조용히 되돌아간다 — 별도 배치/스케줄
// 없이 조회 시점 비교만으로 "이번 달에만 적용" 정책을 구현한다.
func (s *Server) effectiveAIAnalysisLimit(ctx context.Context, profileID string, plan billing.Plan) (int, error) {
	var customLimit sql.NullInt64
	var customMonth sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT custom_ai_analysis_limit, custom_ai_analysis_limit_month FROM company_profiles WHERE id = $1`,
		profileID,
	).Scan(&customLimit, &customMonth)
	if err != nil {
		return 0, err
	}
	if customLimit.Valid && customMonth.Valid && customMonth.String == time.Now().Format("2006-01") {
		return int(customLimit.Int64), nil
	}
	return s.effectivePlanInfo(ctx, plan).MaxAIAnalysisPerMonth, nil
}

func (s *Server) currentPlanSettings(ctx context.Context) map[string]int {
	result := map[string]int{}
	for _, f := range allPlanOverrideEntries() {
		v, _ := s.getSystemSettingInt(ctx, f.settingKey, f.defaultValue)
		result[f.jsonKey] = v
	}
	return result
}

// handleAdminGetPlanSettings — GET /api/admin/plan-settings.
func (s *Server) handleAdminGetPlanSettings(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireSystemAdmin(w, r); !ok {
		return
	}
	writeJSON(w, http.StatusOK, s.currentPlanSettings(r.Context()))
}

// handleAdminUpdatePlanSettings — PUT /api/admin/plan-settings. 요청에 없는
// 필드는 그대로 둔다(부분 업데이트 허용 — 화면은 항상 9개 전부를 보내지만,
// API 자체는 유연하게 열어둔다). 값 하나라도 최소값 미만이면 아무것도
// 반영하지 않고 통째로 거부한다(일부만 반영되는 어중간한 상태 방지).
func (s *Server) handleAdminUpdatePlanSettings(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireSystemAdmin(w, r); !ok {
		return
	}
	var req map[string]int
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_body"})
		return
	}

	entries := allPlanOverrideEntries()
	for _, f := range entries {
		if v, present := req[f.jsonKey]; present && v < f.minValue {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_value", "field": f.jsonKey})
			return
		}
	}
	for _, f := range entries {
		v, present := req[f.jsonKey]
		if !present {
			continue
		}
		if err := s.setSystemSettingInt(r.Context(), f.settingKey, v); err != nil {
			s.logger.Error("admin-update-plan-settings: update failed", "field", f.jsonKey, "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
			return
		}
	}
	writeJSON(w, http.StatusOK, s.currentPlanSettings(r.Context()))
}
