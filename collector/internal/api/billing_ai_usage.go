// billing_ai_usage.go — 대시보드 "AI 분석 N/M건" 카드 + 클릭 시 이번달
// 실제 사용 내역(어떤 서류를 언제 분석했는지) 화면. company_documents가
// 곧 사용 로그다(checkAIAnalysisQuota의 근거와 동일 — 업로드 1건 = Claude
// 호출 1건, 별도 로그 테이블 없음). owner/member 둘 다 조회 가능 — 읽기
// 전용 정보라 handleGetSubscription과 같은 접근 범위.
package api

import (
	"net/http"
	"time"

	"biz-platform/collector/internal/billing"
)

type aiUsageItem struct {
	DocumentKind     string    `json:"documentKind"`
	DocumentKindName string    `json:"documentKindName"`
	OriginalFilename string    `json:"originalFilename"`
	UploadedAt       time.Time `json:"uploadedAt"`
}

// handleGetAIUsage returns this month's AI-extraction usage: the count/limit
// pair (같은 값을 dashboard.go의 요약 카드에도 노출) plus the row-level
// history the card links to.
func (s *Server) handleGetAIUsage(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.currentUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	profile, err := s.getCompanyProfile(r, userID)
	if err != nil {
		s.logger.Error("get-ai-usage: profile lookup failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if profile == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "company_profile_required"})
		return
	}
	ctx := r.Context()

	plan, err := s.effectivePlan(ctx, profile.ID)
	if err != nil {
		s.logger.Error("get-ai-usage: plan lookup failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	limit := billing.Plans[plan].MaxAIAnalysisPerMonth

	rows, err := s.db.QueryContext(ctx, `
		SELECT original_filename, document_kind, uploaded_at
		FROM company_documents
		WHERE company_profile_id = $1 AND uploaded_at >= date_trunc('month', now())
		ORDER BY uploaded_at DESC`, profile.ID)
	if err != nil {
		s.logger.Error("get-ai-usage: query failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	defer rows.Close()

	items := []aiUsageItem{}
	for rows.Next() {
		var filename string
		var kind *string
		var uploadedAt time.Time
		if err := rows.Scan(&filename, &kind, &uploadedAt); err != nil {
			continue
		}
		it := aiUsageItem{OriginalFilename: filename, UploadedAt: uploadedAt, DocumentKindName: "미상"}
		if kind != nil {
			it.DocumentKind = *kind
			if name, ok := documentKindLabels[*kind]; ok {
				it.DocumentKindName = name
			}
		}
		items = append(items, it)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"usedCount": len(items),
		"limit":     limit, // -1 = 무제한, 0 = 이 플랜에서 이용 불가(Free)
		"items":     items,
	})
}
