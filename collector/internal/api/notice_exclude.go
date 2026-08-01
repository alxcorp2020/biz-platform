// notice_exclude.go — 원클릭 참여검토(Phase 1) 상단 버튼 중 "제외". 전체
// 검토 흐름(체크리스트/일정)을 거칠 필요 없이, 이 공고는 아예 참여하지
// 않겠다는 결정을 그 자리에서 바로 기록한다. handleCreatePipelineEntry와
// 마찬가지로 UNIQUE(company_profile_id, notice_id)로 멱등성을 보장하되,
// 이미 파이프라인에 있는 건(어떤 상태든)을 이 버튼이 조용히 덮어쓰지는
// 않는다 — 이미 검토가 진행 중인 건을 "상세페이지 상단 버튼 한 번"으로
// 날려버리면 위험하므로, 그 경우엔 기존 엔트리를 그대로 돌려주고
// 프론트가 "이미 파이프라인에 있습니다"로 안내한다. 되돌리기(원복)는
// 별도 엔드포인트 없이 기존 PATCH /api/pipeline/{id}(status 변경)를
// 그대로 재사용한다.
package api

import (
	"database/sql"
	"net/http"
)

type excludeNoticeResponse struct {
	ID            string `json:"id"`
	Status        string `json:"status"`
	AlreadyExists bool   `json:"alreadyExists"`
}

func (s *Server) handleExcludeNotice(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.currentUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	noticeID := r.PathValue("id")
	ctx := r.Context()

	profile, err := s.getCompanyProfile(r, userID)
	if err != nil {
		s.logger.Error("exclude-notice: profile lookup failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if profile == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "company_profile_required"})
		return
	}

	var existingID, existingStatus string
	err = s.db.QueryRowContext(ctx,
		`SELECT id, status FROM notice_pipeline_entries WHERE company_profile_id = $1 AND notice_id = $2`,
		profile.ID, noticeID,
	).Scan(&existingID, &existingStatus)
	if err == nil {
		writeJSON(w, http.StatusOK, excludeNoticeResponse{ID: existingID, Status: existingStatus, AlreadyExists: true})
		return
	}
	if err != sql.ErrNoRows {
		s.logger.Error("exclude-notice: existing entry lookup failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}

	var deadline sql.NullTime
	err = s.db.QueryRowContext(ctx, `SELECT application_end_at FROM notices WHERE id = $1`, noticeID).Scan(&deadline)
	if err == sql.ErrNoRows {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "notice_not_found"})
		return
	}
	if err != nil {
		s.logger.Error("exclude-notice: notice lookup failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}

	// 제외 건은 checkPipelineEntryQuota가 이미 status != '제외'로 카운트하므로
	// (billing.go) 플랜 한도를 소모하지 않는다 — 별도 쿼터 확인 불필요.
	var entryID string
	err = s.db.QueryRowContext(ctx, `
		INSERT INTO notice_pipeline_entries (company_profile_id, notice_id, status, decided_at, submission_deadline, company_profile_snapshot)
		VALUES ($1, $2, '제외', now(), $3, (SELECT to_jsonb(cp) FROM company_profiles cp WHERE cp.id = $1)) RETURNING id`,
		profile.ID, noticeID, deadline,
	).Scan(&entryID)
	if err != nil {
		s.logger.Error("exclude-notice: insert failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}

	s.recordAuditLog(ctx, userID, "notice_excluded", "notice_pipeline_entry", entryID, map[string]any{"noticeId": noticeID})

	writeJSON(w, http.StatusCreated, excludeNoticeResponse{ID: entryID, Status: "제외", AlreadyExists: false})
}
