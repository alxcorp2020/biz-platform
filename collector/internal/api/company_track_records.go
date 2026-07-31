// 수행실적 — company_licenses/certifications와 같은 패턴이지만 건별
// 리스트라 status/upsert 개념이 없다(면허/인증은 "보유 여부", 재무는
// 연도별 1행이지만 실적은 그냥 계속 추가되는 이력 목록).
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

// fetchTrackRecordMaxAmount returns MAX(contract_amount) across a company's
// track records — the coarse capacity signal scoring.go's trackRecordThin
// uses for the 공동수급 검토(joint-venture-review) grade. Callers fetch this
// once per request/company (same pattern as region/industry/size), never
// per notice, so scoreNoticeForCompany itself stays DB-free.
func (s *Server) fetchTrackRecordMaxAmount(ctx context.Context, profileID string) (sql.NullInt64, error) {
	var maxAmount sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT MAX(contract_amount) FROM company_track_records WHERE company_profile_id = $1`, profileID,
	).Scan(&maxAmount)
	return maxAmount, err
}

type trackRecordCandidate struct {
	ProjectName    string `json:"projectName"`
	ClientName     string `json:"clientName"`
	ContractDate   string `json:"contractDate"`
	PeriodStart    string `json:"periodStart"`
	PeriodEnd      string `json:"periodEnd"`
	ContractAmount string `json:"contractAmount"`
	ProjectType    string `json:"projectType"`
	IndustryField  string `json:"industryField"`
	Region         string `json:"region"`
	IsJointVenture string `json:"isJointVenture"`
	ShareRatio     string `json:"shareRatio"`
	Scope          string `json:"scope"`
	CoreTechnology string `json:"coreTechnology"`
	IsCompleted    string `json:"isCompleted"`
}

func (s *Server) handleUploadTrackRecordDocument(w http.ResponseWriter, r *http.Request) {
	_, documentID, body, ext, mediaType, ok := s.receiveCompanyDocument(w, r)
	if !ok {
		return
	}
	candidate, err := s.extractTrackRecordCandidate(r.Context(), body, ext, mediaType)
	if err != nil {
		s.logger.Error("upload-track-record-document: claude extraction failed", "error", err)
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "extraction_failed", "documentId": documentID})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"documentId": documentID, "candidate": candidate})
}

func (s *Server) extractTrackRecordCandidate(ctx context.Context, body []byte, ext, mediaType string) (*trackRecordCandidate, error) {
	b64 := base64.StdEncoding.EncodeToString(body)
	var docBlock anthropic.ContentBlockParamUnion
	if ext == "pdf" {
		docBlock = anthropic.NewDocumentBlock(anthropic.Base64PDFSourceParam{Data: b64})
	} else {
		docBlock = anthropic.NewImageBlockBase64(mediaType, b64)
	}

	triStateEnum := []string{"예", "아니오", "확인불가"}
	tool := anthropic.ToolParam{
		Name: "extract_track_record_fields",
		Description: anthropic.String(
			"업로드된 수행실적증명서/계약서/세금계산서에서 실제로 문서에 적혀 있는 정보만 추출합니다. " +
				"세금계산서처럼 수행기간·공동수급여부 등이 없는 문서 형식이면 해당 필드는 빈 문자열로 둡니다. " +
				"문서에 없는 내용은 절대 만들어내지 마세요. 확인할 수 없는 필드는 빈 문자열로 두세요.",
		),
		InputSchema: anthropic.ToolInputSchemaParam{
			Properties: map[string]any{
				"projectName":    map[string]any{"type": "string", "description": "사업명. 없으면 빈 문자열"},
				"clientName":     map[string]any{"type": "string", "description": "발주기관/고객사. 없으면 빈 문자열"},
				"contractDate":   map[string]any{"type": "string", "description": "계약일(YYYY-MM-DD). 없으면 빈 문자열"},
				"periodStart":    map[string]any{"type": "string", "description": "수행기간 시작일(YYYY-MM-DD). 없으면 빈 문자열"},
				"periodEnd":      map[string]any{"type": "string", "description": "수행기간 종료일(YYYY-MM-DD). 없으면 빈 문자열"},
				"contractAmount": map[string]any{"type": "string", "description": "계약금액(숫자만). 없으면 빈 문자열"},
				"projectType":    map[string]any{"type": "string", "description": "사업유형. 없으면 빈 문자열"},
				"industryField":  map[string]any{"type": "string", "description": "산업분야. 없으면 빈 문자열"},
				"region":         map[string]any{"type": "string", "description": "수행지역. 없으면 빈 문자열"},
				"isJointVenture": map[string]any{"type": "string", "description": "공동수급 여부", "enum": triStateEnum},
				"shareRatio":     map[string]any{"type": "string", "description": "지분율(%, 숫자만, 공동수급 아니면 빈 문자열)"},
				"scope":          map[string]any{"type": "string", "description": "수행범위. 없으면 빈 문자열"},
				"coreTechnology": map[string]any{"type": "string", "description": "핵심기술. 없으면 빈 문자열"},
				"isCompleted":    map[string]any{"type": "string", "description": "완료 여부", "enum": triStateEnum},
			},
			Required: []string{
				"projectName", "clientName", "contractDate", "periodStart", "periodEnd", "contractAmount",
				"projectType", "industryField", "region", "isJointVenture", "shareRatio", "scope",
				"coreTechnology", "isCompleted",
			},
			ExtraFields: map[string]any{"additionalProperties": false},
		},
		Strict: anthropic.Bool(true),
	}

	resp, err := s.anthropicClient.Messages.New(ctx, anthropic.MessageNewParams{
		Model:      companyDocumentModel,
		MaxTokens:  1024,
		Tools:      []anthropic.ToolUnionParam{{OfTool: &tool}},
		ToolChoice: anthropic.ToolChoiceParamOfTool("extract_track_record_fields"),
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(docBlock, anthropic.NewTextBlock("이 수행실적 증빙서류에서 정보를 추출하세요.")),
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
			var candidate trackRecordCandidate
			if err := json.Unmarshal(tu.Input, &candidate); err != nil {
				return nil, fmt.Errorf("parse tool input: %w", err)
			}
			sanitizeTrackRecordCandidate(&candidate)
			return &candidate, nil
		}
	}
	return nil, fmt.Errorf("no tool_use block in response (stop_reason=%s)", resp.StopReason)
}

func sanitizeTrackRecordCandidate(c *trackRecordCandidate) {
	for _, f := range []*string{&c.ContractDate, &c.PeriodStart, &c.PeriodEnd} {
		if *f != "" && !isBlankOrValidDate(*f) {
			*f = ""
		}
	}
	for _, f := range []*string{&c.ContractAmount, &c.ShareRatio} {
		if *f != "" && !numericLikePattern.MatchString(*f) {
			*f = ""
		}
	}
	for _, f := range []*string{
		&c.ProjectName, &c.ClientName, &c.ProjectType, &c.IndustryField,
		&c.Region, &c.Scope, &c.CoreTechnology,
	} {
		if looksMalformed(*f) {
			*f = ""
		}
	}
	if c.IsJointVenture != "" && c.IsJointVenture != "예" && c.IsJointVenture != "아니오" && c.IsJointVenture != "확인불가" {
		c.IsJointVenture = ""
	}
	if c.IsCompleted != "" && c.IsCompleted != "예" && c.IsCompleted != "아니오" && c.IsCompleted != "확인불가" {
		c.IsCompleted = ""
	}
}

type trackRecordRequest struct {
	ProjectName      string  `json:"projectName"`
	ClientName       *string `json:"clientName"`
	ContractDate     *string `json:"contractDate"`
	PeriodStart      *string `json:"periodStart"`
	PeriodEnd        *string `json:"periodEnd"`
	ContractAmount   *string `json:"contractAmount"`
	ProjectType      *string `json:"projectType"`
	IndustryField    *string `json:"industryField"`
	Region           *string `json:"region"`
	IsJointVenture   *string `json:"isJointVenture"`
	ShareRatio       *string `json:"shareRatio"`
	Scope            *string `json:"scope"`
	CoreTechnology   *string `json:"coreTechnology"`
	IsCompleted      *string `json:"isCompleted"`
	SourceDocumentID *string `json:"sourceDocumentId"`
}

func (s *Server) handleCreateTrackRecord(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.currentUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	profile, err := s.getCompanyProfile(r, userID)
	if err != nil {
		s.logger.Error("create-track-record: profile lookup failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if profile == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "company_profile_required"})
		return
	}

	var req trackRecordRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_body"})
		return
	}
	if strings.TrimSpace(req.ProjectName) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "project_name_required"})
		return
	}

	contractDate, err1 := parseOptionalDate(req.ContractDate)
	periodStart, err2 := parseOptionalDate(req.PeriodStart)
	periodEnd, err3 := parseOptionalDate(req.PeriodEnd)
	contractAmount, err4 := parseOptionalInt64(strOrEmpty(req.ContractAmount))
	shareRatio, err5 := parseOptionalFloat64(strOrEmpty(req.ShareRatio))
	isJointVenture, err6 := parseTriStateBool(strOrEmpty(req.IsJointVenture))
	isCompleted, err7 := parseTriStateBool(strOrEmpty(req.IsCompleted))
	if err1 != nil || err2 != nil || err3 != nil || err4 != nil || err5 != nil || err6 != nil || err7 != nil {
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
			s.logger.Error("create-track-record: source document check failed", "error", err)
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
		INSERT INTO company_track_records (
			company_profile_id, project_name, client_name, contract_date, period_start, period_end,
			contract_amount, project_type, industry_field, region, is_joint_venture, share_ratio,
			scope, core_technology, is_completed, source_document_id, confidence, verified_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17, now())
		RETURNING id`,
		profile.ID, req.ProjectName, req.ClientName, contractDate, periodStart, periodEnd,
		contractAmount, req.ProjectType, req.IndustryField, req.Region, isJointVenture, shareRatio,
		req.Scope, req.CoreTechnology, isCompleted, req.SourceDocumentID, confidence,
	).Scan(&id)
	if err != nil {
		s.logger.Error("create-track-record: insert failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}

	writeJSON(w, http.StatusCreated, map[string]string{"id": id, "confidence": confidence})
}

type trackRecordItem struct {
	ID               string     `json:"id"`
	ProjectName      string     `json:"projectName"`
	ClientName       *string    `json:"clientName"`
	ContractDate     *string    `json:"contractDate"`
	PeriodStart      *string    `json:"periodStart"`
	PeriodEnd        *string    `json:"periodEnd"`
	ContractAmount   *int64     `json:"contractAmount"`
	ProjectType      *string    `json:"projectType"`
	IndustryField    *string    `json:"industryField"`
	Region           *string    `json:"region"`
	IsJointVenture   *bool      `json:"isJointVenture"`
	ShareRatio       *float64   `json:"shareRatio"`
	Scope            *string    `json:"scope"`
	CoreTechnology   *string    `json:"coreTechnology"`
	IsCompleted      *bool      `json:"isCompleted"`
	SourceDocumentID *string    `json:"sourceDocumentId"`
	Confidence       string     `json:"confidence"`
	VerifiedAt       *time.Time `json:"verifiedAt"`
	CreatedAt        time.Time  `json:"createdAt"`
	UpdatedAt        time.Time  `json:"updatedAt"`
}

func (s *Server) handleListTrackRecords(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.currentUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	profile, err := s.getCompanyProfile(r, userID)
	if err != nil {
		s.logger.Error("list-track-records: profile lookup failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if profile == nil {
		writeJSON(w, http.StatusOK, map[string]any{"items": []trackRecordItem{}})
		return
	}

	rows, err := s.db.QueryContext(r.Context(), `
		SELECT id, project_name, client_name, contract_date, period_start, period_end, contract_amount,
		       project_type, industry_field, region, is_joint_venture, share_ratio, scope, core_technology,
		       is_completed, source_document_id, confidence, verified_at, created_at, updated_at
		FROM company_track_records WHERE company_profile_id = $1 ORDER BY created_at DESC`, profile.ID)
	if err != nil {
		s.logger.Error("list-track-records: query failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	defer rows.Close()

	items := []trackRecordItem{}
	for rows.Next() {
		var it trackRecordItem
		var clientName, projectType, industryField, region, scope, coreTechnology, sourceDocID sql.NullString
		var contractDate, periodStart, periodEnd, verifiedAt sql.NullTime
		var contractAmount sql.NullInt64
		var shareRatio sql.NullFloat64
		var isJointVenture, isCompleted sql.NullBool
		if err := rows.Scan(&it.ID, &it.ProjectName, &clientName, &contractDate, &periodStart, &periodEnd,
			&contractAmount, &projectType, &industryField, &region, &isJointVenture, &shareRatio, &scope,
			&coreTechnology, &isCompleted, &sourceDocID, &it.Confidence, &verifiedAt, &it.CreatedAt, &it.UpdatedAt); err != nil {
			s.logger.Error("list-track-records: scan failed", "error", err)
			continue
		}
		it.ClientName = nullStringPtr(clientName)
		it.ProjectType = nullStringPtr(projectType)
		it.IndustryField = nullStringPtr(industryField)
		it.Region = nullStringPtr(region)
		it.Scope = nullStringPtr(scope)
		it.CoreTechnology = nullStringPtr(coreTechnology)
		it.SourceDocumentID = nullStringPtr(sourceDocID)
		it.ContractAmount = nullInt64Ptr(contractAmount)
		if contractDate.Valid {
			v := contractDate.Time.Format("2006-01-02")
			it.ContractDate = &v
		}
		if periodStart.Valid {
			v := periodStart.Time.Format("2006-01-02")
			it.PeriodStart = &v
		}
		if periodEnd.Valid {
			v := periodEnd.Time.Format("2006-01-02")
			it.PeriodEnd = &v
		}
		if shareRatio.Valid {
			it.ShareRatio = &shareRatio.Float64
		}
		if isJointVenture.Valid {
			it.IsJointVenture = &isJointVenture.Bool
		}
		if isCompleted.Valid {
			it.IsCompleted = &isCompleted.Bool
		}
		if verifiedAt.Valid {
			it.VerifiedAt = &verifiedAt.Time
		}
		items = append(items, it)
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}
