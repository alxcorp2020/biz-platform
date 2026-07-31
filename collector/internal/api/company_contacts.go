// company_contacts.go — 담당자 관리. 파이프라인 생성(참여 검토) 시
// assignee_name/email/phone을 매번 빈칸부터 입력하지 않도록, 회사가 미리
// 등록해두는 담당자 목록. is_default인 담당자가 있으면 handleCreatePipelineEntry
// (company_pipeline.go)가 그 값을 자동으로 채운다 — 강제가 아니라 자동채움일
// 뿐, 사용자가 그 자리에서 수정할 수 있다.
//
// is_default는 프로필당 최대 1명 — DB 제약이 아니라 이 파일의 트랜잭션이
// 보장한다(새로 기본 지정 시 기존 기본 해제 후 지정, 한 트랜잭션 안에서).
package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"time"
)

type contactItem struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Email     *string   `json:"email"`
	Phone     *string   `json:"phone"`
	IsDefault bool      `json:"isDefault"`
	CreatedAt time.Time `json:"createdAt"`
}

const contactSelect = `SELECT id, name, email, phone, is_default, created_at FROM company_contacts`

func scanContact(row interface{ Scan(dest ...any) error }) (*contactItem, error) {
	var c contactItem
	var email, phone sql.NullString
	if err := row.Scan(&c.ID, &c.Name, &email, &phone, &c.IsDefault, &c.CreatedAt); err != nil {
		return nil, err
	}
	c.Email = nullStringPtr(email)
	c.Phone = nullStringPtr(phone)
	return &c, nil
}

// handleListContacts is readable by both roles(owner/member) — member가
// "담당자 관리" 탭을 읽기 전용으로 볼 수 있어야 참여검토 자동채움이 어떤
// 담당자를 쓸지 스스로 확인할 수 있다.
func (s *Server) handleListContacts(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.currentUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	profile, err := s.getCompanyProfile(r, userID)
	if err != nil {
		s.logger.Error("list-contacts: profile lookup failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if profile == nil {
		writeJSON(w, http.StatusOK, map[string]any{"items": []contactItem{}})
		return
	}

	rows, err := s.db.QueryContext(r.Context(),
		contactSelect+` WHERE company_profile_id = $1 ORDER BY is_default DESC, created_at ASC`,
		profile.ID)
	if err != nil {
		s.logger.Error("list-contacts: query failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	defer rows.Close()

	items := []contactItem{}
	for rows.Next() {
		c, err := scanContact(rows)
		if err != nil {
			s.logger.Error("list-contacts: scan failed", "error", err)
			continue
		}
		items = append(items, *c)
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

type contactRequest struct {
	Name      string  `json:"name"`
	Email     *string `json:"email"`
	Phone     *string `json:"phone"`
	IsDefault bool    `json:"isDefault"`
}

func (s *Server) handleCreateContact(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.currentUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	profile, err := s.getCompanyProfile(r, userID)
	if err != nil {
		s.logger.Error("create-contact: profile lookup failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if profile == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "company_profile_required"})
		return
	}
	if profile.Role != "owner" {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "owner_only"})
		return
	}

	var req contactRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_request_body"})
		return
	}
	if req.Name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name_required"})
		return
	}

	ctx := r.Context()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		s.logger.Error("create-contact: begin tx failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	defer tx.Rollback()

	if req.IsDefault {
		if err := unsetDefaultContact(ctx, tx, profile.ID); err != nil {
			s.logger.Error("create-contact: unset default failed", "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
			return
		}
	}

	var id string
	err = tx.QueryRowContext(ctx, `
		INSERT INTO company_contacts (company_profile_id, name, email, phone, is_default)
		VALUES ($1,$2,$3,$4,$5) RETURNING id`,
		profile.ID, req.Name, req.Email, req.Phone, req.IsDefault,
	).Scan(&id)
	if err != nil {
		s.logger.Error("create-contact: insert failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if err := tx.Commit(); err != nil {
		s.logger.Error("create-contact: commit failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"id": id, "name": req.Name, "email": req.Email, "phone": req.Phone, "isDefault": req.IsDefault,
	})
}

func (s *Server) handleUpdateContact(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.currentUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	profile, err := s.getCompanyProfile(r, userID)
	if err != nil {
		s.logger.Error("update-contact: profile lookup failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if profile == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "company_profile_required"})
		return
	}
	if profile.Role != "owner" {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "owner_only"})
		return
	}
	contactID := r.PathValue("id")

	var req contactRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_request_body"})
		return
	}
	if req.Name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name_required"})
		return
	}

	ctx := r.Context()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		s.logger.Error("update-contact: begin tx failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	defer tx.Rollback()

	if req.IsDefault {
		if err := unsetDefaultContact(ctx, tx, profile.ID); err != nil {
			s.logger.Error("update-contact: unset default failed", "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
			return
		}
	}

	res, err := tx.ExecContext(ctx, `
		UPDATE company_contacts SET name = $1, email = $2, phone = $3, is_default = $4
		WHERE id = $5 AND company_profile_id = $6`,
		req.Name, req.Email, req.Phone, req.IsDefault, contactID, profile.ID,
	)
	if err != nil {
		s.logger.Error("update-contact: update failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "contact_not_found"})
		return
	}
	if err := tx.Commit(); err != nil {
		s.logger.Error("update-contact: commit failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"status": "updated"})
}

func (s *Server) handleDeleteContact(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.currentUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	profile, err := s.getCompanyProfile(r, userID)
	if err != nil {
		s.logger.Error("delete-contact: profile lookup failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if profile == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "company_profile_required"})
		return
	}
	if profile.Role != "owner" {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "owner_only"})
		return
	}
	contactID := r.PathValue("id")

	res, err := s.db.ExecContext(r.Context(),
		`DELETE FROM company_contacts WHERE id = $1 AND company_profile_id = $2`,
		contactID, profile.ID)
	if err != nil {
		s.logger.Error("delete-contact: delete failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "contact_not_found"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// unsetDefaultContact clears is_default on every other contact for this
// profile — called before setting a new default so at most one ever stays
// true. Must run inside the same tx as the subsequent insert/update.
func unsetDefaultContact(ctx context.Context, tx *sql.Tx, profileID string) error {
	_, err := tx.ExecContext(ctx,
		`UPDATE company_contacts SET is_default = false WHERE company_profile_id = $1 AND is_default = true`,
		profileID)
	return err
}

// fetchDefaultContact returns this profile's is_default=true contact, or nil
// if none is set — used by handleCreatePipelineEntry (company_pipeline.go)
// to auto-fill assignee_name/email/phone.
func (s *Server) fetchDefaultContact(ctx context.Context, profileID string) (*contactItem, error) {
	row := s.db.QueryRowContext(ctx,
		contactSelect+` WHERE company_profile_id = $1 AND is_default = true LIMIT 1`, profileID)
	c, err := scanContact(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return c, nil
}
