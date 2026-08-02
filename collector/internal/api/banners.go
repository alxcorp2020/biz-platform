// 홈 화면 배너 슬라이드(관리자 CMS 1번). 이 파일은 공개 조회 API만
// 담당한다 — 관리자 등록/수정/삭제 API는 3단계(#/admin/banners)에서
// admin_banners.go 등으로 별도 추가 예정.
package api

import (
	"database/sql"
	"net/http"
	"strings"
)

type bannerItem struct {
	ID           string  `json:"id"`
	Title        string  `json:"title"`
	ImageURL     string  `json:"imageUrl"`
	LinkURL      *string `json:"linkUrl"`
	DisplayOrder int     `json:"displayOrder"`
}

// brandNameToken — 배너 제목(및 향후 다른 관리자 입력 텍스트)에 이 토큰을
// 넣으면 조회 시점의 company_info.brand_name으로 치환된다. 시드 시점
// 값을 고정으로 박아 넣지 않고 "항상 최신 브랜드명"을 원하는 관리자
// 콘텐츠에 쓰는 관례 — 관리자가 배너를 수정하면서 이 토큰을 지우면
// 그 배너는 그 순간부터 그냥 고정 텍스트가 된다(의도된 동작).
const brandNameToken = "{brand_name}"

// handleListBanners — GET /api/banners. 공개 API(로그인 불필요) — 배너
// 슬라이드는 비로그인 방문자용 마케팅 화면(renderMarketingHero)에도
// 노출된다. is_active=true이고 노출기간(starts_at~ends_at, NULL이면 그
// 방향은 무제한) 안에 있는 배너만 display_order 오름차순으로 반환한다.
// 제목에 brandNameToken이 들어있으면 현재 브랜드명으로 치환해서 내려준다
// (관리자 화면 GET /api/admin/banners는 원본 그대로 보여줘서 관리자가
// 토큰 존재 자체를 알아볼 수 있게 함 — admin_banners.go 참고).
func (s *Server) handleListBanners(w http.ResponseWriter, r *http.Request) {
	var brandName string
	if err := s.db.QueryRowContext(r.Context(), `SELECT brand_name FROM company_info WHERE id = 1`).Scan(&brandName); err != nil {
		brandName = "공공사업 AI 비서"
	}

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
		it.Title = strings.ReplaceAll(it.Title, brandNameToken, brandName)
		it.LinkURL = nullStringPtr(linkURL)
		items = append(items, it)
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}
