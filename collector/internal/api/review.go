// 규칙 기반 구조화 추출 결과 중 review_status='review_required'로 표시된
// 항목(표 근처라 추출이 불완전할 수 있는 자격조건/제출서류)을 사람이
// 승인(confirmed)/반려(rejected)하는 내부 검토 워크플로우.
//
// users.role이 analyst/operator/system_admin인 계정만 접근 가능하다 —
// 일반 회원가입 사용자(role='user')나 발주기관 담당자(company_admin)는
// 데이터 신뢰성 검토 권한이 없다.
package api

import (
	"context"
	"encoding/json"
	"net/http"
)

var reviewerRoles = map[string]bool{
	"analyst":      true,
	"operator":     true,
	"system_admin": true,
}

// requireReviewer checks both authentication and role, writing the
// appropriate error response itself so callers can just early-return on !ok.
func (s *Server) requireReviewer(w http.ResponseWriter, r *http.Request) (userID string, ok bool) {
	userID, authed := s.currentUserID(r)
	if !authed {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return "", false
	}
	role, err := s.userRole(r.Context(), userID)
	if err != nil {
		s.logger.Error("review: role lookup failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return "", false
	}
	if !reviewerRoles[role] {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
		return "", false
	}
	return userID, true
}

func (s *Server) userRole(ctx context.Context, userID string) (string, error) {
	var role string
	err := s.db.QueryRowContext(ctx, `SELECT role FROM users WHERE id = $1`, userID).Scan(&role)
	return role, err
}

type reviewQueueItem struct {
	Type          string  `json:"type"` // "eligibility_condition" | "required_document"
	ID            string  `json:"id"`
	NoticeID      string  `json:"noticeId"`
	NoticeTitle   string  `json:"noticeTitle"`
	Category      string  `json:"category,omitempty"`
	ConditionName string  `json:"conditionName,omitempty"`
	DocumentName  string  `json:"documentName,omitempty"`
	SourceText    string  `json:"sourceText"`
	Confidence    float64 `json:"confidence"`
}

// reviewQueueLimit caps each table's contribution to the queue. 1차 버전이라
// 페이지네이션은 없다 — 실사용 중 큐가 이 한도를 넘어서면 그때 추가한다.
const reviewQueueLimit = 500

func (s *Server) handleReviewQueue(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireReviewer(w, r); !ok {
		return
	}
	ctx := r.Context()
	items := []reviewQueueItem{}

	eligRows, err := s.db.QueryContext(ctx, `
		SELECT ec.id, n.id, n.title, ec.category, ec.condition_name, ec.source_text, ec.confidence
		FROM eligibility_conditions ec
		JOIN notice_versions nv ON nv.id = ec.notice_version_id
		JOIN notices n ON n.id = nv.notice_id
		WHERE ec.review_status = 'review_required' AND ec.source_attachment_id IS NOT NULL
		ORDER BY ec.created_at
		LIMIT `+itoa(reviewQueueLimit))
	if err != nil {
		s.logger.Error("review queue: eligibility query failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	for eligRows.Next() {
		var it reviewQueueItem
		if err := eligRows.Scan(&it.ID, &it.NoticeID, &it.NoticeTitle, &it.Category, &it.ConditionName, &it.SourceText, &it.Confidence); err != nil {
			continue
		}
		it.Type = "eligibility_condition"
		items = append(items, it)
	}
	eligRows.Close()

	docRows, err := s.db.QueryContext(ctx, `
		SELECT rd.id, n.id, n.title, rd.document_name, COALESCE(rd.source_text, ''), rd.confidence
		FROM required_documents rd
		JOIN notice_versions nv ON nv.id = rd.notice_version_id
		JOIN notices n ON n.id = nv.notice_id
		WHERE rd.review_status = 'review_required'
		ORDER BY rd.id
		LIMIT `+itoa(reviewQueueLimit))
	if err != nil {
		s.logger.Error("review queue: required documents query failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	for docRows.Next() {
		var it reviewQueueItem
		if err := docRows.Scan(&it.ID, &it.NoticeID, &it.NoticeTitle, &it.DocumentName, &it.SourceText, &it.Confidence); err != nil {
			continue
		}
		it.Type = "required_document"
		items = append(items, it)
	}
	docRows.Close()

	writeJSON(w, http.StatusOK, map[string]any{"items": items, "count": len(items)})
}

type reviewActionRequest struct {
	Action string `json:"action"` // "confirm" | "reject"
}

func reviewStatusForAction(action string) (string, bool) {
	switch action {
	case "confirm":
		return "confirmed", true
	case "reject":
		return "rejected", true
	default:
		return "", false
	}
}

func (s *Server) handleReviewEligibilityCondition(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireReviewer(w, r); !ok {
		return
	}
	s.applyReviewAction(w, r, "eligibility_conditions")
}

func (s *Server) handleReviewRequiredDocument(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireReviewer(w, r); !ok {
		return
	}
	s.applyReviewAction(w, r, "required_documents")
}

func (s *Server) applyReviewAction(w http.ResponseWriter, r *http.Request, table string) {
	id := r.PathValue("id")

	var req reviewActionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_body"})
		return
	}
	status, ok := reviewStatusForAction(req.Action)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_action"})
		return
	}

	res, err := s.db.ExecContext(r.Context(),
		`UPDATE `+table+` SET review_status = $2 WHERE id = $1`, id, status)
	if err != nil {
		s.logger.Error("review: update failed", "table", table, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": id, "reviewStatus": status})
}
