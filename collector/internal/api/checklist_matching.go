// checklist_matching.go — Step 3(2026-08-09). 회사가 이미 등록한 서류를 공고
// 체크리스트 요구서류와 자동 연결한다. 목표는 하나: "회사가 이미 올린 서류를
// 공고마다 다시 업로드하지 않게 한다." 새 문서 시스템을 만들지 않고 기존
// company_documents / company_licenses / company_certifications / company_profiles를
// 그대로 재사용한다.
//
// 매칭은 3단계(보수적, AI fuzzy 자동확정 없음):
//  1. 면허/인증 이름 정확일치(기존 로직, 만료 반영) — match_method=EXACT
//  2. 문서 종류(document_kind)/프로필 추출값 기반          — match_method=CATEGORY
//  3. 동의어(alias) 사전으로 정규화 후 (2)와 동일 증거 확인 — match_method=ALIAS
//
// 내부 판정(MATCHED/NEEDS_CONFIRMATION/MISSING)은 기존 checklist 5값 상태로
// 매핑해 저장한다(새 enum 없음):
//
//	MATCHED→보유, 만료→갱신필요, NEEDS_CONFIRMATION→확인필요,
//	MISSING(회사서류)→발급필요, 공고 전용 양식→신규작성.
package api

import (
	"context"
	"database/sql"
	"strings"
	"time"
	"unicode"
)

// match_method(pipeline_checklist_items.match_method에 저장, 운영/디버깅용).
const (
	matchMethodExact    = "EXACT"    // 면허/인증 이름 정확일치
	matchMethodCategory = "CATEGORY" // 요구서류명이 표준 문서명과 정규화-일치 + 종류/프로필 증거
	matchMethodAlias    = "ALIAS"    // 동의어 변형으로 표준 문서명에 매핑 + 종류/프로필 증거
	matchMethodManual   = "MANUAL"   // 사용자 수동 연결(Step 15, 이후 단계)
)

// canonical 문서 타입 키(alias 사전이 한 곳에서 관리).
const (
	docTypeBusinessRegistration = "BUSINESS_REGISTRATION"
	docTypeFinancialStatement   = "FINANCIAL_STATEMENT"
	docTypeTrackRecord          = "TRACK_RECORD"
	docTypeFourInsurance        = "FOUR_INSURANCE_MEMBER_LIST"
)

// docCanonical — 자동매칭 가능한 표준 문서 타입 하나. kind가 있으면
// company_documents.document_kind 존재로, profileField면 프로필 추출값으로
// 증거를 판단한다. 우리 데이터에 증거원이 있는 타입만 여기 넣는다(예: 국세/
// 지방세 납세증명서는 저장소가 없어 아직 자동매칭 대상이 아니다).
type docCanonical struct {
	key          string
	primaryLabel string   // 표준 문서명(정규화-일치 시 CATEGORY)
	aliases      []string // 동의어 변형(정규화-일치 시 ALIAS)
	kind         string   // company_documents.document_kind 증거(빈값이면 미사용)
	profileField bool     // company_profiles 추출값 증거(사업자등록번호 등)
}

var docCanonicals = []docCanonical{
	{docTypeBusinessRegistration, "사업자등록증",
		[]string{"사업자 등록증", "사업자등록증명", "사업자등록증명원", "사업자등록"}, "", true},
	{docTypeFinancialStatement, "재무제표",
		[]string{"재무제표증명", "표준재무제표증명원", "표준재무제표증명", "결산재무제표"}, documentKindFinancial, false},
	{docTypeTrackRecord, "실적증명서",
		[]string{"수행실적증명서", "납품실적증명서", "공사실적증명서", "용역실적증명서", "실적증명"}, documentKindTrackRecord, false},
	{docTypeFourInsurance, "4대보험가입자명부",
		[]string{"4대 사회보험 사업장 가입자 명부", "사대보험가입자명부", "국민연금가입자명부", "건강보험가입자명부"}, documentKindEmployeeVerification, false},
}

// docNameLookup — 정규화된 문서명 → (canonical, method). init에서 1회 구성한다.
type docLookupEntry struct {
	canonical docCanonical
	method    string
}

var docNameLookup map[string]docLookupEntry

func init() {
	docNameLookup = map[string]docLookupEntry{}
	// primary(표준명)를 먼저 등록해 CATEGORY로 확정한다. alias가 정규화 후
	// primary와 같은 문자열이 되는 경우(예: "사업자 등록증"→"사업자등록증")엔
	// 덮어쓰지 않아 primary(CATEGORY)가 우선한다.
	for _, c := range docCanonicals {
		docNameLookup[normalizeDocName(c.primaryLabel)] = docLookupEntry{c, matchMethodCategory}
	}
	for _, c := range docCanonicals {
		for _, a := range c.aliases {
			key := normalizeDocName(a)
			if _, exists := docNameLookup[key]; !exists {
				docNameLookup[key] = docLookupEntry{c, matchMethodAlias}
			}
		}
	}
}

// normalizeDocName — 비교 전용 정규화. 원문명은 절대 덮어쓰지 않는다.
// 소문자화 → 부가어("사본"/"원본"/"제출용") 제거 → 글자·숫자만 남김(공백/괄호/
// 특수문자 제거). 한글 음절은 IsLetter로 유지된다.
func normalizeDocName(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	for _, w := range []string{"사본", "원본", "제출용"} {
		s = strings.ReplaceAll(s, w, "")
	}
	var b strings.Builder
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// isAmbiguousRequirement — "기타 증빙서류"류 모호한 요구는 자동완료하지 않는다.
func isAmbiguousRequirement(name string) bool {
	n := strings.ReplaceAll(name, " ", "")
	for _, k := range []string{"기타", "관련증빙", "필요시", "추가서류", "해당서류", "기타증빙", "관련자료", "필요서류"} {
		if strings.Contains(n, k) {
			return true
		}
	}
	return false
}

// isNoticeSpecificForm — 발주기관 전용 제출양식은 회사 공통서류와 이름이 비슷해도
// 자동완료하지 않는다(공고별 작성 대상). 회사 공통서류와 반드시 구분한다.
func isNoticeSpecificForm(name string) bool {
	n := strings.ReplaceAll(name, " ", "")
	for _, k := range []string{"서식", "양식", "별지", "붙임", "제안서", "참가신청", "청렴서약", "제출확약", "확약서", "서약서", "산출내역"} {
		if strings.Contains(n, k) {
			return true
		}
	}
	return false
}

// resolveChecklistMatch — 요구서류명 하나에 대해 (상태, match_method, matchedDocID)를
// 결정한다. 모든 조회는 company_profile_id로 스코프해 회사 간 문서가 절대 섞이지
// 않게 한다(테넌트 격리).
func (s *Server) resolveChecklistMatch(ctx context.Context, profileID, docName string) (status, method string, matchedDocID *string) {
	name := strings.TrimSpace(docName)
	if name == "" {
		return "확인필요", "", nil
	}
	// 공고 전용 양식 → 신규작성(자동매칭 금지).
	if isNoticeSpecificForm(name) {
		return "신규작성", "", nil
	}
	// 모호한 요구 → 확인필요(자동완료 금지).
	if isAmbiguousRequirement(name) {
		return "확인필요", "", nil
	}

	// 1단계: 면허/인증 이름 정확일치(기존 로직, 만료 반영).
	if st, ok := s.matchLicenseCertByName(ctx, profileID, name); ok {
		return st, matchMethodExact, nil
	}

	// 2·3단계: 정규화 + alias 사전으로 표준 타입 해석 후 회사 문서/프로필 증거 확인.
	if e, ok := docNameLookup[normalizeDocName(name)]; ok {
		if matched, docID := s.docTypeEvidence(ctx, profileID, e.canonical); matched {
			return "보유", e.method, docID
		}
		return "발급필요", "", nil // 표준 타입은 맞으나 회사 문서가 없음
	}

	// 어디에도 해당 없음 → 기존 기본값 유지(회귀 없음).
	return "확인필요", "", nil
}

// matchLicenseCertByName — company_licenses/certifications에서 이름 정확일치(TRIM)로
// 찾아 5값 상태를 돌려준다. 기존 matchChecklistStatus의 로직을 그대로 추출한 것
// (면허/인증 매칭 회귀 방지). 매칭 자체가 없으면 ok=false.
func (s *Server) matchLicenseCertByName(ctx context.Context, profileID, documentName string) (string, bool) {
	name := strings.TrimSpace(documentName)
	if name == "" {
		return "", false
	}
	var licenseStatus string
	var expiresAt sql.NullTime
	err := s.db.QueryRowContext(ctx, `
		SELECT status, expires_at FROM (
			SELECT status, expires_at, created_at FROM company_licenses
			WHERE company_profile_id = $1 AND TRIM(name) = $2
			UNION ALL
			SELECT status, expires_at, created_at FROM company_certifications
			WHERE company_profile_id = $1 AND TRIM(name) = $2
		) matched ORDER BY created_at DESC LIMIT 1`,
		profileID, name,
	).Scan(&licenseStatus, &expiresAt)
	if err != nil {
		if err != sql.ErrNoRows {
			s.logger.Error("checklist match: license/cert query failed", "error", err)
		}
		return "", false
	}
	switch licenseStatus {
	case "보유":
		if expiresAt.Valid && expiresAt.Time.Before(time.Now()) {
			return "갱신필요", true // 만료 → 자동완료 금지
		}
		return "보유", true
	case "미보유":
		return "발급필요", true
	default: // 확인되지않음
		return "확인필요", true
	}
}

// docTypeEvidence — 표준 문서 타입의 증거를 회사 범위로 조회. 있으면 (true,
// matchedDocID). matchedDocID는 회사 문서 파일이 증거일 때만 채워지고(프로필
// 값 증거면 nil), 이후 "이 회사 문서로 자동 확인됨" 추적/표시에 쓴다.
func (s *Server) docTypeEvidence(ctx context.Context, profileID string, c docCanonical) (bool, *string) {
	if c.profileField && c.key == docTypeBusinessRegistration {
		var brn sql.NullString
		if err := s.db.QueryRowContext(ctx,
			`SELECT business_registration_number FROM company_profiles WHERE id = $1`, profileID).Scan(&brn); err != nil {
			return false, nil
		}
		return brn.Valid && strings.TrimSpace(brn.String) != "", nil
	}
	if c.kind != "" {
		var id string
		err := s.db.QueryRowContext(ctx,
			`SELECT id FROM company_documents WHERE company_profile_id = $1 AND document_kind = $2
			 ORDER BY uploaded_at DESC LIMIT 1`, profileID, c.kind).Scan(&id)
		if err != nil {
			return false, nil
		}
		return true, &id
	}
	return false, nil
}
