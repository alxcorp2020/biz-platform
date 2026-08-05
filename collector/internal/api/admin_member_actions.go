// admin_member_actions.go — 관리자가 개별 회원에게 실행하는 상태 변경
// 작업: 탈퇴 처리(POST .../deactivate)와 이번달 AI 분석 한도 임시조정
// (PUT .../ai-limit). 조회 전용인 admin.go의 handleAdminGetMember와 달리
// 이 파일의 핸들러는 전부 데이터를 바꾸므로 audit_logs에 기록한다.
package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// deactivateOutcome — deactivateUserAccount의 결과. Blocked가 비어있지
// 않으면 아무것도 바뀌지 않은 것이고(호출부가 그 사유로 4xx를 내림),
// 비어있으면 실제로 탈퇴 처리가 커밋된 것이다.
type deactivateOutcome struct {
	Blocked          string // "" | "already_deactivated" | "owner_has_other_members"
	OtherMemberCount int
	OriginalEmail    string
}

// deactivateUserAccount — 관리자 탈퇴 처리(handleAdminDeactivateMember)와
// 본인 셀프 탈퇴(account_settings.go의 handleSelfDeactivateAccount)가
// 공유하는 핵심 로직. 되돌릴 수 없다(양쪽 다 프론트가 확인 모달로 이걸
// 명시). 계정을 실제로 DELETE하지 않는다 — payment_log/audit_logs 등이
// 이 users 행을 FK로 참조하고, 법적 보관기간을 고려해 유지해야 하기
// 때문. 대신:
//  1. email을 "deleted-{id}@deleted.local"로, password_hash/phone_number를
//     NULL로 바꿔 개인정보를 익명화한다(원래 이메일은 호출부가 audit_logs.
//     detail에 남긴다 — 감사 목적의 최소 보존).
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
// 사용자가 명시적으로 선택한 정책. 그 경우 먼저 팀을 정리(팀원 내보내기
// 또는 소유권 이전)한 뒤 다시 시도해야 한다.
func (s *Server) deactivateUserAccount(ctx context.Context, targetID string) (deactivateOutcome, error) {
	var out deactivateOutcome
	var alreadyDeactivated sql.NullTime
	var companyProfileID, teamRole sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT u.email, u.deactivated_at, cm.company_profile_id, cm.role
		FROM users u
		LEFT JOIN company_members cm ON cm.user_id = u.id
		WHERE u.id = $1`, targetID,
	).Scan(&out.OriginalEmail, &alreadyDeactivated, &companyProfileID, &teamRole)
	if err != nil {
		return out, err
	}
	if alreadyDeactivated.Valid {
		out.Blocked = "already_deactivated"
		return out, nil
	}
	if teamRole.Valid && teamRole.String == "owner" {
		var otherCount int
		if err := s.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM company_members WHERE company_profile_id = $1 AND user_id != $2`,
			companyProfileID.String, targetID,
		).Scan(&otherCount); err != nil {
			return out, err
		}
		if otherCount > 0 {
			out.Blocked = "owner_has_other_members"
			out.OtherMemberCount = otherCount
			return out, nil
		}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()

	anonymizedEmail := fmt.Sprintf("deleted-%s@deleted.local", targetID)
	if _, err := tx.ExecContext(ctx, `
		UPDATE users SET email = $1, password_hash = NULL, phone_number = NULL, deactivated_at = now()
		WHERE id = $2`, anonymizedEmail, targetID,
	); err != nil {
		return out, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM company_members WHERE user_id = $1`, targetID); err != nil {
		return out, err
	}
	if err := tx.Commit(); err != nil {
		return out, err
	}
	return out, nil
}

// handleAdminDeactivateMember — POST /api/admin/members/{id}/deactivate.
// 전체 대상 일괄 처리는 이번 범위에서 제외했다(실수로 대량 탈퇴되는
// 리스크가 커서 — 필요하면 나중에 "장기 미접속 회원 목록"을 뽑아 하나씩
// 확인 후 처리하는 방식으로 검토).
func (s *Server) handleAdminDeactivateMember(w http.ResponseWriter, r *http.Request) {
	adminID, ok := s.requireSystemAdmin(w, r)
	if !ok {
		return
	}
	targetID := r.PathValue("id")
	ctx := r.Context()

	outcome, err := s.deactivateUserAccount(ctx, targetID)
	if errors.Is(err, sql.ErrNoRows) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "member_not_found"})
		return
	}
	if err != nil {
		s.logger.Error("admin-deactivate-member: failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	switch outcome.Blocked {
	case "already_deactivated":
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "already_deactivated"})
		return
	case "owner_has_other_members":
		writeJSON(w, http.StatusConflict, map[string]any{"error": "owner_has_other_members", "otherMemberCount": outcome.OtherMemberCount})
		return
	}

	s.recordAuditLog(ctx, adminID, "admin_member_deactivated", "user", targetID, map[string]any{
		"originalEmail": outcome.OriginalEmail,
	})
	writeJSON(w, http.StatusOK, map[string]string{"status": "deactivated"})
}

// handleAdminDeleteMember — DELETE /api/admin/members/{id}. 개발 단계에서
// 반복 생성한 테스트 계정을 흔적 없이 지우기 위한 완전 삭제(하드 삭제).
// 위 탈퇴 처리(익명화, 데이터는 보존)와 달리 이 계정과 — owner인 경우 —
// 그가 소유한 회사(company_profiles)에 딸린 모든 데이터(문서/파이프라인/
// 구독/결제기록 등)를 실제로 DELETE한다. 되돌릴 방법이 없으므로 운영 중인
// 실사용자 계정에는 쓰지 말 것(법적 보관 의무가 있는 결제기록까지 지워짐
// — 그런 경우는 위 탈퇴 처리를 쓴다).
//
// 다른 팀원이 남아있는 조직의 owner는 삭제를 거부한다(owner_has_other_members,
// 탈퇴 처리와 동일한 정책) — 하드 삭제는 회사 데이터를 통째로 없애버리므로
// 다른 팀원이 남아있으면 그들의 접근 권한이 예고 없이 사라지는 훨씬 위험한
// 상황이 된다. 관리자 자기 자신은 삭제할 수 없다(복구 수단이 없어짐).
func (s *Server) handleAdminDeleteMember(w http.ResponseWriter, r *http.Request) {
	adminID, ok := s.requireSystemAdmin(w, r)
	if !ok {
		return
	}
	targetID := r.PathValue("id")
	if targetID == adminID {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "cannot_delete_self"})
		return
	}
	ctx := r.Context()

	var targetEmail string
	var companyProfileID, teamRole sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT u.email, cm.company_profile_id, cm.role
		FROM users u
		LEFT JOIN company_members cm ON cm.user_id = u.id
		WHERE u.id = $1`, targetID,
	).Scan(&targetEmail, &companyProfileID, &teamRole)
	if errors.Is(err, sql.ErrNoRows) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "member_not_found"})
		return
	}
	if err != nil {
		s.logger.Error("admin-delete-member: lookup failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}

	deleteWholeCompany := false
	if teamRole.Valid && teamRole.String == "owner" {
		var otherCount int
		if err := s.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM company_members WHERE company_profile_id = $1 AND user_id != $2`,
			companyProfileID.String, targetID,
		).Scan(&otherCount); err != nil {
			s.logger.Error("admin-delete-member: other member count failed", "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
			return
		}
		if otherCount > 0 {
			writeJSON(w, http.StatusConflict, map[string]any{"error": "owner_has_other_members", "otherMemberCount": otherCount})
			return
		}
		deleteWholeCompany = true
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		s.logger.Error("admin-delete-member: begin tx failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	defer tx.Rollback()

	// company_profiles.employee_count_source_document_id가 company_documents를
	// 가리키고, company_documents.company_profile_id가 다시 company_profiles를
	// 가리키는 순환 참조가 있어 — company_documents를 지우려면 이 컬럼을
	// 먼저 NULL로 끊어야 한다.
	if deleteWholeCompany {
		profileID := companyProfileID.String
		cascadeStmts := []struct {
			query string
			args  []any
		}{
			{`UPDATE company_profiles SET employee_count_source_document_id = NULL WHERE id = $1`, []any{profileID}},
			{`DELETE FROM pipeline_checklist_items WHERE pipeline_entry_id IN (SELECT id FROM notice_pipeline_entries WHERE company_profile_id = $1)`, []any{profileID}},
			{`DELETE FROM notification_log WHERE pipeline_entry_id IN (SELECT id FROM notice_pipeline_entries WHERE company_profile_id = $1)`, []any{profileID}},
			{`DELETE FROM notification_log WHERE contact_id IN (SELECT id FROM company_contacts WHERE company_profile_id = $1)`, []any{profileID}},
			{`DELETE FROM in_app_notifications WHERE company_profile_id = $1`, []any{profileID}},
			{`DELETE FROM notice_pipeline_entries WHERE company_profile_id = $1`, []any{profileID}},
			{`DELETE FROM company_contacts WHERE company_profile_id = $1`, []any{profileID}},
			{`DELETE FROM eligibility_evaluations WHERE company_profile_id = $1`, []any{profileID}},
			{`DELETE FROM document_checklist_items WHERE company_profile_id = $1`, []any{profileID}},
			{`DELETE FROM company_licenses WHERE company_profile_id = $1`, []any{profileID}},
			{`DELETE FROM company_certifications WHERE company_profile_id = $1`, []any{profileID}},
			{`DELETE FROM company_financials WHERE company_profile_id = $1`, []any{profileID}},
			{`DELETE FROM company_track_records WHERE company_profile_id = $1`, []any{profileID}},
			{`DELETE FROM company_intellectual_property WHERE company_profile_id = $1`, []any{profileID}},
			{`DELETE FROM company_personnel WHERE company_profile_id = $1`, []any{profileID}},
			{`DELETE FROM company_documents WHERE company_profile_id = $1`, []any{profileID}},
			{`DELETE FROM company_invitations WHERE company_profile_id = $1`, []any{profileID}},
			{`DELETE FROM reports WHERE company_profile_id = $1`, []any{profileID}},
			{`DELETE FROM payment_log WHERE subscription_id IN (SELECT id FROM subscriptions WHERE company_profile_id = $1)`, []any{profileID}},
			{`DELETE FROM subscriptions WHERE company_profile_id = $1`, []any{profileID}},
			{`DELETE FROM company_members WHERE company_profile_id = $1`, []any{profileID}},
			{`DELETE FROM company_profiles WHERE id = $1`, []any{profileID}},
		}
		for _, stmt := range cascadeStmts {
			if _, err := tx.ExecContext(ctx, stmt.query, stmt.args...); err != nil {
				s.logger.Error("admin-delete-member: company cascade delete failed", "query", stmt.query, "error", err)
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
				return
			}
		}
	}

	// 회사 소유 여부와 무관하게, 이 회원 개인에게 달린 데이터 정리.
	// user_oauth_identities는 ON DELETE CASCADE라 users 삭제 시 자동 정리됨.
	//
	// ⚠️ 2026-08-04 실제 500 에러 원인: password_reset_tokens/
	// email_verification_tokens가 이 목록에 빠져 있었다 — 이메일 가입 시
	// 필수 이메일 인증(email_verification.go)이 모든 이메일 가입 계정에
	// email_verification_tokens 행을 남기고, 비밀번호 찾기를 한 번이라도
	// 쓴 계정은 password_reset_tokens 행도 남는다. 둘 다 users(id)를
	// NOT NULL FK로 참조하고 ON DELETE CASCADE가 아니라서, 이 목록에
	// 없으면 DELETE FROM users에서 그대로 FK 위반 500이 난다(실제 로컬
	// 재현: "violates foreign key constraint password_reset_tokens_user_id_fkey").
	// broadcast_messages.created_by도 같은 이유로 추가 — 이 화면은
	// role 필터 없이 모든 계정을 보여주므로 관리자/운영자 계정도 삭제
	// 대상이 될 수 있고, 그 계정이 공지/배너를 작성한 적 있으면 똑같이
	// 걸린다. 앞으로 users(id)를 참조하는 새 테이블을 추가하면 이
	// 목록에도 반드시 추가할 것 — `grep -n "REFERENCES users(id)"
	// db/migrations/001_init.sql collector/internal/migrate/migrate.go`로
	// 전체 목록을 다시 확인.
	userScopedStmts := []string{
		`DELETE FROM company_members WHERE user_id = $1`,
		`DELETE FROM notification_log WHERE user_id = $1`,
		`DELETE FROM in_app_notifications WHERE user_id = $1`,
		`DELETE FROM notice_bookmarks WHERE user_id = $1`,
		`DELETE FROM credit_usage_log WHERE user_id = $1`,
		`DELETE FROM analysis_credits WHERE user_id = $1`,
		`DELETE FROM push_subscriptions WHERE user_id = $1`,
		`DELETE FROM terms_agreements WHERE user_id = $1`,
		`DELETE FROM audit_logs WHERE actor_user_id = $1`,
		`DELETE FROM company_invitations WHERE invited_by_user_id = $1`,
		`DELETE FROM password_reset_tokens WHERE user_id = $1`,
		`DELETE FROM email_verification_tokens WHERE user_id = $1`,
		`DELETE FROM broadcast_messages WHERE created_by = $1`,
	}
	for _, q := range userScopedStmts {
		if _, err := tx.ExecContext(ctx, q, targetID); err != nil {
			s.logger.Error("admin-delete-member: user-scoped delete failed", "query", q, "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
			return
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM users WHERE id = $1`, targetID); err != nil {
		s.logger.Error("admin-delete-member: user delete failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if err := tx.Commit(); err != nil {
		s.logger.Error("admin-delete-member: commit failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}

	// audit_logs.target_id는 FK가 아니라 TEXT라, 방금 지운 targetID를 그대로
	// 남겨도 참조 무결성 문제가 없다(actor_user_id만 실제 FK이고 이건
	// 여전히 존재하는 adminID를 가리킨다).
	s.recordAuditLog(ctx, adminID, "admin_member_deleted", "user", targetID, map[string]any{
		"email":             targetEmail,
		"deletedCompanyToo": deleteWholeCompany,
	})
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
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
