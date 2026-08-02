// billing_ai_usage.go — 대시보드 "AI 분석 N/M건" 카드 + 클릭 시 이번달
// 실제 사용 내역(어떤 서류를 언제 분석했는지) 화면. company_documents가
// 곧 사용 로그다(별도 로그 테이블 없음) — 단, "사용량"으로 카운트되는 건
// 그중 extraction_status='success'인 행뿐이다(countAIAnalysisThisMonth,
// billing.go — 2026-08-03 정책: 실패는 한도를 안 깎음). 실패/처리중 건도
// items 목록에는 계속 나오지만 usedCount에는 안 잡힌다. owner/member 둘 다
// 조회 가능 — 읽기 전용 정보라 handleGetSubscription과 같은 접근 범위.
package api

import (
	"database/sql"
	"net/http"
	"time"
)

type aiUsageItem struct {
	ID               string    `json:"id"`
	DocumentKind     string    `json:"documentKind"`
	DocumentKindName string    `json:"documentKindName"`
	OriginalFilename string    `json:"originalFilename"`
	UploadedAt       time.Time `json:"uploadedAt"`
	// ExtractionStatus — "success"/"failed"/"processing"(DB의 NULL을 이
	// 값으로 바꿔서 내려준다, extraction_status 컬럼 자체엔 'processing'
	// 값이 없음). CanRetry는 프론트가 "재시도" 버튼을 보여줄지 판단하는
	// 값 — 실패했고 & document_kind를 알아야(재시도 시 어느 추출 함수를
	// 부를지 결정) 재시도가 가능하다.
	ExtractionStatus string  `json:"extractionStatus"`
	FailureReason    *string `json:"failureReason"`
	CanRetry         bool    `json:"canRetry"`
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
	limit, err := s.effectiveAIAnalysisLimit(ctx, profile.ID, plan)
	if err != nil {
		s.logger.Error("get-ai-usage: limit lookup failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT id, original_filename, document_kind, uploaded_at, extraction_status, failure_reason
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
		var id, filename string
		var kind, status, failureReason sql.NullString
		var uploadedAt time.Time
		if err := rows.Scan(&id, &filename, &kind, &uploadedAt, &status, &failureReason); err != nil {
			continue
		}
		it := aiUsageItem{
			ID:               id,
			OriginalFilename: filename,
			UploadedAt:       uploadedAt,
			DocumentKindName: "미상",
			ExtractionStatus: "processing",
		}
		if kind.Valid {
			it.DocumentKind = kind.String
			if name, ok := documentKindLabels[kind.String]; ok {
				it.DocumentKindName = name
			}
		}
		if status.Valid {
			it.ExtractionStatus = status.String
		}
		if failureReason.Valid {
			it.FailureReason = &failureReason.String
		}
		it.CanRetry = it.ExtractionStatus == extractionStatusFailed && it.DocumentKind != ""
		items = append(items, it)
	}

	// usedCount는 반드시 성공한 것만(countAIAnalysisThisMonth, billing.go —
	// 2026-08-03 정책: 실패는 한도를 안 깎음) — len(items)를 쓰면 안 된다.
	// items 목록 자체는 이력 확인용이라 실패/처리중 건도 계속 그대로 보여준다.
	usedCount, err := s.countAIAnalysisThisMonth(ctx, profile.ID)
	if err != nil {
		s.logger.Error("get-ai-usage: used count query failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"usedCount": usedCount,
		"limit":     limit, // -1 = 무제한, 0 = 이 플랜에서 이용 불가(Free)
		"items":     items,
	})
}
