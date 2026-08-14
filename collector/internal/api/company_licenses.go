// 면허·인증 확정 저장 + 목록 조회 (spec 3.3/3.4). company_licenses와
// company_certifications는 스키마가 동일하므로 테이블명만 다르게 받는
// 공용 핸들러로 구현한다(테이블명은 항상 이 파일의 상수 리터럴로만 넘어오고
// 요청 입력에서 오지 않으므로 SQL 인젝션 우려 없음).
package api

import (
	"context"
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

// directProductionStatusRequest — 참여검토에서 "직접생산확인 있어요/없어요"에 대한 답.
type directProductionStatusRequest struct {
	Status string `json:"status"`
}

// handleSetDirectProductionStatus — 직접생산확인 보유상태(3-상태)를 저장한다(STEP 2-B).
// 참여검토 질문에 "있어요/없어요"로 답하면 호출된다("잘 모르겠어요"는 프론트가
// 저장하지 않으므로 여기 오지 않는다). direct_production_status 컬럼에만 쓰고
// legacy boolean(direct_production_cert)은 건드리지 않는다 — 표시/온보딩 등 기존
// 경로 회귀를 피하려고(읽는 쪽 companyDirectProductionStatus가 새 컬럼 우선).
// status는 반드시 보유/미보유/확인되지않음 중 하나(미보유가 자동 처리되지 않도록).
func (s *Server) handleSetDirectProductionStatus(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.currentUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	profile, err := s.getCompanyProfile(r, userID)
	if err != nil {
		s.logger.Error("direct-production: profile lookup failed", "error", err)
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
	var req directProductionStatusRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_body"})
		return
	}
	req.Status = strings.TrimSpace(req.Status)
	if !validLicenseStatuses[req.Status] {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_status"})
		return
	}
	if _, err := s.db.ExecContext(r.Context(),
		`UPDATE company_profiles SET direct_production_status = $1 WHERE id = $2`,
		req.Status, profile.ID,
	); err != nil {
		s.logger.Error("direct-production: update failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": req.Status})
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

// displayNamesWithFallback — STEP 1 read-side canonical화(2026-08-14). 회사정보 조회 시
// 면허·인증 표시 목록을 구조화 테이블(canonical)에서 만든다: status='보유'인 이름(TRIM,
// 중복제거)을 우선하고, legacy TEXT[] 중 구조화에 없는 이름만 fallback으로 뒤에 붙인다
// (기존 운영 데이터 호환). 미보유/확인되지않음/삭제된 행은 표시에서 제외한다. 쿼리 실패
// 시에는 회귀 방지를 위해 legacy 배열을 그대로 반환한다. table은 코드 리터럴(인젝션 무관).
func (s *Server) displayNamesWithFallback(ctx context.Context, profileID, table string, legacy []string) []string {
	rows, err := s.db.QueryContext(ctx,
		fmt.Sprintf(`SELECT DISTINCT btrim(name) FROM %s WHERE company_profile_id = $1 AND status = '보유' AND btrim(name) <> ''`, table),
		profileID,
	)
	if err != nil {
		s.logger.Error("read-side canonical: structured names query failed", "error", err, "table", table)
		return legacy
	}
	defer rows.Close()
	seen := map[string]bool{}
	out := []string{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			continue
		}
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	for _, raw := range legacy {
		n := strings.TrimSpace(raw)
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	return out
}

// syncStructuredFromLegacyArray — STEP 1(회사정보 canonical 통일, 2026-08-14).
// 회사정보 모달/PUT /api/me/company-profile은 면허·인증을 company_profiles.licenses/
// certifications TEXT[]에만 저장해 왔는데, 실제 참가자격 판정(buildParticipationJudgment)은
// 구조화 테이블(company_licenses/certifications)만 읽는다 — 그래서 모달로 등록한 면허가
// 판정에서 무시되던 정합성 문제가 있었다(사전 진단 CRITICAL).
//
// 이 헬퍼는 프로필 배열(names)의 각 이름 중 구조화 테이블에 아직 "같은 이름(TRIM)"이 없는
// 것만 status='보유', confidence='C'(직접입력·증빙 없음)로 추가한다 — additive·멱등이며
// 파괴적 삭제/덮어쓰기를 하지 않는다(문서 증빙 B 또는 탭에서 등록한 구조화 행 보존, 중복 방지).
// TEXT[]는 하위호환으로 계속 저장되고 legacy로 유지된다(이번 단계에서 즉시 삭제하지 않음).
// best-effort: 실패해도 프로필 저장은 이미 끝났으므로 로그만 남기고 계속한다(saved_searches
// 동기화와 동일 패턴). table/category는 코드 리터럴이므로 SQL 인젝션 우려 없음.
func (s *Server) syncStructuredFromLegacyArray(ctx context.Context, profileID, table, category string, names []string) {
	for _, raw := range names {
		name := strings.TrimSpace(raw)
		if name == "" {
			continue
		}
		var exists bool
		if err := s.db.QueryRowContext(ctx,
			fmt.Sprintf(`SELECT EXISTS (SELECT 1 FROM %s WHERE company_profile_id = $1 AND btrim(name) = $2)`, table),
			profileID, name,
		).Scan(&exists); err != nil {
			s.logger.Error("legacy->structured sync: existence check failed", "error", err, "table", table)
			continue
		}
		if exists {
			continue
		}
		if _, err := s.db.ExecContext(ctx,
			fmt.Sprintf(`INSERT INTO %s (company_profile_id, category, name, confidence, status) VALUES ($1,$2,$3,'C','보유')`, table),
			profileID, category, name,
		); err != nil {
			s.logger.Error("legacy->structured sync: insert failed", "error", err, "table", table)
			continue
		}
	}
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
