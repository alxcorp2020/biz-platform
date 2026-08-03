// system_settings.go — 관리자가 재배포 없이 조절하는 런타임 설정값
// (system_settings 테이블, key-value). 첫 사용처는 free_plan_email_limit
// (notifications.go의 checkEmailNotificationQuota가 읽음)뿐이지만, 테이블
// 자체와 get/set 헬퍼는 범용으로 만들어 다음 설정도 같은 방식으로 추가할
// 수 있게 했다.
package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"strconv"
)

const freePlanEmailLimitSettingKey = "free_plan_email_limit"

// phoneVerificationRequiredSettingKey — 관리자가 재배포 없이 껐다 켰다
// 하는 휴대폰 SMS 인증 사용 여부(2026-08-04). auth.go의 handleSignup과
// signup_agreement.go의 handleSignupAgreement 둘 다 이 값을 읽어 휴대폰
// 인증을 강제할지(기존 동작) 선택 입력으로 완화할지 결정한다.
const phoneVerificationRequiredSettingKey = "phone_verification_required"

// defaultPhoneVerificationRequired — system_settings에 값이 없을 때의
// 안전한 기본값. 지금까지의 동작(휴대폰 인증 필수)을 그대로 유지하는
// 쪽으로 fail-closed — 파싱 실패/행 누락을 "인증 생략 가능"으로 잘못
// 해석하면 회원가입 보안 수준이 조용히 낮아지므로 항상 true로 폴백한다.
const defaultPhoneVerificationRequired = true

// defaultFreePlanEmailLimit — system_settings에 값이 없을 때(예: 이 컬럼이
// 생기기 전에 만들어진 DB에서 마이그레이션이 아직 시드를 못 넣은 아주
// 짧은 순간, 또는 값이 실수로 지워진 경우) 안전하게 쓰는 기본값. 정상
// 배포에서는 migrate.go의 ensureSystemSettingsTable이 이 값을 이미
// system_settings에 심어둔다.
const defaultFreePlanEmailLimit = 20

// getSystemSettingInt reads a system_settings row and parses it as an int,
// falling back to defaultValue if the row is missing or the stored value
// isn't a valid integer(설정값이 깨져도 기능이 죽지 않고 안전한 기본값으로
// 동작해야 한다 — 특히 이 값은 "얼마나 보낼 수 있는지" 한도라, 파싱 실패를
// "무제한"으로 해석하면 안 되므로 항상 defaultValue로 폴백한다).
func (s *Server) getSystemSettingInt(ctx context.Context, key string, defaultValue int) (int, error) {
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM system_settings WHERE key = $1`, key).Scan(&raw)
	if err == sql.ErrNoRows {
		return defaultValue, nil
	}
	if err != nil {
		return defaultValue, err
	}
	v, convErr := strconv.Atoi(raw)
	if convErr != nil {
		s.logger.Error("system-settings: stored value is not an integer, using default", "key", key, "value", raw)
		return defaultValue, nil
	}
	return v, nil
}

func (s *Server) setSystemSettingInt(ctx context.Context, key string, value int) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO system_settings (key, value, updated_at) VALUES ($1, $2, now())
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now()`,
		key, strconv.Itoa(value))
	return err
}

// getSystemSettingBool/setSystemSettingBool — getSystemSettingInt와 같은
// fail-safe 원칙(값이 없거나 깨졌으면 defaultValue로 폴백, 에러 로깅만).
func (s *Server) getSystemSettingBool(ctx context.Context, key string, defaultValue bool) (bool, error) {
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM system_settings WHERE key = $1`, key).Scan(&raw)
	if err == sql.ErrNoRows {
		return defaultValue, nil
	}
	if err != nil {
		return defaultValue, err
	}
	v, convErr := strconv.ParseBool(raw)
	if convErr != nil {
		s.logger.Error("system-settings: stored value is not a bool, using default", "key", key, "value", raw)
		return defaultValue, nil
	}
	return v, nil
}

func (s *Server) setSystemSettingBool(ctx context.Context, key string, value bool) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO system_settings (key, value, updated_at) VALUES ($1, $2, now())
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now()`,
		key, strconv.FormatBool(value))
	return err
}

// handleAdminGetSettings — GET /api/admin/settings. 지금은 필드 하나뿐이지만
// 응답을 객체로 열어둬 다음 설정이 추가돼도 하위호환되게 했다.
func (s *Server) handleAdminGetSettings(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireSystemAdmin(w, r); !ok {
		return
	}
	ctx := r.Context()
	limit, err := s.getSystemSettingInt(ctx, freePlanEmailLimitSettingKey, defaultFreePlanEmailLimit)
	if err != nil {
		s.logger.Error("admin-get-settings: query failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	phoneVerificationRequired, err := s.getSystemSettingBool(ctx, phoneVerificationRequiredSettingKey, defaultPhoneVerificationRequired)
	if err != nil {
		s.logger.Error("admin-get-settings: query failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"freePlanEmailLimit":        limit,
		"phoneVerificationRequired": phoneVerificationRequired,
	})
}

// handleAdminSetPhoneVerificationRequired — PUT /api/admin/settings/phone-verification-required.
// 값 변경은 system_settings UPDATE 한 줄이라 재배포 없이 다음 회원가입
// 시도부터 바로 반영된다(handleSignup/handleSignupAgreement가 매번 직접
// 읽음, 캐시 없음). 끄면 새 가입자의 휴대폰번호가 선택 입력으로 바뀌고
// SMS 인증 없이도 가입할 수 있다 — 이미 인증을 마친 기존 회원에는
// 영향 없다.
func (s *Server) handleAdminSetPhoneVerificationRequired(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireSystemAdmin(w, r); !ok {
		return
	}
	var req struct {
		Value bool `json:"value"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_body"})
		return
	}
	if err := s.setSystemSettingBool(r.Context(), phoneVerificationRequiredSettingKey, req.Value); err != nil {
		s.logger.Error("admin-set-settings: update failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"phoneVerificationRequired": req.Value})
}

// handleGetAuthConfig — GET /api/auth/config(공개, 인증 불필요). 회원가입/
// 소셜로그인 온보딩 화면이 휴대폰 인증 단계를 보여줄지(OTP 발송/확인
// 버튼) 아니면 선택 입력 필드 하나만 보여줄지 결정하는 데 쓴다 — 관리자
// 전용 GET /api/admin/settings와 달리 이 값 하나만 비로그인 사용자에게도
// 공개한다.
func (s *Server) handleGetAuthConfig(w http.ResponseWriter, r *http.Request) {
	phoneVerificationRequired, err := s.getSystemSettingBool(r.Context(), phoneVerificationRequiredSettingKey, defaultPhoneVerificationRequired)
	if err != nil {
		s.logger.Error("get-auth-config: query failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"phoneVerificationRequired": phoneVerificationRequired})
}

// handleAdminSetFreePlanEmailLimit — PUT /api/admin/settings/free-plan-email-limit.
// 값 변경은 system_settings UPDATE 한 줄이라 재배포 없이 다음 발송
// 체크(checkEmailNotificationQuota)부터 바로 반영된다 — 캐시를 전혀
// 두지 않고 매번 DB에서 직접 읽기 때문(호출 빈도가 아주 낮아 캐싱 없이도
// 부담 없음: 알림성 이메일 발송 시점에만 조회함).
func (s *Server) handleAdminSetFreePlanEmailLimit(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireSystemAdmin(w, r); !ok {
		return
	}
	var req struct {
		Value int `json:"value"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Value < 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_body"})
		return
	}
	if err := s.setSystemSettingInt(r.Context(), freePlanEmailLimitSettingKey, req.Value); err != nil {
		s.logger.Error("admin-set-settings: update failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"freePlanEmailLimit": req.Value})
}
