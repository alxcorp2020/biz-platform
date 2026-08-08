package api

import (
	"net/http"
	"strings"
)

// handleNoticeSuggest — GET /api/notices/suggest?q=... 공고명 자동완성(타이핑 중
// 후보 제시). 진행중(열림) 공고 제목 중 q를 부분일치로 포함하는 것들을 중복
// 제거해 최대 10건 돌려준다. 접두어 일치를 먼저, 그다음 짧은 제목 순으로
// 정렬해 더 관련 있는 후보가 위로 오게 한다. 공개 엔드포인트(로그인 불필요).
//
// 열림 판정 조건은 handleListNotices의 기본 검색(includeClosed 미지정)과 동일하게
// 맞춰, 자동완성으로 고른 키워드가 실제 검색 결과와 어긋나지 않게 한다.
func (s *Server) handleNoticeSuggest(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	// 최소 2글자부터 — 1글자는 후보가 너무 많고 의미가 적다.
	if len([]rune(q)) < 2 {
		writeJSON(w, http.StatusOK, map[string]any{"suggestions": []string{}})
		return
	}
	rows, err := s.db.QueryContext(r.Context(), `
		SELECT title FROM (
			SELECT DISTINCT title FROM notices
			WHERE status NOT IN ('closed','cancelled')
			  AND (application_end_at IS NULL OR application_end_at >= CURRENT_DATE)
			  AND title ILIKE $1
		) t
		ORDER BY (title ILIKE $2) DESC, char_length(title), title
		LIMIT 10`,
		"%"+q+"%", q+"%")
	if err != nil {
		s.logger.Error("notice-suggest: query failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	defer rows.Close()

	suggestions := []string{}
	for rows.Next() {
		var title string
		if err := rows.Scan(&title); err != nil {
			s.logger.Error("notice-suggest: scan failed", "error", err)
			break
		}
		suggestions = append(suggestions, title)
	}
	writeJSON(w, http.StatusOK, map[string]any{"suggestions": suggestions})
}
