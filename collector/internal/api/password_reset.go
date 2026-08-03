// password_reset.go — 비밀번호 찾기(2026-08-04). 이메일로 재설정 링크를
// 발송한다(기존 팀초대 이메일과 같은 Resend 인프라 재사용, company_team.go의
// 토큰+링크+notification_log 감사기록 패턴을 그대로 따른다). 소셜로그인
// 전용 계정(password_hash NULL)은 애초에 비밀번호가 없어 재설정 대상이
// 아니다.
//
// handleResetPasswordRequest는 "이 이메일이 실제로 존재하는지"를 응답으로
// 흘리지 않는다(표준적인 비밀번호 재설정 anti-enumeration 관행) — 계정이
// 없거나 소셜전용이어도 항상 동일한 200 응답을 반환하고 실제 발송 여부만
// 내부적으로 갈린다. find_email.go의 아이디 찾기는 반대로 "인증 후
// 표시한다"는 게 기능 스펙 자체라 계정 존재여부 노출이 목적이므로 이
// 원칙을 적용하지 않는다.
//
// 토큰은 평문 저장하지 않는다(token_hash = SHA-256, phone_verification.go의
// code_hash와 같은 이유 — 1시간 남짓의 짧은 수명이라 bcrypt는 과함).
// 1회용(used_at)이다.
package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const (
	passwordResetTokenValidity = time.Hour
	notifyEventPasswordReset   = "password_reset"
)

func generatePasswordResetToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func hashResetToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// authLookupRateLimited — find_email.go/password_reset.go 공용. OTP
// 챌린지가 없는 두 조회성 엔드포인트가 각각의 identifier(전화번호/이메일)
// 무차별 대입에 노출되지 않도록, phone_verification.go의
// phoneVerificationRateLimited와 같은 기준(같은 identifier 1분 1회, 1일
// 5회)을 재사용한다. kind로 두 기능의 시도 횟수를 분리해서 센다.
func authLookupRateLimited(ctx context.Context, db *sql.DB, kind, identifier string) (bool, error) {
	var recentCount, dailyCount int
	if err := db.QueryRowContext(ctx, `
		SELECT
			count(*) FILTER (WHERE created_at > now() - interval '1 minute'),
			count(*) FILTER (WHERE created_at > now() - interval '1 day')
		FROM auth_lookup_attempts WHERE kind = $1 AND identifier = $2`, kind, identifier,
	).Scan(&recentCount, &dailyCount); err != nil {
		return false, err
	}
	return recentCount > 0 || dailyCount >= 5, nil
}

type resetPasswordRequestRequest struct {
	Email string `json:"email"`
}

// handleResetPasswordRequest — POST /api/auth/reset-password-request.
func (s *Server) handleResetPasswordRequest(w http.ResponseWriter, r *http.Request) {
	var req resetPasswordRequestRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_body"})
		return
	}
	email := strings.TrimSpace(strings.ToLower(req.Email))
	if !isValidEmail(email) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_email"})
		return
	}

	ctx := r.Context()
	blocked, err := authLookupRateLimited(ctx, s.db, "reset_password", email)
	if err != nil {
		s.logger.Error("reset-password-request: rate limit check failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if blocked {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "rate_limited"})
		return
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO auth_lookup_attempts (kind, identifier) VALUES ('reset_password', $1)`, email); err != nil {
		s.logger.Error("reset-password-request: attempt log insert failed", "error", err)
	}

	// 아래 조회 결과와 무관하게 응답은 항상 이 함수 끝의 동일한 200
	// "ok"다(anti-enumeration) — 계정이 없거나(sql.ErrNoRows) 소셜전용
	// 계정이거나 탈퇴 처리됐어도 호출자 입장에서는 구분할 수 없다.
	var userID string
	var passwordHash sql.NullString
	var deactivatedAt sql.NullTime
	lookupErr := s.db.QueryRowContext(ctx,
		`SELECT id, password_hash, deactivated_at FROM users WHERE email = $1`, email,
	).Scan(&userID, &passwordHash, &deactivatedAt)
	if lookupErr != nil && lookupErr != sql.ErrNoRows {
		s.logger.Error("reset-password-request: query failed", "error", lookupErr)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	eligible := lookupErr == nil && passwordHash.Valid && !deactivatedAt.Valid

	if eligible && s.notify != nil {
		token, tokErr := generatePasswordResetToken()
		if tokErr != nil {
			s.logger.Error("reset-password-request: token generation failed", "error", tokErr)
			writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
			return
		}
		if _, err := s.db.ExecContext(ctx, `
			INSERT INTO password_reset_tokens (user_id, token_hash, expires_at)
			VALUES ($1, $2, $3)`,
			userID, hashResetToken(token), time.Now().Add(passwordResetTokenValidity),
		); err != nil {
			s.logger.Error("reset-password-request: insert failed", "error", err)
			writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
			return
		}

		resetLink := s.appBaseURL + "/#/auth/reset-password?token=" + token
		subject := "비밀번호 재설정 안내"
		body := fmt.Sprintf(
			"<p>비밀번호 재설정을 요청하셨습니다.</p><p>아래 링크를 클릭해 새 비밀번호를 설정해주세요.</p><p><a href=\"%s\">%s</a></p><p>이 링크는 1시간 동안 유효하며, 본인이 요청하지 않았다면 이 메일은 무시하셔도 됩니다.</p>",
			resetLink, resetLink,
		)
		sendErr := s.notify.Send(ctx, email, subject, body)
		status, errMsg := "sent", sql.NullString{}
		if sendErr != nil {
			status = "failed"
			errMsg = sql.NullString{String: sendErr.Error(), Valid: true}
			s.logger.Error("reset-password-request: send failed", "error", sendErr)
		}
		if _, logErr := s.db.ExecContext(ctx, `
			INSERT INTO notification_log (event_type, channel, recipient_email, user_id, subject, status, error_message)
			VALUES ($1,$2,$3,$4,$5,$6,$7)`,
			notifyEventPasswordReset, notifyChannelEmail, email, userID, subject, status, errMsg,
		); logErr != nil {
			s.logger.Error("reset-password-request: log insert failed", "error", logErr)
		}
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

type resetPasswordRequest struct {
	Token    string `json:"token"`
	Password string `json:"password"`
}

// handleResetPassword — POST /api/auth/reset-password. 토큰 검증(존재+
// 미사용+미만료) 후 새 비밀번호로 교체하고 토큰을 소모 처리한다. 세션은
// HMAC 서명 쿠키라 서버측 세션 테이블이 없어(auth.go 참고) 재설정해도
// 다른 기기에 이미 로그인돼있던 세션은 그대로 유효하다 — 이 프로젝트의
// 기존 아키텍처 한계이며 이번 범위에서 별도로 손대지 않는다.
func (s *Server) handleResetPassword(w http.ResponseWriter, r *http.Request) {
	var req resetPasswordRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_body"})
		return
	}
	token := strings.TrimSpace(req.Token)
	if token == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_body"})
		return
	}
	if len(req.Password) < 8 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "password_too_short"})
		return
	}

	ctx := r.Context()
	var tokenID, userID string
	var expiresAt time.Time
	var usedAt sql.NullTime
	err := s.db.QueryRowContext(ctx, `
		SELECT id, user_id, expires_at, used_at FROM password_reset_tokens
		WHERE token_hash = $1`, hashResetToken(token),
	).Scan(&tokenID, &userID, &expiresAt, &usedAt)
	if err == sql.ErrNoRows {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_token"})
		return
	}
	if err != nil {
		s.logger.Error("reset-password: query failed", "error", err)
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

	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		s.logger.Error("reset-password: hash failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal_error"})
		return
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		s.logger.Error("reset-password: begin tx failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE users SET password_hash = $1 WHERE id = $2`, string(hash), userID); err != nil {
		s.logger.Error("reset-password: update failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if _, err := tx.ExecContext(ctx, `UPDATE password_reset_tokens SET used_at = now() WHERE id = $1`, tokenID); err != nil {
		s.logger.Error("reset-password: mark used failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if err := tx.Commit(); err != nil {
		s.logger.Error("reset-password: commit failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
