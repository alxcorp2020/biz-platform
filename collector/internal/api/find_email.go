// find_email.go — 아이디(이메일) 찾기(2026-08-04). 가입 시 인증된
// 휴대폰번호로 계정을 조회해 이메일 일부를 마스킹해 보여준다. 본인인증
// 방법은 우선 "가입 시 등록한 휴대폰번호 입력"만 지원한다(SMS 인증코드
// 재확인은 SMS 인프라 비용 문제로 이번엔 생략 — phone_verification.go의
// OTP와는 별개, 나중에 추가 예정). OTP 챌린지가 없는 만큼
// authLookupRateLimited(password_reset.go)로 같은 번호의 조회 시도 자체를
// 제한해 전화번호 목록 무차별 대입으로 이메일을 알아내는 것을 늦춘다.
package api

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"
)

type findEmailRequest struct {
	PhoneNumber string `json:"phoneNumber"`
}

// maskEmail — "ab***@domain.com" 형태. 로컬파트 앞 2글자만 남기고 나머지는
// 원래 길이와 무관하게 항상 "*" 3개로 고정 표시한다(글자 수로 원래 길이를
// 추측하지 못하게). 로컬파트가 2글자 이하면 있는 만큼만 보여준다.
func maskEmail(email string) string {
	at := strings.IndexByte(email, '@')
	if at < 0 {
		return email
	}
	local, domain := email[:at], email[at:]
	keep := local
	if len(local) > 2 {
		keep = local[:2]
	}
	return keep + "***" + domain
}

// handleFindEmail — POST /api/auth/find-email.
func (s *Server) handleFindEmail(w http.ResponseWriter, r *http.Request) {
	var req findEmailRequest
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
	blocked, err := authLookupRateLimited(ctx, s.db, "find_email", phone)
	if err != nil {
		s.logger.Error("find-email: rate limit check failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if blocked {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "rate_limited"})
		return
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO auth_lookup_attempts (kind, identifier) VALUES ('find_email', $1)`, phone); err != nil {
		s.logger.Error("find-email: attempt log insert failed", "error", err)
	}

	// 탈퇴 처리된 계정(deactivated_at)은 이메일이 이미 익명화돼있어
	// 애초에 이 조회로 유의미한 결과가 안 나온다(admin_member_actions.go)
	// — 명시적으로도 한 번 더 제외한다(방어적 이중 체크, handleLogin과 동일 원칙).
	// phone_number가 이론상 유일하다는 DB 제약은 없어(가입 시 SMS 인증은
	// 거치지만 전화번호 자체의 UNIQUE 제약은 없음) ORDER BY id로 결정론적인
	// 한 건만 고른다.
	var email string
	err = s.db.QueryRowContext(ctx, `
		SELECT email FROM users
		WHERE phone_number = $1 AND phone_verified_at IS NOT NULL AND deactivated_at IS NULL
		ORDER BY id LIMIT 1`, phone,
	).Scan(&email)
	if err == sql.ErrNoRows {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "account_not_found"})
		return
	}
	if err != nil {
		s.logger.Error("find-email: query failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"email": maskEmail(email)})
}
