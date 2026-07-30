// checklist.go — 제출서류 체크리스트(사용자가 준비 여부를 표시). "AI 비서"
// 1단계: 공고 상세 화면에서 서류별 준비 상태를 체크하면 진행률 바에 반영된다.
package api

import (
	"database/sql"
	"encoding/json"
	"net/http"
)

type checklistToggleRequest struct {
	Checked bool `json:"checked"`
}

func (s *Server) handleToggleChecklistItem(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.currentUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	noticeID := r.PathValue("noticeId")
	documentID := r.PathValue("documentId")

	var req checklistToggleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_body"})
		return
	}

	ctx := r.Context()
	var profileID string
	err := s.db.QueryRowContext(ctx, `SELECT id FROM company_profiles WHERE user_id = $1`, userID).Scan(&profileID)
	if err == sql.ErrNoRows {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "company_profile_required"})
		return
	}
	if err != nil {
		s.logger.Error("checklist: profile lookup failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}

	// documentID가 실제로 이 공고의 현재 버전 소속인지 확인 — 다른 공고의
	// required_document_id를 임의로 넘기는 것을 막는다.
	var belongs bool
	err = s.db.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM required_documents rd
			JOIN notice_versions nv ON nv.id = rd.notice_version_id
			JOIN notices n ON n.id = nv.notice_id AND nv.version_number = n.current_version
			WHERE rd.id = $1 AND n.id = $2
		)`, documentID, noticeID,
	).Scan(&belongs)
	if err != nil {
		s.logger.Error("checklist: ownership check failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if !belongs {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "document_not_found"})
		return
	}

	_, err = s.db.ExecContext(ctx, `
		INSERT INTO document_checklist_items (company_profile_id, required_document_id, is_checked, checked_at)
		VALUES ($1, $2, $3, now())
		ON CONFLICT (company_profile_id, required_document_id)
		DO UPDATE SET is_checked = $3, checked_at = now()`,
		profileID, documentID, req.Checked)
	if err != nil {
		s.logger.Error("checklist: upsert failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"documentId": documentID, "checked": req.Checked})
}
