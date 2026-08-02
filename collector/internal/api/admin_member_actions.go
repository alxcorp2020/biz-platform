// admin_member_actions.go — 관리자가 개별 회원에게 실행하는 상태 변경
// 작업: 탈퇴 처리(POST .../deactivate)와 이번달 AI 분석 한도 임시조정
// (PUT .../ai-limit). 조회 전용인 admin.go의 handleAdminGetMember와 달리
// 이 파일의 핸들러는 전부 데이터를 바꾸므로 audit_logs에 기록한다.
package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// handleAdminDeactivateMember — POST /api/admin/members/{id}/deactivate.
// 되돌릴 수 없다(프론트가 확인 모달로 이걸 명시). 계정을 실제로 DELETE하지
// 않는다 — payment_log/audit_logs 등이 이 users 행을 FK로 참조하고,
// 법적 보관기간을 고려해 유지해야 하기 때문. 대신:
//  1. email을 "deleted-{id}@deleted.local"로, password_hash/phone_number를
//     NULL로 바꿔 개인정보를 익명화한다(원래 이메일은 audit_logs.detail에만
//     남는다 — 감사 목적의 최소 보존).
//  2. deactivated_at을 찍어 handleLogin/handleOAuthCallback이 로그인을
//     막게 한다. user_oauth_identities 행은 일부러 안 지운다 — 지우면
//     resolveOAuthUser(oauth_login.go)가 그 사람을 "새 계정"으로 오인해
//     같은 소셜 계정으로 재가입을 허용해버려 탈퇴가 무력화된다.
//  3. company_members에서 제거해 팀 자리를 비운다(조직의 다른 데이터 —
//     company_profiles/company_documents/notice_pipeline_entries/
//     payment_log 등 — 는 전혀 건드리지 않는다).
//
// 조직(company_profiles)의 owner이고 다른 팀원이 남아있으면 거부한다
// (owner_has_other_members) — 오너가 없어지는 조직을 만들지 않기 위해
// 사용자가 명시적으로 선택한 정책. 그 경우 관리자가 먼저 팀을 정리(팀원
// 내보내기)한 뒤 다시 시도해야 한다. 전체 대상 일괄 처리는 이번 범위에서
// 제외했다(실수로 대량 탈퇴되는 리스크가 커서 — 필요하면 나중에 "장기
// 미접속 회원 목록"을 뽑아 하나씩 확인 후 처리하는 방식으로 검토).
func (s *Server) handleAdminDeactivateMember(w http.ResponseWriter, r *http.Request) {
	adminID, ok := s.requireSystemAdmin(w, r)
	if !ok {
		return
	}
	targetID := r.PathValue("id")
	ctx := r.Context()

	var targetEmail string
	var alreadyDeactivated sql.NullTime
	var companyProfileID, teamRole sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT u.email, u.deactivated_at, cm.company_profile_id, cm.role
		FROM users u
		LEFT JOIN company_members cm ON cm.user_id = u.id
		WHERE u.id = $1`, targetID,
	).Scan(&targetEmail, &alreadyDeactivated, &companyProfileID, &teamRole)
	if errors.Is(err, sql.ErrNoRows) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "member_not_found"})
		return
	}
	if err != nil {
		s.logger.Error("admin-deactivate-member: lookup failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if alreadyDeactivated.Valid {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "already_deactivated"})
		return
	}
	if teamRole.Valid && teamRole.String == "owner" {
		var otherCount int
		if err := s.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM company_members WHERE company_profile_id = $1 AND user_id != $2`,
			companyProfileID.String, targetID,
		).Scan(&otherCount); err != nil {
			s.logger.Error("admin-deactivate-member: other member count failed", "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
			return
		}
		if otherCount > 0 {
			writeJSON(w, http.StatusConflict, map[string]any{"error": "owner_has_other_members", "otherMemberCount": otherCount})
			return
		}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		s.logger.Error("admin-deactivate-member: begin tx failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	defer tx.Rollback()

	anonymizedEmail := fmt.Sprintf("deleted-%s@deleted.local", targetID)
	if _, err := tx.ExecContext(ctx, `
		UPDATE users SET email = $1, password_hash = NULL, phone_number = NULL, deactivated_at = now()
		WHERE id = $2`, anonymizedEmail, targetID,
	); err != nil {
		s.logger.Error("admin-deactivate-member: anonymize failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM company_members WHERE user_id = $1`, targetID); err != nil {
		s.logger.Error("admin-deactivate-member: remove membership failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if err := tx.Commit(); err != nil {
		s.logger.Error("admin-deactivate-member: commit failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}

	s.recordAuditLog(ctx, adminID, "admin_member_deactivated", "user", targetID, map[string]any{
		"originalEmail": targetEmail,
	})
	writeJSON(w, http.StatusOK, map[string]string{"status": "deactivated"})
}

type adminSetAIAnalysisLimitRequest struct {
	Limit  *int   `json:"limit"` // null → 오버라이드 해제
	Reason string `json:"reason"`
}

// handleAdminSetAIAnalysisLimit — PUT /api/admin/members/{id}/ai-limit.
// {id}는 회원 계정(users.id)이지만 한도 자체는 그 계정이 속한 조직
// (company_profiles)에 적용된다(AI 분석 한도가 원래 조직 단위이므로 —
// checkAIAnalysisQuota 참고). limit이 null이면 오버라이드를 해제해 플랜
// 기본값으로 복귀시키고, 값이 있으면 "이번 달"(time.Now())에만 적용되는
// 임시 한도로 저장한다 — 사용자가 확정한 정책대로 다음 달이 되면
// custom_ai_analysis_limit_month가 더 이상 이번 달과 안 맞아 자동으로
// 원복된다(effectiveAIAnalysisLimit, plan_settings.go). 값을 설정할 때는
// 사유가 필수(해제할 때는 불필요) — 나중에 왜 조정했는지 감사로그로 추적
// 가능하게.
func (s *Server) handleAdminSetAIAnalysisLimit(w http.ResponseWriter, r *http.Request) {
	adminID, ok := s.requireSystemAdmin(w, r)
	if !ok {
		return
	}
	targetUserID := r.PathValue("id")
	ctx := r.Context()

	var req adminSetAIAnalysisLimitRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_body"})
		return
	}

	var companyProfileID sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT company_profile_id FROM company_members WHERE user_id = $1`, targetUserID,
	).Scan(&companyProfileID)
	if errors.Is(err, sql.ErrNoRows) || !companyProfileID.Valid {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "company_profile_required"})
		return
	}
	if err != nil {
		s.logger.Error("admin-set-ai-limit: profile lookup failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	profileID := companyProfileID.String

	var oldLimit sql.NullInt64
	var oldMonth sql.NullString
	if err := s.db.QueryRowContext(ctx,
		`SELECT custom_ai_analysis_limit, custom_ai_analysis_limit_month FROM company_profiles WHERE id = $1`, profileID,
	).Scan(&oldLimit, &oldMonth); err != nil {
		s.logger.Error("admin-set-ai-limit: previous value lookup failed", "error", err)
	}
	var previousLimit, previousMonth any
	if oldLimit.Valid {
		previousLimit = oldLimit.Int64
	}
	if oldMonth.Valid {
		previousMonth = oldMonth.String
	}

	if req.Limit == nil {
		if _, err := s.db.ExecContext(ctx, `
			UPDATE company_profiles
			SET custom_ai_analysis_limit = NULL, custom_ai_analysis_limit_month = NULL, custom_ai_analysis_limit_reason = NULL
			WHERE id = $1`, profileID,
		); err != nil {
			s.logger.Error("admin-set-ai-limit: clear failed", "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
			return
		}
		s.recordAuditLog(ctx, adminID, "admin_ai_limit_cleared", "company_profile", profileID, map[string]any{
			"previousLimit": previousLimit,
			"previousMonth": previousMonth,
		})
		writeJSON(w, http.StatusOK, map[string]string{"status": "cleared"})
		return
	}

	if *req.Limit < -1 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_value"})
		return
	}
	reason := strings.TrimSpace(req.Reason)
	if reason == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "reason_required"})
		return
	}
	month := time.Now().Format("2006-01")
	if _, err := s.db.ExecContext(ctx, `
		UPDATE company_profiles
		SET custom_ai_analysis_limit = $1, custom_ai_analysis_limit_month = $2, custom_ai_analysis_limit_reason = $3
		WHERE id = $4`, *req.Limit, month, reason, profileID,
	); err != nil {
		s.logger.Error("admin-set-ai-limit: update failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	s.recordAuditLog(ctx, adminID, "admin_ai_limit_set", "company_profile", profileID, map[string]any{
		"newLimit":      *req.Limit,
		"month":         month,
		"reason":        reason,
		"previousLimit": previousLimit,
		"previousMonth": previousMonth,
	})
	writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}
