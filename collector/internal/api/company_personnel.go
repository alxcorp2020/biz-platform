// 인력정보 — company_licenses/certifications와 같은 패턴이지만 건별
// 리스트다(면허/인증의 status, 재무의 fiscal_year upsert 같은 개념 없음).
//
// 개인정보 최소화 원칙: 이름/연락처 등 개인식별정보는 저장하지 않는다.
// 증빙서류(기술인력현황표 등)에 이름이 적혀 있어도 AI 추출 프롬프트에서
// 명시적으로 제외를 지시하고, 스키마 자체에도 그런 필드를 두지 않는다 —
// 매칭에 필요한 수준(직무/기술분야/경력/등급/자격)까지만 다룬다.
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
	"github.com/lib/pq"
)

type personnelCandidate struct {
	Role           string `json:"role"`
	TechField      string `json:"techField"`
	CareerYears    string `json:"careerYears"`
	TechGrade      string `json:"techGrade"`
	Qualifications string `json:"qualifications"` // 콤마로 구분된 자격 목록(서버에서 배열로 분리)
	RecentProject  string `json:"recentProject"`
	AvailableFrom  string `json:"availableFrom"`
}

func (s *Server) handleUploadPersonnelDocument(w http.ResponseWriter, r *http.Request) {
	_, documentID, body, ext, mediaType, ok := s.receiveCompanyDocument(w, r, documentKindPersonnel)
	if !ok {
		return
	}
	candidate, err := s.extractPersonnelCandidate(r.Context(), body, ext, mediaType)
	if err != nil {
		s.logger.Error("upload-personnel-document: claude extraction failed", "error", err)
		reason := classifyExtractionFailureReason(err)
		s.markDocumentExtractionResult(r.Context(), documentID, extractionStatusFailed, &reason)
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "extraction_failed", "documentId": documentID})
		return
	}
	s.markDocumentExtractionResult(r.Context(), documentID, extractionStatusSuccess, nil)
	writeJSON(w, http.StatusOK, map[string]any{"documentId": documentID, "candidate": candidate})
}

func (s *Server) extractPersonnelCandidate(ctx context.Context, body []byte, ext, mediaType string) (*personnelCandidate, error) {
	b64 := base64.StdEncoding.EncodeToString(body)
	var docBlock anthropic.ContentBlockParamUnion
	if ext == "pdf" {
		docBlock = anthropic.NewDocumentBlock(anthropic.Base64PDFSourceParam{Data: b64})
	} else {
		docBlock = anthropic.NewImageBlockBase64(mediaType, b64)
	}

	tool := anthropic.ToolParam{
		Name: "extract_personnel_fields",
		Description: anthropic.String(
			"업로드된 기술인력현황표/경력증명서에서 실제로 문서에 적혀 있는 정보만 추출합니다. " +
				"문서에 없는 내용은 절대 만들어내지 마세요. 확인할 수 없는 필드는 빈 문자열로 두세요. " +
				"매우 중요: 이름, 생년월일, 연락처, 주소, 주민등록번호 등 개인식별정보는 문서에 있더라도 " +
				"절대 추출하지 마세요 — 직무/기술분야/경력/자격/등급 등 매칭에 필요한 정보만 추출합니다.",
		),
		InputSchema: anthropic.ToolInputSchemaParam{
			Properties: map[string]any{
				"role":           map[string]any{"type": "string", "description": "직무(예: PM, 개발자, 설계기술자 등). 없으면 빈 문자열"},
				"techField":      map[string]any{"type": "string", "description": "기술분야. 없으면 빈 문자열"},
				"careerYears":    map[string]any{"type": "string", "description": "경력연수(숫자만, 소수 가능). 없으면 빈 문자열"},
				"techGrade":      map[string]any{"type": "string", "description": "기술등급(특급/고급/중급/초급 등). 없으면 빈 문자열"},
				"qualifications": map[string]any{"type": "string", "description": "보유자격을 콤마로 구분해 나열. 없으면 빈 문자열"},
				"recentProject":  map[string]any{"type": "string", "description": "최근수행실적 요약(간단 텍스트, 개인식별정보 제외). 없으면 빈 문자열"},
				"availableFrom":  map[string]any{"type": "string", "description": "투입가능일(YYYY-MM-DD). 없으면 빈 문자열"},
			},
			Required:    []string{"role", "techField", "careerYears", "techGrade", "qualifications", "recentProject", "availableFrom"},
			ExtraFields: map[string]any{"additionalProperties": false},
		},
		Strict: anthropic.Bool(true),
	}

	resp, err := s.anthropicClient.Messages.New(ctx, anthropic.MessageNewParams{
		Model:      companyDocumentModel,
		MaxTokens:  1024,
		Tools:      []anthropic.ToolUnionParam{{OfTool: &tool}},
		ToolChoice: anthropic.ToolChoiceParamOfTool("extract_personnel_fields"),
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(docBlock, anthropic.NewTextBlock("이 기술인력 증빙서류에서 정보를 추출하세요(이름/연락처 등 개인식별정보는 제외).")),
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
			var candidate personnelCandidate
			if err := json.Unmarshal(tu.Input, &candidate); err != nil {
				return nil, fmt.Errorf("parse tool input: %w", err)
			}
			sanitizePersonnelCandidate(&candidate)
			return &candidate, nil
		}
	}
	return nil, fmt.Errorf("no tool_use block in response (stop_reason=%s)", resp.StopReason)
}

func sanitizePersonnelCandidate(c *personnelCandidate) {
	if c.AvailableFrom != "" && !isBlankOrValidDate(c.AvailableFrom) {
		c.AvailableFrom = ""
	}
	if c.CareerYears != "" && !numericLikePattern.MatchString(c.CareerYears) {
		c.CareerYears = ""
	}
	for _, f := range []*string{&c.Role, &c.TechField, &c.TechGrade, &c.Qualifications, &c.RecentProject} {
		if looksMalformed(*f) {
			*f = ""
		}
	}
}

type personnelRequest struct {
	Role             *string  `json:"role"`
	TechField        *string  `json:"techField"`
	CareerYears      *string  `json:"careerYears"`
	TechGrade        *string  `json:"techGrade"`
	Qualifications   []string `json:"qualifications"`
	RecentProject    *string  `json:"recentProject"`
	AvailableFrom    *string  `json:"availableFrom"`
	SourceDocumentID *string  `json:"sourceDocumentId"`
}

func (s *Server) handleCreatePersonnel(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.currentUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	profile, err := s.getCompanyProfile(r, userID)
	if err != nil {
		s.logger.Error("create-personnel: profile lookup failed", "error", err)
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

	var req personnelRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_body"})
		return
	}

	careerYears, err1 := parseOptionalFloat64(strOrEmpty(req.CareerYears))
	availableFrom, err2 := parseOptionalDate(req.AvailableFrom)
	if err1 != nil || err2 != nil {
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
			s.logger.Error("create-personnel: source document check failed", "error", err)
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
		INSERT INTO company_personnel (
			company_profile_id, role, tech_field, career_years, tech_grade, qualifications,
			recent_project, available_from, source_document_id, confidence, verified_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10, now())
		RETURNING id`,
		profile.ID, req.Role, req.TechField, careerYears, req.TechGrade, pq.Array(req.Qualifications),
		req.RecentProject, availableFrom, req.SourceDocumentID, confidence,
	).Scan(&id)
	if err != nil {
		s.logger.Error("create-personnel: insert failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}

	writeJSON(w, http.StatusCreated, map[string]string{"id": id, "confidence": confidence})
}

type personnelItem struct {
	ID               string     `json:"id"`
	Role             *string    `json:"role"`
	TechField        *string    `json:"techField"`
	CareerYears      *float64   `json:"careerYears"`
	TechGrade        *string    `json:"techGrade"`
	Qualifications   []string   `json:"qualifications"`
	RecentProject    *string    `json:"recentProject"`
	AvailableFrom    *string    `json:"availableFrom"`
	SourceDocumentID *string    `json:"sourceDocumentId"`
	Confidence       string     `json:"confidence"`
	VerifiedAt       *time.Time `json:"verifiedAt"`
	CreatedAt        time.Time  `json:"createdAt"`
	UpdatedAt        time.Time  `json:"updatedAt"`
}

func (s *Server) handleListPersonnel(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.currentUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	profile, err := s.getCompanyProfile(r, userID)
	if err != nil {
		s.logger.Error("list-personnel: profile lookup failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if profile == nil {
		writeJSON(w, http.StatusOK, map[string]any{"items": []personnelItem{}})
		return
	}

	rows, err := s.db.QueryContext(r.Context(), `
		SELECT id, role, tech_field, career_years, tech_grade, qualifications, recent_project,
		       available_from, source_document_id, confidence, verified_at, created_at, updated_at
		FROM company_personnel WHERE company_profile_id = $1 ORDER BY created_at DESC`, profile.ID)
	if err != nil {
		s.logger.Error("list-personnel: query failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	defer rows.Close()

	items := []personnelItem{}
	for rows.Next() {
		var it personnelItem
		var role, techField, techGrade, recentProject, sourceDocID sql.NullString
		var qualifications pq.StringArray
		var careerYears sql.NullFloat64
		var availableFrom, verifiedAt sql.NullTime
		if err := rows.Scan(&it.ID, &role, &techField, &careerYears, &techGrade, &qualifications,
			&recentProject, &availableFrom, &sourceDocID, &it.Confidence, &verifiedAt, &it.CreatedAt, &it.UpdatedAt); err != nil {
			s.logger.Error("list-personnel: scan failed", "error", err)
			continue
		}
		it.Role = nullStringPtr(role)
		it.TechField = nullStringPtr(techField)
		it.TechGrade = nullStringPtr(techGrade)
		it.RecentProject = nullStringPtr(recentProject)
		it.SourceDocumentID = nullStringPtr(sourceDocID)
		it.Qualifications = []string(qualifications)
		if careerYears.Valid {
			it.CareerYears = &careerYears.Float64
		}
		if availableFrom.Valid {
			v := availableFrom.Time.Format("2006-01-02")
			it.AvailableFrom = &v
		}
		if verifiedAt.Valid {
			it.VerifiedAt = &verifiedAt.Time
		}
		items = append(items, it)
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}
