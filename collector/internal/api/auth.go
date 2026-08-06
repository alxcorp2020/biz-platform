// Auth endpoints: signup/login/logout via a signed cookie session (no
// server-side session table — the cookie itself carries user_id + expiry,
// authenticated with HMAC-SHA256 under SESSION_SECRET) and the company
// profile the eligibility rule engine (spec 5.7/11.1) will later read.
package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"math"
	"net/http"
	"net/mail"
	"strconv"
	"strings"
	"time"

	"github.com/lib/pq"
	"golang.org/x/crypto/bcrypt"
)

const (
	sessionCookieName = "session"
	sessionTTL        = 7 * 24 * time.Hour
)

func (s *Server) signSession(userID string, expiresAt time.Time) string {
	payload := userID + "|" + strconv.FormatInt(expiresAt.Unix(), 10)
	mac := hmac.New(sha256.New, s.sessionSecret)
	mac.Write([]byte(payload))
	sig := mac.Sum(nil)
	return base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func (s *Server) verifySession(value string) (userID string, ok bool) {
	parts := strings.SplitN(value, ".", 2)
	if len(parts) != 2 {
		return "", false
	}
	payloadRaw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return "", false
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", false
	}
	mac := hmac.New(sha256.New, s.sessionSecret)
	mac.Write(payloadRaw)
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return "", false
	}
	fields := strings.SplitN(string(payloadRaw), "|", 2)
	if len(fields) != 2 {
		return "", false
	}
	expUnix, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil || time.Now().Unix() > expUnix {
		return "", false
	}
	return fields[0], true
}

func (s *Server) setSessionCookie(w http.ResponseWriter, userID string) {
	expiresAt := time.Now().Add(sessionTTL)
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    s.signSession(userID, expiresAt),
		Path:     "/",
		Expires:  expiresAt,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
}

func (s *Server) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
}

func (s *Server) currentUserID(r *http.Request) (string, bool) {
	c, err := r.Cookie(sessionCookieName)
	if err != nil {
		return "", false
	}
	return s.verifySession(c.Value)
}

func isValidEmail(email string) bool {
	if email == "" || strings.ContainsAny(email, " \t\n") {
		return false
	}
	_, err := mail.ParseAddress(email)
	return err == nil
}

type authRequest struct {
	Email         string `json:"email"`
	Password      string `json:"password"`
	PhoneNumber   string `json:"phoneNumber"`
	TermsAgreed   bool   `json:"termsAgreed"`
	PrivacyAgreed bool   `json:"privacyAgreed"`
}

// handleSignup — POST /api/auth/signup. 2026-08-03 온보딩 재설계(Phase 1)
// 이후로는 이 한 요청이 이메일 가입의 전부다 — 예전엔 계정 생성(1단계)과
// 휴대폰번호+약관동의(2단계, signup_agreement.go 재사용)가 분리돼 있었지만,
// 지금은 회원가입 화면 자체가 단일 폼(이메일/휴대폰/비밀번호/약관동의)이라
// 계정 생성 시점에 전부 함께 처리한다. 휴대폰번호는 SMS 인증(phone_verification.go)
// 이 이 요청 *이전에* 이미 끝나있어야 하고, 여기서는 consumeVerifiedPhone으로
// 그 사실을 서버가 직접 재확인한다(클라이언트가 "인증 통과했다"고
// 주장하는 값을 그대로 믿지 않음 — 결제금액 재계산 등과 같은 원칙).
// 소셜 로그인(구글/네이버/카카오) 가입자는 이 엔드포인트를 거치지 않고
// oauth_login.go 콜백이 계정을 바로 만든 뒤 signup_agreement.go의
// handleSignupAgreement(여기와 같은 방식으로 휴대폰 인증을 재확인)로
// 휴대폰번호+약관동의를 받는다 — 두 경로 모두 결과적으로
// users.phone_verified_at이 채워져야 온보딩 완료로 간주된다.
func (s *Server) handleSignup(w http.ResponseWriter, r *http.Request) {
	var req authRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_body"})
		return
	}
	email := strings.TrimSpace(strings.ToLower(req.Email))
	if !isValidEmail(email) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_email"})
		return
	}
	if len(req.Password) < 8 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "password_too_short"})
		return
	}
	if !req.TermsAgreed || !req.PrivacyAgreed {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "agreement_required"})
		return
	}

	ctx := r.Context()
	// 관리자가 #/admin에서 재배포 없이 껐다 켰다 하는 설정(system_settings.go) —
	// 꺼져 있으면 휴대폰번호는 선택 입력이 되고 SMS 인증도 요구하지 않는다.
	phoneRequired, err := s.getSystemSettingBool(ctx, phoneVerificationRequiredSettingKey, defaultPhoneVerificationRequired)
	if err != nil {
		s.logger.Error("signup: phone verification setting lookup failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	// 2026-08-05: "SMS 인증 요구 여부"와 "휴대폰번호 입력 자체가 필수인지"는
	// 별개 정책이라는 사용자 확인 — phoneRequired가 꺼져 있어도 번호 입력과
	// 형식 검증은 항상 요구하고, consumeVerifiedPhone(OTP 인증 확인)만
	// phoneRequired일 때만 추가로 요구한다.
	phone := strings.TrimSpace(req.PhoneNumber)
	if !phoneNumberPattern.MatchString(phone) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_phone_number"})
		return
	}
	phoneValue := sql.NullString{String: phone, Valid: true}
	var phoneVerifiedAt sql.NullTime
	if phoneRequired {
		verified, err := consumeVerifiedPhone(ctx, s.db, phone)
		if err != nil {
			s.logger.Error("signup: phone verification check failed", "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
			return
		}
		if !verified {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "phone_not_verified"})
			return
		}
		phoneVerifiedAt = sql.NullTime{Time: time.Now(), Valid: true}
	}

	var exists bool
	if err := s.db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM users WHERE email = $1)`, email,
	).Scan(&exists); err != nil {
		s.logger.Error("signup: check email failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if exists {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "email_already_registered"})
		return
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		s.logger.Error("signup: hash failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal_error"})
		return
	}

	// 약관 버전은 클라이언트가 보낸 값을 안 믿고 서버가 그 순간의 활성
	// 버전을 직접 조회해서 기록한다(signup_agreement.go와 동일 원칙).
	var termsVersion, privacyVersion string
	if err := s.db.QueryRowContext(ctx,
		`SELECT version FROM legal_documents WHERE type = 'terms' AND is_active = true`,
	).Scan(&termsVersion); err != nil {
		s.logger.Error("signup: terms version lookup failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if err := s.db.QueryRowContext(ctx,
		`SELECT version FROM legal_documents WHERE type = 'privacy' AND is_active = true`,
	).Scan(&privacyVersion); err != nil {
		s.logger.Error("signup: privacy version lookup failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		s.logger.Error("signup: begin tx failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	defer tx.Rollback()

	var userID string
	if err := tx.QueryRowContext(ctx,
		`INSERT INTO users (email, password_hash, phone_number, phone_verified_at) VALUES ($1, $2, $3, $4) RETURNING id`,
		email, string(hash), phoneValue, phoneVerifiedAt,
	).Scan(&userID); err != nil {
		s.logger.Error("signup: insert failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO terms_agreements (user_id, terms_version, privacy_version, ip_address)
		VALUES ($1, $2, $3, $4)`,
		userID, termsVersion, privacyVersion, clientIP(r),
	); err != nil {
		s.logger.Error("signup: terms agreement insert failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if err := tx.Commit(); err != nil {
		s.logger.Error("signup: commit failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}

	s.setSessionCookie(w, userID)
	// 이메일 가입 필수 이메일 인증(2026-08-04) — 발송 실패해도 가입 자체는
	// 이미 완료됐으니 로그만 남기고 응답은 그대로 성공 처리한다. 인증
	// 전까지는 route()가 대부분의 화면 접근을 막고 재발송 버튼이 있는
	// 대기 화면으로 돌려보낸다(email_verification.go 참고).
	if err := s.sendEmailVerificationEmail(ctx, userID, email); err != nil {
		s.logger.Error("signup: email verification send failed", "error", err)
	}
	writeJSON(w, http.StatusCreated, map[string]string{"id": userID, "email": email})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req authRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_body"})
		return
	}
	email := strings.TrimSpace(strings.ToLower(req.Email))

	var userID string
	var passwordHash sql.NullString
	var deactivatedAt sql.NullTime
	err := s.db.QueryRowContext(r.Context(),
		`SELECT id, password_hash, deactivated_at FROM users WHERE email = $1`, email,
	).Scan(&userID, &passwordHash, &deactivatedAt)
	if err == sql.ErrNoRows {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid_credentials"})
		return
	}
	if err != nil {
		s.logger.Error("login: query failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	// 관리자가 탈퇴 처리한 계정 — 이메일 자체가 익명화되어 원래 이메일로는
	// 이 SELECT가 애초에 못 찾는 게 정상이지만(admin_member_actions.go),
	// 명시적으로도 한 번 더 막는다(방어적 이중 체크).
	if deactivatedAt.Valid {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "account_deactivated"})
		return
	}

	// 간편로그인(구글/네이버/카카오)으로만 가입한 계정은 password_hash가
	// NULL이다 — 이메일/비밀번호로는 로그인할 수 없으니 "이 계정으로는
	// 비번 로그인 불가"를 명확히 구분해 알려준다(invalid_credentials로
	// 뭉개면 사용자가 비번을 계속 재시도하게 됨).
	if !passwordHash.Valid {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "social_login_only"})
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(passwordHash.String), []byte(req.Password)) != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid_credentials"})
		return
	}

	// 관리자 화면(admin.go) 회원목록의 "마지막 로그인" 근거. 실패해도
	// 로그인 자체를 막을 이유는 없어 에러만 로깅한다.
	if _, err := s.db.ExecContext(r.Context(), `UPDATE users SET last_login_at = now() WHERE id = $1`, userID); err != nil {
		s.logger.Error("login: last_login_at update failed", "error", err)
	}

	s.setSessionCookie(w, userID)
	writeJSON(w, http.StatusOK, map[string]string{"id": userID, "email": email})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	s.clearSessionCookie(w)
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

type companyProfileDTO struct {
	ID string `json:"id"`
	// Role — 팀기능: 이 조직에서 호출자의 역할(owner/member). owner만
	// 프로필/재무/실적/인력/면허/지식재산권/구독을 쓸 수 있고, member는
	// 파이프라인만 쓸 수 있다(company_pipeline.go는 이 필드를 안 봄 —
	// 애초에 두 역할 다 허용). 각 owner-only 핸들러가 이 값으로 403을
	// 판단한다.
	Role                       string   `json:"role"`
	BusinessRegistrationNumber *string  `json:"businessRegistrationNumber"`
	CompanyName                *string  `json:"companyName"`
	RepresentativeName         *string  `json:"representativeName"`
	Address                    *string  `json:"address"`
	BusinessType               []string `json:"businessType"`
	Region                     *string  `json:"region"`
	Industry                   []string `json:"industry"`
	// FoundingDate/BusinessAgeYears — 2026-08-06, 업력 계산의 source of
	// truth를 개업일 원본으로 전환(migrate.go ensureCompanyProfileFoundingDateColumn
	// 주석 참고). FoundingDate가 있으면 BusinessAgeYears는 매 조회마다
	// computeBusinessAgeYears로 다시 계산한 값이다(저장된 옛 값을 그대로
	// 안 돌려줌 — 그래야 재입력 안 해도 업력이 계속 정확하다). FoundingDate가
	// 없는 옛 계정만 컬럼에 저장된 옛 값을 그대로 돌려준다(하위호환).
	FoundingDate         *string  `json:"foundingDate"`
	BusinessAgeYears     *float64 `json:"businessAgeYears"`
	RevenueAmount        *int64   `json:"revenueAmount"`
	EmployeeCount        *int64   `json:"employeeCount"`
	CompanySize          *string  `json:"companySize"`
	Licenses             []string `json:"licenses"`
	Certifications       []string `json:"certifications"`
	DirectProductionCert bool     `json:"directProductionCert"`
	MaxPerformanceAmount *int64   `json:"maxPerformanceAmount"`
	CreditRating         *string  `json:"creditRating"`
	// EmployeeCountConfidence/VerifiedAt은 4대보험 사업장 가입자명부로
	// employee_count를 확인했을 때만 채워진다(company_employee_verification.go).
	EmployeeCountConfidence *string    `json:"employeeCountConfidence"`
	EmployeeCountVerifiedAt *time.Time `json:"employeeCountVerifiedAt"`
	// 알림 설정(email/phone/sms)은 팀기능 이전엔 users에 있었으나 "조직
	// 단위로 공유"하기로 해 company_profiles로 옮겨왔다 — 이 응답이
	// 그 값의 유일한 출처다(currentUser.emailNotificationsEnabled 등은
	// 더 이상 없음, currentProfile 쪽에서 읽어야 함).
	EmailNotificationsEnabled bool    `json:"emailNotificationsEnabled"`
	PhoneNumber               *string `json:"phoneNumber"`
	SMSNotificationsEnabled   bool    `json:"smsNotificationsEnabled"`
	// NotificationDaysBefore — 제출마감 리마인더를 보낼 D-N 목록(7/3/1
	// 중 다중선택, 기본 [3,1]). notifications.go의 sendDeadlineReminders가
	// 호출부에서 넘기는 offsetDays가 이 배열에 있을 때만 실제로 대상이
	// 된다.
	NotificationDaysBefore []int `json:"notificationDaysBefore"`
	// OnboardingCompletedAt — 2026-08-05 온보딩 재설계. NULL이면 신규 필수
	// 온보딩(AI 분석 커버리지 50% 이상 확인)을 아직 안 마친 것 — route()
	// 게이트가 이 값으로 온보딩 화면 강제 여부를 판단한다. 기존 회원은
	// 마이그레이션 때 일괄 백필돼 전부 non-null이라 소급 적용되지 않는다.
	OnboardingCompletedAt *time.Time `json:"onboardingCompletedAt"`
}

// getCompanyProfile resolves "which organization does this user belong to"
// via company_members — the single chokepoint for the whole multi-user-per-
// org model (팀기능). Every other handler that needs a company profile goes
// through this function, so redirecting the JOIN here (company_members →
// company_profiles) instead of the old direct company_profiles.user_id
// lookup is what makes every one of those ~15 call sites work correctly for
// both owners and members without individually touching them.
// computeBusinessAgeYears mirrors the frontend's computeBusinessAgeYears
// (index.html) exactly — (오늘 - 개업일) / 365.25, 소수점 첫째 자리까지.
// 서버에서도 필요한 이유: getCompanyProfile이 매 조회마다 founding_date
// 기준으로 다시 계산해 응답해야 재입력 없이도 업력이 항상 최신이다(프론트
// 계산에만 의존하면 응답이 캐시되거나 다른 화면에서 그대로 표시될 때
// 시점이 어긋날 수 있음).
func computeBusinessAgeYears(founding time.Time) float64 {
	days := time.Since(founding).Hours() / 24
	if days < 0 {
		days = 0
	}
	return math.Round((days/365.25)*10) / 10
}

func (s *Server) getCompanyProfile(r *http.Request, userID string) (*companyProfileDTO, error) {
	var p companyProfileDTO
	var region, companySize, creditRating, employeeCountConfidence, phoneNumber sql.NullString
	var bizRegNumber, companyName, repName, address sql.NullString
	var businessAgeYears sql.NullFloat64
	var foundingDate sql.NullTime
	var revenueAmount, employeeCount, maxPerformanceAmount sql.NullInt64
	var employeeCountVerifiedAt, onboardingCompletedAt sql.NullTime
	var businessType, industry, licenses, certs pq.StringArray
	var notificationDaysBefore pq.Int64Array

	err := s.db.QueryRowContext(r.Context(), `
		SELECT cp.id, cm.role, cp.business_registration_number, cp.company_name, cp.representative_name, cp.address,
		       cp.business_type, cp.region, cp.industry, cp.business_age_years, cp.founding_date, cp.revenue_amount,
		       cp.employee_count, cp.company_size, cp.licenses, cp.certifications,
		       cp.direct_production_cert, cp.max_performance_amount, cp.credit_rating,
		       cp.employee_count_confidence, cp.employee_count_verified_at,
		       cp.email_notifications_enabled, cp.phone_number, cp.sms_notifications_enabled,
		       cp.notification_days_before, cp.onboarding_completed_at
		FROM company_members cm
		JOIN company_profiles cp ON cp.id = cm.company_profile_id
		WHERE cm.user_id = $1`, userID,
	).Scan(&p.ID, &p.Role, &bizRegNumber, &companyName, &repName, &address,
		&businessType, &region, &industry, &businessAgeYears, &foundingDate, &revenueAmount,
		&employeeCount, &companySize, &licenses, &certs,
		&p.DirectProductionCert, &maxPerformanceAmount, &creditRating,
		&employeeCountConfidence, &employeeCountVerifiedAt,
		&p.EmailNotificationsEnabled, &phoneNumber, &p.SMSNotificationsEnabled,
		&notificationDaysBefore, &onboardingCompletedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	p.BusinessRegistrationNumber = nullStringPtr(bizRegNumber)
	p.CompanyName = nullStringPtr(companyName)
	p.RepresentativeName = nullStringPtr(repName)
	p.Address = nullStringPtr(address)
	p.BusinessType = []string(businessType)
	p.Region = nullStringPtr(region)
	p.Industry = []string(industry)
	p.CompanySize = nullStringPtr(companySize)
	p.CreditRating = nullStringPtr(creditRating)
	p.RevenueAmount = nullInt64Ptr(revenueAmount)
	p.EmployeeCount = nullInt64Ptr(employeeCount)
	p.MaxPerformanceAmount = nullInt64Ptr(maxPerformanceAmount)
	if foundingDate.Valid {
		dateStr := foundingDate.Time.Format("2006-01-02")
		p.FoundingDate = &dateStr
		age := computeBusinessAgeYears(foundingDate.Time)
		p.BusinessAgeYears = &age
	} else if businessAgeYears.Valid {
		p.BusinessAgeYears = &businessAgeYears.Float64
	}
	p.Licenses = []string(licenses)
	p.Certifications = []string(certs)
	p.EmployeeCountConfidence = nullStringPtr(employeeCountConfidence)
	if employeeCountVerifiedAt.Valid {
		p.EmployeeCountVerifiedAt = &employeeCountVerifiedAt.Time
	}
	p.PhoneNumber = nullStringPtr(phoneNumber)
	if onboardingCompletedAt.Valid {
		p.OnboardingCompletedAt = &onboardingCompletedAt.Time
	}
	p.NotificationDaysBefore = make([]int, len(notificationDaysBefore))
	for i, v := range notificationDaysBefore {
		p.NotificationDaysBefore[i] = int(v)
	}
	return &p, nil
}

func nullStringPtr(n sql.NullString) *string {
	if !n.Valid {
		return nil
	}
	return &n.String
}

func nullInt64Ptr(n sql.NullInt64) *int64 {
	if !n.Valid {
		return nil
	}
	return &n.Int64
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.currentUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	var email, role, plan string
	var phoneVerified, emailVerified bool
	err := s.db.QueryRowContext(r.Context(),
		`SELECT email, role, plan, phone_verified_at IS NOT NULL, email_verified_at IS NOT NULL FROM users WHERE id = $1`, userID,
	).Scan(&email, &role, &plan, &phoneVerified, &emailVerified)
	if err == sql.ErrNoRows {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	if err != nil {
		s.logger.Error("me: query failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}

	// onboarded — "약관동의(+휴대폰 인증, 켜져 있을 때만)까지 마쳤는가".
	// 관리자가 휴대폰 인증을 끌 수 있게 되면서(system_settings.go)
	// phone_verified_at만으로는 온보딩 완료 여부를 판단할 수 없어졌다 —
	// terms_agreements 행 존재 여부는 휴대폰 인증 필수 여부와 무관하게
	// handleSignup/handleSignupAgreement 둘 다 항상 같은 트랜잭션에서
	// 만들어 넣으므로, 이 값이 더 안정적인 "온보딩 완료" 신호다.
	var onboarded bool
	if err := s.db.QueryRowContext(r.Context(),
		`SELECT EXISTS (SELECT 1 FROM terms_agreements WHERE user_id = $1)`, userID,
	).Scan(&onboarded); err != nil {
		s.logger.Error("me: onboarded check failed", "error", err)
	}

	// 알림 설정(email/phone/sms)은 팀기능으로 조직 단위(company_profiles)로
	// 옮겨졌다 — currentUser에는 더 이상 없고 companyProfile 쪽에서 읽는다
	// (조직이 아직 없으면 프로필 자체가 null이라 자연히 알림 설정도 없음).
	profile, err := s.getCompanyProfile(r, userID)
	if err != nil {
		s.logger.Error("me: profile query failed", "error", err)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"user": map[string]any{
			"id": userID, "email": email, "role": role, "plan": plan,
			"phoneVerified": phoneVerified, "emailVerified": emailVerified, "onboarded": onboarded,
		},
		"companyProfile": profile,
	})
}

type companyProfileRequest struct {
	// 사업자등록증 OCR 자동생성(Phase UX-01, 2026-08-04) 전용 4개 필드 —
	// business_registration.go가 추출한 값을 사용자가 확인한 뒤 이 필드로
	// 그대로 전송한다. 수동 "기업 프로필 수정" 화면에서도 편집 가능.
	BusinessRegistrationNumber *string  `json:"businessRegistrationNumber"`
	CompanyName                *string  `json:"companyName"`
	RepresentativeName         *string  `json:"representativeName"`
	Address                    *string  `json:"address"`
	BusinessType               []string `json:"businessType"`
	Region                     *string  `json:"region"`
	Industry                   []string `json:"industry"`
	// FoundingDate — 2026-08-06. company_profiles.founding_date(YYYY-MM-DD)로
	// 그대로 저장되고, 서버가 이 값 기준으로 BusinessAgeYears를 계산해
	// 함께 저장한다. 다른 필드와 달리 "생략하면 기존 값을 그대로 둔다"
	// (COALESCE, handleUpsertCompanyProfile 참고) — 이 값은 매번 재전송을
	// 강제하지 않는다(개업일을 실수로 안 보내서 지워지는 사고를 막기
	// 위함, 프론트는 그래도 항상 현재 값을 프리필해서 재전송하지만
	// 안전장치로 서버도 이렇게 동작). BusinessAgeYears는 더 이상 요청
	// 필드로 안 받는다 — 항상 FoundingDate에서 파생.
	FoundingDate         *string  `json:"foundingDate"`
	RevenueAmount        *int64   `json:"revenueAmount"`
	EmployeeCount        *int64   `json:"employeeCount"`
	CompanySize          *string  `json:"companySize"`
	Licenses             []string `json:"licenses"`
	Certifications       []string `json:"certifications"`
	DirectProductionCert bool     `json:"directProductionCert"`
	MaxPerformanceAmount *int64   `json:"maxPerformanceAmount"`
	CreditRating         *string  `json:"creditRating"`
	// PhoneNumber — 사업자 대표전화번호(회사 단위, 개인 휴대폰번호인
	// users.phone_number와 별개). 회원가입 2단계(업체정보)에서 필수로
	// 받기 시작했고, 이후 "회사정보 수정" 화면에서도 같은 필드를 공유한다.
	PhoneNumber *string `json:"phoneNumber"`
}

// handleUpsertCompanyProfile creates the org on first call(문서상 "회사
// 정보 등록") — 그 순간 호출자를 owner로 company_members에 함께 등록한다
// (두 INSERT를 한 트랜잭션으로 묶어 "프로필은 있는데 멤버십이 없는" 반쪽
// 상태를 방지). 이미 조직이 있으면 owner만 수정 가능(member는 403 —
// "member(파이프라인 조회+참여만)" 스펙).
func (s *Server) handleUpsertCompanyProfile(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.currentUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	var req companyProfileRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_body"})
		return
	}
	for _, g := range req.Industry {
		if !isKnownIndustryGroup(g) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_industry_group"})
			return
		}
	}

	// foundingDate — 제공되면 파싱해서 businessAgeYears를 서버에서 다시
	// 계산한다(계산식을 클라이언트에 의존하지 않음). nil이면 두 컬럼 다
	// 건드리지 않고(COALESCE) 기존 값을 그대로 둔다 — INSERT/UPDATE 각각
	// 아래에서 이 두 변수를 그대로 파라미터로 쓴다.
	var foundingDate *time.Time
	var businessAgeYears *float64
	if req.FoundingDate != nil && *req.FoundingDate != "" {
		parsed, err := time.Parse("2006-01-02", *req.FoundingDate)
		if err != nil || parsed.After(time.Now()) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_founding_date"})
			return
		}
		foundingDate = &parsed
		age := computeBusinessAgeYears(parsed)
		businessAgeYears = &age
	}

	businessType := pq.Array(req.BusinessType)
	industry := pq.Array(req.Industry)
	licenses := pq.Array(req.Licenses)
	certifications := pq.Array(req.Certifications)

	existing, err := s.getCompanyProfile(r, userID)
	if err != nil {
		s.logger.Error("company-profile: lookup failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}

	if existing == nil {
		// 2026-08-03 온보딩 재설계 이후로는 회사 프로필이 회원가입과 완전히
		// 분리됐다 — 회사 프로필 생성 자체는 아무 때나(사업자등록증 업로드
		// 온보딩, 또는 나중에 수동 입력) 일어날 수 있다. 다만 그와 무관하게
		// "휴대폰번호 SMS 인증을 마친 계정"이라는 최소 전제는 여기서도
		// 그대로 지킨다 — phone_number 존재 여부(과거 체크)가 아니라
		// phone_verified_at으로 확인한다(더 강한 조건 — 형식만 맞고
		// 검증되지 않은 번호는 통과 못 함). 단, 관리자가 휴대폰 인증
		// 자체를 껐다면(2026-08-04, system_settings.go) phone_verified_at은
		// 그 계정에서 영원히 NULL일 수 있으므로 이 가드 자체를 건너뛴다
		// — 안 그러면 그런 계정은 회사 프로필을 영영 만들 수 없게 된다.
		phoneRequired, err := s.getSystemSettingBool(r.Context(), phoneVerificationRequiredSettingKey, defaultPhoneVerificationRequired)
		if err != nil {
			s.logger.Error("company-profile: phone verification setting lookup failed", "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
			return
		}
		if phoneRequired {
			var phoneVerified bool
			if err := s.db.QueryRowContext(r.Context(),
				`SELECT phone_verified_at IS NOT NULL FROM users WHERE id = $1`, userID,
			).Scan(&phoneVerified); err != nil {
				s.logger.Error("company-profile: phone verification lookup failed", "error", err)
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
				return
			}
			if !phoneVerified {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "phone_number_required"})
				return
			}
		}

		tx, err := s.db.BeginTx(r.Context(), nil)
		if err != nil {
			s.logger.Error("company-profile: begin tx failed", "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
			return
		}
		defer tx.Rollback()

		var newID string
		if err := tx.QueryRowContext(r.Context(), `
			INSERT INTO company_profiles (
				user_id, business_type, region, industry, business_age_years, founding_date,
				revenue_amount, employee_count, company_size, licenses, certifications,
				direct_production_cert, max_performance_amount, credit_rating, phone_number,
				business_registration_number, company_name, representative_name, address
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19)
			RETURNING id`,
			userID, businessType, req.Region, industry, businessAgeYears, foundingDate,
			req.RevenueAmount, req.EmployeeCount, req.CompanySize, licenses, certifications,
			req.DirectProductionCert, req.MaxPerformanceAmount, req.CreditRating, req.PhoneNumber,
			req.BusinessRegistrationNumber, req.CompanyName, req.RepresentativeName, req.Address,
		).Scan(&newID); err != nil {
			s.logger.Error("company-profile: insert failed", "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
			return
		}
		if _, err := tx.ExecContext(r.Context(),
			`INSERT INTO company_members (company_profile_id, user_id, role) VALUES ($1,$2,'owner')`,
			newID, userID,
		); err != nil {
			s.logger.Error("company-profile: owner membership insert failed", "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
			return
		}
		if err := tx.Commit(); err != nil {
			s.logger.Error("company-profile: commit failed", "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
			return
		}
	} else {
		if existing.Role != "owner" {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "owner_only"})
			return
		}
		// business_age_years/founding_date만 COALESCE로 "안 보내면 기존 값
		// 유지" — 이 endpoint의 다른 필드는 전부 항상 전체 재전송을
		// 기대하지만(프론트 companyProfileUpdatePayload 관례), 개업일은
		// 실수로 안 보내는 것만으로 업력이 지워지는 사고를 막기 위한
		// 예외(companyProfileRequest.FoundingDate 주석 참고).
		_, err = s.db.ExecContext(r.Context(), `
			UPDATE company_profiles SET
				business_type = $2, region = $3, industry = $4,
				business_age_years = COALESCE($5, business_age_years), founding_date = COALESCE($6, founding_date),
				revenue_amount = $7, employee_count = $8, company_size = $9, licenses = $10,
				certifications = $11, direct_production_cert = $12,
				max_performance_amount = $13, credit_rating = $14, phone_number = $15,
				business_registration_number = $16, company_name = $17, representative_name = $18, address = $19
			WHERE id = $1`,
			existing.ID, businessType, req.Region, industry,
			businessAgeYears, foundingDate,
			req.RevenueAmount, req.EmployeeCount, req.CompanySize, licenses, certifications,
			req.DirectProductionCert, req.MaxPerformanceAmount, req.CreditRating, req.PhoneNumber,
			req.BusinessRegistrationNumber, req.CompanyName, req.RepresentativeName, req.Address)
		if err != nil {
			s.logger.Error("company-profile: update failed", "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
			return
		}
	}

	// 맞춤공고 "내 기본 조건" 동기화 — 2026-08-06. 온보딩 완료 시 자동
	// 생성된 조건(origin='onboarding')이 있으면 지역/업종을 방금 저장한
	// 값으로 계속 맞춰준다(사용자 확정: "갱신하는 게 자연스럽다"). 사용자가
	// 직접 만든 다른 조건들은 origin이 NULL이라 이 UPDATE 대상이 아니다.
	// 그 "기본 조건"을 사용자가 삭제했으면 이 UPDATE는 그냥 0행에 적용돼
	// 조용히 끝난다(되살리지 않음). 업종은 saved_searches와 마찬가지로
	// 첫 번째 값만 쓴다.
	var syncIndustry *string
	if len(req.Industry) > 0 {
		syncIndustry = &req.Industry[0]
	}
	if _, err := s.db.ExecContext(r.Context(), `
		UPDATE saved_searches SET region = $1, industry = $2, updated_at = now()
		WHERE user_id = $3 AND origin = 'onboarding'`,
		req.Region, syncIndustry, userID,
	); err != nil {
		s.logger.Error("company-profile: default saved search sync failed", "error", err)
	}

	profile, err := s.getCompanyProfile(r, userID)
	if err != nil {
		s.logger.Error("company-profile: reload failed", "error", err)
	}
	writeJSON(w, http.StatusOK, map[string]any{"companyProfile": profile})
}
