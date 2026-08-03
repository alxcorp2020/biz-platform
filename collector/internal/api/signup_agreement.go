// signup_agreement.go — 소셜 로그인(구글/네이버/카카오) 온보딩 화면
// (renderSignupProfileStep, #/auth/oauth-profile)에서 휴대폰번호 인증
// 확인 + 필수 약관 동의를 처리한다. 2026-08-03 온보딩 재설계 이전에는
// 이메일 가입 2단계에서도 같은 화면/엔드포인트를 공유했지만, 지금은
// 이메일 가입이 단일 폼(휴대폰 인증까지 포함)으로 합쳐져 handleSignup이
// 이 역할을 대신하므로 이 엔드포인트는 소셜 로그인 전용이다 — 소셜
// 로그인은 콜백이 계정을 이미 만들어버려서 "계정 생성 시점에 함께
// 처리"가 불가능하기 때문에 로그인 이후 별도 확인 단계가 필요하다.
// PUT /api/me/company-profile(handleUpsertCompanyProfile)과 일부러
// 분리했다 — 그 엔드포인트는 이후 "회사 정보 수정" 화면에서도 재사용
// 되는데, 거기에 약관동의를 필수로 묶으면 기존 회원이 프로필을 고칠
// 때마다 약관에 매번 동의해야 하는 이상한 흐름이 된다.
package api

import (
	"database/sql"
	"encoding/json"
	"net"
	"net/http"
	"regexp"
	"strings"
)

// 010은 11자리(3-4-4), 011/016/017/018/019(옛 016~019 통신사 번호)는
// 10자리(3-3-4)로 자릿수 자체가 다르다 — 프론트(formatMobilePhoneInput,
// index.html)의 자동 하이픈 포맷과 정확히 같은 두 가지 형식만 허용한다.
var phoneNumberPattern = regexp.MustCompile(`^(010-\d{4}-\d{4}|01[16789]-\d{3}-\d{4})$`)

type signupAgreementRequest struct {
	PhoneNumber   string `json:"phoneNumber"`
	TermsAgreed   bool   `json:"termsAgreed"`
	PrivacyAgreed bool   `json:"privacyAgreed"`
}

// handleSignupAgreement — POST /api/me/signup-agreement. 약관 버전은
// 클라이언트가 보낸 값을 쓰지 않고 서버가 그 순간의 활성 버전을 직접
// 조회해서 기록한다(클라이언트 값을 믿지 않는다는 이 프로젝트의 결제
// 금액 검증과 같은 원칙 — billing.go 참고). 연락처는 형식만이 아니라
// SMS 인증 완료 여부까지 재확인(consumeVerifiedPhone, phone_verification.go)
// 한 뒤에만 users.phone_number/phone_verified_at에 저장한다.
func (s *Server) handleSignupAgreement(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.currentUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	var req signupAgreementRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_body"})
		return
	}
	if !req.TermsAgreed || !req.PrivacyAgreed {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "agreement_required"})
		return
	}

	ctx := r.Context()
	// 관리자가 #/admin에서 재배포 없이 껐다 켰다 하는 설정(system_settings.go) —
	// 꺼져 있으면 휴대폰번호는 선택 입력이 되고 SMS 인증도 요구하지 않는다
	// (handleSignup과 동일한 분기).
	phoneRequired, err := s.getSystemSettingBool(ctx, phoneVerificationRequiredSettingKey, defaultPhoneVerificationRequired)
	if err != nil {
		s.logger.Error("signup-agreement: phone verification setting lookup failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	phone := strings.TrimSpace(req.PhoneNumber)
	if phoneRequired {
		if !phoneNumberPattern.MatchString(phone) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_phone_number"})
			return
		}
		verified, err := consumeVerifiedPhone(ctx, s.db, phone)
		if err != nil {
			s.logger.Error("signup-agreement: phone verification check failed", "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
			return
		}
		if !verified {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "phone_not_verified"})
			return
		}
	} else if phone != "" && !phoneNumberPattern.MatchString(phone) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_phone_number"})
		return
	}
	var termsVersion, privacyVersion string
	if err := s.db.QueryRowContext(ctx,
		`SELECT version FROM legal_documents WHERE type = 'terms' AND is_active = true`,
	).Scan(&termsVersion); err != nil {
		s.logger.Error("signup-agreement: terms version lookup failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if err := s.db.QueryRowContext(ctx,
		`SELECT version FROM legal_documents WHERE type = 'privacy' AND is_active = true`,
	).Scan(&privacyVersion); err != nil {
		s.logger.Error("signup-agreement: privacy version lookup failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		s.logger.Error("signup-agreement: begin tx failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	defer tx.Rollback()

	if phoneRequired {
		if _, err := tx.ExecContext(ctx, `UPDATE users SET phone_number = $1, phone_verified_at = now() WHERE id = $2`, phone, userID); err != nil {
			s.logger.Error("signup-agreement: phone update failed", "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
			return
		}
	} else {
		var phoneArg sql.NullString
		if phone != "" {
			phoneArg = sql.NullString{String: phone, Valid: true}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE users SET phone_number = $1 WHERE id = $2`, phoneArg, userID); err != nil {
			s.logger.Error("signup-agreement: phone update failed", "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
			return
		}
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO terms_agreements (user_id, terms_version, privacy_version, ip_address)
		VALUES ($1, $2, $3, $4)`,
		userID, termsVersion, privacyVersion, clientIP(r),
	); err != nil {
		s.logger.Error("signup-agreement: insert failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if err := tx.Commit(); err != nil {
		s.logger.Error("signup-agreement: commit failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "agreed"})
}

// clientIP best-effort로 실제 클라이언트 IP를 얻는다. Render 등 리버스
// 프록시 뒤에서는 X-Forwarded-For의 첫 번째 값이 원 클라이언트다(그 뒤에는
// 프록시들이 자신의 주소를 덧붙임). 이 값은 클라이언트가 위조 가능한
// 헤더라 보안 판단에는 쓰지 않는다 — terms_agreements.ip_address는
// 법적 증빙 보조용 참고 정보일 뿐이라 위조 가능성을 감수한다.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		return strings.TrimSpace(parts[0])
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}
