// 직원 수(company_profiles.employee_count) 검증 — 4대보험 사업장
// 가입자명부 증빙. 다른 5개 카테고리(면허/인증/재무/실적/인력)처럼 새
// 테이블을 만들지 않고, company_profiles에 검증 메타데이터 3개 컬럼만
// 추가해 기존 employee_count 필드 자체를 갱신한다.
//
// 개인정보 최소화: 가입자명부엔 이름 등 개인식별정보가 있지만, 이 문서에서
// 추출하는 건 총 가입자 수 하나뿐이다 — company_personnel과 같은 원칙.
package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
)

type employeeCountCandidate struct {
	TotalSubscriberCount string `json:"totalSubscriberCount"`
}

func (s *Server) handleUploadEmployeeVerificationDocument(w http.ResponseWriter, r *http.Request) {
	_, documentID, body, ext, mediaType, ok := s.receiveCompanyDocument(w, r, documentKindEmployeeVerification)
	if !ok {
		return
	}
	candidate, err := s.extractEmployeeCountCandidate(r.Context(), body, ext, mediaType)
	if err != nil {
		s.logger.Error("upload-employee-verification-document: claude extraction failed", "error", err)
		reason := classifyExtractionFailureReason(err)
		s.markDocumentExtractionResult(r.Context(), documentID, extractionStatusFailed, &reason)
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "extraction_failed", "documentId": documentID})
		return
	}
	s.markDocumentExtractionResult(r.Context(), documentID, extractionStatusSuccess, nil)
	writeJSON(w, http.StatusOK, map[string]any{"documentId": documentID, "candidate": candidate})
}

func (s *Server) extractEmployeeCountCandidate(ctx context.Context, body []byte, ext, mediaType string) (*employeeCountCandidate, error) {
	b64 := base64.StdEncoding.EncodeToString(body)
	var docBlock anthropic.ContentBlockParamUnion
	if ext == "pdf" {
		docBlock = anthropic.NewDocumentBlock(anthropic.Base64PDFSourceParam{Data: b64})
	} else {
		docBlock = anthropic.NewImageBlockBase64(mediaType, b64)
	}

	tool := anthropic.ToolParam{
		Name: "extract_employee_count",
		Description: anthropic.String(
			"업로드된 4대보험 사업장 가입자명부에서 총 가입자 수만 추출합니다. " +
				"매우 중요: 가입자 개인의 이름, 생년월일, 주민등록번호 등 개인식별정보는 문서에 있더라도 " +
				"절대 추출하지 마세요 — 오직 총 가입자 수(숫자) 하나만 필요합니다. " +
				"문서에 없는 내용은 절대 만들어내지 마세요.",
		),
		InputSchema: anthropic.ToolInputSchemaParam{
			Properties: map[string]any{
				"totalSubscriberCount": map[string]any{
					"type":        "string",
					"description": "총 가입자 수(숫자만). 확인할 수 없으면 빈 문자열",
				},
			},
			Required:    []string{"totalSubscriberCount"},
			ExtraFields: map[string]any{"additionalProperties": false},
		},
		Strict: anthropic.Bool(true),
	}

	resp, err := s.anthropicClient.Messages.New(ctx, anthropic.MessageNewParams{
		Model:      companyDocumentModel,
		MaxTokens:  512,
		Tools:      []anthropic.ToolUnionParam{{OfTool: &tool}},
		ToolChoice: anthropic.ToolChoiceParamOfTool("extract_employee_count"),
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(docBlock, anthropic.NewTextBlock("이 4대보험 사업장 가입자명부에서 총 가입자 수를 추출하세요. 개인명단은 추출하지 마세요.")),
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
			var candidate employeeCountCandidate
			if err := json.Unmarshal(tu.Input, &candidate); err != nil {
				return nil, fmt.Errorf("parse tool input: %w", err)
			}
			if candidate.TotalSubscriberCount != "" && !numericLikePattern.MatchString(candidate.TotalSubscriberCount) {
				candidate.TotalSubscriberCount = ""
			}
			return &candidate, nil
		}
	}
	return nil, fmt.Errorf("no tool_use block in response (stop_reason=%s)", resp.StopReason)
}

type employeeVerificationRequest struct {
	TotalSubscriberCount *string `json:"totalSubscriberCount"`
	SourceDocumentID     *string `json:"sourceDocumentId"`
}

func (s *Server) handleConfirmEmployeeVerification(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.currentUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	profile, err := s.getCompanyProfile(r, userID)
	if err != nil {
		s.logger.Error("confirm-employee-verification: profile lookup failed", "error", err)
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

	var req employeeVerificationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_body"})
		return
	}
	count, err := parseOptionalInt64(strOrEmpty(req.TotalSubscriberCount))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_field_format"})
		return
	}
	if count == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "total_subscriber_count_required"})
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
			s.logger.Error("confirm-employee-verification: source document check failed", "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
			return
		}
		if !owns {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_source_document"})
			return
		}
		confidence = "B"
	}

	_, err = s.db.ExecContext(r.Context(), `
		UPDATE company_profiles SET
			employee_count = $1, employee_count_source_document_id = $2,
			employee_count_confidence = $3, employee_count_verified_at = now(), updated_at = now()
		WHERE id = $4`,
		*count, req.SourceDocumentID, confidence, profile.ID,
	)
	if err != nil {
		s.logger.Error("confirm-employee-verification: update failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"employeeCount": *count, "confidence": confidence})
}
