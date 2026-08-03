// legal_documents.go — 이용약관/개인정보처리방침(legal_documents 테이블)
// 공개 조회 API + 관리자 발행 API. 콘텐츠 안의 {brand_name}/{company_name}
// 등 토큰은 company_info(회사 정보, company_info.go)에서 매 조회 시
// 치환한다 — 회사정보를 이 문서에 중복 입력하지 않기 위한 장치로,
// banners.go의 {brand_name} 토큰 치환과 같은 패턴이다.
//
// ⚠️ 초기 시드 콘텐츠(migrate/legal_documents_seed.go)는 법률 검토 전
// 초안이다 — 그 경고를 관리자 화면에도 그대로 노출한다(handleAdminListLegalDocuments
// 응답의 draftWarning 필드).
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

const legalDocumentDraftWarning = "이 문서들은 표준 템플릿 기반 초안입니다. 실제 발행 전 반드시 법률 전문가(변호사)의 검토를 받아야 합니다."

var legalDocumentTypes = map[string]bool{"terms": true, "privacy": true}

// substituteCompanyInfoTokens replaces {brand_name}/{company_name}/... tokens
// in content with the current company_info row's values — nullable fields
// substitute to "" when empty(약관 본문에 "정보없음" 같은 이상한 문구가
// 남지 않도록, 그냥 빈 문자열로 치환). company_info는 항상 정확히 1행
// (id=1)인 싱글턴이라 조회 실패는 거의 없지만, 실패해도 원본 텍스트를
// 그대로 반환한다(토큰이 안 지워진 채로 보이는 게, 문서 조회 자체가
// 깨지는 것보다 낫다).
func (s *Server) substituteCompanyInfoTokens(ctx context.Context, content string) string {
	var brandName string
	var companyName, bizNumber, repName, address, phone, email *string
	err := s.db.QueryRowContext(ctx, `
		SELECT brand_name, company_name, business_registration_number, representative_name, address, main_phone, contact_email
		FROM company_info WHERE id = 1`,
	).Scan(&brandName, &companyName, &bizNumber, &repName, &address, &phone, &email)
	if err != nil {
		s.logger.Error("substitute-company-info-tokens: query failed", "error", err)
		return content
	}
	replacer := strings.NewReplacer(
		"{brand_name}", brandName,
		"{company_name}", strOrEmptyPtr(companyName),
		"{business_registration_number}", strOrEmptyPtr(bizNumber),
		"{representative_name}", strOrEmptyPtr(repName),
		"{address}", strOrEmptyPtr(address),
		"{main_phone}", strOrEmptyPtr(phone),
		"{contact_email}", strOrEmptyPtr(email),
	)
	return replacer.Replace(content)
}

func strOrEmptyPtr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

type legalDocumentDTO struct {
	Type          string    `json:"type"`
	Version       string    `json:"version"`
	Content       string    `json:"content"`
	EffectiveDate string    `json:"effectiveDate"`
	CreatedAt     time.Time `json:"createdAt"`
}

// handleGetLegalDocument — GET /api/legal-documents/{type}. 공개(로그인
// 불필요) — 회원가입 화면의 "보기" 링크, #/terms, #/privacy 화면이 쓴다.
func (s *Server) handleGetLegalDocument(w http.ResponseWriter, r *http.Request) {
	docType := r.PathValue("type")
	if !legalDocumentTypes[docType] {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_legal_document_type"})
		return
	}
	var doc legalDocumentDTO
	var effectiveDate time.Time
	err := s.db.QueryRowContext(r.Context(), `
		SELECT type, version, content, effective_date, created_at
		FROM legal_documents WHERE type = $1 AND is_active = true`,
		docType,
	).Scan(&doc.Type, &doc.Version, &doc.Content, &effectiveDate, &doc.CreatedAt)
	if err != nil {
		s.logger.Error("get-legal-document: query failed", "error", err, "type", docType)
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "legal_document_not_found"})
		return
	}
	doc.EffectiveDate = effectiveDate.Format("2006-01-02")
	doc.Content = s.substituteCompanyInfoTokens(r.Context(), doc.Content)
	writeJSON(w, http.StatusOK, doc)
}

type legalDocumentHistoryItem struct {
	ID            string    `json:"id"`
	Type          string    `json:"type"`
	Version       string    `json:"version"`
	Content       string    `json:"content"`
	EffectiveDate string    `json:"effectiveDate"`
	IsActive      bool      `json:"isActive"`
	CreatedAt     time.Time `json:"createdAt"`
}

// handleAdminListLegalDocuments — GET /api/admin/legal-documents. 두 타입
// 전체 이력(활성+과거 버전)을 최신순으로 반환한다 — content는 원본(토큰
// 치환 전) 그대로 내려줘서 관리자가 그 토큰을 보고 다음 버전 작성 시
// 재사용할 수 있게 한다(치환된 값을 보여주면 관리자가 토큰 문법을 몰라
// 회사정보를 문서에 직접 하드코딩하는 실수를 할 수 있음).
func (s *Server) handleAdminListLegalDocuments(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireSystemAdmin(w, r); !ok {
		return
	}
	rows, err := s.db.QueryContext(r.Context(), `
		SELECT id, type, version, content, effective_date, is_active, created_at
		FROM legal_documents ORDER BY type, created_at DESC`)
	if err != nil {
		s.logger.Error("admin-list-legal-documents: query failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	defer rows.Close()

	items := []legalDocumentHistoryItem{}
	for rows.Next() {
		var it legalDocumentHistoryItem
		var effectiveDate time.Time
		if err := rows.Scan(&it.ID, &it.Type, &it.Version, &it.Content, &effectiveDate, &it.IsActive, &it.CreatedAt); err != nil {
			s.logger.Error("admin-list-legal-documents: scan failed", "error", err)
			continue
		}
		it.EffectiveDate = effectiveDate.Format("2006-01-02")
		items = append(items, it)
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "draftWarning": legalDocumentDraftWarning})
}

type legalDocumentPublishRequest struct {
	Type          string `json:"type"`
	Version       string `json:"version"`
	Content       string `json:"content"`
	EffectiveDate string `json:"effectiveDate"`
}

// handleAdminPublishLegalDocument — POST /api/admin/legal-documents. 새
// 버전을 "발행"한다(초안 저장이 아니라 즉시 활성화) — 같은 type의 기존
// 활성 버전을 비활성화하고 새 행을 활성으로 추가하는 걸 한 트랜잭션으로
// 묶어서, 같은 type에 is_active=true가 2개 이상 동시에 존재하는 순간이
// 없게 한다. 이전 버전은 삭제하지 않고 이력으로 남긴다.
func (s *Server) handleAdminPublishLegalDocument(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireSystemAdmin(w, r); !ok {
		return
	}
	var req legalDocumentPublishRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_body"})
		return
	}
	if !legalDocumentTypes[req.Type] {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_legal_document_type"})
		return
	}
	version := strings.TrimSpace(req.Version)
	content := strings.TrimSpace(req.Content)
	if version == "" || content == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "version_and_content_required"})
		return
	}
	effectiveDate, err := time.Parse("2006-01-02", req.EffectiveDate)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_effective_date"})
		return
	}

	ctx := r.Context()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		s.logger.Error("admin-publish-legal-document: begin tx failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx,
		`UPDATE legal_documents SET is_active = false WHERE type = $1 AND is_active = true`, req.Type,
	); err != nil {
		s.logger.Error("admin-publish-legal-document: deactivate previous failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	var id string
	if err := tx.QueryRowContext(ctx, `
		INSERT INTO legal_documents (type, version, content, effective_date, is_active)
		VALUES ($1,$2,$3,$4,true) RETURNING id`,
		req.Type, version, content, effectiveDate,
	).Scan(&id); err != nil {
		s.logger.Error("admin-publish-legal-document: insert failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if err := tx.Commit(); err != nil {
		s.logger.Error("admin-publish-legal-document: commit failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": id, "status": "published"})
}
