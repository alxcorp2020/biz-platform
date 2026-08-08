// business_registration.go — 사업자등록증 OCR 자동 회사생성(Phase UX-01,
// 2026-08-04). 계정만 있고 아직 회사 프로필이 없는 사용자가 사업자등록증을
// 올리면 Claude가 필드를 추출해 candidate로 돌려준다 — company_documents.go의
// 6개 카테고리와 같은 원칙(DB에 저장하지 않고, 사용자가 확인해야만
// handleUpsertCompanyProfile(auth.go)로 저장된다). company_profiles가 아직
// 없어도 동작해야 해서 receiveCompanyDocument(프로필 존재를 강제함)를
// 재사용하지 않고 파일 검증을 이 파일에서 독립적으로 수행한다.
package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
)

const maxBizRegDocumentBytes = 10 << 20 // 10MB, company_documents.go와 동일

// allowedBizRegDocumentTypes — company_documents.go의 allowedCompanyDocumentTypes에
// WEBP를 추가(스펙 요구사항). HEIC는 매직바이트 직접 구현이 필요해 이번
// 범위에서 제외(대부분 스캔앱/PDF로 변환해 올리는 경우가 많다고 판단).
var allowedBizRegDocumentTypes = map[string]string{
	"pdf":  "application/pdf",
	"jpg":  "image/jpeg",
	"jpeg": "image/jpeg",
	"png":  "image/png",
	"webp": "image/webp",
}

// bizRegRegionOptions — 프론트 REGION_OPTIONS(index.html)와 순서·값 반드시
// 일치. Claude 추출 스키마의 region enum을 강제하는 데만 쓰인다 — 별도
// 주소 파싱 로직 없이 AI가 직접 이 목록 중 하나로 매핑한다.
var bizRegRegionOptions = []string{
	"전국", "서울특별시", "부산광역시", "대구광역시", "인천광역시", "광주광역시",
	"대전광역시", "울산광역시", "세종특별자치시", "경기도", "강원특별자치도",
	"충청북도", "충청남도", "전북특별자치도", "전라남도", "경상북도", "경상남도", "제주특별자치도",
}

type businessRegistrationCandidate struct {
	BusinessRegistrationNumber string   `json:"businessRegistrationNumber"`
	CompanyName                string   `json:"companyName"`
	RepresentativeName         string   `json:"representativeName"`
	Address                    string   `json:"address"`
	Region                     string   `json:"region"`
	Industry                   []string `json:"industry"`
	BusinessType               []string `json:"businessType"`
	// FoundingDate — 2026-08-05. "개업연월일"(YYYY-MM-DD). 온보딩 채팅이
	// 이 값으로 업력을 자동 계산해 저장하고, 더 이상 업력을 따로 묻지
	// 않는다(구간select로 저장되던 기존 방식의 근본 원인 제거).
	FoundingDate string `json:"foundingDate"`
	// PublicBidFit — 2026-08-08. 업태 기준 공공입찰 적합도("none"/"low"/"normal").
	// 온보딩 OCR 확인 카드가 이 값으로 "공공입찰이 매우 적을 수 있음" 경고를
	// 바로 띄운다(public_bid_fit.go).
	PublicBidFit string `json:"publicBidFit"`
}

// handleExtractBusinessRegistration — POST /api/me/business-registration/extract
// (로그인 필요, 회사 프로필 존재 여부 무관). DB에 아무것도 저장하지 않고
// candidate JSON만 응답한다 — 6개 서류 카테고리와 동일 원칙. 프로필이 없는
// 상태에서 호출되므로 checkAIAnalysisQuota/checkFileRetryRateLimit(둘 다
// profileID 기준)는 쓸 수 없어, 대신 authLookupRateLimited(password_reset.go
// 공용, identifier=userID)로 남용을 방지한다.
//
// ⚠️ 2026-08-04 재확인: "같은 파일 1시간 3회 실패 시 재시도 차단"
// (checkFileRetryRateLimit) 정책이 이 카테고리에만 없다는 게 맞다 —
// 의도적인 차이다. 이 엔드포인트는 파일 해시를 저장할 company_documents
// 행 자체가 없어(회사 프로필이 아직 없는 시점) 파일 단위로 카운트할
// 방법이 없고, 대신 authLookupRateLimited가 userID 기준 1분1회+1일5회로
// 더 보수적으로 막는다(파일을 바꿔 재시도해도 여전히 막힘) — 사용자
// 확인 완료, 별도 파일해시 기반 장치를 새로 만들지 않기로 함. 재무제표/
// 4대보험/면허·인증/수행실적 4개는 공용 receiveCompanyDocument를 거쳐
// checkFileRetryRateLimit이 전부 적용되고, 직접생산확인은 AI 분석 자체가
// 없어(company_profiles.direct_production_cert 체크박스) 해당사항 없음.
func (s *Server) handleExtractBusinessRegistration(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.currentUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	ctx := r.Context()
	blocked, err := authLookupRateLimited(ctx, s.db, "biz_reg_extract", userID)
	if err != nil {
		s.logger.Error("extract-business-registration: rate limit check failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if blocked {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "rate_limited"})
		return
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO auth_lookup_attempts (kind, identifier) VALUES ('biz_reg_extract', $1)`, userID); err != nil {
		s.logger.Error("extract-business-registration: attempt log insert failed", "error", err)
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxBizRegDocumentBytes+(1<<20)) // 멀티파트 오버헤드 여유 1MB
	if err := r.ParseMultipartForm(maxBizRegDocumentBytes); err != nil {
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
	mediaType, isAllowedType := allowedBizRegDocumentTypes[ext]
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
	if len(body) > maxBizRegDocumentBytes {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "file_too_large"})
		return
	}
	// company_documents.go의 receiveCompanyDocument와 동일한 매직바이트 검증
	// (확장자만 보고 통과시키면 파일명을 속여 검증을 우회할 수 있음).
	if detected := http.DetectContentType(body); !companyDocumentContentMatchesType(detected, mediaType) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "file_content_mismatch"})
		return
	}

	candidate, err := s.extractBusinessRegistrationCandidate(ctx, body, ext, mediaType)
	if err != nil {
		s.logger.Error("extract-business-registration: claude extraction failed", "error", err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "extraction_failed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"candidate": candidate})
}

// extractBusinessRegistrationCandidate — company_documents.go의
// extractLicenseCandidate와 동일한 패턴(강제 tool_choice+strict schema,
// PDF는 document block/이미지는 image block). region/industry는 자유텍스트
// 매핑 후처리 없이 enum을 강제해 AI가 직접 REGION_OPTIONS/industryGroups
// 값으로 매핑하게 한다(기존 면허추출기의 documentType enum 강제와 동일 원칙).
func (s *Server) extractBusinessRegistrationCandidate(ctx context.Context, body []byte, ext, mediaType string) (*businessRegistrationCandidate, error) {
	b64 := base64.StdEncoding.EncodeToString(body)

	// industry enum — Phase 2b: 조달청 공공조달분류 중분류(industry_taxonomy)로
	// AI가 직접 매핑하게 강제한다. taxonomy가 비어 있으면(초기 배포 등) 레거시
	// 10그룹으로 폴백한다(매칭은 두 값을 다 처리하므로 안전 — expandCompanyIndustries).
	industryEnum := s.activeIndustryMids(ctx)
	if len(industryEnum) == 0 {
		// taxonomy가 비어 있을 때만 레거시 폴백. "기타"는 선택지에서 제외한다
		// (activeIndustryMids는 industry_taxonomy에서 이미 비활성 처리로 빠지지만,
		// 폴백 경로에도 동일 원칙을 적용).
		for _, g := range industryGroups {
			if g == "기타" {
				continue
			}
			industryEnum = append(industryEnum, g)
		}
	}

	var docBlock anthropic.ContentBlockParamUnion
	if ext == "pdf" {
		docBlock = anthropic.NewDocumentBlock(anthropic.Base64PDFSourceParam{Data: b64})
	} else {
		docBlock = anthropic.NewImageBlockBase64(mediaType, b64)
	}

	tool := anthropic.ToolParam{
		Name: "extract_business_registration",
		Description: anthropic.String(
			"업로드된 사업자등록증에서 실제로 문서에 적혀 있는 정보만 추출합니다. " +
				"문서에 없는 내용은 절대 만들어내지 마세요. 확인할 수 없는 필드는 빈 문자열(배열은 빈 배열)로 " +
				"두세요. region은 사업장 주소를 보고 제공된 17개 광역시도 중 하나로 매핑하세요(정확히 " +
				"특정할 수 없으면 \"전국\"). industry는 업태/종목을 보고 제공된 조달청 공공조달분류 중분류 중 " +
				"'정부·공공기관에 그 업무를 납품/용역으로 제공한다면 어느 분류인지'를 기준으로 가장 가까운 것을 " +
				"모두 선택하세요(겸업 반영). 예: 급식·식자재·구내식당 운영→\"음식서비스\", 소프트웨어 개발→" +
				"\"SW 및 시스템 개발\", 건축·토목 설계→\"설계\", 청소·시설관리→\"시설물관리, 청소 등\", " +
				"인쇄·간판·현수막→\"매체제작\". \"기타\"는 실제로 기타 조달 용역을 수행하는 경우에만 고르고, " +
				"일반 소매·요식업 등 공공조달과 직접 관련이 없어 어느 분류에도 맞지 않으면 \"기타\"로 억지 매핑하지 " +
				"말고 빈 배열로 두세요.",
		),
		InputSchema: anthropic.ToolInputSchemaParam{
			Properties: map[string]any{
				"businessRegistrationNumber": map[string]any{"type": "string", "description": "사업자등록번호(하이픈 포함 원문 그대로). 없으면 빈 문자열"},
				"companyName":                map[string]any{"type": "string", "description": "상호. 없으면 빈 문자열"},
				"representativeName":         map[string]any{"type": "string", "description": "대표자 성명. 없으면 빈 문자열"},
				"address":                    map[string]any{"type": "string", "description": "사업장 소재지 주소 원문. 없으면 빈 문자열"},
				"region": map[string]any{
					"type":        "string",
					"description": "사업장 주소를 광역시도 단위로 매핑한 값",
					"enum":        bizRegRegionOptions,
				},
				"industry": map[string]any{
					"type":        "array",
					"description": "업태/종목을 조달청 공공조달분류 중분류로 매핑한 값(복수 선택 가능). 어느 분류에도 맞지 않으면 \"기타\" 대신 빈 배열",
					"items": map[string]any{
						"type": "string",
						"enum": industryEnum,
					},
				},
				"businessType": map[string]any{
					"type":        "array",
					"description": "업태 원문(문서에 적힌 그대로, 여러 줄이면 각각 하나씩)",
					"items":       map[string]any{"type": "string"},
				},
				"foundingDate": map[string]any{
					"type":        "string",
					"description": "개업연월일을 YYYY-MM-DD 형식으로. 없거나 판독 불가하면 빈 문자열",
				},
			},
			Required: []string{
				"businessRegistrationNumber", "companyName", "representativeName",
				"address", "region", "industry", "businessType", "foundingDate",
			},
			ExtraFields: map[string]any{"additionalProperties": false},
		},
		Strict: anthropic.Bool(true),
	}

	resp, err := s.anthropicClient.Messages.New(ctx, anthropic.MessageNewParams{
		Model:      companyDocumentModel,
		MaxTokens:  1024,
		Tools:      []anthropic.ToolUnionParam{{OfTool: &tool}},
		ToolChoice: anthropic.ToolChoiceParamOfTool("extract_business_registration"),
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(
				docBlock,
				anthropic.NewTextBlock("이 사업자등록증에서 정보를 추출하세요."),
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
			var candidate businessRegistrationCandidate
			if err := json.Unmarshal(tu.Input, &candidate); err != nil {
				return nil, fmt.Errorf("parse tool input: %w", err)
			}
			if candidate.FoundingDate != "" {
				if _, err := time.Parse("2006-01-02", candidate.FoundingDate); err != nil {
					candidate.FoundingDate = "" // 모델이 형식을 안 지켰으면 프론트 업력 계산이 틀어지느니 그냥 비운다
				}
			}
			candidate.PublicBidFit = classifyPublicBidFit(candidate.BusinessType, candidate.Industry)
			return &candidate, nil
		}
	}
	return nil, fmt.Errorf("no tool_use block in response (stop_reason=%s)", resp.StopReason)
}
