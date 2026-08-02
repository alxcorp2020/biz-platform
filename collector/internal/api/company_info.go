// company_info.go — 랜딩페이지 푸터에 표시되는 회사 정보. company_info는
// 항상 정확히 1행(id=1)인 싱글턴 테이블(migrate.go의 ensureCompanyInfoTable
// 참고) — 조직(company_profiles)과 무관하게 이 서비스 운영사 자체의
// 정보라 사용자별/조직별 테이블이 아니다.
package api

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

type companyInfo struct {
	CompanyName                *string   `json:"companyName"`
	BusinessRegistrationNumber *string   `json:"businessRegistrationNumber"`
	RepresentativeName         *string   `json:"representativeName"`
	Address                    *string   `json:"address"`
	MainPhone                  *string   `json:"mainPhone"`
	ContactEmail               *string   `json:"contactEmail"`
	PartnershipEmail           *string   `json:"partnershipEmail"`
	PatentNumber               *string   `json:"patentNumber"`
	UpdatedAt                  time.Time `json:"updatedAt"`
}

// handleGetCompanyInfo — GET /api/company-info(공개, 인증 불필요) —
// 랜딩페이지(비로그인 방문자)가 읽는다. 값이 없는 필드는 null로 내려가고,
// 프론트가 항목별로(그리고 전부 null이면 블록 전체를) 조용히 숨긴다.
func (s *Server) handleGetCompanyInfo(w http.ResponseWriter, r *http.Request) {
	var info companyInfo
	var companyName, bizNumber, repName, address, phone, email, partnershipEmail, patentNumber sql.NullString
	err := s.db.QueryRowContext(r.Context(), `
		SELECT company_name, business_registration_number, representative_name, address,
		       main_phone, contact_email, partnership_email, patent_number, updated_at
		FROM company_info WHERE id = 1`,
	).Scan(&companyName, &bizNumber, &repName, &address, &phone, &email, &partnershipEmail, &patentNumber, &info.UpdatedAt)
	if err != nil {
		s.logger.Error("get-company-info: query failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	info.CompanyName = nullStringPtr(companyName)
	info.BusinessRegistrationNumber = nullStringPtr(bizNumber)
	info.RepresentativeName = nullStringPtr(repName)
	info.Address = nullStringPtr(address)
	info.MainPhone = nullStringPtr(phone)
	info.ContactEmail = nullStringPtr(email)
	info.PartnershipEmail = nullStringPtr(partnershipEmail)
	info.PatentNumber = nullStringPtr(patentNumber)
	writeJSON(w, http.StatusOK, info)
}

type companyInfoRequest struct {
	CompanyName                *string `json:"companyName"`
	BusinessRegistrationNumber *string `json:"businessRegistrationNumber"`
	RepresentativeName         *string `json:"representativeName"`
	Address                    *string `json:"address"`
	MainPhone                  *string `json:"mainPhone"`
	ContactEmail               *string `json:"contactEmail"`
	PartnershipEmail           *string `json:"partnershipEmail"`
	PatentNumber               *string `json:"patentNumber"`
}

// normalizeCompanyInfoField trims whitespace and converts an empty result to
// nil — "값이 비어있으면(null 또는 빈 문자열)" 조건부 숨김 기준을 저장
// 단계에서부터 통일해둔다(빈 문자열 ""과 NULL을 프론트가 따로 구분해
// 처리할 필요가 없게).
func normalizeCompanyInfoField(v *string) *string {
	if v == nil {
		return nil
	}
	trimmed := strings.TrimSpace(*v)
	if trimmed == "" {
		return nil
	}
	return &trimmed
}

// handleAdminUpdateCompanyInfo — PUT /api/admin/company-info(관리자 전용).
// 항상 id=1 행 하나만 UPDATE한다(싱글턴이라 INSERT/식별 로직 불필요).
func (s *Server) handleAdminUpdateCompanyInfo(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireSystemAdmin(w, r); !ok {
		return
	}
	var req companyInfoRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_body"})
		return
	}

	_, err := s.db.ExecContext(r.Context(), `
		UPDATE company_info SET
			company_name = $1, business_registration_number = $2, representative_name = $3,
			address = $4, main_phone = $5, contact_email = $6, partnership_email = $7,
			patent_number = $8, updated_at = now()
		WHERE id = 1`,
		normalizeCompanyInfoField(req.CompanyName),
		normalizeCompanyInfoField(req.BusinessRegistrationNumber),
		normalizeCompanyInfoField(req.RepresentativeName),
		normalizeCompanyInfoField(req.Address),
		normalizeCompanyInfoField(req.MainPhone),
		normalizeCompanyInfoField(req.ContactEmail),
		normalizeCompanyInfoField(req.PartnershipEmail),
		normalizeCompanyInfoField(req.PatentNumber),
	)
	if err != nil {
		s.logger.Error("admin-update-company-info: update failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}
