// 업무자동화: 참여 파이프라인 — 발견(추천)→참여결정→담당자→서류→일정을
// 잇는 흐름. "참여 검토" 원클릭 API가 엔트리와 체크리스트를 함께
// 생성하고, 체크리스트 초기 상태는 company_licenses/certifications.name과
// required_documents.document_name의 정확 일치(유사도 스코어링 없음)로만
// 판정한다 — 애매하면 전부 "확인필요"로 남겨 사용자가 직접 확정한다.
package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

var validPipelineStatuses = map[string]bool{
	"검토전": true, "참여검토": true, "승인대기": true, "준비중": true,
	"제출완료": true, "낙찰": true, "탈락": true, "보류": true, "제외": true,
}

var validChecklistStatuses = map[string]bool{
	"보유": true, "갱신필요": true, "신규작성": true, "발급필요": true, "확인필요": true,
}

type pipelineChecklistItem struct {
	ID                 string  `json:"id"`
	DocumentName       string  `json:"documentName"`
	Status             string  `json:"status"`
	RequiredDocumentID *string `json:"requiredDocumentId"`
}

type pipelineEntry struct {
	ID                      string     `json:"id"`
	NoticeID                string     `json:"noticeId"`
	NoticeTitle             string     `json:"noticeTitle"`
	OrganizationName        *string    `json:"organizationName"`
	Status                  string     `json:"status"`
	AssigneeName            *string    `json:"assigneeName"`
	AssigneeEmail           *string    `json:"assigneeEmail"`
	AssigneePhone           *string    `json:"assigneePhone"`
	DecidedAt               *time.Time `json:"decidedAt"`
	SubmissionDeadline      *string    `json:"submissionDeadline"`
	Memo                    *string    `json:"memo"`
	CreatedAt               time.Time  `json:"createdAt"`
	UpdatedAt               time.Time  `json:"updatedAt"`
	IncompleteDocumentCount int        `json:"incompleteDocumentCount"` // handleListPipeline만 채움(대시보드 카드 클릭 → 서류확인필요 필터용) — handleGetPipelineEntry는 체크리스트 원본을 따로 내려주므로 항상 0
}

// pipelineEntryRowScanner is satisfied by both *sql.Row (QueryRowContext)
// and *sql.Rows (QueryContext, per row) — lets fetchPipelineEntry and
// handleListPipeline share one scan routine instead of duplicating it.
type pipelineEntryRowScanner interface {
	Scan(dest ...any) error
}

func scanPipelineEntry(row pipelineEntryRowScanner) (*pipelineEntry, error) {
	var e pipelineEntry
	var org, assignee, assigneeEmail, assigneePhone, memo sql.NullString
	var decidedAt, deadline sql.NullTime
	err := row.Scan(&e.ID, &e.NoticeID, &e.NoticeTitle, &org, &e.Status, &assignee, &assigneeEmail, &assigneePhone,
		&decidedAt, &deadline, &memo, &e.CreatedAt, &e.UpdatedAt)
	if err != nil {
		return nil, err
	}
	e.OrganizationName = nullStringPtr(org)
	e.AssigneeName = nullStringPtr(assignee)
	e.AssigneeEmail = nullStringPtr(assigneeEmail)
	e.AssigneePhone = nullStringPtr(assigneePhone)
	e.Memo = nullStringPtr(memo)
	if decidedAt.Valid {
		e.DecidedAt = &decidedAt.Time
	}
	if deadline.Valid {
		v := deadline.Time.Format("2006-01-02")
		e.SubmissionDeadline = &v
	}
	return &e, nil
}

const pipelineEntrySelect = `
	SELECT pe.id, pe.notice_id, n.title, n.organization_name, pe.status, pe.assignee_name, pe.assignee_email, pe.assignee_phone,
	       pe.decided_at, pe.submission_deadline, pe.memo, pe.created_at, pe.updated_at
	FROM notice_pipeline_entries pe
	JOIN notices n ON n.id = pe.notice_id`

func (s *Server) fetchPipelineEntry(ctx context.Context, entryID string) (*pipelineEntry, error) {
	row := s.db.QueryRowContext(ctx, pipelineEntrySelect+" WHERE pe.id = $1", entryID)
	return scanPipelineEntry(row)
}

// pipelineEntryOwnerProfileID looks up which company_profile_id owns a
// pipeline entry — shared by PATCH/GET handlers to check the caller's
// profile actually owns the entry before returning/mutating it (sql.ErrNoRows
// covers both "doesn't exist" and "exists but isn't visible to a 404 caller").
func (s *Server) pipelineEntryOwnerProfileID(ctx context.Context, entryID string) (string, error) {
	var profileID string
	err := s.db.QueryRowContext(ctx,
		`SELECT company_profile_id FROM notice_pipeline_entries WHERE id = $1`, entryID,
	).Scan(&profileID)
	return profileID, err
}

func (s *Server) handleCreatePipelineEntry(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.currentUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	noticeID := r.PathValue("id")
	ctx := r.Context()

	profile, err := s.getCompanyProfile(r, userID)
	if err != nil {
		s.logger.Error("create-pipeline: profile lookup failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if profile == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "company_profile_required"})
		return
	}

	// 멱등성: 이미 있으면 그대로 반환(재클릭해도 체크리스트 재생성 안 함).
	var existingID string
	err = s.db.QueryRowContext(ctx,
		`SELECT id FROM notice_pipeline_entries WHERE company_profile_id = $1 AND notice_id = $2`,
		profile.ID, noticeID,
	).Scan(&existingID)
	if err == nil {
		entry, err := s.fetchPipelineEntry(ctx, existingID)
		if err != nil {
			s.logger.Error("create-pipeline: fetch existing entry failed", "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
			return
		}
		writeJSON(w, http.StatusOK, entry)
		return
	}
	if err != sql.ErrNoRows {
		s.logger.Error("create-pipeline: existing entry lookup failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}

	if quotaOK, limit, err := s.checkPipelineEntryQuota(ctx, profile.ID); err != nil {
		s.logger.Error("create-pipeline: quota check failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	} else if !quotaOK {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "pipeline_quota_exceeded", "limit": limit})
		return
	}

	var currentVersion int
	var deadline sql.NullTime
	err = s.db.QueryRowContext(ctx,
		`SELECT current_version, application_end_at FROM notices WHERE id = $1`, noticeID,
	).Scan(&currentVersion, &deadline)
	if err == sql.ErrNoRows {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "notice_not_found"})
		return
	}
	if err != nil {
		s.logger.Error("create-pipeline: notice lookup failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}

	// 기본 담당자가 등록돼 있으면 assignee_*를 자동으로 채운다 — 강제가
	// 아니라 출발점일 뿐, 사용자가 상세화면에서 바로 수정할 수 있다.
	// 등록된 담당자가 하나도 없으면 그냥 NULL로 남고, 프론트가 "담당자
	// 관리에서 미리 등록해두면 편리합니다" 안내를 보여준다.
	defaultContact, err := s.fetchDefaultContact(ctx, profile.ID)
	if err != nil {
		s.logger.Error("create-pipeline: default contact lookup failed", "error", err)
	}
	var assigneeName, assigneeEmail, assigneePhone *string
	if defaultContact != nil {
		assigneeName, assigneeEmail, assigneePhone = &defaultContact.Name, defaultContact.Email, defaultContact.Phone
	}

	var entryID string
	err = s.db.QueryRowContext(ctx, `
		INSERT INTO notice_pipeline_entries (company_profile_id, notice_id, status, submission_deadline, assignee_name, assignee_email, assignee_phone)
		VALUES ($1, $2, '검토전', $3, $4, $5, $6) RETURNING id`,
		profile.ID, noticeID, deadline, assigneeName, assigneeEmail, assigneePhone,
	).Scan(&entryID)
	if err != nil {
		s.logger.Error("create-pipeline: insert failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}

	versionID, err := s.currentVersionID(ctx, noticeID, currentVersion)
	if err != nil {
		s.logger.Error("create-pipeline: current version lookup failed", "error", err)
	} else if err := s.generateChecklistItems(ctx, entryID, versionID, profile.ID); err != nil {
		s.logger.Error("create-pipeline: checklist generation failed", "error", err)
	}

	entry, err := s.fetchPipelineEntry(ctx, entryID)
	if err != nil {
		s.logger.Error("create-pipeline: fetch new entry failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	writeJSON(w, http.StatusCreated, entry)
}

// generateChecklistItems creates one pipeline_checklist_items row per
// required_documents row for this notice version, with an auto-matched
// initial status (see matchChecklistStatus).
func (s *Server) generateChecklistItems(ctx context.Context, entryID, versionID, profileID string) error {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, document_name FROM required_documents
		WHERE notice_version_id = $1 AND review_status != 'rejected'`, versionID)
	if err != nil {
		return err
	}
	type reqDoc struct{ id, name string }
	var docs []reqDoc
	for rows.Next() {
		var d reqDoc
		if err := rows.Scan(&d.id, &d.name); err != nil {
			continue
		}
		docs = append(docs, d)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	for _, d := range docs {
		status := s.matchChecklistStatus(ctx, profileID, d.name)
		if _, err := s.db.ExecContext(ctx, `
			INSERT INTO pipeline_checklist_items (pipeline_entry_id, document_name, status, required_document_id)
			VALUES ($1, $2, $3, $4)`,
			entryID, d.name, status, d.id,
		); err != nil {
			return err
		}
	}
	return nil
}

// matchChecklistStatus looks for an exact-name match (TRIM'd, no fuzzy
// scoring) in company_licenses/certifications and maps its status/expiry
// into the checklist's 5-value status. No match, or an ambiguous source
// status, always falls back to 확인필요 — never guessed as 신규작성.
func (s *Server) matchChecklistStatus(ctx context.Context, profileID, documentName string) string {
	name := strings.TrimSpace(documentName)
	if name == "" {
		return "확인필요"
	}
	var licenseStatus string
	var expiresAt sql.NullTime
	err := s.db.QueryRowContext(ctx, `
		SELECT status, expires_at FROM (
			SELECT status, expires_at, created_at FROM company_licenses
			WHERE company_profile_id = $1 AND TRIM(name) = $2
			UNION ALL
			SELECT status, expires_at, created_at FROM company_certifications
			WHERE company_profile_id = $1 AND TRIM(name) = $2
		) matched ORDER BY created_at DESC LIMIT 1`,
		profileID, name,
	).Scan(&licenseStatus, &expiresAt)
	if err != nil {
		if err != sql.ErrNoRows {
			s.logger.Error("pipeline: checklist match query failed", "error", err)
		}
		return "확인필요"
	}

	switch licenseStatus {
	case "보유":
		if expiresAt.Valid && expiresAt.Time.Before(time.Now()) {
			return "갱신필요"
		}
		return "보유"
	case "미보유":
		return "발급필요"
	default: // 확인되지않음
		return "확인필요"
	}
}

func (s *Server) fetchChecklistItems(ctx context.Context, entryID string) ([]pipelineChecklistItem, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, document_name, status, required_document_id
		FROM pipeline_checklist_items WHERE pipeline_entry_id = $1 ORDER BY created_at`, entryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	items := []pipelineChecklistItem{}
	for rows.Next() {
		var it pipelineChecklistItem
		var reqDocID sql.NullString
		if err := rows.Scan(&it.ID, &it.DocumentName, &it.Status, &reqDocID); err != nil {
			continue
		}
		it.RequiredDocumentID = nullStringPtr(reqDocID)
		items = append(items, it)
	}
	return items, rows.Err()
}

// handleUpdatePipelineEntry does a partial update: only JSON keys actually
// present in the request body are applied (checked via a raw map first —
// a *string field can't tell "omitted" from "explicit null" on its own).
// status/assigneeName/memo can each be cleared by sending "" — this project
// has no separate "unset" sentinel, so empty string means "no value" here.
func (s *Server) handleUpdatePipelineEntry(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.currentUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	entryID := r.PathValue("id")
	ctx := r.Context()

	profile, err := s.getCompanyProfile(r, userID)
	if err != nil {
		s.logger.Error("update-pipeline: profile lookup failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if profile == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "company_profile_required"})
		return
	}

	var currentStatus, ownerProfileID string
	err = s.db.QueryRowContext(ctx,
		`SELECT company_profile_id, status FROM notice_pipeline_entries WHERE id = $1`, entryID,
	).Scan(&ownerProfileID, &currentStatus)
	if err == sql.ErrNoRows {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "pipeline_entry_not_found"})
		return
	}
	if err != nil {
		s.logger.Error("update-pipeline: entry lookup failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if ownerProfileID != profile.ID {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "pipeline_entry_not_found"})
		return
	}

	var raw map[string]json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_body"})
		return
	}

	sets := []string{}
	args := []any{}
	addSet := func(column string, value any) {
		args = append(args, value)
		sets = append(sets, fmt.Sprintf("%s = $%d", column, len(args)))
	}

	statusChanged := false
	if rawStatus, present := raw["status"]; present {
		var status string
		if err := json.Unmarshal(rawStatus, &status); err != nil || !validPipelineStatuses[status] {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_status"})
			return
		}
		addSet("status", status)
		if status != currentStatus {
			addSet("decided_at", time.Now())
			statusChanged = true
		}
	}
	if rawAssignee, present := raw["assigneeName"]; present {
		var name string
		json.Unmarshal(rawAssignee, &name)
		if strings.TrimSpace(name) == "" {
			addSet("assignee_name", nil)
		} else {
			addSet("assignee_name", name)
		}
	}
	if rawAssigneeEmail, present := raw["assigneeEmail"]; present {
		var email string
		json.Unmarshal(rawAssigneeEmail, &email)
		if strings.TrimSpace(email) == "" {
			addSet("assignee_email", nil)
		} else {
			addSet("assignee_email", strings.TrimSpace(email))
		}
	}
	if rawAssigneePhone, present := raw["assigneePhone"]; present {
		var phone string
		json.Unmarshal(rawAssigneePhone, &phone)
		if strings.TrimSpace(phone) == "" {
			addSet("assignee_phone", nil)
		} else {
			addSet("assignee_phone", strings.TrimSpace(phone))
		}
	}
	if rawMemo, present := raw["memo"]; present {
		var memo string
		json.Unmarshal(rawMemo, &memo)
		if memo == "" {
			addSet("memo", nil)
		} else {
			addSet("memo", memo)
		}
	}
	if len(sets) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no_fields_to_update"})
		return
	}
	addSet("updated_at", time.Now())

	args = append(args, entryID)
	query := "UPDATE notice_pipeline_entries SET " + strings.Join(sets, ", ") + fmt.Sprintf(" WHERE id = $%d", len(args))
	if _, err := s.db.ExecContext(ctx, query, args...); err != nil {
		s.logger.Error("update-pipeline: update failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}

	entry, err := s.fetchPipelineEntry(ctx, entryID)
	if err != nil {
		s.logger.Error("update-pipeline: fetch updated entry failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}

	hasAssigneeEmail := entry.AssigneeEmail != nil && *entry.AssigneeEmail != ""
	hasAssigneePhone := entry.AssigneePhone != nil && *entry.AssigneePhone != ""
	if statusChanged && (hasAssigneeEmail || hasAssigneePhone) {
		var email, phone string
		if hasAssigneeEmail {
			email = *entry.AssigneeEmail
		}
		if hasAssigneePhone {
			phone = *entry.AssigneePhone
		}
		title, noticeID, status := entry.NoticeTitle, entry.NoticeID, entry.Status
		go s.notifyAssigneeStatusChange(context.Background(), email, phone, entryID, noticeID, title, status)
	}

	writeJSON(w, http.StatusOK, entry)
}

func (s *Server) handleUpdateChecklistItem(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.currentUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	entryID := r.PathValue("id")
	itemID := r.PathValue("itemId")
	ctx := r.Context()

	profile, err := s.getCompanyProfile(r, userID)
	if err != nil {
		s.logger.Error("update-checklist-item: profile lookup failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if profile == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "company_profile_required"})
		return
	}

	ownerProfileID, err := s.pipelineEntryOwnerProfileID(ctx, entryID)
	if err == sql.ErrNoRows {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "pipeline_entry_not_found"})
		return
	}
	if err != nil {
		s.logger.Error("update-checklist-item: entry lookup failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if ownerProfileID != profile.ID {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "pipeline_entry_not_found"})
		return
	}

	var req struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_body"})
		return
	}
	if !validChecklistStatuses[req.Status] {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_status"})
		return
	}

	res, err := s.db.ExecContext(ctx,
		`UPDATE pipeline_checklist_items SET status = $1 WHERE id = $2 AND pipeline_entry_id = $3`,
		req.Status, itemID, entryID,
	)
	if err != nil {
		s.logger.Error("update-checklist-item: update failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "checklist_item_not_found"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"id": itemID, "status": req.Status})
}

func (s *Server) handleListPipeline(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.currentUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	profile, err := s.getCompanyProfile(r, userID)
	if err != nil {
		s.logger.Error("list-pipeline: profile lookup failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if profile == nil {
		writeJSON(w, http.StatusOK, map[string]any{"items": []pipelineEntry{}})
		return
	}

	rows, err := s.db.QueryContext(r.Context(),
		pipelineEntrySelect+" WHERE pe.company_profile_id = $1 ORDER BY pe.submission_deadline ASC NULLS LAST",
		profile.ID,
	)
	if err != nil {
		s.logger.Error("list-pipeline: query failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	defer rows.Close()

	incompleteDocCounts, err := s.fetchIncompleteChecklistCounts(r.Context(), profile.ID)
	if err != nil {
		s.logger.Error("list-pipeline: checklist counts query failed", "error", err)
	}

	items := []pipelineEntry{}
	for rows.Next() {
		entry, err := scanPipelineEntry(rows)
		if err != nil {
			s.logger.Error("list-pipeline: scan failed", "error", err)
			continue
		}
		entry.IncompleteDocumentCount = incompleteDocCounts[entry.ID]
		items = append(items, *entry)
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) handleGetPipelineEntry(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.currentUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	entryID := r.PathValue("id")
	ctx := r.Context()

	profile, err := s.getCompanyProfile(r, userID)
	if err != nil {
		s.logger.Error("get-pipeline: profile lookup failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if profile == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "company_profile_required"})
		return
	}

	ownerProfileID, err := s.pipelineEntryOwnerProfileID(ctx, entryID)
	if err == sql.ErrNoRows {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "pipeline_entry_not_found"})
		return
	}
	if err != nil {
		s.logger.Error("get-pipeline: entry lookup failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if ownerProfileID != profile.ID {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "pipeline_entry_not_found"})
		return
	}

	entry, err := s.fetchPipelineEntry(ctx, entryID)
	if err != nil {
		s.logger.Error("get-pipeline: fetch entry failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	checklist, err := s.fetchChecklistItems(ctx, entryID)
	if err != nil {
		s.logger.Error("get-pipeline: fetch checklist failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"entry": entry, "checklist": checklist})
}
