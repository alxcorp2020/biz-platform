// bookmark.go — 관심공고(북마크). 로그인한 사용자가 공고를 "찜"해두고
// 마이페이지(#/me/bookmarks)에서 모아볼 수 있다. 기업 프로필과 무관하게
// user_id에만 연결된다 — 로그인만 하면 되고 프로필 등록은 필요 없다.
package api

import (
	"database/sql"
	"encoding/json"
	"net/http"
)

type bookmarkToggleRequest struct {
	Bookmarked bool `json:"bookmarked"`
}

func (s *Server) handleToggleBookmark(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.currentUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	noticeID := r.PathValue("id")

	var req bookmarkToggleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_body"})
		return
	}

	ctx := r.Context()
	var exists bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM notices WHERE id = $1)`, noticeID).Scan(&exists); err != nil {
		s.logger.Error("bookmark: notice existence check failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if !exists {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "notice_not_found"})
		return
	}

	var err error
	if req.Bookmarked {
		_, err = s.db.ExecContext(ctx, `
			INSERT INTO notice_bookmarks (user_id, notice_id)
			VALUES ($1, $2)
			ON CONFLICT (user_id, notice_id) DO NOTHING`, userID, noticeID)
	} else {
		_, err = s.db.ExecContext(ctx, `DELETE FROM notice_bookmarks WHERE user_id = $1 AND notice_id = $2`, userID, noticeID)
	}
	if err != nil {
		s.logger.Error("bookmark: upsert/delete failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"noticeId": noticeID, "bookmarked": req.Bookmarked})
}

// handleListBookmarks는 마감 임박순으로 정렬한다(대시보드 추천 목록과 같은
// 기준) — 관심공고 중 뭐가 급한지 바로 보이게.
func (s *Server) handleListBookmarks(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.currentUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	rows, err := s.db.QueryContext(r.Context(), `
		SELECT n.id, n.title, n.organization_name, n.region, n.industry, n.status,
		       n.application_end_at, n.budget_amount, n.official_url, n.current_version
		FROM notice_bookmarks nb
		JOIN notices n ON n.id = nb.notice_id
		WHERE nb.user_id = $1
		ORDER BY n.application_end_at ASC NULLS LAST`, userID)
	if err != nil {
		s.logger.Error("list bookmarks query failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	defer rows.Close()

	items := []noticeListItem{}
	for rows.Next() {
		var it noticeListItem
		var org, region, industry, officialURL sql.NullString
		var budget sql.NullInt64
		var deadline sql.NullTime
		if err := rows.Scan(&it.ID, &it.Title, &org, &region, &industry, &it.Status,
			&deadline, &budget, &officialURL, &it.CurrentVersion); err != nil {
			s.logger.Error("scan bookmark row failed", "error", err)
			continue
		}
		it.OrganizationName, it.Region, it.Industry, it.OfficialURL = org.String, region.String, industry.String, officialURL.String
		it.IsBookmarked = true
		if budget.Valid {
			it.BudgetAmount = &budget.Int64
		}
		if deadline.Valid {
			it.ApplicationEndAt = &deadline.Time
		}
		items = append(items, it)
	}
	if err := rows.Err(); err != nil {
		s.logger.Error("scan bookmarks rows failed", "error", err)
	}

	writeJSON(w, http.StatusOK, map[string]any{"items": items, "count": len(items)})
}
