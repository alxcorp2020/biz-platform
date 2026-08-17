// feature_usage.go — 플랜별 소비형 사용량(usage quota) 공용 진입점(2026-08-18).
//
// 세 종류를 구분한다(billing/plan.go 주석 참고):
//   - Entitlement: 기능 허용/불가 → entitlements.go(PlanHasFeature).
//   - Capacity: 지금 보유 중인 개수(saved_searches·pipeline·team) → 실제 row count로 제한.
//     이 파일의 checkSavedSearchCapacity가 그 예. 월 사용량으로 세지 않는다.
//   - Consumption usage: 기간 동안 실제로 쓴 횟수 → feature_usage 테이블(이 파일).
//
// 소비 규칙:
//   - subject_key로 dedup: 같은 (회사, 기능, 기간, 대상)은 몇 번 요청해도 1건. 참여판정은 공고 id,
//     제안서는 새 초안 id, OCR은 파일 해시, SMS는 알림 식별자.
//   - 동시성: 같은 (회사, 기능, 기간)에 대해 트랜잭션 advisory lock을 잡고 "대상 존재 → 카운트 →
//     INSERT"를 직렬화한다. 한도 3에 동시 10 요청이 와도 신규 대상은 정확히 3개만 승인된다.
//     외부 lock 시스템 없이 PostgreSQL만 쓴다.
//   - 실패 미차감: 외부 provider(OCR·SMS)는 "예약(consume) → 호출 → 실패 시 release(삭제)" 패턴.
//     provider 호출을 DB 트랜잭션 안에 두지 않는다. 제안서는 composer가 결정론(외부 호출 없음)이라
//     초안 INSERT와 사용량 INSERT를 같은 트랜잭션에 묶어 원자적으로 처리한다.
//   - 기간키: 월은 한국 표준시 기준 'YYYY-MM'(고정 오프셋 — distroless 이미지에 tzdata가 없어
//     time.LoadLocation을 쓰지 않는다), Free 제안서 체험은 'lifetime'.
package api

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"time"

	"biz-platform/collector/internal/billing"
)

// errorQuotaExceeded — 사용량/보유량 한도 초과 응답의 machine-readable 코드(403). 프론트는
// 사용 가능한 기능은 그대로 보여주되 이 코드를 받으면 "이번 달 이용량을 모두 사용했습니다"
// + 유료 플랜 안내로 처리한다(페이지 이동마다 팝업 금지).
const errorQuotaExceeded = "quota_exceeded"

// usagePeriodLifetime — Free 제안서 체험처럼 기간이 없는 사용량의 period_key.
const usagePeriodLifetime = "lifetime"

// kstZone — 월 경계는 한국 시각 기준. distroless 런타임에 tzdata가 없어 고정 오프셋을 쓴다.
var kstZone = time.FixedZone("KST", 9*60*60)

// usagePeriodMonth — 소비형 사용량의 월 기간키('YYYY-MM', KST).
func usagePeriodMonth(t time.Time) string {
	return t.In(kstZone).Format("2006-01")
}

// usageDecision — consumeFeatureUsage 결과.
type usageDecision struct {
	Allowed        bool // 이 요청을 진행해도 되는가(이미 센 대상이거나 한도 안에서 새로 셈)
	NewlyCounted   bool // 이번 호출로 새 행이 생겼는가(실패 시 release 대상)
	AlreadyCounted bool // 같은 대상이 이미 있었는가(dedup)
	Used           int  // 결정 후 사용량(같은 대상 재요청이면 변동 없음)
	Limit          int  // 적용 한도(-1 무제한)
}

// consumeFeatureUsage — (회사, 기능, 기간, 대상)을 원자적으로 소비한다. limit<0이면 무제한이지만
// 기록은 남긴다(사용량 표시·감사용). limit==0이면 항상 거부(기록 없음).
// 이미 열린 tx가 있으면 그 안에서 실행해 호출부의 다른 INSERT와 함께 커밋/롤백되게 한다.
func (s *Server) consumeFeatureUsage(ctx context.Context, tx *sql.Tx, profileID string, feature billing.UsageFeature, periodKey, subjectKey string, limit int) (usageDecision, error) {
	d := usageDecision{Limit: limit}
	if profileID == "" || subjectKey == "" {
		return d, fmt.Errorf("consumeFeatureUsage: profileID/subjectKey required")
	}
	if limit == 0 {
		used, err := s.countFeatureUsage(ctx, profileID, feature, periodKey)
		d.Used = used
		return d, err
	}
	own := false
	if tx == nil {
		var err error
		tx, err = s.db.BeginTx(ctx, nil)
		if err != nil {
			return d, err
		}
		own = true
		defer tx.Rollback()
	}
	// 같은 (회사, 기능, 기간)의 동시 요청을 직렬화. 트랜잭션 종료 시 자동 해제.
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`,
		profileID+"|"+string(feature)+"|"+periodKey); err != nil {
		return d, err
	}
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM feature_usage WHERE company_profile_id = $1 AND feature_key = $2 AND period_key = $3 AND subject_key = $4)`,
		profileID, string(feature), periodKey, subjectKey).Scan(&exists); err != nil {
		return d, err
	}
	var used int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(quantity),0) FROM feature_usage WHERE company_profile_id = $1 AND feature_key = $2 AND period_key = $3`,
		profileID, string(feature), periodKey).Scan(&used); err != nil {
		return d, err
	}
	d.Used = used
	if exists {
		d.Allowed, d.AlreadyCounted = true, true
	} else if limit >= 0 && used >= limit {
		d.Allowed = false
	} else {
		if _, err := tx.ExecContext(ctx, `INSERT INTO feature_usage (company_profile_id, feature_key, period_key, subject_key, quantity) VALUES ($1,$2,$3,$4,1)`,
			profileID, string(feature), periodKey, subjectKey); err != nil {
			return d, err
		}
		d.Allowed, d.NewlyCounted = true, true
		d.Used = used + 1
	}
	if own {
		if err := tx.Commit(); err != nil {
			return d, err
		}
	}
	return d, nil
}

// releaseFeatureUsage — 예약(consume) 뒤 실제 기능이 실패했을 때 되돌린다(실패 미차감 원칙).
// NewlyCounted였던 경우에만 부른다 — 이미 있던 대상(dedup)을 지우면 안 된다.
func (s *Server) releaseFeatureUsage(ctx context.Context, profileID string, feature billing.UsageFeature, periodKey, subjectKey string) {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM feature_usage WHERE company_profile_id = $1 AND feature_key = $2 AND period_key = $3 AND subject_key = $4`,
		profileID, string(feature), periodKey, subjectKey); err != nil {
		s.logger.Error("feature usage release failed", "feature", string(feature), "profileId", profileID, "error", err)
	}
}

// countFeatureUsage — 표시용 현재 사용량(잠금 없음).
func (s *Server) countFeatureUsage(ctx context.Context, profileID string, feature billing.UsageFeature, periodKey string) (int, error) {
	var used int
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(quantity),0) FROM feature_usage WHERE company_profile_id = $1 AND feature_key = $2 AND period_key = $3`,
		profileID, string(feature), periodKey).Scan(&used)
	return used, err
}

// hasFeatureUsageSubject — 이 대상이 이미 소비됐는가(잠금 없음, 표시/조회 판단용).
func (s *Server) hasFeatureUsageSubject(ctx context.Context, profileID string, feature billing.UsageFeature, periodKey, subjectKey string) (bool, error) {
	var exists bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM feature_usage WHERE company_profile_id = $1 AND feature_key = $2 AND period_key = $3 AND subject_key = $4)`,
		profileID, string(feature), periodKey, subjectKey).Scan(&exists)
	return exists, err
}

// proposalDraftUsagePeriod — 제안서 사용량의 기간키/한도. Free는 평생 체험 1회(lifetime), 유료는
// 월 한도. Business는 -1(무제한, 이번 출시 판매 대상 아님).
func proposalDraftUsagePeriod(plan billing.Plan, info billing.PlanInfo, now time.Time) (periodKey string, limit int) {
	if plan == billing.PlanFree {
		return usagePeriodLifetime, billing.FreeProposalTrialLifetime
	}
	return usagePeriodMonth(now), info.MonthlyLimit(billing.UsageProposalDraft)
}

// writeQuotaExceeded — 403 + {error: quota_exceeded, feature, used, limit}. 기존 quota 거부
// (ai_analysis_quota_exceeded 등)와 같은 403 계열.
func writeQuotaExceeded(w http.ResponseWriter, feature string, used, limit int) {
	writeJSON(w, http.StatusForbidden, map[string]any{
		"error":   errorQuotaExceeded,
		"feature": feature,
		"used":    used,
		"limit":   limit,
	})
}

// ---------- Capacity: saved_searches ----------

// countSavedSearchesForProfile — 회사 소속 사용자 전체의 맞춤공고 보유 수(활성/비활성 무관 —
// 비활성으로 돌려 자리를 비우는 우회를 막는다). 삭제하면 줄어든다.
func (s *Server) countSavedSearchesForProfile(ctx context.Context, profileID string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `
		SELECT count(*) FROM saved_searches
		WHERE user_id IN (SELECT user_id FROM company_members WHERE company_profile_id = $1)`, profileID).Scan(&n)
	return n, err
}

// checkSavedSearchCapacity — 새 맞춤공고를 만들 자리가 있는가. (ok, used, limit)
func (s *Server) checkSavedSearchCapacity(ctx context.Context, profileID string) (bool, int, int, error) {
	plan, err := s.effectivePlan(ctx, profileID)
	if err != nil {
		return false, 0, 0, err
	}
	limit := s.effectivePlanInfo(ctx, plan).MaxSavedSearches
	used, err := s.countSavedSearchesForProfile(ctx, profileID)
	if err != nil {
		return false, 0, limit, err
	}
	if limit < 0 {
		return true, used, limit, nil
	}
	return used < limit, used, limit, nil
}

// ---------- 표시용 요약(/api/me) ----------

type usageEntry struct {
	Used   int    `json:"used"`
	Limit  int    `json:"limit"`  // -1 무제한
	Period string `json:"period"` // 'YYYY-MM' 또는 'lifetime'
}

// usageSummaryFor — /api/me의 usage(소비형)·capacities(보유형) 맵. 프론트는 "2 / 3" 같은 사용량
// 표시와 소진 시 안내에만 쓰고, 실제 강제는 서버 게이트가 한다.
func (s *Server) usageSummaryFor(ctx context.Context, profileID string, plan billing.Plan) (map[string]usageEntry, map[string]usageEntry) {
	usage := map[string]usageEntry{}
	caps := map[string]usageEntry{}
	if profileID == "" {
		return usage, caps
	}
	info := s.effectivePlanInfo(ctx, plan)
	now := time.Now()
	month := usagePeriodMonth(now)
	for _, f := range billing.AllUsageFeatures {
		period, limit := month, info.MonthlyLimit(f)
		if f == billing.UsageProposalDraft {
			period, limit = proposalDraftUsagePeriod(plan, info, now)
		}
		used, err := s.countFeatureUsage(ctx, profileID, f, period)
		if err != nil {
			s.logger.Warn("usage summary: count failed", "feature", string(f), "error", err)
		}
		usage[string(f)] = usageEntry{Used: used, Limit: limit, Period: period}
	}
	if used, err := s.countSavedSearchesForProfile(ctx, profileID); err == nil {
		caps["saved_search"] = usageEntry{Used: used, Limit: info.MaxSavedSearches, Period: "current"}
	}
	return usage, caps
}
