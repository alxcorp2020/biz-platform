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
	"database/sql"
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
	"time"

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

// companyDocumentContentMatchesType compares http.DetectContentType's result
// against the media type the file extension claims. DetectContentType can
// append parameters (e.g. "text/plain; charset=utf-8") for text-ish content,
// so only the base type before ';' is compared — the binary signatures we
// care about (PDF/JPEG/PNG) never carry parameters, but stripping defensively
// costs nothing.
func companyDocumentContentMatchesType(detectedType, expectedType string) bool {
	if i := strings.Index(detectedType, ";"); i >= 0 {
		detectedType = detectedType[:i]
	}
	return strings.TrimSpace(detectedType) == expectedType
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
	_, documentID, body, ext, mediaType, ok := s.receiveCompanyDocument(w, r, documentKindLicenseOrCertification)
	if !ok {
		return
	}

	candidate, err := s.extractLicenseCandidate(r.Context(), body, ext, mediaType)
	if err != nil {
		s.logger.Error("upload-document: claude extraction failed", "error", err)
		reason := classifyExtractionFailureReason(err)
		s.markDocumentExtractionResult(r.Context(), documentID, extractionStatusFailed, &reason)
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "extraction_failed", "documentId": documentID})
		return
	}
	s.markDocumentExtractionResult(r.Context(), documentID, extractionStatusSuccess, nil)

	writeJSON(w, http.StatusOK, map[string]any{
		"documentId": documentID,
		"candidate":  candidate,
	})
}

// Document kind constants recorded on company_documents.document_kind — one
// per AI-extraction upload endpoint, used only to label rows on the AI 사용
// 내역 화면(billing_ai_usage.go)의 "어떤 서류" column.
const (
	documentKindLicenseOrCertification = "license_or_certification"
	documentKindFinancial              = "financial"
	documentKindTrackRecord            = "track_record"
	documentKindPersonnel              = "personnel"
	documentKindIntellectualProperty   = "intellectual_property"
	documentKindEmployeeVerification   = "employee_verification"
)

// documentKindLabels — 사람이 읽는 한글 라벨. AI 사용내역 화면에서만 쓰인다.
var documentKindLabels = map[string]string{
	documentKindLicenseOrCertification: "면허/인증",
	documentKindFinancial:              "재무제표",
	documentKindTrackRecord:            "실적증명",
	documentKindPersonnel:              "인력/기술등급",
	documentKindIntellectualProperty:   "지식재산권",
	documentKindEmployeeVerification:   "4대보험 가입자명부",
}

// company_documents.extraction_status 값 — #/ai-usage 화면(billing_ai_usage.go)이
// 성공/실패 배지를 표시하는 데 쓴다. NULL(둘 다 아닌 상태)은 "처리중"으로
// 취급한다(정상 흐름에서는 같은 요청 안에서 곧바로 이 값 중 하나로 갱신되므로
// 실제로 NULL이 오래 남는 경우는 거의 없다).
const (
	extractionStatusSuccess = "success"
	extractionStatusFailed  = "failed"
)

// markDocumentExtractionResult persists the Claude 호출 성공/실패 결과 —
// countAIAnalysisThisMonth(billing.go)가 이 값으로 한도 카운트 여부를
// 결정하고(success만 카운트), #/ai-usage 화면이 사용자에게 사후에 성공/실패를
// 보여주는 근거이기도 하다. 이 UPDATE 자체가 실패해도 이미 클라이언트에는
// (성공/extraction_failed) 응답을 보낸 뒤이므로 로그만 남기고 별도 에러를
// 반환하지 않는다 — 부가 정보 기록이 주 응답 흐름에 영향을 주면 안 된다.
func (s *Server) markDocumentExtractionResult(ctx context.Context, documentID, status string, failureReason *string) {
	if _, err := s.db.ExecContext(ctx, `
		UPDATE company_documents SET extraction_status = $1, failure_reason = $2 WHERE id = $3`,
		status, failureReason, documentID,
	); err != nil {
		s.logger.Error("mark-document-extraction-result: update failed", "error", err, "document_id", documentID)
	}
}

// classifyExtractionFailureReason maps a raw extract*Candidate 에러(6개
// 카테고리 전부 동일한 3가지 형태로 반환함 — extractLicenseCandidate 참고:
// "claude api error (status %d)"로 감싼 API 에러, 감싸지지 않은 원본
// 네트워크/타임아웃 에러, "parse tool input"(JSON 파싱 실패), "no tool_use
// block"(모델이 유효한 결과를 안 줌))를 사용자 친화적 문구로 바꾼다. API
// 원본 에러 메시지(상태 코드, 응답 본문 등)는 절대 그대로 노출하지 않는다.
//
// "parse tool input"/"no tool_use block"만 명시적으로 잡고 나머지는 전부
// "일시적 오류"로 처리한다 — 감싸지지 않은 네트워크 에러처럼 아직 못 본
// 에러 형태가 와도 무해한 기본값(일시적 오류)으로 떨어지게 하기 위함이다.
func classifyExtractionFailureReason(err error) string {
	msg := err.Error()
	if strings.Contains(msg, "parse tool input") || strings.Contains(msg, "no tool_use block") {
		return "문서를 인식하지 못했습니다. 스캔 품질을 확인하거나 다른 파일로 다시 시도해주세요."
	}
	return "일시적인 오류로 분석에 실패했습니다. 잠시 후 다시 시도해주세요."
}

// fileRetryRateLimitWindow/fileRetryRateLimitMaxFailures — 비용 남용 방지
// 장치(2026-08-03 정책: 실패는 한도를 안 깎으므로, 이게 없으면 같은 문제
// 파일을 무한정 재시도해도 아무 제약이 없다). 한도와는 완전히 별개다.
const (
	fileRetryRateLimitWindow      = time.Hour
	fileRetryRateLimitMaxFailures = 3
)

// checkFileRetryRateLimit — 같은 조직이 같은 파일(file_hash)로 최근
// fileRetryRateLimitWindow(1시간) 안에 fileRetryRateLimitMaxFailures(3)회
// 이상 실패했으면 그 파일에 한해 더 이상 시도(신규 업로드든 재시도든)를
// 막는다. 별도 "차단 해제 시각" 컬럼 없이 조회 시점마다 "최근 1시간 안의
// 실패 횟수"를 다시 세는 방식이라(company_documents.extraction_status='failed'
// AND uploaded_at >= now()-1시간), 오래된 실패가 창(window) 밖으로 밀려나면
// 배치/스케줄 없이 자연히 다시 허용된다("1시간 지나면 다시 시도 가능"을
// 그대로 구현). 같은 파일 내용이라도 조직이 다르면 서로 영향을 안 주도록
// company_profile_id로 스코프한다(디스크 저장은 해시로 전역 dedup되지만,
// "이 조직이 이 파일로 반복 실패 중"이라는 판단은 조직 단위가 맞다).
func (s *Server) checkFileRetryRateLimit(ctx context.Context, profileID, fileHash string) (ok bool, err error) {
	var failureCount int
	err = s.db.QueryRowContext(ctx, `
		SELECT count(*) FROM company_documents
		WHERE company_profile_id = $1 AND file_hash = $2 AND extraction_status = 'failed'
		  AND uploaded_at >= now() - ($3 * interval '1 second')`,
		profileID, fileHash, fileRetryRateLimitWindow.Seconds(),
	).Scan(&failureCount)
	if err != nil {
		return false, err
	}
	return failureCount < fileRetryRateLimitMaxFailures, nil
}

// extractCandidateForKind dispatches to the category-specific extract*Candidate
// function by document_kind — handleRetryDocumentExtraction이 재시도 대상
// 서류의 document_kind만 보고 어느 추출 함수를 불러야 할지 알아내는 데 쓴다.
// 반환 타입이 카테고리마다 달라 any로 받는다(호출부는 JSON으로 그대로
// writeJSON에 넘기기만 하므로 구체 타입이 필요 없다).
func (s *Server) extractCandidateForKind(ctx context.Context, kind string, body []byte, ext, mediaType string) (any, error) {
	switch kind {
	case documentKindLicenseOrCertification:
		return s.extractLicenseCandidate(ctx, body, ext, mediaType)
	case documentKindFinancial:
		return s.extractFinancialCandidate(ctx, body, ext, mediaType)
	case documentKindTrackRecord:
		return s.extractTrackRecordCandidate(ctx, body, ext, mediaType)
	case documentKindPersonnel:
		return s.extractPersonnelCandidate(ctx, body, ext, mediaType)
	case documentKindIntellectualProperty:
		return s.extractIPCandidate(ctx, body, ext, mediaType)
	case documentKindEmployeeVerification:
		return s.extractEmployeeCountCandidate(ctx, body, ext, mediaType)
	default:
		return nil, fmt.Errorf("unknown document_kind: %q", kind)
	}
}

// handleRetryDocumentExtraction — POST /api/me/documents/{id}/retry. 실패한
// 업로드를 다시 분석한다(#/ai-usage 화면의 "재시도" 버튼). 이미 디스크에
// 저장된 원본 파일(해시 기반 dedup 저장이라 계속 남아있음)을 재사용해
// 새 멀티파트 업로드 없이 바로 Claude를 다시 부르며, company_documents에
// 새 행을 만든다(원래 실패했던 행은 이력으로 그대로 남겨둔다 — 지우거나
// 갱신하지 않음). 이 재시도가 다시 실패하면 한도는 전혀 안 깎이고
// (2026-08-03 정책), 성공하면 그 성공 건 하나가 카운트된다 — 같은 파일을
// 몇 번을 재시도하든 결국 성공하기 전까지는 비용만 발생하고 한도는 그대로인
// 상황을 막기 위해 checkFileRetryRateLimit(같은 파일 해시 1시간 내 3회
// 이상 실패 시 그 파일 재시도 차단)을 여기서도 검사한다.
func (s *Server) handleRetryDocumentExtraction(w http.ResponseWriter, r *http.Request) {
	userID, authed := s.currentUserID(r)
	if !authed {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	profile, err := s.getCompanyProfile(r, userID)
	if err != nil {
		s.logger.Error("retry-document: profile lookup failed", "error", err)
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

	ctx := r.Context()
	id := r.PathValue("id")

	var kind sql.NullString
	var storedFilename, fileType, originalFilename, fileHash string
	var fileSize int64
	err = s.db.QueryRowContext(ctx, `
		SELECT document_kind, stored_filename, file_type, original_filename, file_hash, file_size_bytes
		FROM company_documents WHERE id = $1 AND company_profile_id = $2`,
		id, profile.ID,
	).Scan(&kind, &storedFilename, &fileType, &originalFilename, &fileHash, &fileSize)
	if errors.Is(err, sql.ErrNoRows) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "document_not_found"})
		return
	}
	if err != nil {
		s.logger.Error("retry-document: lookup failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if !kind.Valid || kind.String == "" {
		// document_kind 컬럼이 생기기 전(예전) 업로드 — 어느 추출 함수를
		// 불러야 할지 알 수 없어 재시도를 지원하지 않는다.
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "retry_unavailable"})
		return
	}

	if ok, err := s.checkFileRetryRateLimit(ctx, profile.ID, fileHash); err != nil {
		s.logger.Error("retry-document: retry rate limit check failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	} else if !ok {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "file_retry_blocked"})
		return
	}

	if quotaOK, limit, err := s.checkAIAnalysisQuota(ctx, profile.ID); err != nil {
		s.logger.Error("retry-document: AI quota check failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	} else if !quotaOK {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "ai_analysis_quota_exceeded", "limit": limit})
		return
	}

	body, err := os.ReadFile(filepath.Join(s.attachmentDir, "company-documents", storedFilename))
	if err != nil {
		s.logger.Error("retry-document: read stored file failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "original_file_missing"})
		return
	}
	mediaType := allowedCompanyDocumentTypes[fileType]

	var documentID string
	err = s.db.QueryRowContext(ctx, `
		INSERT INTO company_documents (company_profile_id, original_filename, stored_filename, file_type, file_size_bytes, file_hash, document_kind)
		VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING id`,
		profile.ID, originalFilename, storedFilename, fileType, fileSize, fileHash, kind.String,
	).Scan(&documentID)
	if err != nil {
		s.logger.Error("retry-document: insert failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}

	candidate, err := s.extractCandidateForKind(ctx, kind.String, body, fileType, mediaType)
	if err != nil {
		s.logger.Error("retry-document: claude extraction failed", "error", err)
		reason := classifyExtractionFailureReason(err)
		s.markDocumentExtractionResult(ctx, documentID, extractionStatusFailed, &reason)
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "extraction_failed", "documentId": documentID})
		return
	}
	s.markDocumentExtractionResult(ctx, documentID, extractionStatusSuccess, nil)

	writeJSON(w, http.StatusOK, map[string]any{
		"documentId": documentID,
		"candidate":  candidate,
	})
}

// receiveCompanyDocument handles the part of "증빙서류 업로드" that's
// identical across every category (license/certification/financial/
// track-record/personnel): auth, company-profile check, multipart parsing,
// file-type/size validation, hash-based disk storage, and the
// company_documents row insert. Each category's upload handler calls this
// first, then runs its own AI extraction with its own tool schema. kind is
// one of the documentKind* constants above, recorded for the AI 사용내역 화면.
//
// On failure this already writes the JSON error response and returns
// ok=false — callers should just `if !ok { return }`.
func (s *Server) receiveCompanyDocument(w http.ResponseWriter, r *http.Request, kind string) (profile *companyProfileDTO, documentID string, body []byte, ext string, mediaType string, ok bool) {
	userID, authed := s.currentUserID(r)
	if !authed {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return nil, "", nil, "", "", false
	}
	profile, err := s.getCompanyProfile(r, userID)
	if err != nil {
		s.logger.Error("upload-document: profile lookup failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return nil, "", nil, "", "", false
	}
	if profile == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "company_profile_required"})
		return nil, "", nil, "", "", false
	}
	// 팀기능: 증빙서류 업로드는 전부 "전체데이터" 쓰기 권한이 있는
	// owner만 — member는 파이프라인 조회+참여만 가능. 이 함수 하나가
	// 면허/인증/재무/실적/인력/지식재산권/4대보험 업로드 전부의 공용
	// 진입점이라 여기 한 곳만 막으면 모든 업로드 엔드포인트에 적용된다.
	if profile.Role != "owner" {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "owner_only"})
		return nil, "", nil, "", "", false
	}

	if quotaOK, limit, err := s.checkAIAnalysisQuota(r.Context(), profile.ID); err != nil {
		s.logger.Error("upload-document: AI quota check failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return nil, "", nil, "", "", false
	} else if !quotaOK {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "ai_analysis_quota_exceeded", "limit": limit})
		return nil, "", nil, "", "", false
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxCompanyDocumentBytes+(1<<20)) // 멀티파트 오버헤드 여유 1MB
	if err := r.ParseMultipartForm(maxCompanyDocumentBytes); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "file_too_large_or_invalid"})
		return nil, "", nil, "", "", false
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "file_required"})
		return nil, "", nil, "", "", false
	}
	defer file.Close()

	ext = strings.ToLower(strings.TrimPrefix(filepath.Ext(header.Filename), "."))
	var isAllowedType bool
	mediaType, isAllowedType = allowedCompanyDocumentTypes[ext]
	if !isAllowedType {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unsupported_file_type"})
		return nil, "", nil, "", "", false
	}

	body, err = io.ReadAll(file)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "file_read_failed"})
		return nil, "", nil, "", "", false
	}
	if len(body) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "file_empty"})
		return nil, "", nil, "", "", false
	}
	if len(body) > maxCompanyDocumentBytes {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "file_too_large"})
		return nil, "", nil, "", "", false
	}

	// 확장자만 보고 통과시키면 파일명을 속여(예: 악성 실행파일에 .pdf를
	// 붙이는 식으로) 검증을 우회할 수 있다 — 실제 내용의 매직바이트까지
	// 확인해 확장자가 주장하는 타입과 일치하는지 대조한다. http.DetectContentType은
	// 표준 라이브러리의 WHATWG MIME sniffing 구현이라 PDF(%PDF-)/JPEG(FF D8 FF)/
	// PNG(89 50 4E 47 ...) 시그니처를 이미 인식하므로 별도 의존성이 필요 없다.
	if detected := http.DetectContentType(body); !companyDocumentContentMatchesType(detected, mediaType) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "file_content_mismatch"})
		return nil, "", nil, "", "", false
	}

	sum := sha256.Sum256(body)
	hash := hex.EncodeToString(sum[:])

	if ok, err := s.checkFileRetryRateLimit(r.Context(), profile.ID, hash); err != nil {
		s.logger.Error("upload-document: retry rate limit check failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return nil, "", nil, "", "", false
	} else if !ok {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "file_retry_blocked"})
		return nil, "", nil, "", "", false
	}

	storedKey := hash + "." + ext
	if err := s.writeCompanyDocumentFile(storedKey, body); err != nil {
		s.logger.Error("upload-document: write to disk failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "storage_failed"})
		return nil, "", nil, "", "", false
	}

	err = s.db.QueryRowContext(r.Context(), `
		INSERT INTO company_documents (company_profile_id, original_filename, stored_filename, file_type, file_size_bytes, file_hash, document_kind)
		VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING id`,
		profile.ID, header.Filename, storedKey, ext, int64(len(body)), hash, kind,
	).Scan(&documentID)
	if err != nil {
		s.logger.Error("upload-document: insert failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return nil, "", nil, "", "", false
	}

	return profile, documentID, body, ext, mediaType, true
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
			"업로드된 증빙서류(사업자등록증/중소기업확인서/직접생산확인증명서/면허증/인증서/법인등기사항증명서/" +
				"경쟁입찰참가자격등록증/기업부설연구소 인정서 등)에서 " +
				"실제로 문서에 적혀 있는 정보만 추출합니다. 문서에 없는 내용은 절대 만들어내지 마세요. " +
				"확인할 수 없는 필드는 빈 문자열로 두세요. 특허·상표 등록증은 이 도구가 아니라 " +
				"별도의 지식재산권 전용 업로드로 처리하니 여기서는 제안하지 마세요.",
		),
		InputSchema: anthropic.ToolInputSchemaParam{
			Properties: map[string]any{
				"documentType": map[string]any{
					"type":        "string",
					"description": "문서 종류",
					"enum": []string{
						"사업자등록증", "중소기업확인서", "직접생산확인증명서", "면허증", "인증서",
						"법인등기사항증명서", "경쟁입찰참가자격등록증",
						"기업부설연구소 인정서", "기타",
					},
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
