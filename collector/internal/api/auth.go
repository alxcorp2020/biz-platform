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
	Email    string `json:"email"`
	Password string `json:"password"`
}

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

	var exists bool
	if err := s.db.QueryRowContext(r.Context(),
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

	var userID string
	if err := s.db.QueryRowContext(r.Context(),
		`INSERT INTO users (email, password_hash) VALUES ($1, $2) RETURNING id`,
		email, string(hash),
	).Scan(&userID); err != nil {
		s.logger.Error("signup: insert failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}

	s.setSessionCookie(w, userID)
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
	err := s.db.QueryRowContext(r.Context(),
		`SELECT id, password_hash FROM users WHERE email = $1`, email,
	).Scan(&userID, &passwordHash)
	if err == sql.ErrNoRows {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid_credentials"})
		return
	}
	if err != nil {
		s.logger.Error("login: query failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
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
	Role                 string   `json:"role"`
	BusinessType         []string `json:"businessType"`
	Region               *string  `json:"region"`
	Industry             []string `json:"industry"`
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
}

// getCompanyProfile resolves "which organization does this user belong to"
// via company_members — the single chokepoint for the whole multi-user-per-
// org model (팀기능). Every other handler that needs a company profile goes
// through this function, so redirecting the JOIN here (company_members →
// company_profiles) instead of the old direct company_profiles.user_id
// lookup is what makes every one of those ~15 call sites work correctly for
// both owners and members without individually touching them.
func (s *Server) getCompanyProfile(r *http.Request, userID string) (*companyProfileDTO, error) {
	var p companyProfileDTO
	var region, companySize, creditRating, employeeCountConfidence, phoneNumber sql.NullString
	var businessAgeYears sql.NullFloat64
	var revenueAmount, employeeCount, maxPerformanceAmount sql.NullInt64
	var employeeCountVerifiedAt sql.NullTime
	var businessType, industry, licenses, certs pq.StringArray
	var notificationDaysBefore pq.Int64Array

	err := s.db.QueryRowContext(r.Context(), `
		SELECT cp.id, cm.role, cp.business_type, cp.region, cp.industry, cp.business_age_years, cp.revenue_amount,
		       cp.employee_count, cp.company_size, cp.licenses, cp.certifications,
		       cp.direct_production_cert, cp.max_performance_amount, cp.credit_rating,
		       cp.employee_count_confidence, cp.employee_count_verified_at,
		       cp.email_notifications_enabled, cp.phone_number, cp.sms_notifications_enabled,
		       cp.notification_days_before
		FROM company_members cm
		JOIN company_profiles cp ON cp.id = cm.company_profile_id
		WHERE cm.user_id = $1`, userID,
	).Scan(&p.ID, &p.Role, &businessType, &region, &industry, &businessAgeYears, &revenueAmount,
		&employeeCount, &companySize, &licenses, &certs,
		&p.DirectProductionCert, &maxPerformanceAmount, &creditRating,
		&employeeCountConfidence, &employeeCountVerifiedAt,
		&p.EmailNotificationsEnabled, &phoneNumber, &p.SMSNotificationsEnabled,
		&notificationDaysBefore)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	p.BusinessType = []string(businessType)
	p.Region = nullStringPtr(region)
	p.Industry = []string(industry)
	p.CompanySize = nullStringPtr(companySize)
	p.CreditRating = nullStringPtr(creditRating)
	p.RevenueAmount = nullInt64Ptr(revenueAmount)
	p.EmployeeCount = nullInt64Ptr(employeeCount)
	p.MaxPerformanceAmount = nullInt64Ptr(maxPerformanceAmount)
	if businessAgeYears.Valid {
		p.BusinessAgeYears = &businessAgeYears.Float64
	}
	p.Licenses = []string(licenses)
	p.Certifications = []string(certs)
	p.EmployeeCountConfidence = nullStringPtr(employeeCountConfidence)
	if employeeCountVerifiedAt.Valid {
		p.EmployeeCountVerifiedAt = &employeeCountVerifiedAt.Time
	}
	p.PhoneNumber = nullStringPtr(phoneNumber)
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
	err := s.db.QueryRowContext(r.Context(),
		`SELECT email, role, plan FROM users WHERE id = $1`, userID,
	).Scan(&email, &role, &plan)
	if err == sql.ErrNoRows {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	if err != nil {
		s.logger.Error("me: query failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
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
		},
		"companyProfile": profile,
	})
}

type companyProfileRequest struct {
	BusinessType         []string `json:"businessType"`
	Region               *string  `json:"region"`
	Industry             []string `json:"industry"`
	BusinessAgeYears     *float64 `json:"businessAgeYears"`
	RevenueAmount        *int64   `json:"revenueAmount"`
	EmployeeCount        *int64   `json:"employeeCount"`
	CompanySize          *string  `json:"companySize"`
	Licenses             []string `json:"licenses"`
	Certifications       []string `json:"certifications"`
	DirectProductionCert bool     `json:"directProductionCert"`
	MaxPerformanceAmount *int64   `json:"maxPerformanceAmount"`
	CreditRating         *string  `json:"creditRating"`
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
				user_id, business_type, region, industry, business_age_years,
				revenue_amount, employee_count, company_size, licenses, certifications,
				direct_production_cert, max_performance_amount, credit_rating
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
			RETURNING id`,
			userID, businessType, req.Region, industry, req.BusinessAgeYears,
			req.RevenueAmount, req.EmployeeCount, req.CompanySize, licenses, certifications,
			req.DirectProductionCert, req.MaxPerformanceAmount, req.CreditRating,
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
		_, err = s.db.ExecContext(r.Context(), `
			UPDATE company_profiles SET
				business_type = $2, region = $3, industry = $4, business_age_years = $5,
				revenue_amount = $6, employee_count = $7, company_size = $8, licenses = $9,
				certifications = $10, direct_production_cert = $11,
				max_performance_amount = $12, credit_rating = $13
			WHERE id = $1`,
			existing.ID, businessType, req.Region, industry, req.BusinessAgeYears,
			req.RevenueAmount, req.EmployeeCount, req.CompanySize, licenses, certifications,
			req.DirectProductionCert, req.MaxPerformanceAmount, req.CreditRating)
		if err != nil {
			s.logger.Error("company-profile: update failed", "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
			return
		}
	}

	profile, err := s.getCompanyProfile(r, userID)
	if err != nil {
		s.logger.Error("company-profile: reload failed", "error", err)
	}
	writeJSON(w, http.StatusOK, map[string]any{"companyProfile": profile})
}
