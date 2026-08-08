// pipeline_auto_transition.go — 파이프라인 상태 자동전환. 사용자가 계속
// 미루거나 놓친 건이 "검토전/참여검토/승인대기/준비중"(진행 중) 상태로
// 영원히 남아 목록을 어지럽히지 않도록, 명백히 더 이상 진행할 수 없게 된
// 건을 자동으로 "제외"로 옮긴다. 판정 기준은 이미 시스템에 있는 신호
// 2가지만 쓴다(새 추측성 로직 없음):
//   1. 제출마감일이 지났는데 아직 제출완료로 넘어가지 못한 경우
//   2. 공고 자체가 취소된 경우(g2b.go가 ntceKindNm=="취소공고"를
//      notices.status='cancelled'로 반영해줌)
// 🚨 2026-08-06: 아래 autoExcludeNoticeClosed의 IN 목록에 있는 'closed'는
// 사실상 도달 불가능한 조건이다 — g2b/bizinfo 수집기는 마감일이 지나도
// notices.status를 절대 'closed'로 바꾸지 않는다(의도된 설계: 마감
// 여부는 항상 application_end_at을 조회 시점에 비교해서 판단하고,
// status는 'open'/'reannounced'/'cancelled'만 실제로 쓰인다 —
// bizinfo.go/g2b.go의 Normalize 참고). 그래도 문제가 되지 않는 이유는
// 바로 위 1번(autoExcludeDeadlinePassed)이 이미 날짜 기준으로 마감경과를
// 정확히 처리하고 있어서다 — 이 함수는 "취소"만 실질적으로 담당한다.
// 두 경우 모두 memo에 사유와 날짜를 남겨, 사용자가 "왜 자동으로
// 바뀌었는지" 나중에 확인할 수 있게 한다 — 조용히 상태만 바꾸지 않는다.
//
// RunDailyNotifications과 같은 매일 1회 배치 성격이라 cmd/apiserver의
// 같은 일일 티커에서 이 함수를 먼저 호출한 뒤 알림을 보낸다(그래야 오늘
// 막 자동 제외된 건이 그 사이에 한 번 더 마감 리마인더를 받는 낭비가
// 없다).
package api

import (
	"context"
	"net/http"
)

const autoExcludeStatus = "제외"

// 자동전환 대상 상태는 SQL의 IN 목록에 직접 나열한다(dashboard.go의
// pipelineActiveStatuses와 정확히 같은 4개: 검토전/참여검토/승인대기/준비중
// — 제출완료 이후(낙찰/탈락 등)는 이미 결론이 났으므로 대상이 아니다).

// RunPipelineAutoTransitions applies both auto-exclude rules and returns the
// number of entries changed by each — cmd/apiserver's daily ticker and the
// admin manual-trigger endpoint both call this and log/report the counts.
func (s *Server) RunPipelineAutoTransitions(ctx context.Context) (deadlinePassed, noticeClosed int, err error) {
	deadlinePassed, err = s.autoExcludeDeadlinePassed(ctx)
	if err != nil {
		return deadlinePassed, 0, err
	}
	noticeClosed, err = s.autoExcludeNoticeClosed(ctx)
	return deadlinePassed, noticeClosed, err
}

func (s *Server) autoExcludeDeadlinePassed(ctx context.Context) (int, error) {
	rows, err := s.db.QueryContext(ctx, `
		UPDATE notice_pipeline_entries
		SET status = $1,
		    decided_at = now(),
		    updated_at = now(),
		    memo = COALESCE(memo || E'\n', '') ||
		           '(자동) 제출마감(' || submission_deadline || ') 경과로 자동 제외 처리됨 — ' || to_char(now(), 'YYYY-MM-DD')
		WHERE status IN ('검토중','준비중')
		  AND submission_deadline IS NOT NULL
		  AND submission_deadline < CURRENT_DATE
		RETURNING id`, autoExcludeStatus)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		n++
	}
	return n, rows.Err()
}

// autoExcludeNoticeClosed — 이름과 달리 실질적으로는 "취소된 공고"만
// 잡는다. IN 목록의 'closed'는 실제로 도달 불가능하다(파일 상단 주석
// 참고) — 이름은 과거 설계 흔적으로 그대로 남겨둔다(동작에는 영향 없음).
func (s *Server) autoExcludeNoticeClosed(ctx context.Context) (int, error) {
	rows, err := s.db.QueryContext(ctx, `
		UPDATE notice_pipeline_entries pe
		SET status = $1,
		    decided_at = now(),
		    updated_at = now(),
		    memo = COALESCE(pe.memo || E'\n', '') ||
		           '(자동) 공고가 마감/취소되어 자동 제외 처리됨 — ' || to_char(now(), 'YYYY-MM-DD')
		FROM notices n
		WHERE pe.notice_id = n.id
		  AND pe.status IN ('검토중','준비중')
		  AND n.status IN ('closed','cancelled')
		RETURNING pe.id`, autoExcludeStatus)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		n++
	}
	return n, rows.Err()
}

// handleRunPipelineAutoTransitions manually fires the auto-exclude batch on
// demand — same system_admin-only pattern as handleRunNotifications. The
// only other trigger is the daily ticker in cmd/apiserver, right before the
// notification batch.
func (s *Server) handleRunPipelineAutoTransitions(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.currentUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	role, err := s.userRole(r.Context(), userID)
	if err != nil {
		s.logger.Error("run-pipeline-auto-transitions: role lookup failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if role != "system_admin" {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
		return
	}

	deadlinePassed, noticeClosed, err := s.RunPipelineAutoTransitions(r.Context())
	if err != nil {
		s.logger.Error("run-pipeline-auto-transitions: batch failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "completed", "deadlinePassedCount": deadlinePassed, "noticeClosedCount": noticeClosed,
	})
}
