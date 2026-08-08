package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
)

// 관리자 업종 분류(industry_taxonomy) CMS — Phase 3. 관리자가 조달청 중분류
// 목록을 추가/재분류(대분류 변경)/비활성화할 수 있다. mid_name은 회사 프로필과
// 공고 매칭의 키(company_profiles.industry / notices.industry에 그 문자열이 그대로
// 저장·비교됨)라 수정하지 않는다 — 이름을 바꾸면 기존 선택값이 매칭에서 떨어진다.
// 삭제도 제공하지 않는다: 기존 회사가 이미 고른 업종을 보존하면서 신규 노출만
// 막으려면 active=false(비활성)로 두는 게 안전하다(비활성은 선택 UI 자동완성에서만
// 빠지고, 이미 저장된 값의 매칭에는 영향 없음).

type adminIndustryTaxonomyItem struct {
	ID        int64  `json:"id"`
	MidName   string `json:"midName"`
	LargeName string `json:"largeName"`
	Active    bool   `json:"active"`
	SortOrder int    `json:"sortOrder"`
}

func (s *Server) handleAdminListIndustryTaxonomy(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireSystemAdmin(w, r); !ok {
		return
	}
	rows, err := s.db.QueryContext(r.Context(),
		`SELECT id, mid_name, large_name, active, sort_order FROM industry_taxonomy ORDER BY large_name, sort_order, mid_name`)
	if err != nil {
		s.logger.Error("admin-industry-taxonomy: list failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	defer rows.Close()
	items := []adminIndustryTaxonomyItem{}
	for rows.Next() {
		var it adminIndustryTaxonomyItem
		if err := rows.Scan(&it.ID, &it.MidName, &it.LargeName, &it.Active, &it.SortOrder); err != nil {
			s.logger.Error("admin-industry-taxonomy: scan failed", "error", err)
			break
		}
		items = append(items, it)
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) handleAdminCreateIndustryTaxonomy(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireSystemAdmin(w, r); !ok {
		return
	}
	var req struct {
		MidName   string `json:"midName"`
		LargeName string `json:"largeName"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_body"})
		return
	}
	mid := strings.TrimSpace(req.MidName)
	large := strings.TrimSpace(req.LargeName)
	if mid == "" || large == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "mid_and_large_required"})
		return
	}
	var exists bool
	if err := s.db.QueryRowContext(r.Context(),
		`SELECT EXISTS(SELECT 1 FROM industry_taxonomy WHERE mid_name = $1)`, mid).Scan(&exists); err != nil {
		s.logger.Error("admin-industry-taxonomy: exists check failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if exists {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "mid_already_exists"})
		return
	}
	if _, err := s.db.ExecContext(r.Context(),
		`INSERT INTO industry_taxonomy (mid_name, large_name) VALUES ($1, $2)`, mid, large); err != nil {
		s.logger.Error("admin-industry-taxonomy: insert failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "created"})
}

func (s *Server) handleAdminUpdateIndustryTaxonomy(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireSystemAdmin(w, r); !ok {
		return
	}
	id := r.PathValue("id")
	// large_name(재분류)/active(비활성)/sort_order만 수정. mid_name은 불변.
	var req struct {
		LargeName *string `json:"largeName"`
		Active    *bool   `json:"active"`
		SortOrder *int    `json:"sortOrder"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_body"})
		return
	}
	sets := []string{}
	args := []any{}
	n := 0
	add := func(clause string, v any) { n++; sets = append(sets, clause+"$"+strconv.Itoa(n)); args = append(args, v) }
	if req.LargeName != nil {
		large := strings.TrimSpace(*req.LargeName)
		if large == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "large_required"})
			return
		}
		add("large_name = ", large)
	}
	if req.Active != nil {
		add("active = ", *req.Active)
	}
	if req.SortOrder != nil {
		add("sort_order = ", *req.SortOrder)
	}
	if len(sets) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no_fields"})
		return
	}
	n++
	args = append(args, id)
	res, err := s.db.ExecContext(r.Context(),
		"UPDATE industry_taxonomy SET "+strings.Join(sets, ", ")+" WHERE id = $"+strconv.Itoa(n), args...)
	if err != nil {
		s.logger.Error("admin-industry-taxonomy: update failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if cnt, _ := res.RowsAffected(); cnt == 0 {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}
