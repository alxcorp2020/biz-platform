// 면허·인증 증빙서류 업로드 + AI 구조화 추출 (spec 3.3/3.4/4).
//
// analyzer(Python)의 PDF 텍스트 추출 파이프라인은 재사용하지 않는다 — 운영
// apiserver는 distroless 이미지라 Python을 실행할 수 없고, Render 무료 플랜은
// 별도 워커 서비스를 지원하지 않는다. 대신 Claude Messages API가 PDF/이미지를
// 텍스트 추출 없이 직접 읽을 수 있으므로, 업로드된 파일을 그대로 Claude에
// 전달해 같은 요청 안에서 후보 값을 받는다.
//
// analyzer/ai_extract.py의 "원문에 없는 내용은 만들지 마세요" 원칙을 프롬프트에
// 담되, 그쪽처럼 원문 문자열 대조 검증은 할 수 없다(추출된 원문 텍스트 자체가
// 없음 — 멀티모달이 직접 읽음). 대신 이 후보는 절대 즉시 DB에 저장되지 않고,
// 사용자가 검토·수정 후 명시적으로 승인해야만(POST /api/me/licenses 등)
// company_licenses/company_certifications에 저장된다 — 그 확인 절차 자체가
// 이 기능의 grounding 안전장치다.
package api

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
)

const (
	maxCompanyDocumentBytes = 10 << 20 // 10MB
	companyDocumentModel    = "claude-sonnet-5"
)

// allowedCompanyDocumentTypes 값은 Claude Messages API가 받는 media type이다.
var allowedCompanyDocumentTypes = map[string]string{
	"pdf":  "application/pdf",
	"jpg":  "image/jpeg",
	"jpeg": "image/jpeg",
	"png":  "image/png",
}

type licenseCandidate struct {
	DocumentType       string `json:"documentType"`
	Name               string `json:"name"`
	RegistrationNumber string `json:"registrationNumber"`
	IssuingAuthority   string `json:"issuingAuthority"`
	IssuedAt           string `json:"issuedAt"`
	ExpiresAt          string `json:"expiresAt"`
	ApplicableIndustry string `json:"applicableIndustry"`
}

func (s *Server) handleUploadCompanyDocument(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.currentUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	profile, err := s.getCompanyProfile(r, userID)
	if err != nil {
		s.logger.Error("upload-document: profile lookup failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if profile == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "company_profile_required"})
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxCompanyDocumentBytes+(1<<20)) // 멀티파트 오버헤드 여유 1MB
	if err := r.ParseMultipartForm(maxCompanyDocumentBytes); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "file_too_large_or_invalid"})
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "file_required"})
		return
	}
	defer file.Close()

	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(header.Filename), "."))
	mediaType, isAllowedType := allowedCompanyDocumentTypes[ext]
	if !isAllowedType {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unsupported_file_type"})
		return
	}

	body, err := io.ReadAll(file)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "file_read_failed"})
		return
	}
	if len(body) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "file_empty"})
		return
	}
	if len(body) > maxCompanyDocumentBytes {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "file_too_large"})
		return
	}

	sum := sha256.Sum256(body)
	hash := hex.EncodeToString(sum[:])
	storedKey := hash + "." + ext
	if err := s.writeCompanyDocumentFile(storedKey, body); err != nil {
		s.logger.Error("upload-document: write to disk failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "storage_failed"})
		return
	}

	var documentID string
	err = s.db.QueryRowContext(r.Context(), `
		INSERT INTO company_documents (company_profile_id, original_filename, stored_filename, file_type, file_size_bytes, file_hash)
		VALUES ($1,$2,$3,$4,$5,$6) RETURNING id`,
		profile.ID, header.Filename, storedKey, ext, int64(len(body)), hash,
	).Scan(&documentID)
	if err != nil {
		s.logger.Error("upload-document: insert failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}

	candidate, err := s.extractLicenseCandidate(r.Context(), body, ext, mediaType)
	if err != nil {
		s.logger.Error("upload-document: claude extraction failed", "error", err)
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "extraction_failed", "documentId": documentID})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"documentId": documentID,
		"candidate":  candidate,
	})
}

// writeCompanyDocumentFile writes body under attachmentDir/company-documents/storedKey,
// skipping the write if a file with that content hash already exists on disk —
// same dedup-by-hash pattern as runner.go's writeAttachmentFile.
func (s *Server) writeCompanyDocumentFile(storedKey string, body []byte) error {
	dir := filepath.Join(s.attachmentDir, "company-documents")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(dir, storedKey)
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	return os.WriteFile(path, body, 0o644)
}

// extractLicenseCandidate sends the raw uploaded file to Claude as a
// document/image content block and forces a strict-schema tool call to get
// back structured fields. The prompt explicitly forbids fabricating values
// not present in the document (same principle as analyzer/ai_extract.py),
// but unlike that Python path there's no separate extracted-text string to
// verify quotes against — the human confirmation step before saving is the
// grounding safeguard here instead.
func (s *Server) extractLicenseCandidate(ctx context.Context, body []byte, ext, mediaType string) (*licenseCandidate, error) {
	b64 := base64.StdEncoding.EncodeToString(body)

	var docBlock anthropic.ContentBlockParamUnion
	if ext == "pdf" {
		docBlock = anthropic.NewDocumentBlock(anthropic.Base64PDFSourceParam{Data: b64})
	} else {
		docBlock = anthropic.NewImageBlockBase64(mediaType, b64)
	}

	tool := anthropic.ToolParam{
		Name: "extract_document_fields",
		Description: anthropic.String(
			"업로드된 증빙서류(사업자등록증/중소기업확인서/직접생산확인증명서/면허증/인증서 등)에서 " +
				"실제로 문서에 적혀 있는 정보만 추출합니다. 문서에 없는 내용은 절대 만들어내지 마세요. " +
				"확인할 수 없는 필드는 빈 문자열로 두세요.",
		),
		InputSchema: anthropic.ToolInputSchemaParam{
			Properties: map[string]any{
				"documentType": map[string]any{
					"type":        "string",
					"description": "문서 종류",
					"enum":        []string{"사업자등록증", "중소기업확인서", "직접생산확인증명서", "면허증", "인증서", "기타"},
				},
				"name":               map[string]any{"type": "string", "description": "문서상의 명칭(상호명, 인증명 등). 없으면 빈 문자열"},
				"registrationNumber": map[string]any{"type": "string", "description": "등록번호/사업자등록번호/인증번호. 없으면 빈 문자열"},
				"issuingAuthority":   map[string]any{"type": "string", "description": "발급기관. 없으면 빈 문자열"},
				"issuedAt":           map[string]any{"type": "string", "description": "발급일(YYYY-MM-DD). 없거나 불확실하면 빈 문자열"},
				"expiresAt":          map[string]any{"type": "string", "description": "유효기간/만료일(YYYY-MM-DD). 없으면 빈 문자열"},
				"applicableIndustry": map[string]any{"type": "string", "description": "적용업종. 없으면 빈 문자열"},
			},
			Required: []string{
				"documentType", "name", "registrationNumber", "issuingAuthority",
				"issuedAt", "expiresAt", "applicableIndustry",
			},
			ExtraFields: map[string]any{"additionalProperties": false},
		},
		Strict: anthropic.Bool(true),
	}

	resp, err := s.anthropicClient.Messages.New(ctx, anthropic.MessageNewParams{
		Model:      companyDocumentModel,
		MaxTokens:  1024,
		Tools:      []anthropic.ToolUnionParam{{OfTool: &tool}},
		ToolChoice: anthropic.ToolChoiceParamOfTool("extract_document_fields"),
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(
				docBlock,
				anthropic.NewTextBlock("이 증빙서류에서 정보를 추출하세요."),
			),
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
			var candidate licenseCandidate
			if err := json.Unmarshal(tu.Input, &candidate); err != nil {
				return nil, fmt.Errorf("parse tool input: %w", err)
			}
			sanitizeCandidate(&candidate)
			return &candidate, nil
		}
	}
	return nil, fmt.Errorf("no tool_use block in response (stop_reason=%s)", resp.StopReason)
}

var candidateDatePattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// sanitizeCandidate defends against the model not reliably following the
// "확인할 수 없는 필드는 빈 문자열로 두세요" instruction: verified against the
// real API, a field with no grounded answer sometimes comes back as
// malformed placeholder-like text (observed once as a literal XML-tag
// fragment) instead of "". Any date field not in YYYY-MM-DD, or any text
// field containing characters that don't belong in a document's actual
// name/number/authority/industry text, is blanked rather than shown to the
// user as if Claude had read it from the document.
func sanitizeCandidate(c *licenseCandidate) {
	if !isBlankOrValidDate(c.IssuedAt) {
		c.IssuedAt = ""
	}
	if !isBlankOrValidDate(c.ExpiresAt) {
		c.ExpiresAt = ""
	}
	for _, f := range []*string{&c.Name, &c.RegistrationNumber, &c.IssuingAuthority, &c.ApplicableIndustry} {
		if looksMalformed(*f) {
			*f = ""
		}
	}
}

func isBlankOrValidDate(s string) bool {
	return s == "" || candidateDatePattern.MatchString(s)
}

func looksMalformed(s string) bool {
	return len(s) > 200 || strings.ContainsAny(s, "<>{}")
}
