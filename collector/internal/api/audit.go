// audit.go — audit_logs(감사 로그) 기록 공용 헬퍼. 이 테이블은 초기
// 스키마(db/migrations/001_init.sql "16.3")부터 존재했지만 실제로 INSERT하는
// 코드가 지금까지 하나도 없었다 — 원클릭 참여검토(Phase 1)의 "참여 검토 시작
// 전후 상태를 감사로그에서 확인할 수 있다" 요구사항을 계기로 처음 사용을
// 시작한다. 기록 실패는 본 요청을 절대 막지 않는다(로그 실패로 사용자
// 작업이 실패하면 안 됨) — 에러만 남기고 계속 진행한다.
package api

import (
	"context"
	"encoding/json"
)

func (s *Server) recordAuditLog(ctx context.Context, actorUserID, action, targetType, targetID string, detail map[string]any) {
	var detailJSON []byte
	if detail != nil {
		var err error
		detailJSON, err = json.Marshal(detail)
		if err != nil {
			s.logger.Error("audit: detail marshal failed", "action", action, "error", err)
			detailJSON = nil
		}
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO audit_logs (actor_user_id, action, target_type, target_id, detail)
		VALUES ($1, $2, $3, $4, $5)`,
		actorUserID, action, targetType, targetID, detailJSON,
	); err != nil {
		s.logger.Error("audit: insert failed", "action", action, "error", err)
	}
}
