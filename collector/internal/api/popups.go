// popups.go — 관리자 CMS 5번(팝업 관리). 배너 레이어와 같은 "홈 진입 시
// 오버레이" 방식이지만 별개 콘텐츠(공지성 팝업, 단일 이미지+마크다운
// 텍스트)라 테이블/화면을 분리했다. "오늘 하루 보지 않기"는 클라이언트
// localStorage(popup_dismissed_{id}_{date})가 담당하므로 서버는 그냥
// 활성 팝업 목록만 내려주면 된다.
package api

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

type popupItem struct {
	ID       string  `json:"id"`
	Title    string  `json:"title"`
	ImageURL *string `json:"imageUrl"`
	Content  string  `json:"content"`
}

// handleListPopups — GET /api/popups(공개, 인증 불필요). 활성 + 노출기간
// 안의 팝업을 최신순으로 반환 — 여러 개 있어도 프론트는 그중 오늘 아직 안
// 닫은 첫 번째 것 하나만 보여준다(renderPopupLayer).
func (s *Server) handleListPopups(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.QueryContext(r.Context(), `
		SELECT id, title, image_url, content FROM popups
		WHERE is_active = true
		  AND (starts_at IS NULL OR starts_at <= now())
		  AND (ends_at IS NULL OR ends_at >= now())
		ORDER BY created_at DESC`)
	if err != nil {
		s.logger.Error("list-popups: query failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	defer rows.Close()

	items := []popupItem{}
	for rows.Next() {
		var it popupItem
		var imageURL sql.NullString
		if err := rows.Scan(&it.ID, &it.Title, &imageURL, &it.Content); err != nil {
			s.logger.Error("list-popups: scan failed", "error", err)
			continue
		}
		it.ImageURL = nullStringPtr(imageURL)
		items = append(items, it)
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

type adminPopupItem struct {
	ID        string     `json:"id"`
	Title     string     `json:"title"`
	ImageURL  *string    `json:"imageUrl"`
	Content   string     `json:"content"`
	IsActive  bool       `json:"isActive"`
	StartsAt  *time.Time `json:"startsAt"`
	EndsAt    *time.Time `json:"endsAt"`
	CreatedAt time.Time  `json:"createdAt"`
}

func (s *Server) handleAdminListPopups(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireSystemAdmin(w, r); !ok {
		return
	}
	rows, err := s.db.QueryContext(r.Context(), `
		SELECT id, title, image_url, content, is_active, starts_at, ends_at, created_at
		FROM popups ORDER BY created_at DESC`)
	if err != nil {
		s.logger.Error("admin-list-popups: query failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	defer rows.Close()

	items := []adminPopupItem{}
	for rows.Next() {
		var it adminPopupItem
		var imageURL sql.NullString
		var startsAt, endsAt sql.NullTime
		if err := rows.Scan(&it.ID, &it.Title, &imageURL, &it.Content, &it.IsActive, &startsAt, &endsAt, &it.CreatedAt); err != nil {
			s.logger.Error("admin-list-popups: scan failed", "error", err)
			continue
		}
		it.ImageURL = nullStringPtr(imageURL)
		if startsAt.Valid {
			it.StartsAt = &startsAt.Time
		}
		if endsAt.Valid {
			it.EndsAt = &endsAt.Time
		}
		items = append(items, it)
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

type popupRequest struct {
	Title    string     `json:"title"`
	ImageURL *string    `json:"imageUrl"`
	Content  string     `json:"content"`
	IsActive bool       `json:"isActive"`
	StartsAt *time.Time `json:"startsAt"`
	EndsAt   *time.Time `json:"endsAt"`
}

func (s *Server) handleAdminCreatePopup(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireSystemAdmin(w, r); !ok {
		return
	}
	var req popupRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_body"})
		return
	}
	req.Title = strings.TrimSpace(req.Title)
	req.Content = strings.TrimSpace(req.Content)
	if req.Title == "" || req.Content == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "title_and_content_required"})
		return
	}

	var id string
	err := s.db.QueryRowContext(r.Context(), `
		INSERT INTO popups (title, image_url, content, is_active, starts_at, ends_at)
		VALUES ($1,$2,$3,$4,$5,$6) RETURNING id`,
		req.Title, req.ImageURL, req.Content, req.IsActive, req.StartsAt, req.EndsAt,
	).Scan(&id)
	if err != nil {
		s.logger.Error("admin-create-popup: insert failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": id})
}

func (s *Server) handleAdminUpdatePopup(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireSystemAdmin(w, r); !ok {
		return
	}
	id := r.PathValue("id")
	var req popupRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_body"})
		return
	}
	req.Title = strings.TrimSpace(req.Title)
	req.Content = strings.TrimSpace(req.Content)
	if req.Title == "" || req.Content == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "title_and_content_required"})
		return
	}

	res, err := s.db.ExecContext(r.Context(), `
		UPDATE popups SET title=$1, image_url=$2, content=$3, is_active=$4, starts_at=$5, ends_at=$6
		WHERE id=$7`,
		req.Title, req.ImageURL, req.Content, req.IsActive, req.StartsAt, req.EndsAt, id)
	if err != nil {
		s.logger.Error("admin-update-popup: update failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "popup_not_found"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

func (s *Server) handleAdminDeletePopup(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireSystemAdmin(w, r); !ok {
		return
	}
	id := r.PathValue("id")
	res, err := s.db.ExecContext(r.Context(), `DELETE FROM popups WHERE id=$1`, id)
	if err != nil {
		s.logger.Error("admin-delete-popup: delete failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "popup_not_found"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}
