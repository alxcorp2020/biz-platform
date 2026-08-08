package api

import (
	"database/sql"
	"net/http"
	"strings"
)

// handleGetIndustryTaxonomy — GET /api/industry-taxonomy?q=
// 조달청 공공조달분류를 대분류로 그룹핑한 중분류 목록으로 반환한다(2026-08-08
// Phase 2a). Phase 2b의 업종 자동완성 picker가 소비할 참조 데이터로, 아직
// 매칭/온보딩 UI를 바꾸지 않으므로 이 단계에서는 소비처가 없다(토대만 마련).
// q가 있으면 중분류명 부분일치로 필터한다. 로그인만 요구(참조 데이터).
func (s *Server) handleGetIndustryTaxonomy(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.currentUserID(r); !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	q := strings.TrimSpace(r.URL.Query().Get("q"))
	var rows *sql.Rows
	var err error
	if q != "" {
		rows, err = s.db.QueryContext(r.Context(), `
			SELECT large_name, mid_name FROM industry_taxonomy
			WHERE active AND mid_name ILIKE $1
			ORDER BY large_name, sort_order, mid_name`, "%"+q+"%")
	} else {
		rows, err = s.db.QueryContext(r.Context(), `
			SELECT large_name, mid_name FROM industry_taxonomy
			WHERE active
			ORDER BY large_name, sort_order, mid_name`)
	}
	if err != nil {
		s.logger.Error("industry-taxonomy: query failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	defer rows.Close()

	type taxonomyGroup struct {
		Large string   `json:"large"`
		Mids  []string `json:"mids"`
	}
	groups := []taxonomyGroup{}
	idx := map[string]int{}
	for rows.Next() {
		var large, mid string
		if err := rows.Scan(&large, &mid); err != nil {
			s.logger.Error("industry-taxonomy: scan failed", "error", err)
			break
		}
		i, ok := idx[large]
		if !ok {
			i = len(groups)
			idx[large] = i
			groups = append(groups, taxonomyGroup{Large: large})
		}
		groups[i].Mids = append(groups[i].Mids, mid)
	}
	writeJSON(w, http.StatusOK, map[string]any{"taxonomy": groups})
}
