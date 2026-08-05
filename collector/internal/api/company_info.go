// company_info.go — 랜딩페이지 푸터에 표시되는 회사 정보. company_info는
// 항상 정확히 1행(id=1)인 싱글턴 테이블(migrate.go의 ensureCompanyInfoTable
// 참고) — 조직(company_profiles)과 무관하게 이 서비스 운영사 자체의
// 정보라 사용자별/조직별 테이블이 아니다.
package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

type companyInfo struct {
	// BrandName — 나머지 필드와 달리 포인터가 아니다(NOT NULL). 사이트
	// 전체(탭 제목/헤더/사이드바/랜딩페이지 로고·푸터/PWA manifest)에
	// 쓰이는 값이라 빈 값을 허용하면 화면이 깨진다.
	BrandName                   string    `json:"brandName"`
	CompanyName                 *string   `json:"companyName"`
	BusinessRegistrationNumber  *string   `json:"businessRegistrationNumber"`
	RepresentativeName          *string   `json:"representativeName"`
	Address                     *string   `json:"address"`
	MainPhone                   *string   `json:"mainPhone"`
	ContactEmail                *string   `json:"contactEmail"`
	PartnershipEmail            *string   `json:"partnershipEmail"`
	PatentNumber                *string   `json:"patentNumber"`
	MailOrderRegistrationNumber *string   `json:"mailOrderRegistrationNumber"`
	UpdatedAt                   time.Time `json:"updatedAt"`
}

// fetchCompanyInfo — company_info(id=1 싱글턴) 조회를 한 곳으로 모았다.
// handleGetCompanyInfo(공개 API)와 sendRecommendationDigest(다이제스트
// 이메일 하단 회사정보, notifications.go)가 둘 다 이 함수를 쓴다 — 같은
// 조회 로직을 두 곳에서 각자 짜면 필드가 하나 늘 때마다 한쪽만 고치는
// 사고가 나기 쉽다.
func (s *Server) fetchCompanyInfo(ctx context.Context) (companyInfo, error) {
	var info companyInfo
	var companyName, bizNumber, repName, address, phone, email, partnershipEmail, patentNumber, mailOrderNumber sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT brand_name, company_name, business_registration_number, representative_name, address,
		       main_phone, contact_email, partnership_email, patent_number, mail_order_registration_number, updated_at
		FROM company_info WHERE id = 1`,
	).Scan(&info.BrandName, &companyName, &bizNumber, &repName, &address, &phone, &email, &partnershipEmail, &patentNumber, &mailOrderNumber, &info.UpdatedAt)
	if err != nil {
		return companyInfo{}, err
	}
	info.CompanyName = nullStringPtr(companyName)
	info.BusinessRegistrationNumber = nullStringPtr(bizNumber)
	info.RepresentativeName = nullStringPtr(repName)
	info.Address = nullStringPtr(address)
	info.MainPhone = nullStringPtr(phone)
	info.ContactEmail = nullStringPtr(email)
	info.PartnershipEmail = nullStringPtr(partnershipEmail)
	info.PatentNumber = nullStringPtr(patentNumber)
	info.MailOrderRegistrationNumber = nullStringPtr(mailOrderNumber)
	return info, nil
}

// handleGetCompanyInfo — GET /api/company-info(공개, 인증 불필요) —
// 랜딩페이지(비로그인 방문자)와 앱 부팅 시(브랜드명 적용) 둘 다 읽는다.
// brandName을 제외한 값이 없는 필드는 null로 내려가고, 프론트가 항목별로
// (그리고 전부 null이면 블록 전체를) 조용히 숨긴다.
func (s *Server) handleGetCompanyInfo(w http.ResponseWriter, r *http.Request) {
	info, err := s.fetchCompanyInfo(r.Context())
	if err != nil {
		s.logger.Error("get-company-info: query failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	writeJSON(w, http.StatusOK, info)
}

type companyInfoRequest struct {
	BrandName                   string  `json:"brandName"`
	CompanyName                 *string `json:"companyName"`
	BusinessRegistrationNumber  *string `json:"businessRegistrationNumber"`
	RepresentativeName          *string `json:"representativeName"`
	Address                     *string `json:"address"`
	MainPhone                   *string `json:"mainPhone"`
	ContactEmail                *string `json:"contactEmail"`
	PartnershipEmail            *string `json:"partnershipEmail"`
	PatentNumber                *string `json:"patentNumber"`
	MailOrderRegistrationNumber *string `json:"mailOrderRegistrationNumber"`
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
// brandName은 나머지 필드와 달리 필수 — 빈 문자열이면 저장 자체를
// 거부한다(사이트 전체에 표시되는 값이 빈 채로 저장되면 화면이 깨짐).
func (s *Server) handleAdminUpdateCompanyInfo(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireSystemAdmin(w, r); !ok {
		return
	}
	var req companyInfoRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_body"})
		return
	}
	brandName := strings.TrimSpace(req.BrandName)
	if brandName == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "brand_name_required"})
		return
	}

	_, err := s.db.ExecContext(r.Context(), `
		UPDATE company_info SET
			brand_name = $1, company_name = $2, business_registration_number = $3, representative_name = $4,
			address = $5, main_phone = $6, contact_email = $7, partnership_email = $8,
			patent_number = $9, mail_order_registration_number = $10, updated_at = now()
		WHERE id = 1`,
		brandName,
		normalizeCompanyInfoField(req.CompanyName),
		normalizeCompanyInfoField(req.BusinessRegistrationNumber),
		normalizeCompanyInfoField(req.RepresentativeName),
		normalizeCompanyInfoField(req.Address),
		normalizeCompanyInfoField(req.MainPhone),
		normalizeCompanyInfoField(req.ContactEmail),
		normalizeCompanyInfoField(req.PartnershipEmail),
		normalizeCompanyInfoField(req.PatentNumber),
		normalizeCompanyInfoField(req.MailOrderRegistrationNumber),
	)
	if err != nil {
		s.logger.Error("admin-update-company-info: update failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

// handleManifest — GET /manifest.json. PWA 설치(홈 화면 추가) 시 뜨는
// 이름도 브랜드명 변경에 맞춰 즉시 바뀌어야 해서, static/manifest.json
// 정적 파일 대신 이 핸들러가 요청마다 brand_name을 읽어 만들어 낸다
// (server.go에서 "/manifest.json" 정확 경로로 등록 — Go 1.22+ ServeMux는
// 더 구체적인 패턴을 "/" 캐치올보다 우선하므로 webui.Handler()의 정적
// 서빙과 충돌하지 않는다). 아이콘 경로 등 나머지 필드는 정적 파일 그대로.
func (s *Server) handleManifest(w http.ResponseWriter, r *http.Request) {
	var brandName string
	if err := s.db.QueryRowContext(r.Context(), `SELECT brand_name FROM company_info WHERE id = 1`).Scan(&brandName); err != nil {
		s.logger.Error("manifest: brand name query failed", "error", err)
		brandName = "공공사업 AI 비서"
	}
	// writeJSON을 안 쓰는 이유: 그 헬퍼가 Content-Type을 "application/json"
	// 으로 고정해버려서, 아래에서 지정하는 매니페스트 전용 MIME 타입이
	// 덮어써진다.
	w.Header().Set("Content-Type", "application/manifest+json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]any{
		"name":             brandName,
		"short_name":       brandName,
		"description":      "우리 회사에 맞는 공공사업을 AI가 찾아드립니다",
		"start_url":        "/",
		"display":          "standalone",
		"background_color": "#FFFFFF",
		"theme_color":      "#3182F6",
		"lang":             "ko",
		"icons": []map[string]string{
			{"src": "/icons/icon-192.png", "sizes": "192x192", "type": "image/png", "purpose": "any maskable"},
			{"src": "/icons/icon-512.png", "sizes": "512x512", "type": "image/png", "purpose": "any maskable"},
		},
	})
}
