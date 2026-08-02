// admin_banners.go — 관리자 CMS 3번(배너 관리). banners.go의
// handleListBanners(공개 조회)와 짝을 이루는 관리자 전용 CRUD.
package api

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

type adminBannerItem struct {
	ID           string     `json:"id"`
	Title        string     `json:"title"`
	ImageURL     string     `json:"imageUrl"`
	LinkURL      *string    `json:"linkUrl"`
	DisplayOrder int        `json:"displayOrder"`
	IsActive     bool       `json:"isActive"`
	StartsAt     *time.Time `json:"startsAt"`
	EndsAt       *time.Time `json:"endsAt"`
	CreatedAt    time.Time  `json:"createdAt"`
}

// handleAdminListBanners — GET /api/admin/banners. 공개 API(banners.go)와
// 달리 활성/노출기간 필터 없이 전체를 display_order 순으로 보여준다 —
// 관리자는 비활성/기간만료 배너도 관리(재활성화 등)할 수 있어야 한다.
func (s *Server) handleAdminListBanners(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireSystemAdmin(w, r); !ok {
		return
	}
	rows, err := s.db.QueryContext(r.Context(), `
		SELECT id, title, image_url, link_url, display_order, is_active, starts_at, ends_at, created_at
		FROM banners ORDER BY display_order ASC, created_at ASC`)
	if err != nil {
		s.logger.Error("admin-list-banners: query failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	defer rows.Close()

	items := []adminBannerItem{}
	for rows.Next() {
		it, err := scanAdminBanner(rows)
		if err != nil {
			s.logger.Error("admin-list-banners: scan failed", "error", err)
			continue
		}
		items = append(items, *it)
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func scanAdminBanner(row interface{ Scan(dest ...any) error }) (*adminBannerItem, error) {
	var it adminBannerItem
	var linkURL sql.NullString
	var startsAt, endsAt sql.NullTime
	if err := row.Scan(&it.ID, &it.Title, &it.ImageURL, &linkURL, &it.DisplayOrder, &it.IsActive, &startsAt, &endsAt, &it.CreatedAt); err != nil {
		return nil, err
	}
	it.LinkURL = nullStringPtr(linkURL)
	if startsAt.Valid {
		it.StartsAt = &startsAt.Time
	}
	if endsAt.Valid {
		it.EndsAt = &endsAt.Time
	}
	return &it, nil
}

type bannerRequest struct {
	Title    string     `json:"title"`
	ImageURL string     `json:"imageUrl"`
	LinkURL  *string    `json:"linkUrl"`
	IsActive bool       `json:"isActive"`
	StartsAt *time.Time `json:"startsAt"`
	EndsAt   *time.Time `json:"endsAt"`
}

func (s *Server) handleAdminCreateBanner(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireSystemAdmin(w, r); !ok {
		return
	}
	var req bannerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_body"})
		return
	}
	req.Title = strings.TrimSpace(req.Title)
	if req.Title == "" || req.ImageURL == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "title_and_image_required"})
		return
	}

	var id string
	err := s.db.QueryRowContext(r.Context(), `
		INSERT INTO banners (title, image_url, link_url, display_order, is_active, starts_at, ends_at)
		VALUES ($1,$2,$3, COALESCE((SELECT MAX(display_order)+1 FROM banners), 0), $4,$5,$6)
		RETURNING id`,
		req.Title, req.ImageURL, req.LinkURL, req.IsActive, req.StartsAt, req.EndsAt,
	).Scan(&id)
	if err != nil {
		s.logger.Error("admin-create-banner: insert failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": id})
}

func (s *Server) handleAdminUpdateBanner(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireSystemAdmin(w, r); !ok {
		return
	}
	id := r.PathValue("id")
	var req bannerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_body"})
		return
	}
	req.Title = strings.TrimSpace(req.Title)
	if req.Title == "" || req.ImageURL == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "title_and_image_required"})
		return
	}

	res, err := s.db.ExecContext(r.Context(), `
		UPDATE banners SET title=$1, image_url=$2, link_url=$3, is_active=$4, starts_at=$5, ends_at=$6
		WHERE id=$7`,
		req.Title, req.ImageURL, req.LinkURL, req.IsActive, req.StartsAt, req.EndsAt, id)
	if err != nil {
		s.logger.Error("admin-update-banner: update failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "banner_not_found"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

func (s *Server) handleAdminDeleteBanner(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireSystemAdmin(w, r); !ok {
		return
	}
	id := r.PathValue("id")
	res, err := s.db.ExecContext(r.Context(), `DELETE FROM banners WHERE id=$1`, id)
	if err != nil {
		s.logger.Error("admin-delete-banner: delete failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "banner_not_found"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// handleAdminMoveBanner — POST /api/admin/banners/{id}/move {"direction":"up"|"down"}.
// 드래그앤드롭 대신 화살표 버튼 방식(스펙에서 "또는"으로 허용한 더 단순한
// 쪽)을 택했다 — display_order가 정수 나열이라 인접 배너와 값을 맞바꾸는
// 것만으로 순서 이동이 끝난다. 이미 맨 위/맨 아래라 이동할 대상이 없으면
// 조용히 no-op(에러 아님).
func (s *Server) handleAdminMoveBanner(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireSystemAdmin(w, r); !ok {
		return
	}
	id := r.PathValue("id")
	var req struct {
		Direction string `json:"direction"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || (req.Direction != "up" && req.Direction != "down") {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_direction"})
		return
	}

	ctx := r.Context()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		s.logger.Error("admin-move-banner: begin tx failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	defer tx.Rollback()

	var currentOrder int
	if err := tx.QueryRowContext(ctx, `SELECT display_order FROM banners WHERE id=$1`, id).Scan(&currentOrder); err == sql.ErrNoRows {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "banner_not_found"})
		return
	} else if err != nil {
		s.logger.Error("admin-move-banner: lookup failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}

	neighborQuery := `SELECT id, display_order FROM banners WHERE display_order > $1 ORDER BY display_order ASC LIMIT 1`
	if req.Direction == "up" {
		neighborQuery = `SELECT id, display_order FROM banners WHERE display_order < $1 ORDER BY display_order DESC LIMIT 1`
	}
	var neighborID string
	var neighborOrder int
	err = tx.QueryRowContext(ctx, neighborQuery, currentOrder).Scan(&neighborID, &neighborOrder)
	if err == sql.ErrNoRows {
		tx.Commit()
		writeJSON(w, http.StatusOK, map[string]string{"status": "noop"})
		return
	}
	if err != nil {
		s.logger.Error("admin-move-banner: neighbor lookup failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}

	if _, err := tx.ExecContext(ctx, `UPDATE banners SET display_order=$1 WHERE id=$2`, neighborOrder, id); err != nil {
		s.logger.Error("admin-move-banner: update self failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if _, err := tx.ExecContext(ctx, `UPDATE banners SET display_order=$1 WHERE id=$2`, currentOrder, neighborID); err != nil {
		s.logger.Error("admin-move-banner: update neighbor failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if err := tx.Commit(); err != nil {
		s.logger.Error("admin-move-banner: commit failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "moved"})
}
