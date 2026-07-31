// 면허·인증 확정 저장 + 목록 조회 (spec 3.3/3.4). company_licenses와
// company_certifications는 스키마가 동일하므로 테이블명만 다르게 받는
// 공용 핸들러로 구현한다(테이블명은 항상 이 파일의 상수 리터럴로만 넘어오고
// 요청 입력에서 오지 않으므로 SQL 인젝션 우려 없음).
package api

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

var validLicenseStatuses = map[string]bool{
	"보유":     true,
	"미보유":    true,
	"확인되지않음": true,
}

type licenseRequest struct {
	Category           string  `json:"category"`
	Name               string  `json:"name"`
	RegistrationNumber *string `json:"registrationNumber"`
	IssuingAuthority   *string `json:"issuingAuthority"`
	IssuedAt           *string `json:"issuedAt"`  // YYYY-MM-DD
	ExpiresAt          *string `json:"expiresAt"` // YYYY-MM-DD
	ApplicableIndustry *string `json:"applicableIndustry"`
	Status             string  `json:"status"` // 보유/미보유/확인되지않음 중 하나 — 그 외 값은 거부
	SourceDocumentID   *string `json:"sourceDocumentId"`
}

type licenseItem struct {
	ID                  string     `json:"id"`
	Category            string     `json:"category"`
	Name                string     `json:"name"`
	RegistrationNumber  *string    `json:"registrationNumber"`
	IssuingAuthority    *string    `json:"issuingAuthority"`
	IssuedAt            *string    `json:"issuedAt"`
	ExpiresAt           *string    `json:"expiresAt"`
	ApplicableIndustry  *string    `json:"applicableIndustry"`
	SourceDocumentID    *string    `json:"sourceDocumentId"`
	Confidence          string     `json:"confidence"`
	Status              string     `json:"status"`
	VerificationExpired bool       `json:"verificationExpired"`
	VerifiedAt          *time.Time `json:"verifiedAt"`
	CreatedAt           time.Time  `json:"createdAt"`
	UpdatedAt           time.Time  `json:"updatedAt"`
}

func (s *Server) handleCreateLicense(w http.ResponseWriter, r *http.Request) {
	s.handleCreateLicenseOrCertification(w, r, "company_licenses")
}

func (s *Server) handleCreateCertification(w http.ResponseWriter, r *http.Request) {
	s.handleCreateLicenseOrCertification(w, r, "company_certifications")
}

func (s *Server) handleListLicenses(w http.ResponseWriter, r *http.Request) {
	s.handleListLicensesOrCertifications(w, r, "company_licenses")
}

func (s *Server) handleListCertifications(w http.ResponseWriter, r *http.Request) {
	s.handleListLicensesOrCertifications(w, r, "company_certifications")
}

// handleCreateLicenseOrCertification saves the user-confirmed (possibly
// user-edited) final values. confidence is derived, never client-supplied:
// 'B' when sourceDocumentId references a document actually uploaded by this
// company profile (AI candidate confirmed against real evidence), otherwise
// 'C' (typed in directly, no evidence attached). status must be exactly one
// of 보유/미보유/확인되지않음 — "정보없음"이 "미보유"로 자동 처리되지 않도록
// 호출하는 쪽(프론트)이 항상 명시적으로 값을 골라야 한다.
func (s *Server) handleCreateLicenseOrCertification(w http.ResponseWriter, r *http.Request, table string) {
	userID, ok := s.currentUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	profile, err := s.getCompanyProfile(r, userID)
	if err != nil {
		s.logger.Error("create-license: profile lookup failed", "error", err, "table", table)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if profile == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "company_profile_required"})
		return
	}
	if profile.Role != "owner" {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "owner_only"})
		return
	}

	var req licenseRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_body"})
		return
	}
	if strings.TrimSpace(req.Category) == "" || strings.TrimSpace(req.Name) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "category_and_name_required"})
		return
	}
	if !validLicenseStatuses[req.Status] {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_status"})
		return
	}
	issuedAt, err := parseOptionalDate(req.IssuedAt)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_issued_at"})
		return
	}
	expiresAt, err := parseOptionalDate(req.ExpiresAt)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_expires_at"})
		return
	}

	confidence := "C"
	if req.SourceDocumentID != nil && strings.TrimSpace(*req.SourceDocumentID) != "" {
		var owns bool
		err := s.db.QueryRowContext(r.Context(),
			`SELECT EXISTS (SELECT 1 FROM company_documents WHERE id = $1 AND company_profile_id = $2)`,
			*req.SourceDocumentID, profile.ID,
		).Scan(&owns)
		if err != nil {
			s.logger.Error("create-license: source document check failed", "error", err, "table", table)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
			return
		}
		if !owns {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_source_document"})
			return
		}
		confidence = "B"
	}

	query := fmt.Sprintf(`
		INSERT INTO %s (
			company_profile_id, category, name, registration_number, issuing_authority,
			issued_at, expires_at, applicable_industry, source_document_id, confidence, status, verified_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11, now())
		RETURNING id`, table)
	var id string
	err = s.db.QueryRowContext(r.Context(), query,
		profile.ID, req.Category, req.Name, req.RegistrationNumber, req.IssuingAuthority,
		issuedAt, expiresAt, req.ApplicableIndustry, req.SourceDocumentID, confidence, req.Status,
	).Scan(&id)
	if err != nil {
		s.logger.Error("create-license: insert failed", "error", err, "table", table)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}

	writeJSON(w, http.StatusCreated, map[string]string{"id": id, "confidence": confidence})
}

// handleListLicensesOrCertifications computes verificationExpired at read
// time (status='보유' AND expires_at가 지남) without touching the stored
// status — 삭제하지 않고 표시만 다르게 하라는 지시를 응답 레벨에서 구현.
func (s *Server) handleListLicensesOrCertifications(w http.ResponseWriter, r *http.Request, table string) {
	userID, ok := s.currentUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	profile, err := s.getCompanyProfile(r, userID)
	if err != nil {
		s.logger.Error("list-licenses: profile lookup failed", "error", err, "table", table)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if profile == nil {
		writeJSON(w, http.StatusOK, map[string]any{"items": []licenseItem{}})
		return
	}

	query := fmt.Sprintf(`
		SELECT id, category, name, registration_number, issuing_authority, issued_at, expires_at,
		       applicable_industry, source_document_id, confidence, status, verified_at, created_at, updated_at
		FROM %s WHERE company_profile_id = $1 ORDER BY created_at DESC`, table)
	rows, err := s.db.QueryContext(r.Context(), query, profile.ID)
	if err != nil {
		s.logger.Error("list-licenses: query failed", "error", err, "table", table)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	defer rows.Close()

	today := time.Now().Truncate(24 * time.Hour)
	items := []licenseItem{}
	for rows.Next() {
		var it licenseItem
		var regNum, issuer, industry, sourceDocID sql.NullString
		var issuedAt, expiresAt, verifiedAt sql.NullTime
		if err := rows.Scan(&it.ID, &it.Category, &it.Name, &regNum, &issuer, &issuedAt, &expiresAt,
			&industry, &sourceDocID, &it.Confidence, &it.Status, &verifiedAt, &it.CreatedAt, &it.UpdatedAt); err != nil {
			s.logger.Error("list-licenses: scan failed", "error", err, "table", table)
			continue
		}
		it.RegistrationNumber = nullStringPtr(regNum)
		it.IssuingAuthority = nullStringPtr(issuer)
		it.ApplicableIndustry = nullStringPtr(industry)
		it.SourceDocumentID = nullStringPtr(sourceDocID)
		if issuedAt.Valid {
			v := issuedAt.Time.Format("2006-01-02")
			it.IssuedAt = &v
		}
		if expiresAt.Valid {
			v := expiresAt.Time.Format("2006-01-02")
			it.ExpiresAt = &v
			it.VerificationExpired = it.Status == "보유" && expiresAt.Time.Before(today)
		}
		if verifiedAt.Valid {
			it.VerifiedAt = &verifiedAt.Time
		}
		items = append(items, it)
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func parseOptionalDate(s *string) (*time.Time, error) {
	if s == nil || strings.TrimSpace(*s) == "" {
		return nil, nil
	}
	t, err := time.Parse("2006-01-02", strings.TrimSpace(*s))
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// parseOptionalInt64/parseOptionalFloat64/parseTriStateBool are shared by
// company_financials.go/company_track_records.go/company_personnel.go for
// parsing AI-extracted candidate fields (always plain strings — same
// sanitize-then-parse approach as license candidates' date fields) and
// user-submitted form values alike. An empty string means "정보 없음",
// not zero/false — callers must not coerce a missing value into 0/false.
func parseOptionalInt64(s string) (*int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	v, err := strconv.ParseInt(strings.ReplaceAll(s, ",", ""), 10, 64)
	if err != nil {
		return nil, err
	}
	return &v, nil
}

func parseOptionalFloat64(s string) (*float64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	v, err := strconv.ParseFloat(strings.ReplaceAll(s, ",", ""), 64)
	if err != nil {
		return nil, err
	}
	return &v, nil
}

// parseTriStateBool maps the AI/manual-entry tri-state string ("예"/
// "아니오"/"확인불가" or empty) to a nullable bool — nil means "확인 안
// 됨", never silently false. Any other value is an error (caller should
// discard/blank the field rather than guess).
func parseTriStateBool(s string) (*bool, error) {
	switch strings.TrimSpace(s) {
	case "", "확인불가":
		return nil, nil
	case "예":
		v := true
		return &v, nil
	case "아니오":
		v := false
		return &v, nil
	default:
		return nil, fmt.Errorf("invalid tri-state value: %q", s)
	}
}
