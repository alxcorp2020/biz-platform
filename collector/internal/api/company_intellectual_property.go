// 지식재산권(특허·상표·디자인·실용신안) — 증빙서류 17종 확대의 일부.
// 면허/인증(company_licenses/certifications)과 달리 "보유 여부"가 아니라
// 출원~등록~거절~소멸까지 상태가 이어지는 별개 개념이라 새 테이블/새
// 업로드 흐름으로 분리했다(company_documents.go의 일반 면허·인증
// 업로드에서는 더 이상 특허·상표 등록증을 문서 종류로 제안하지 않는다).
// 그 외 패턴(업로드+AI추출→사용자 확인→저장, confidence A~D, 매직바이트/
// 10MB 검증은 receiveCompanyDocument 공용)은 company_track_records.go와 동일.
package api

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
)

var ipTypes = []string{"특허", "상표", "디자인", "실용신안"}
var ipStatuses = []string{"등록", "출원중", "거절", "소멸", "확인필요"}

type ipCandidate struct {
	IPType             string `json:"ipType"`
	Title              string `json:"title"`
	ApplicationNumber  string `json:"applicationNumber"`
	RegistrationNumber string `json:"registrationNumber"`
	ApplicantName      string `json:"applicantName"`
	ApplicationDate    string `json:"applicationDate"`
	RegistrationDate   string `json:"registrationDate"`
	ExpiresAt          string `json:"expiresAt"`
	Status             string `json:"status"`
}

func (s *Server) handleUploadIPDocument(w http.ResponseWriter, r *http.Request) {
	_, documentID, body, ext, mediaType, ok := s.receiveCompanyDocument(w, r, documentKindIntellectualProperty)
	if !ok {
		return
	}
	candidate, err := s.extractIPCandidate(r.Context(), body, ext, mediaType)
	if err != nil {
		s.logger.Error("upload-ip-document: claude extraction failed", "error", err)
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "extraction_failed", "documentId": documentID})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"documentId": documentID, "candidate": candidate})
}

func (s *Server) extractIPCandidate(ctx context.Context, body []byte, ext, mediaType string) (*ipCandidate, error) {
	b64 := base64.StdEncoding.EncodeToString(body)
	var docBlock anthropic.ContentBlockParamUnion
	if ext == "pdf" {
		docBlock = anthropic.NewDocumentBlock(anthropic.Base64PDFSourceParam{Data: b64})
	} else {
		docBlock = anthropic.NewImageBlockBase64(mediaType, b64)
	}

	tool := anthropic.ToolParam{
		Name: "extract_ip_fields",
		Description: anthropic.String(
			"업로드된 지식재산권 등록증(특허등록증/상표등록증/디자인등록증/실용신안등록증) 또는 출원 관련 " +
				"서류에서 실제로 문서에 적혀 있는 정보만 추출합니다. 문서에 없는 내용은 절대 만들어내지 마세요. " +
				"확인할 수 없는 필드는 빈 문자열로 두세요.",
		),
		InputSchema: anthropic.ToolInputSchemaParam{
			Properties: map[string]any{
				"ipType":             map[string]any{"type": "string", "description": "지식재산권 종류", "enum": ipTypes},
				"title":              map[string]any{"type": "string", "description": "발명의 명칭 또는 상표명/디자인명. 없으면 빈 문자열"},
				"applicationNumber":  map[string]any{"type": "string", "description": "출원번호. 없으면 빈 문자열"},
				"registrationNumber": map[string]any{"type": "string", "description": "등록번호. 아직 출원 단계라 없으면 빈 문자열"},
				"applicantName":      map[string]any{"type": "string", "description": "출원인/권리자명. 없으면 빈 문자열"},
				"applicationDate":    map[string]any{"type": "string", "description": "출원일(YYYY-MM-DD). 없으면 빈 문자열"},
				"registrationDate":   map[string]any{"type": "string", "description": "등록일(YYYY-MM-DD). 없으면 빈 문자열"},
				"expiresAt":          map[string]any{"type": "string", "description": "존속기간 만료일(YYYY-MM-DD). 없으면 빈 문자열"},
				"status":             map[string]any{"type": "string", "description": "현재 상태 — 문서에서 명확히 확인 안 되면 확인필요", "enum": ipStatuses},
			},
			Required: []string{
				"ipType", "title", "applicationNumber", "registrationNumber", "applicantName",
				"applicationDate", "registrationDate", "expiresAt", "status",
			},
			ExtraFields: map[string]any{"additionalProperties": false},
		},
		Strict: anthropic.Bool(true),
	}

	resp, err := s.anthropicClient.Messages.New(ctx, anthropic.MessageNewParams{
		Model:      companyDocumentModel,
		MaxTokens:  1024,
		Tools:      []anthropic.ToolUnionParam{{OfTool: &tool}},
		ToolChoice: anthropic.ToolChoiceParamOfTool("extract_ip_fields"),
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(docBlock, anthropic.NewTextBlock("이 지식재산권 증빙서류에서 정보를 추출하세요.")),
		},
	})
	if err != nil {
		var apiErr *anthropic.Error
		if errors.As(err, &apiErr) {
			return nil, fmt.Errorf("claude api error (status %d): %w", apiErr.StatusCode, err)
		}
		return nil, err
	}

	for _, block := range resp.Content {
		if tu, ok := block.AsAny().(anthropic.ToolUseBlock); ok {
			var candidate ipCandidate
			if err := json.Unmarshal(tu.Input, &candidate); err != nil {
				return nil, fmt.Errorf("parse tool input: %w", err)
			}
			sanitizeIPCandidate(&candidate)
			return &candidate, nil
		}
	}
	return nil, fmt.Errorf("no tool_use block in response (stop_reason=%s)", resp.StopReason)
}

func sanitizeIPCandidate(c *ipCandidate) {
	if c.IPType != "" && !containsString(ipTypes, c.IPType) {
		c.IPType = ""
	}
	if c.Status != "" && !containsString(ipStatuses, c.Status) {
		c.Status = ""
	}
	for _, f := range []*string{&c.ApplicationDate, &c.RegistrationDate, &c.ExpiresAt} {
		if *f != "" && !isBlankOrValidDate(*f) {
			*f = ""
		}
	}
	for _, f := range []*string{&c.Title, &c.ApplicantName} {
		if looksMalformed(*f) {
			*f = ""
		}
	}
}

type ipRequest struct {
	IPType             string  `json:"ipType"`
	Title              string  `json:"title"`
	ApplicationNumber  *string `json:"applicationNumber"`
	RegistrationNumber *string `json:"registrationNumber"`
	ApplicantName      *string `json:"applicantName"`
	ApplicationDate    *string `json:"applicationDate"`
	RegistrationDate   *string `json:"registrationDate"`
	ExpiresAt          *string `json:"expiresAt"`
	Status             string  `json:"status"`
	SourceDocumentID   *string `json:"sourceDocumentId"`
}

func (s *Server) handleCreateIP(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.currentUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	profile, err := s.getCompanyProfile(r, userID)
	if err != nil {
		s.logger.Error("create-ip: profile lookup failed", "error", err)
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

	var req ipRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_body"})
		return
	}
	if strings.TrimSpace(req.Title) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "title_required"})
		return
	}
	if !containsString(ipTypes, req.IPType) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_ip_type"})
		return
	}
	if !containsString(ipStatuses, req.Status) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_status"})
		return
	}

	applicationDate, err1 := parseOptionalDate(req.ApplicationDate)
	registrationDate, err2 := parseOptionalDate(req.RegistrationDate)
	expiresAt, err3 := parseOptionalDate(req.ExpiresAt)
	if err1 != nil || err2 != nil || err3 != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_field_format"})
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
			s.logger.Error("create-ip: source document check failed", "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
			return
		}
		if !owns {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_source_document"})
			return
		}
		confidence = "B"
	}

	var id string
	err = s.db.QueryRowContext(r.Context(), `
		INSERT INTO company_intellectual_property (
			company_profile_id, ip_type, title, application_number, registration_number, applicant_name,
			application_date, registration_date, expires_at, status, source_document_id, confidence, verified_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12, now())
		RETURNING id`,
		profile.ID, req.IPType, req.Title, req.ApplicationNumber, req.RegistrationNumber, req.ApplicantName,
		applicationDate, registrationDate, expiresAt, req.Status, req.SourceDocumentID, confidence,
	).Scan(&id)
	if err != nil {
		s.logger.Error("create-ip: insert failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}

	writeJSON(w, http.StatusCreated, map[string]string{"id": id, "confidence": confidence})
}

type ipItem struct {
	ID                 string     `json:"id"`
	IPType             string     `json:"ipType"`
	Title              string     `json:"title"`
	ApplicationNumber  *string    `json:"applicationNumber"`
	RegistrationNumber *string    `json:"registrationNumber"`
	ApplicantName      *string    `json:"applicantName"`
	ApplicationDate    *string    `json:"applicationDate"`
	RegistrationDate   *string    `json:"registrationDate"`
	ExpiresAt          *string    `json:"expiresAt"`
	Status             string     `json:"status"`
	SourceDocumentID   *string    `json:"sourceDocumentId"`
	Confidence         string     `json:"confidence"`
	VerifiedAt         *time.Time `json:"verifiedAt"`
	CreatedAt          time.Time  `json:"createdAt"`
	UpdatedAt          time.Time  `json:"updatedAt"`
}

func (s *Server) handleListIP(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.currentUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	profile, err := s.getCompanyProfile(r, userID)
	if err != nil {
		s.logger.Error("list-ip: profile lookup failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if profile == nil {
		writeJSON(w, http.StatusOK, map[string]any{"items": []ipItem{}})
		return
	}

	rows, err := s.db.QueryContext(r.Context(), `
		SELECT id, ip_type, title, application_number, registration_number, applicant_name,
		       application_date, registration_date, expires_at, status, source_document_id,
		       confidence, verified_at, created_at, updated_at
		FROM company_intellectual_property WHERE company_profile_id = $1 ORDER BY created_at DESC`, profile.ID)
	if err != nil {
		s.logger.Error("list-ip: query failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	defer rows.Close()

	items := []ipItem{}
	for rows.Next() {
		var it ipItem
		var applicationNumber, registrationNumber, applicantName, sourceDocID sql.NullString
		var applicationDate, registrationDate, expiresAt, verifiedAt sql.NullTime
		if err := rows.Scan(&it.ID, &it.IPType, &it.Title, &applicationNumber, &registrationNumber, &applicantName,
			&applicationDate, &registrationDate, &expiresAt, &it.Status, &sourceDocID,
			&it.Confidence, &verifiedAt, &it.CreatedAt, &it.UpdatedAt); err != nil {
			s.logger.Error("list-ip: scan failed", "error", err)
			continue
		}
		it.ApplicationNumber = nullStringPtr(applicationNumber)
		it.RegistrationNumber = nullStringPtr(registrationNumber)
		it.ApplicantName = nullStringPtr(applicantName)
		it.SourceDocumentID = nullStringPtr(sourceDocID)
		if applicationDate.Valid {
			v := applicationDate.Time.Format("2006-01-02")
			it.ApplicationDate = &v
		}
		if registrationDate.Valid {
			v := registrationDate.Time.Format("2006-01-02")
			it.RegistrationDate = &v
		}
		if expiresAt.Valid {
			v := expiresAt.Time.Format("2006-01-02")
			it.ExpiresAt = &v
		}
		if verifiedAt.Valid {
			it.VerifiedAt = &verifiedAt.Time
		}
		items = append(items, it)
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}
