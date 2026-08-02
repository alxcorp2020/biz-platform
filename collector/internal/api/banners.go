// 홈 화면 배너 슬라이드(관리자 CMS 1번). 이 파일은 공개 조회 API만
// 담당한다 — 관리자 등록/수정/삭제 API는 3단계(#/admin/banners)에서
// admin_banners.go 등으로 별도 추가 예정.
package api

import (
	"database/sql"
	"net/http"
)

type bannerItem struct {
	ID           string  `json:"id"`
	Title        string  `json:"title"`
	ImageURL     string  `json:"imageUrl"`
	LinkURL      *string `json:"linkUrl"`
	DisplayOrder int     `json:"displayOrder"`
}

// handleListBanners — GET /api/banners. 공개 API(로그인 불필요) — 배너
// 슬라이드는 비로그인 방문자용 마케팅 화면(renderMarketingHero)에도
// 노출된다. is_active=true이고 노출기간(starts_at~ends_at, NULL이면 그
// 방향은 무제한) 안에 있는 배너만 display_order 오름차순으로 반환한다.
func (s *Server) handleListBanners(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.QueryContext(r.Context(), `
		SELECT id, title, image_url, link_url, display_order
		FROM banners
		WHERE is_active = true
		  AND (starts_at IS NULL OR starts_at <= now())
		  AND (ends_at IS NULL OR ends_at >= now())
		ORDER BY display_order ASC, created_at ASC`)
	if err != nil {
		s.logger.Error("list banners query failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	defer rows.Close()

	items := []bannerItem{}
	for rows.Next() {
		var it bannerItem
		var linkURL sql.NullString
		if err := rows.Scan(&it.ID, &it.Title, &it.ImageURL, &linkURL, &it.DisplayOrder); err != nil {
			s.logger.Error("scan banner row failed", "error", err)
			continue
		}
		it.LinkURL = nullStringPtr(linkURL)
		items = append(items, it)
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}
