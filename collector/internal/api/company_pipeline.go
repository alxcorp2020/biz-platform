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

	"github.com/lib/pq"
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
	ID               string  `json:"id"`
	NoticeID         string  `json:"noticeId"`
	NoticeTitle      string  `json:"noticeTitle"`
	OrganizationName *string `json:"organizationName"`
	// NoticeType/Region/Industry/BudgetAmount/NoticeStatus — 2026-08-06,
	// 목록 화면을 공고검색 결과 카드와 같은 형식(발주기관·지역·업종·
	// 예산·공고상태)으로 보여주기 위해 추가. "Status"(파이프라인 자체
	// 진행상태: 검토전/참여검토/.../낙찰 등)와는 별개 — 헷갈리지 않게
	// 이쪽은 전부 Notice 접두어를 붙였다.
	NoticeType              string     `json:"noticeType"`
	Region                  *string    `json:"region"`
	Industry                *string    `json:"industry"`
	BudgetAmount            *int64     `json:"budgetAmount"`
	NoticeStatus            string     `json:"noticeStatus"`
	Status                  string     `json:"status"`
	AssigneeName            *string    `json:"assigneeName"`
	AssigneeEmail           *string    `json:"assigneeEmail"`
	AssigneePhone           *string    `json:"assigneePhone"`
	AssigneeUserID          *string    `json:"assigneeUserId"` // 회원계정으로 지정된 경우만(자유텍스트 담당자면 nil)
	DecidedAt               *time.Time `json:"decidedAt"`
	SubmissionDeadline      *string    `json:"submissionDeadline"`
	Memo                    *string    `json:"memo"`
	AwardedAmount           *int64     `json:"awardedAmount"` // 성장분석 ROI 근거 — status='낙찰'일 때 사용자가 직접 입력
	CreatedAt               time.Time  `json:"createdAt"`
	UpdatedAt               time.Time  `json:"updatedAt"`
	IncompleteDocumentCount int        `json:"incompleteDocumentCount"` // handleListPipeline만 채움(대시보드 카드 클릭 → 서류확인필요 필터용) — handleGetPipelineEntry는 체크리스트 원본을 따로 내려주므로 항상 0
	AIGrade                 string     `json:"aiGrade,omitempty"`       // handleListPipeline만 채움(Phase 3 칸반/표 뷰용). 영속 컬럼이 아니라 growth_analytics.go의 fetchGradeDistribution과 동일하게 요청 시점에 scoreNoticeForCompany로 계산한다.
}

// pipelineEntryRowScanner is satisfied by both *sql.Row (QueryRowContext)
// and *sql.Rows (QueryContext, per row) — lets fetchPipelineEntry and
// handleListPipeline share one scan routine instead of duplicating it.
type pipelineEntryRowScanner interface {
	Scan(dest ...any) error
}

func scanPipelineEntry(row pipelineEntryRowScanner) (*pipelineEntry, error) {
	var e pipelineEntry
	var org, assignee, assigneeEmail, assigneePhone, assigneeUserID, memo sql.NullString
	var region, industry sql.NullString
	var budgetAmount sql.NullInt64
	var decidedAt, deadline sql.NullTime
	var awardedAmount sql.NullInt64
	err := row.Scan(&e.ID, &e.NoticeID, &e.NoticeTitle, &org, &e.Status, &assignee, &assigneeEmail, &assigneePhone, &assigneeUserID,
		&decidedAt, &deadline, &memo, &awardedAmount, &e.CreatedAt, &e.UpdatedAt,
		&e.NoticeType, &region, &industry, &budgetAmount, &e.NoticeStatus)
	if err != nil {
		return nil, err
	}
	e.OrganizationName = nullStringPtr(org)
	e.AssigneeName = nullStringPtr(assignee)
	e.AssigneeEmail = nullStringPtr(assigneeEmail)
	e.AssigneePhone = nullStringPtr(assigneePhone)
	e.AssigneeUserID = nullStringPtr(assigneeUserID)
	e.Memo = nullStringPtr(memo)
	e.Region = nullStringPtr(region)
	e.Industry = nullStringPtr(industry)
	if decidedAt.Valid {
		e.DecidedAt = &decidedAt.Time
	}
	if deadline.Valid {
		v := deadline.Time.Format("2006-01-02")
		e.SubmissionDeadline = &v
	}
	if awardedAmount.Valid {
		e.AwardedAmount = &awardedAmount.Int64
	}
	if budgetAmount.Valid {
		e.BudgetAmount = &budgetAmount.Int64
	}
	return &e, nil
}

const pipelineEntrySelect = `
	SELECT pe.id, pe.notice_id, n.title, n.organization_name, pe.status, pe.assignee_name, pe.assignee_email, pe.assignee_phone, pe.assignee_user_id,
	       pe.decided_at, pe.submission_deadline, pe.memo, pe.awarded_amount, pe.created_at, pe.updated_at,
	       n.notice_type, n.region, n.industry, n.budget_amount, n.status
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
	// 단, 상태가 "제외"(참여 취소됨)면 "참여 검토 시작"은 재활성으로 보고 검토전으로
	// 되살린다 — 목록 토글에서 담김→취소→다시 담기가 자연스럽게 동작하도록(2026-08-08).
	// 그 외 진행 단계는 초기화하지 않는다(멱등 유지).
	var existingID, existingStatus string
	err = s.db.QueryRowContext(ctx,
		`SELECT id, status FROM notice_pipeline_entries WHERE company_profile_id = $1 AND notice_id = $2`,
		profile.ID, noticeID,
	).Scan(&existingID, &existingStatus)
	if err == nil {
		if existingStatus == "제외" {
			if _, err := s.db.ExecContext(ctx,
				`UPDATE notice_pipeline_entries SET status = '검토전', decided_at = now() WHERE id = $1`, existingID); err != nil {
				s.logger.Error("create-pipeline: reactivate excluded entry failed", "error", err)
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
				return
			}
		}
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

	// 원클릭 참여검토(Phase 1): "참여 검토 시작" 클릭 자체가 이미 검토에
	// 착수했다는 뜻이라, 아직 손대지 않은 '검토전'이 아니라 '참여검토'
	// (검토 중)로 바로 시작한다. '검토전'은 값 자체를 지우지 않았으니
	// 기존 데이터/전이 로직에는 영향이 없다(dashboard.go의
	// pipelineActiveStatuses/pipelineUndecidedStatuses 둘 다 이미 두
	// 상태를 동일하게 취급).
	var entryID string
	err = s.db.QueryRowContext(ctx, `
		INSERT INTO notice_pipeline_entries (company_profile_id, notice_id, status, decided_at, submission_deadline, assignee_name, assignee_email, assignee_phone, company_profile_snapshot)
		VALUES ($1, $2, '참여검토', now(), $3, $4, $5, $6, (SELECT to_jsonb(cp) FROM company_profiles cp WHERE cp.id = $1)) RETURNING id`,
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

	s.recordAuditLog(ctx, userID, "pipeline_entry_created", "notice_pipeline_entry", entryID, map[string]any{
		"noticeId": noticeID, "status": "참여검토",
	})

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
	colIndex := map[string]int{}
	// 같은 컬럼에 두 번 addSet이 호출되면(예: assigneeName과 assigneeUserId가
	// 한 요청에 동시에 와서 둘 다 assignee_name을 건드리는 경우) UPDATE에
	// 같은 컬럼이 중복 등장해 SQL 에러가 나므로, 이미 등록된 컬럼이면 새 SET
	// 절을 추가하지 않고 기존 자리의 값만 덮어쓴다 — 나중 호출이 이긴다.
	addSet := func(column string, value any) {
		if idx, ok := colIndex[column]; ok {
			args[idx] = value
			return
		}
		args = append(args, value)
		colIndex[column] = len(args) - 1
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
		// 자유텍스트 담당자명을 명시적으로 보냈다는 건 회원계정 연결을 쓰지
		// 않겠다는 뜻 — assigneeUserId가 같은 요청에 없으면 기존 연결을 끊는다.
		if _, alsoAssigningAccount := raw["assigneeUserId"]; !alsoAssigningAccount {
			addSet("assignee_user_id", nil)
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
	if rawAssigneeUserID, present := raw["assigneeUserId"]; present {
		var uid string
		json.Unmarshal(rawAssigneeUserID, &uid)
		uid = strings.TrimSpace(uid)
		if uid == "" {
			addSet("assignee_user_id", nil)
		} else {
			// 지정하려는 계정이 실제로 이 회사 소속(company_members)인지 확인 —
			// 다른 회사 소속 계정을 담당자로 잘못/악의적으로 연결할 수 없게 한다.
			var memberEmail string
			lookupErr := s.db.QueryRowContext(ctx, `
				SELECT u.email FROM company_members cm JOIN users u ON u.id = cm.user_id
				WHERE cm.company_profile_id = $1 AND cm.user_id = $2`, profile.ID, uid).Scan(&memberEmail)
			if lookupErr == sql.ErrNoRows {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_assignee_user"})
				return
			}
			if lookupErr != nil {
				s.logger.Error("update-pipeline: assignee member lookup failed", "error", lookupErr)
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
				return
			}
			addSet("assignee_user_id", uid)
			// 표시용 담당자명/이메일도 계정 정보로 맞춰 report.go의 팀원별
			// 통계(assignee_name 기준 GROUP BY)와 대시보드 미배정 판정이
			// 계속 정확히 동작하게 한다 — 회원계정 이메일 외 별도 이름
			// 필드가 없어(company_members에 name 컬럼 없음) 이메일을 그대로
			// 표시명으로 쓴다.
			addSet("assignee_name", memberEmail)
			addSet("assignee_email", memberEmail)
			addSet("assignee_phone", nil)
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
	if rawAwardedAmount, present := raw["awardedAmount"]; present {
		var amount *int64
		if err := json.Unmarshal(rawAwardedAmount, &amount); err != nil || (amount != nil && *amount < 0) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_awarded_amount"})
			return
		}
		addSet("awarded_amount", amount)
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

	// 담당자별 개별 알림 설정 재설계: 수신자는 이제 엔트리 자체의
	// assignee_email/assignee_phone이 아니라 이 회사의 company_contacts
	// 전체(notifications.go의 fetchNotifiableContacts) — 그래서 여기서는
	// entry.AssigneeEmail/Phone을 더 이상 확인하지 않는다.
	if statusChanged {
		title, noticeID, status := entry.NoticeTitle, entry.NoticeID, entry.Status
		go s.notifyAssigneeStatusChange(context.Background(), profile.ID, entryID, noticeID, title, currentStatus, status)
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
	s.attachPipelineGrades(r.Context(), profile, items)
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// attachPipelineGrades computes each entry's AI 판정등급 in place (mutates
// items) using the same live scoreNoticeForCompany path growth_analytics.go's
// fetchGradeDistribution uses — grade isn't a persisted column, so this
// re-derives it per request. Best-effort: a lookup failure just leaves
// AIGrade empty for the affected entries rather than failing the whole list.
func (s *Server) attachPipelineGrades(ctx context.Context, profile *companyProfileDTO, items []pipelineEntry) {
	if len(items) == 0 {
		return
	}
	var region, size sql.NullString
	if profile.Region != nil {
		region = sql.NullString{String: *profile.Region, Valid: true}
	}
	if profile.CompanySize != nil {
		size = sql.NullString{String: *profile.CompanySize, Valid: true}
	}
	trackRecordMax, err := s.fetchTrackRecordMaxAmount(ctx, profile.ID)
	if err != nil {
		s.logger.Error("pipeline-grades: track record lookup failed", "error", err)
	}
	company := companyScoringInput{Region: region, Industry: profile.Industry, Size: size, TrackRecordMaxAmount: trackRecordMax}

	noticeIDs := make([]string, len(items))
	for i, it := range items {
		noticeIDs[i] = it.NoticeID
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, notice_type, region, industry, budget_amount, industry_restricted FROM notices WHERE id = ANY($1)`,
		pq.Array(noticeIDs),
	)
	if err != nil {
		s.logger.Error("pipeline-grades: notice lookup failed", "error", err)
		return
	}
	defer rows.Close()

	notices := make(map[string]noticeScoringInput, len(items))
	for rows.Next() {
		var id, noticeType string
		var noticeRegion, industry sql.NullString
		var budget sql.NullInt64
		var industryRestricted sql.NullBool
		if err := rows.Scan(&id, &noticeType, &noticeRegion, &industry, &budget, &industryRestricted); err != nil {
			continue
		}
		notices[id] = noticeScoringInput{NoticeType: noticeType, Region: noticeRegion, Industry: industry, BudgetAmount: budget, IndustryRestricted: nullBoolPtr(industryRestricted)}
	}

	for i := range items {
		if input, ok := notices[items[i].NoticeID]; ok {
			items[i].AIGrade = scoreNoticeForCompany(input, company).Grade
		}
	}
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

	// documentAnalysisStatus — 2026-08-07, 빈 상태 문구 개선. 체크리스트가
	// 비어있을 때만 계산한다(항목이 있으면 이미 채워진 것이니 불필요).
	documentAnalysisStatus := ""
	if len(checklist) == 0 {
		documentAnalysisStatus, err = s.computeNoticeDocumentAnalysisStatusByNoticeID(ctx, entry.NoticeID)
		if err != nil {
			s.logger.Error("get-pipeline: compute document analysis status failed", "error", err)
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{"entry": entry, "checklist": checklist, "documentAnalysisStatus": documentAnalysisStatus})
}
