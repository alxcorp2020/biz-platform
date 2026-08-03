// phone_verification.go — 회원가입/온보딩 휴대폰번호 SMS 인증(2026-08-03
// 온보딩 재설계, Phase 1). 계정이 아직 없는 상태(이메일 가입은 계정
// 생성 자체가 이 인증 이후에 일어난다)에서도 동작해야 해서 두 엔드포인트
// 모두 로그인 불필요(공개 API)다 — phone_number 문자열 자체로 조회한다.
//
// 코드는 평문으로 저장하지 않는다(code_hash = SHA-256) — 5분 남짓의
// 짧은 수명이라 bcrypt 같은 느린 해시는 과함. 틀린 코드를 5회 누적
// 입력하면 그 코드는 더 못 쓰게 막고 재발송을 유도한다(무차별 대입 방지
// — 6자리 숫자는 100만 경우의 수뿐이라 시도 횟수 제한이 필수).
//
// 재발송 남용 방지: 같은 번호로 1분에 1회, 1일 5회로 제한한다(Aligo가
// 건당 과금이라 — notifications.go의 smsAllowedForPlan과 같은 이유).
// 로그인 전 단계라 조직/플랜 단위로 막을 방법이 없어 전화번호 자체를
// 기준으로 막는다.
package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"time"
)

const (
	phoneVerificationCodeTTL     = 5 * time.Minute
	phoneVerificationMaxAttempts = 5
	// consumeVerifiedPhone이 "최근에 인증됐다"고 인정하는 유효기간 —
	// 인증 완료 후 회원가입/약관동의 화면까지 이동하는 시간을 감안해
	// 코드 자체의 TTL(5분)보다 넉넉하게 잡는다.
	phoneVerificationConsumeWindow = 30 * time.Minute
)

func hashOTPCode(code string) string {
	sum := sha256.Sum256([]byte(code))
	return hex.EncodeToString(sum[:])
}

// generateOTPCode — 000000~999999 균등분포 6자리 숫자(앞자리 0 허용,
// 항상 6자리로 0-패딩). crypto/rand 기반이라 예측 불가능하다.
func generateOTPCode() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1000000))
	if err != nil {
		return "", fmt.Errorf("generate otp: %w", err)
	}
	return fmt.Sprintf("%06d", n.Int64()), nil
}

// digitsOnly strips hyphens for Aligo's receiver 파라미터(하이픈 없는
// 숫자만 받음 — notify/sms.go 참고).
func digitsOnly(phone string) string {
	return strings.ReplaceAll(phone, "-", "")
}

type sendPhoneCodeRequest struct {
	PhoneNumber string `json:"phoneNumber"`
}

// handleSendPhoneVerificationCode — POST /api/auth/phone/send-code.
func (s *Server) handleSendPhoneVerificationCode(w http.ResponseWriter, r *http.Request) {
	var req sendPhoneCodeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_body"})
		return
	}
	phone := strings.TrimSpace(req.PhoneNumber)
	if !phoneNumberPattern.MatchString(phone) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_phone_number"})
		return
	}

	ctx := r.Context()
	blocked, retryErr := phoneVerificationRateLimited(ctx, s.db, phone)
	if retryErr != nil {
		s.logger.Error("phone-verification: rate limit check failed", "error", retryErr)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if blocked {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "phone_code_rate_limited"})
		return
	}

	code, err := generateOTPCode()
	if err != nil {
		s.logger.Error("phone-verification: code generation failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal_error"})
		return
	}

	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO phone_verifications (phone_number, code_hash, expires_at)
		VALUES ($1, $2, $3)`,
		phone, hashOTPCode(code), time.Now().Add(phoneVerificationCodeTTL),
	); err != nil {
		s.logger.Error("phone-verification: insert failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}

	msg := fmt.Sprintf("인증번호는 %s입니다. 5분 이내에 입력해주세요.", code)
	if s.smsNotify.Configured() {
		if err := s.smsNotify.Send(ctx, digitsOnly(phone), msg); err != nil {
			s.logger.Error("phone-verification: sms send failed", "error", err)
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "sms_send_failed"})
			return
		}
	} else {
		// 로컬/개발 환경(ALIGO_* 미설정)에서도 가입 흐름을 끝까지 테스트할
		// 수 있도록 코드를 로그로 남긴다 — 운영 환경은 항상 ALIGO_*가
		// 설정돼있어 이 분기를 타지 않는다(main.go 시작 로그 경고 참고).
		s.logger.Info("phone-verification: SMS not configured, code logged for local testing", "phone", phone, "code", code)
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "sent"})
}

// phoneVerificationRateLimited — 같은 번호로 1분 이내 재요청했거나
// 최근 24시간 안에 5회 이상 요청했으면 true.
func phoneVerificationRateLimited(ctx context.Context, db *sql.DB, phone string) (bool, error) {
	var recentCount, dailyCount int
	if err := db.QueryRowContext(ctx, `
		SELECT
			count(*) FILTER (WHERE created_at > now() - interval '1 minute'),
			count(*) FILTER (WHERE created_at > now() - interval '1 day')
		FROM phone_verifications WHERE phone_number = $1`, phone,
	).Scan(&recentCount, &dailyCount); err != nil {
		return false, err
	}
	return recentCount > 0 || dailyCount >= 5, nil
}

type verifyPhoneCodeRequest struct {
	PhoneNumber string `json:"phoneNumber"`
	Code        string `json:"code"`
}

// handleVerifyPhoneCode — POST /api/auth/phone/verify-code. 가장 최근
// 발송된 코드 1건만 유효하다(재발송 시 이전 코드는 자연히 무효화 —
// 항상 최신 행만 조회하므로 별도로 지우거나 만료 처리할 필요가 없다).
func (s *Server) handleVerifyPhoneCode(w http.ResponseWriter, r *http.Request) {
	var req verifyPhoneCodeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_body"})
		return
	}
	phone := strings.TrimSpace(req.PhoneNumber)
	code := strings.TrimSpace(req.Code)
	if phone == "" || code == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_body"})
		return
	}

	ctx := r.Context()
	var id string
	var codeHash string
	var attemptCount int
	var expiresAt time.Time
	var verifiedAt sql.NullTime
	err := s.db.QueryRowContext(ctx, `
		SELECT id, code_hash, attempt_count, expires_at, verified_at
		FROM phone_verifications WHERE phone_number = $1
		ORDER BY created_at DESC LIMIT 1`, phone,
	).Scan(&id, &codeHash, &attemptCount, &expiresAt, &verifiedAt)
	if err == sql.ErrNoRows {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "code_not_found"})
		return
	}
	if err != nil {
		s.logger.Error("phone-verification: lookup failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if verifiedAt.Valid {
		// 이미 인증된 코드를 다시 확인하는 건 멱등하게 성공 처리(같은
		// 화면을 새로고침하거나 두 번 누른 경우 등) — 실패로 볼 이유가 없다.
		writeJSON(w, http.StatusOK, map[string]string{"status": "verified"})
		return
	}
	if time.Now().After(expiresAt) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "code_expired"})
		return
	}
	if attemptCount >= phoneVerificationMaxAttempts {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "too_many_attempts"})
		return
	}
	if hashOTPCode(code) != codeHash {
		if _, err := s.db.ExecContext(ctx, `UPDATE phone_verifications SET attempt_count = attempt_count + 1 WHERE id = $1`, id); err != nil {
			s.logger.Error("phone-verification: attempt increment failed", "error", err)
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_code"})
		return
	}

	if _, err := s.db.ExecContext(ctx, `UPDATE phone_verifications SET verified_at = now() WHERE id = $1`, id); err != nil {
		s.logger.Error("phone-verification: mark verified failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "verified"})
}

// consumeVerifiedPhone — 이 전화번호가 최근(phoneVerificationConsumeWindow
// 이내)에 인증 완료됐는지 확인한다. handleSignup(이메일 가입)과
// handleSignupAgreement(소셜 가입 온보딩) 둘 다 계정에 전화번호를
// 기록하기 직전 이 함수로 실제 인증 여부를 재확인한다 — 클라이언트가
// "인증 통과했다"고 주장하는 걸 그대로 믿지 않는다(이 프로젝트의
// 다른 서버측 재검증 원칙과 동일 — 결제금액 재계산 등).
func consumeVerifiedPhone(ctx context.Context, db *sql.DB, phone string) (bool, error) {
	var verified bool
	err := db.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM phone_verifications
			WHERE phone_number = $1 AND verified_at IS NOT NULL
			  AND verified_at > $2
		)`, phone, time.Now().Add(-phoneVerificationConsumeWindow),
	).Scan(&verified)
	return verified, err
}
