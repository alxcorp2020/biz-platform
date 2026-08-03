// email_verification.go — 이메일 가입 필수 이메일 인증(2026-08-04). 계정은
// 가입 즉시 생성되고 로그인도 바로 되지만(auth.go의 handleSignup), 이메일
// 인증 링크를 클릭하기 전까지는 프론트(route())가 대부분의 화면 접근을
// 막고 #/auth/verify-email-pending으로 돌려보낸다. 소셜 로그인(구글/네이버/
// 카카오)은 제공자가 이미 검증한 이메일이라 이 게이트를 아예 안 거친다
// (oauth_login.go의 resolveOAuthUser가 신규 계정 생성 시 email_verified_at을
// 바로 채운다).
//
// 토큰은 password_reset.go와 완전히 같은 패턴(generateSecureToken/hashToken
// 공용, SHA-256 해시만 저장, 1회용) — 유효기간만 24시간으로 더 길게 잡았다
// (비밀번호 재설정과 달리 급하지 않고, 메일을 늦게 확인하는 사용자를 감안).
package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

const (
	emailVerificationTokenValidity = 24 * time.Hour
	notifyEventEmailVerification   = "email_verification"
)

// sendEmailVerificationEmail generates a token, stores its hash, and sends
// the verification email. 발송 실패는 로그만 남기고 호출부(가입/재발송)의
// 성공 응답을 막지 않는다 — 계정은 이미 만들어졌고, 재발송 버튼으로
// 언제든 다시 시도할 수 있다(company_team.go 팀초대 이메일과 같은 원칙).
func (s *Server) sendEmailVerificationEmail(ctx context.Context, userID, email string) error {
	token, err := generateSecureToken()
	if err != nil {
		return fmt.Errorf("generate token: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO email_verification_tokens (user_id, token_hash, expires_at)
		VALUES ($1, $2, $3)`,
		userID, hashToken(token), time.Now().Add(emailVerificationTokenValidity),
	); err != nil {
		return fmt.Errorf("insert token: %w", err)
	}

	verifyLink := s.appBaseURL + "/#/auth/verify-email?token=" + token
	subject := "이메일 인증을 완료해주세요"
	body := fmt.Sprintf(
		"<p>가입해주셔서 감사합니다.</p><p>아래 링크를 클릭해 이메일 인증을 완료해주세요.</p><p><a href=\"%s\">%s</a></p><p>이 링크는 24시간 동안 유효합니다. 본인이 가입하지 않았다면 이 메일은 무시하셔도 됩니다.</p>",
		verifyLink, verifyLink,
	)
	var sendErr error
	if s.notify != nil {
		sendErr = s.notify.Send(ctx, email, subject, body)
	}
	status, errMsg := "sent", sql.NullString{}
	if sendErr != nil {
		status = "failed"
		errMsg = sql.NullString{String: sendErr.Error(), Valid: true}
	}
	if _, logErr := s.db.ExecContext(ctx, `
		INSERT INTO notification_log (event_type, channel, recipient_email, user_id, subject, status, error_message)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		notifyEventEmailVerification, notifyChannelEmail, email, userID, subject, status, errMsg,
	); logErr != nil {
		s.logger.Error("email-verification: log insert failed", "error", logErr)
	}
	return sendErr
}

type verifyEmailRequest struct {
	Token string `json:"token"`
}

// handleVerifyEmail — POST /api/auth/verify-email(공개, 로그인 불필요 —
// 가입할 때와 다른 브라우저/기기에서 메일 링크를 열 수도 있다).
func (s *Server) handleVerifyEmail(w http.ResponseWriter, r *http.Request) {
	var req verifyEmailRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_body"})
		return
	}
	token := strings.TrimSpace(req.Token)
	if token == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_body"})
		return
	}

	ctx := r.Context()
	var tokenID, userID string
	var expiresAt time.Time
	var usedAt sql.NullTime
	err := s.db.QueryRowContext(ctx, `
		SELECT id, user_id, expires_at, used_at FROM email_verification_tokens
		WHERE token_hash = $1`, hashToken(token),
	).Scan(&tokenID, &userID, &expiresAt, &usedAt)
	if err == sql.ErrNoRows {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_token"})
		return
	}
	if err != nil {
		s.logger.Error("verify-email: query failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if usedAt.Valid {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "token_already_used"})
		return
	}
	if time.Now().After(expiresAt) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "token_expired"})
		return
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		s.logger.Error("verify-email: begin tx failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE users SET email_verified_at = now() WHERE id = $1 AND email_verified_at IS NULL`, userID); err != nil {
		s.logger.Error("verify-email: update failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if _, err := tx.ExecContext(ctx, `UPDATE email_verification_tokens SET used_at = now() WHERE id = $1`, tokenID); err != nil {
		s.logger.Error("verify-email: mark used failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if err := tx.Commit(); err != nil {
		s.logger.Error("verify-email: commit failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleResendVerificationEmail — POST /api/me/resend-verification-email
// (로그인 필요 — 가입 직후 자기 계정 인증 대기 화면에서만 쓰인다).
func (s *Server) handleResendVerificationEmail(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.currentUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	ctx := r.Context()
	var email string
	var emailVerifiedAt sql.NullTime
	if err := s.db.QueryRowContext(ctx, `SELECT email, email_verified_at FROM users WHERE id = $1`, userID).Scan(&email, &emailVerifiedAt); err != nil {
		s.logger.Error("resend-verification-email: query failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if emailVerifiedAt.Valid {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "already_verified"})
		return
	}

	blocked, err := authLookupRateLimited(ctx, s.db, "email_verify_resend", userID)
	if err != nil {
		s.logger.Error("resend-verification-email: rate limit check failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if blocked {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "rate_limited"})
		return
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO auth_lookup_attempts (kind, identifier) VALUES ('email_verify_resend', $1)`, userID); err != nil {
		s.logger.Error("resend-verification-email: attempt log insert failed", "error", err)
	}

	if err := s.sendEmailVerificationEmail(ctx, userID, email); err != nil {
		s.logger.Error("resend-verification-email: send failed", "error", err)
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
