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

	var userID, passwordHash string
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

	if bcrypt.CompareHashAndPassword([]byte(passwordHash), []byte(req.Password)) != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid_credentials"})
		return
	}

	s.setSessionCookie(w, userID)
	writeJSON(w, http.StatusOK, map[string]string{"id": userID, "email": email})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	s.clearSessionCookie(w)
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

type companyProfileDTO struct {
	ID                   string   `json:"id"`
	BusinessType         *string  `json:"businessType"`
	Region               *string  `json:"region"`
	Industry             *string  `json:"industry"`
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

func (s *Server) getCompanyProfile(r *http.Request, userID string) (*companyProfileDTO, error) {
	var p companyProfileDTO
	var businessType, region, industry, companySize, creditRating sql.NullString
	var businessAgeYears sql.NullFloat64
	var revenueAmount, employeeCount, maxPerformanceAmount sql.NullInt64
	var licenses, certs pq.StringArray

	err := s.db.QueryRowContext(r.Context(), `
		SELECT id, business_type, region, industry, business_age_years, revenue_amount,
		       employee_count, company_size, licenses, certifications,
		       direct_production_cert, max_performance_amount, credit_rating
		FROM company_profiles WHERE user_id = $1`, userID,
	).Scan(&p.ID, &businessType, &region, &industry, &businessAgeYears, &revenueAmount,
		&employeeCount, &companySize, &licenses, &certs,
		&p.DirectProductionCert, &maxPerformanceAmount, &creditRating)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	p.BusinessType = nullStringPtr(businessType)
	p.Region = nullStringPtr(region)
	p.Industry = nullStringPtr(industry)
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

	profile, err := s.getCompanyProfile(r, userID)
	if err != nil {
		s.logger.Error("me: profile query failed", "error", err)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"user": map[string]string{
			"id": userID, "email": email, "role": role, "plan": plan,
		},
		"companyProfile": profile,
	})
}

type companyProfileRequest struct {
	BusinessType         *string  `json:"businessType"`
	Region               *string  `json:"region"`
	Industry             *string  `json:"industry"`
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

	licenses := pq.Array(req.Licenses)
	certifications := pq.Array(req.Certifications)

	var existingID string
	err := s.db.QueryRowContext(r.Context(),
		`SELECT id FROM company_profiles WHERE user_id = $1`, userID,
	).Scan(&existingID)

	switch {
	case err == sql.ErrNoRows:
		_, err = s.db.ExecContext(r.Context(), `
			INSERT INTO company_profiles (
				user_id, business_type, region, industry, business_age_years,
				revenue_amount, employee_count, company_size, licenses, certifications,
				direct_production_cert, max_performance_amount, credit_rating
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
			userID, req.BusinessType, req.Region, req.Industry, req.BusinessAgeYears,
			req.RevenueAmount, req.EmployeeCount, req.CompanySize, licenses, certifications,
			req.DirectProductionCert, req.MaxPerformanceAmount, req.CreditRating)
	case err != nil:
		s.logger.Error("company-profile: lookup failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	default:
		_, err = s.db.ExecContext(r.Context(), `
			UPDATE company_profiles SET
				business_type = $2, region = $3, industry = $4, business_age_years = $5,
				revenue_amount = $6, employee_count = $7, company_size = $8, licenses = $9,
				certifications = $10, direct_production_cert = $11,
				max_performance_amount = $12, credit_rating = $13
			WHERE user_id = $1`,
			userID, req.BusinessType, req.Region, req.Industry, req.BusinessAgeYears,
			req.RevenueAmount, req.EmployeeCount, req.CompanySize, licenses, certifications,
			req.DirectProductionCert, req.MaxPerformanceAmount, req.CreditRating)
	}
	if err != nil {
		s.logger.Error("company-profile: upsert failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}

	profile, err := s.getCompanyProfile(r, userID)
	if err != nil {
		s.logger.Error("company-profile: reload failed", "error", err)
	}
	writeJSON(w, http.StatusOK, map[string]any{"companyProfile": profile})
}
