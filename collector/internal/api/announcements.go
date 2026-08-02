// announcements.go — 관리자 CMS 6번(공지 게시판). 사용자용 목록/상세와
// 관리자용 CRUD가 이 파일 하나를 공유한다 — 공지사항은 배너/팝업과 달리
// "관리자만 보는 목록"이 따로 필요 없다(비공개/임시저장 개념이 없어
// 공개 목록 API를 관리자 화면도 그대로 재사용).
package api

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

type announcementListItem struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	IsPinned  bool      `json:"isPinned"`
	ViewCount int       `json:"viewCount"`
	CreatedAt time.Time `json:"createdAt"`
}

// handleListAnnouncements — GET /api/announcements(공개). 상단고정을
// 먼저, 그 안에서는 최신순 — 목록 화면(#/announcements)과 관리자 화면
// (#/admin/announcements) 둘 다 이 API를 그대로 쓴다.
func (s *Server) handleListAnnouncements(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.QueryContext(r.Context(), `
		SELECT id, title, is_pinned, view_count, created_at
		FROM announcements ORDER BY is_pinned DESC, created_at DESC`)
	if err != nil {
		s.logger.Error("list-announcements: query failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	defer rows.Close()

	items := []announcementListItem{}
	for rows.Next() {
		var it announcementListItem
		if err := rows.Scan(&it.ID, &it.Title, &it.IsPinned, &it.ViewCount, &it.CreatedAt); err != nil {
			s.logger.Error("list-announcements: scan failed", "error", err)
			continue
		}
		items = append(items, it)
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

type announcementDetail struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Content   string    `json:"content"`
	IsPinned  bool      `json:"isPinned"`
	ViewCount int       `json:"viewCount"`
	CreatedAt time.Time `json:"createdAt"`
}

// handleGetAnnouncement — GET /api/announcements/{id}(공개). 조회 자체가
// "읽음"이라 조회수 증가를 별도 API로 안 쪼개고 UPDATE...RETURNING 한 번에
// 처리한다(같은 요청 안에서 증가 후 값까지 그대로 돌려줌 — 조회 직후 별도
// GET을 한 번 더 안 해도 최신 조회수가 응답에 실림).
func (s *Server) handleGetAnnouncement(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var it announcementDetail
	err := s.db.QueryRowContext(r.Context(), `
		UPDATE announcements SET view_count = view_count + 1
		WHERE id = $1
		RETURNING id, title, content, is_pinned, view_count, created_at`, id,
	).Scan(&it.ID, &it.Title, &it.Content, &it.IsPinned, &it.ViewCount, &it.CreatedAt)
	if err == sql.ErrNoRows {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "announcement_not_found"})
		return
	}
	if err != nil {
		s.logger.Error("get-announcement: query failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	writeJSON(w, http.StatusOK, it)
}

// announcementRequest — ViewCount는 등록 시 초기값(예: 다른 게시판에서
// 이전하면서 기존 조회수를 그대로 옮기고 싶을 때)이자 수정 시 값 자체를
// 직접 고칠 수 있는 필드다. 음수는 허용하지 않는다(핸들러에서 검증) —
// 그 외에는 관리자가 입력한 값을 그대로 신뢰한다(집계 로직 없이 단순
// 대입이라 상한을 둘 이유가 없음).
type announcementRequest struct {
	Title     string `json:"title"`
	Content   string `json:"content"`
	IsPinned  bool   `json:"isPinned"`
	ViewCount int    `json:"viewCount"`
}

func (s *Server) handleAdminCreateAnnouncement(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireSystemAdmin(w, r); !ok {
		return
	}
	var req announcementRequest
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
	if req.ViewCount < 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_view_count"})
		return
	}

	var id string
	err := s.db.QueryRowContext(r.Context(), `
		INSERT INTO announcements (title, content, is_pinned, view_count) VALUES ($1,$2,$3,$4) RETURNING id`,
		req.Title, req.Content, req.IsPinned, req.ViewCount,
	).Scan(&id)
	if err != nil {
		s.logger.Error("admin-create-announcement: insert failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": id})
}

func (s *Server) handleAdminUpdateAnnouncement(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireSystemAdmin(w, r); !ok {
		return
	}
	id := r.PathValue("id")
	var req announcementRequest
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
	if req.ViewCount < 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_view_count"})
		return
	}

	res, err := s.db.ExecContext(r.Context(), `
		UPDATE announcements SET title=$1, content=$2, is_pinned=$3, view_count=$4 WHERE id=$5`,
		req.Title, req.Content, req.IsPinned, req.ViewCount, id)
	if err != nil {
		s.logger.Error("admin-update-announcement: update failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "announcement_not_found"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

func (s *Server) handleAdminDeleteAnnouncement(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireSystemAdmin(w, r); !ok {
		return
	}
	id := r.PathValue("id")
	res, err := s.db.ExecContext(r.Context(), `DELETE FROM announcements WHERE id=$1`, id)
	if err != nil {
		s.logger.Error("admin-delete-announcement: delete failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "announcement_not_found"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}
