// account_settings.go — 회원 본인 계정 설정 화면(#/me/account) 전용
// 엔드포인트: 조회(이메일/휴대폰/비밀번호 보유 여부/연결된 소셜 계정),
// 휴대폰번호 수정, 비밀번호 변경(로그인 상태에서 현재 비밀번호 확인 후
// 변경 — password_reset.go의 "비밀번호를 잊어버렸을 때" 흐름과는 다른
// 별개 엔드포인트), 본인 셀프 탈퇴. 이메일 자체는 조회만 가능하다(변경은
// 본인인증이 추가로 필요할 수 있어 이번 범위 밖 — 문의 안내로 대체).
package api

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

// handleGetAccountSettings — GET /api/me/account.
func (s *Server) handleGetAccountSettings(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.currentUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	ctx := r.Context()

	var email string
	var phoneNumber sql.NullString
	var passwordHash sql.NullString
	if err := s.db.QueryRowContext(ctx,
		`SELECT email, phone_number, password_hash FROM users WHERE id = $1`, userID,
	).Scan(&email, &phoneNumber, &passwordHash); err != nil {
		s.logger.Error("get-account-settings: query failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT provider FROM user_oauth_identities WHERE user_id = $1 ORDER BY provider`, userID)
	if err != nil {
		s.logger.Error("get-account-settings: oauth identities query failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	defer rows.Close()
	connectedProviders := []string{}
	for rows.Next() {
		var provider string
		if err := rows.Scan(&provider); err != nil {
			continue
		}
		connectedProviders = append(connectedProviders, provider)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"email":              email,
		"phoneNumber":        nullStringPtr(phoneNumber),
		"hasPassword":        passwordHash.Valid,
		"connectedProviders": connectedProviders,
	})
}

type updateAccountPhoneRequest struct {
	PhoneNumber string `json:"phoneNumber"`
}

// handleUpdateAccountPhone — PATCH /api/me/account. 본인 휴대폰번호만
// 수정한다(이메일은 조회 전용, 이 엔드포인트에서 안 받음). signup_agreement.go
// 의 phoneNumberPattern과 동일한 형식 검증을 그대로 재사용 — 두 군데
// 형식 기준이 어긋나는 사고를 막기 위해 별도 정규식을 새로 안 만든다.
func (s *Server) handleUpdateAccountPhone(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.currentUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	var req updateAccountPhoneRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_body"})
		return
	}
	phone := strings.TrimSpace(req.PhoneNumber)
	if !phoneNumberPattern.MatchString(phone) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_phone_number"})
		return
	}

	if _, err := s.db.ExecContext(r.Context(),
		`UPDATE users SET phone_number = $1 WHERE id = $2`, phone, userID,
	); err != nil {
		s.logger.Error("update-account-phone: update failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"phoneNumber": phone})
}

type changePasswordRequest struct {
	CurrentPassword string `json:"currentPassword"`
	NewPassword     string `json:"newPassword"`
}

// handleChangePassword — POST /api/me/account/change-password. 로그인
// 상태에서 본인이 현재 비밀번호를 확인시켜야 바꿀 수 있다(분실 시
// 재설정 흐름인 password_reset.go와는 별개 — 그쪽은 이메일 링크 토큰만
// 확인하고 현재 비밀번호를 모른다는 전제).
func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.currentUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	var req changePasswordRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_body"})
		return
	}
	if len(req.NewPassword) < 8 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "password_too_short"})
		return
	}

	ctx := r.Context()
	var passwordHash sql.NullString
	if err := s.db.QueryRowContext(ctx,
		`SELECT password_hash FROM users WHERE id = $1`, userID,
	).Scan(&passwordHash); err != nil {
		s.logger.Error("change-password: lookup failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	// 간편로그인 전용 계정(비밀번호 자체가 없음) — 프론트가 이 경우 폼
	// 자체를 안 보여주지만, 우회 호출 방어로 서버도 동일하게 막는다.
	if !passwordHash.Valid {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "social_login_only"})
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(passwordHash.String), []byte(req.CurrentPassword)) != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_current_password"})
		return
	}

	newHash, err := bcrypt.GenerateFromPassword([]byte(req.NewPassword), bcrypt.DefaultCost)
	if err != nil {
		s.logger.Error("change-password: hash failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal_error"})
		return
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE users SET password_hash = $1 WHERE id = $2`, string(newHash), userID,
	); err != nil {
		s.logger.Error("change-password: update failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "changed"})
}

// handleSelfDeactivateAccount — POST /api/me/account/deactivate. 지금까지
// admin_member_actions.go의 handleAdminDeactivateMember(관리자 전용)만
// 있던 탈퇴 처리를, 본인이 직접 신청하는 셀프 탈퇴 경로로도 열어준다 —
// 핵심 로직(익명화+deactivated_at+팀 이탈)은 deactivateUserAccount 공용
// 헬퍼를 그대로 재사용, 대상만 URL 파라미터의 다른 사람이 아니라 본인
// (currentUserID)이다. 관리자 경로와 달리 성공 시 즉시 본인 세션 쿠키도
// 지운다 — 세션이 stateless 쿠키라(서버측 세션 테이블 없음) 그대로 두면
// 쿠키 만료(7일) 전까지 인증 API를 계속 호출할 수 있어버린다.
func (s *Server) handleSelfDeactivateAccount(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.currentUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	ctx := r.Context()

	outcome, err := s.deactivateUserAccount(ctx, userID)
	if err != nil {
		s.logger.Error("self-deactivate-account: failed", "error", err)
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

	s.recordAuditLog(ctx, userID, "self_account_deactivated", "user", userID, map[string]any{
		"originalEmail": outcome.OriginalEmail,
	})
	s.clearSessionCookie(w)
	writeJSON(w, http.StatusOK, map[string]string{"status": "deactivated"})
}
