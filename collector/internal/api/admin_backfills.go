// admin_backfills.go — 2026-08-09. 무거운 데이터 백필을 startup(migrate.Apply)이
// 아니라 관리자 수동 실행으로 분리했다. 이유: 백필이 raw_documents(대용량)를
// LIKE로 스캔하는 무거운 UPDATE라 startup에서 돌리면 부팅이 오래 블로킹돼 Render
// 헬스체크 포트 오픈 전에 배포가 실패("No open ports")했다. 이제 startup은 가벼운
// 컬럼 추가만 하고, 백필은 서비스가 살아있는 상태에서 여기 버튼으로 여유있게 1회
// 실행한다(멱등이라 여러 번 눌러도 안전, NULL인 행만 채움).
package api

import (
	"net/http"

	"biz-platform/collector/internal/migrate"
)

// handleRunNoticeDatetimeBackfill — notices의 시각 컬럼(opening_at 등)을
// raw_documents로 백필. system_admin 전용.
func (s *Server) handleRunNoticeDatetimeBackfill(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireSystemAdmin(w, r); !ok {
		return
	}
	updated, err := migrate.RunNoticeDatetimeBackfill(r.Context(), s.db)
	if err != nil {
		s.logger.Error("run-notice-datetime-backfill: failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "query_failed"})
		return
	}
	s.logger.Info("run-notice-datetime-backfill: completed", "updated", updated)
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "updated": updated})
}

// handleRunProcurementClassBackfill — notices의 공공조달분류 컬럼을 raw_documents로
// 백필. system_admin 전용.
func (s *Server) handleRunProcurementClassBackfill(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireSystemAdmin(w, r); !ok {
		return
	}
	updated, err := migrate.RunProcurementClassBackfill(r.Context(), s.db)
	if err != nil {
		s.logger.Error("run-procurement-class-backfill: failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "query_failed"})
		return
	}
	s.logger.Info("run-procurement-class-backfill: completed", "updated", updated)
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "updated": updated})
}
