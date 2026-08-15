// document_extraction.go — Phase 4(서류자동화 고도화) 1단계: "공고 수집 →
// 제출서류/자격조건 추출"을 사람이 CLI로 수동 실행하던 것에서 자동 배치로
// 바꾼다.
//
// analyzer/ 파이썬 파이프라인 3단계 중 앞뒤는 그대로 둔다:
//  1. run_extraction.py(첨부파일 PDF/HWP → attachments.extracted_text) —
//     여전히 사람이 로컬에서 주기 실행한다. HWP는 애초에 Go 파싱
//     라이브러리가 마땅치 않아 미이식. PDF도 2026-08-02에 Go 이식을
//     실제로 조사·검증했지만 보류했다 — 이 프로젝트의 실제 나라장터
//     PDF로 테스트한 결과, 유일하게 쓸만한 순수 Go 라이브러리인
//     ledongthuc/pdf가 한글 문자 디코딩은 정확하지만 이 문서들이 쓰는
//     임베디드 CID 폰트의 글자 간격(advance width) 계산이 근본적으로
//     깨져있음을 확인했다(글자 W가 항상 0, 읽기 방향과 반대로 X좌표가
//     감소 — 좌표 자체가 틀려서 후처리 재구성 알고리즘으로 못 고치는
//     수준). unipdf는 상용 라이선스 필요, go-fitz(MuPDF)는 cgo 필요해
//     `Dockerfile.apiserver`의 CGO_ENABLED=0 제약과 충돌, pdfcpu는
//     텍스트 해석 API 자체가 없음 — 4개 후보 전부 실사용 불가로 판단.
//     운영 배포(Render distroless)엔 애초에 Python이 없다.
//  3. review.go(관리자 검수 화면) — review_status='review_required'를
//     사람이 승인/반려하는 절차는 그대로 유지.
//
// 이 파일이 자동화하는 건 가운데 두 단계뿐이다:
//
//	2a. extract_sections.py(정규식 기반 1차) → runRuleBasedDocumentExtraction
//	2b. ai_extract.py(Claude 기반 2차 보완) → runAI*SupplementExtraction
//
// 둘 다 extracted_text가 이미 채워진 첨부파일만 대상으로 하므로, run_
// extraction.py가 안 돌아간 새 첨부는 텍스트가 채워질 때까지 자동으로
// 대기했다가(매 배치마다 재확인) 그 다음부터 자동으로 이어진다.
//
// Python 원본과의 차이 — "매시간 자동 재실행"이라는 새 실행 방식 때문에
// 추가한 안전장치:
//   - attachments.section_extraction_processed_at / eligibility_conditions
//     · required_documents.ai_supplement_attempted_at 워터마크 컬럼을 새로
//     둬서, 사람이 수동으로 한 번 돌리던 원본 스크립트와 달리 같은 대상을
//     매시간 무한히 재처리(비용 낭비)하지 않게 막는다. review_required
//     원본 행은 사람이 review.go에서 승인/반려하기 전까지 상태가 안
//     바뀌므로, 이 워터마크가 없으면 AI 보완 호출이 영원히 반복된다.
//   - AI 호출 자체가 실패하면(키 미설정 포함) 워터마크를 찍지 않고 다음
//     배치에서 재시도한다 — 일시적 오류는 자연히 복구되고, 키가 아예
//     없는 환경(로컬 등)에서는 그냥 계속 조용히 재시도만 하다 끝난다.
package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/anthropics/anthropic-sdk-go"
)

// ============================================================
// 2a. 규칙 기반 추출 (analyzer/extract_sections.py 포팅)
// ============================================================

var (
	eligibilityKeywords  = []string{"참가자격", "응모자격", "응찰자격", "자격요건", "참가요건"}
	documentKeywords     = []string{"제출서류", "구비서류", "첨부서류"}
	referenceOnlyMarkers = []string{"참조", "별첨", "별지 서식", "별지서식", "붙임"}
	placeholderTokens    = []string{"<표>", "<그림>", "<이미지>"}
)

const (
	ruleExtractionConfidence = 0.70
	tableLookaheadLines      = 5
	docNameMaxRunes          = 60

	// documentExtractionBatchLimit — 배치(규칙기반/AI보완 각각)당 처리
	// 상한. 워터마크 쿼리는 원래 무제한으로 전체 대상을 매시간 훑었는데,
	// 첨부파일이 한꺼번에 몰리면(예: run_extraction.py가 며칠 밀렸다가
	// 한 번에 텍스트를 채운 경우) 한 배치에서 API 호출/DB 트랜잭션이
	// 폭주할 수 있어 상한을 뒀다 — 못 채운 나머지는 다음 시간 배치가
	// 이어서 처리하므로 데이터가 유실되지는 않는다.
	documentExtractionBatchLimit = 200

	// maxAISupplementAttempts — AI 보완 호출이 이 횟수만큼 연속 실패하면
	// (예: Claude API 키가 만료된 채 방치된 경우) 더 이상 재시도하지 않고
	// ai_supplement_attempted_at을 찍어 포기한다. 사람이 review.go에서
	// 직접 검토해야 하는 review_required 행 자체는 그대로 남으므로 데이터
	// 유실은 아니고, "AI 자동 보완"만 포기하는 것이다.
	maxAISupplementAttempts = 3
)

var headerPrefixRe = regexp.MustCompile(`^([0-9]+[.)]|[①-⑩]|[가나다라마바사아자차카타파하][.)]|[○◦●■□❍▶◆★☆※\-*])`)

// listPrefixRe — 제출서류 "목록 줄" 인식 전용(STEP 2-C-1). headerPrefixRe에 괄호숫자
// "(1)~(99)"를 더한 것. 실제 공고 원문 "(4) …실적을 확인할 수 있는 증빙서류 1부"가
// 목록으로 인식되지 않아 required_documents에서 누락되던 실측 문제(MEDIUM-2) 보정.
// ⚠️ 섹션 경계 판정(looksLikeHeaderLine)은 기존 headerPrefixRe를 그대로 쓴다 —
// 괄호숫자를 헤더로도 취급하면 기존 섹션 절단 동작이 바뀌어 회귀 위험이 있다.
var listPrefixRe = regexp.MustCompile(`^([0-9]+[.)]|\([0-9]{1,2}\)|[①-⑩]|[가나다라마바사아자차카타파하][.)]|[○◦●■□❍▶◆★☆※\-*])`)
var sentenceEndingRe = regexp.MustCompile(`(다|함|음|까|요|임|됨|습니다|입니다)[.]?$`)
var whitespaceRunRe = regexp.MustCompile(`\s+`)

func looksLikeHeaderLine(line string) bool {
	s := strings.TrimSpace(line)
	if s == "" || utf8.RuneCountInString(s) > 30 {
		return false
	}
	for _, tok := range placeholderTokens {
		if s == tok {
			return false // 표/그림 플레이스홀더를 섹션 종료 신호로 오인하면 안 됨
		}
	}
	if headerPrefixRe.MatchString(s) {
		return true
	}
	return !sentenceEndingRe.MatchString(s)
}

func stripListPrefix(line string) string {
	s := strings.TrimSpace(line)
	if loc := listPrefixRe.FindStringIndex(s); loc != nil {
		s = s[loc[1]:]
	}
	return strings.Trim(s, " .)")
}

func containsAnyToken(s string, tokens []string) bool {
	for _, t := range tokens {
		if strings.Contains(s, t) {
			return true
		}
	}
	return false
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

type extractedSection struct {
	anchorText     string
	sectionText    string
	hasTableNearby bool
}

// extractSectionsFromText — extract_sections.py의 extract_sections()와 동일
// 로직: keywords 중 하나라도 포함된 줄을 앵커로 찾아 섹션을 잘라낸다.
func extractSectionsFromText(text string, keywords []string) []extractedSection {
	lines := strings.Split(text, "\n")
	normalized := make([]string, len(lines))
	for i, l := range lines {
		normalized[i] = whitespaceRunRe.ReplaceAllString(l, "")
	}

	var hitIndices []int
	for i, norm := range normalized {
		for _, kw := range keywords {
			if strings.Contains(norm, kw) {
				hitIndices = append(hitIndices, i)
				break
			}
		}
	}

	var sections []extractedSection
	for _, idx := range hitIndices {
		anchorText := strings.TrimSpace(lines[idx])
		isHeader := looksLikeHeaderLine(anchorText)
		start := idx
		searchLimit := min(len(lines), idx+40)
		if isHeader {
			start = idx + 1
			searchLimit = len(lines)
		}

		end := searchLimit
		for j := idx + 1; j < searchLimit; j++ {
			if looksLikeHeaderLine(lines[j]) {
				end = j
				break
			}
		}

		var sectionText string
		if start < end {
			sectionText = strings.TrimSpace(strings.Join(lines[start:end], "\n"))
		}

		lookaheadEnd := min(len(lines), end+tableLookaheadLines)
		tableNearby := containsAnyToken(sectionText, placeholderTokens) || containsAnyToken(anchorText, placeholderTokens)
		if !tableNearby {
			for k := end; k < lookaheadEnd; k++ {
				trimmed := strings.TrimSpace(lines[k])
				for _, tok := range placeholderTokens {
					if trimmed == tok {
						tableNearby = true
					}
				}
			}
		}

		sections = append(sections, extractedSection{
			anchorText:     anchorText,
			sectionText:    sectionText,
			hasTableNearby: tableNearby,
		})
	}
	return sections
}

func isReferenceOnlySection(sectionText string, listLikeLines []string) bool {
	if len(listLikeLines) >= 2 {
		return false
	}
	if sectionText == "" {
		return true
	}
	return containsAnyToken(sectionText, referenceOnlyMarkers)
}

func looksLikeDocumentName(lineAfterPrefix string) bool {
	s := strings.TrimSpace(lineAfterPrefix)
	if s == "" || utf8.RuneCountInString(s) > docNameMaxRunes {
		return false
	}
	return !sentenceEndingRe.MatchString(s)
}

func findListLikeLines(sectionText string) []string {
	var out []string
	for _, raw := range strings.Split(sectionText, "\n") {
		l := strings.TrimSpace(raw)
		if l == "" || l == "<표>" || !listPrefixRe.MatchString(l) {
			continue
		}
		if looksLikeDocumentName(stripListPrefix(l)) {
			out = append(out, l)
		}
	}
	return out
}

func reviewStatusForRule(hasTableNearby bool) string {
	if hasTableNearby {
		return "review_required"
	}
	return "pending"
}

type eligibilityRuleRow struct {
	conditionName string
	sourceText    string
	reviewStatus  string
}

func buildEligibilityRuleRows(sections []extractedSection) []eligibilityRuleRow {
	var rows []eligibilityRuleRow
	for _, sec := range sections {
		rows = append(rows, eligibilityRuleRow{
			conditionName: truncateRunes(sec.anchorText, 200),
			sourceText:    strings.TrimSpace(sec.anchorText + "\n" + sec.sectionText),
			reviewStatus:  reviewStatusForRule(sec.hasTableNearby),
		})
	}
	return rows
}

// ── 수행실적 참가요건 규칙 추출(STEP 2-C, 2026-08-15) ─────────────────────
// 실제 공고는 "실적"이라는 단어를 대부분 ①평가배점 ②제출서류 ③개인정보
// 수집 boilerplate에서 쓴다(로컬 실데이터 12건 실측 확인) — 참가 "필수"
// 실적요건은 극소수다. 그래서 아무 실적 문구나 category='실적' 참가요건으로
// 만들지 않고, 참가자격(eligibilityKeywords) 섹션 안에서 ①평가/서류 문맥이
// 아니고 ②정량 임계(금액 또는 "최근 N년")가 명확한 줄만 구조화한다. 임계가
// 없는 모호한 실적(예: "유사실적 제출")은 여기서 만들지 않고 제출서류 경로
// (required_documents "실적" → Resolver가 TRACK_RECORD로 분류)에 맡긴다(§4/§21).
type trackRecordRuleRow struct {
	conditionName  string
	sourceText     string
	operator       string
	thresholdValue string
	unit           string
	reviewStatus   string
}

// 실적이 참가필수가 아니라 평가배점/제출서류/개인정보 문맥일 때 그 줄을 참가요건으로
// 만들지 않도록 거르는 토큰(§3/§22 오판 방지 — FALSE 참가요건 방지가 최우선).
// STEP 2-C-1: 실공고 스팟 검증(PQ 수행능력 평가기준 문형)에서 줄 자체에 '평가'가 없는
// 케이스가 확인돼 PQ/수행능력/만점을 최소 보강.
var trackRecordExcludeTokens = []string{"평가", "배점", "가점", "우대", "심사", "점수", "제출", "증빙", "서류", "첨부", "붙임", "신용", "납세", "개인정보", "수집", "제공", "동의", "PQ", "수행능력", "만점"}

// trackRecordSectionExcludeTokens — 섹션 제목(anchor) 자체가 평가/PQ 문맥이면 그 섹션의
// 실적 문구 전체를 참가필수 후보에서 제외한다(STEP 2-C-1 LOW 방어). 실측 근거: PQ 공고의
// "사업수행능력 평가서 제출 참가자격" 섹션 하위에 '평가' 단어 없는 실적 인정기준 줄이
// 있었음 — 줄 단위 토큰만으로는 섹션 범위가 넓은 문서에서 오탐 위험. 참가자격 앵커라도
// 제목에 평가류 토큰이 있으면 참가필수 실적 요구로 취급하지 않는다.
var trackRecordSectionExcludeTokens = []string{"평가", "PQ", "수행능력", "적격심사", "배점"}

// 금액 파서: "1억원 이상 / 5천만원 이상 / 50,000,000원 이상 / 3억원 초과". 단위는
// 구조화된 unit 필드로만 넘겨 이후 판정기가 환산한다(source_text로 재환산 금지 — STEP
// 2-B 이중환산 버그 재발 방지). 복합금액(1억 5천만원)·외화(만불)는 과소·오파싱 위험이
// 있어 구조화하지 않고 UNKNOWN으로 둔다(§4/§6/§8).
var trackRecordAmountRe = regexp.MustCompile(`([0-9][0-9,]*)\s*(억|천만|백만|만)?\s*원\s*(이상|이하|초과|미만)?`)
var trackRecordCompoundRe = regexp.MustCompile(`억\s*[0-9]`)
var foreignCurrencyRe = regexp.MustCompile(`불|달러|USD|\$`)

// trackRecordCountRe — 건수형 참가필수 실적(STEP 2-C-1, MEDIUM-1). 실공고 실측 문형
// "…관련 실적 1건 이상 보유…"를 구조화한다. '이상'이 명시된 하한만 인식(과대해석 방지).
var trackRecordCountRe = regexp.MustCompile(`([0-9]+)\s*건\s*이상`)

// parseTrackRecordCount — "실적 N건 이상" → (>=, N, 건). 금액과 무관한 별도 단위라
// unit='건'으로 저장하고, 판정측 금액 환산 로직은 이 unit을 명시적으로 건너뛴다(§3).
func parseTrackRecordCount(line string) (op, thr, unit string, ok bool) {
	m := trackRecordCountRe.FindStringSubmatch(line)
	if m == nil {
		return "", "", "", false
	}
	return ">=", m[1], "건", true
}

func parseTrackRecordAmount(line string) (op, thr, unit string, ok bool) {
	if foreignCurrencyRe.MatchString(line) {
		return "", "", "", false // 외화는 원 비교 불가 → 구조화하지 않음
	}
	m := trackRecordAmountRe.FindStringSubmatch(line)
	if m == nil {
		return "", "", "", false
	}
	digits := strings.ReplaceAll(m[1], ",", "")
	if digits == "" {
		return "", "", "", false
	}
	unitWord := m[2]
	// 복합금액(1억 5천만원 등)은 정규식이 뒷단위(5천만원)만 잡아 과소파싱하므로,
	// 줄에 "억+숫자"가 있으면 금액을 구조화하지 않는다(§4/§6/§8).
	if trackRecordCompoundRe.MatchString(line) {
		return "", "", "", false
	}
	switch m[3] {
	case "이상":
		op = ">="
	case "초과":
		op = ">"
	case "이하":
		op = "<="
	case "미만":
		op = "<"
	default:
		op = ">=" // 참가 실적요건은 통상 하한(이상)
	}
	return op, digits, unitWord + "원", true
}

// buildTrackRecordRuleRows — 참가자격 섹션에서 참가필수 실적요건(정량 임계 명확)만 추출.
func buildTrackRecordRuleRows(sections []extractedSection) []trackRecordRuleRow {
	var rows []trackRecordRuleRow
	seen := map[string]bool{} // 같은 줄이 여러 앵커 섹션에 중복 포함돼도 조건은 1개만(§8)
	for _, sec := range sections {
		// 섹션 제목이 평가/PQ 문맥이면 섹션 전체 제외(STEP 2-C-1 LOW 방어 — 실측 근거는
		// trackRecordSectionExcludeTokens 주석 참고).
		if containsAnyToken(sec.anchorText, trackRecordSectionExcludeTokens) {
			continue
		}
		for _, raw := range strings.Split(sec.anchorText+"\n"+sec.sectionText, "\n") {
			line := strings.TrimSpace(raw)
			if line == "" || !strings.Contains(line, "실적") || seen[line] {
				continue
			}
			if containsAnyToken(line, trackRecordExcludeTokens) {
				continue // 평가/서류/개인정보 문맥 → 참가요건 아님(§22)
			}
			// 배점표 구간(예 "50억원 이상 10억원 미만") 오탐 방지 — 단일 하한이 아니라
			// 점수 구간이므로 참가요건이 아니다. "이상"과 "미만/이하"가 한 줄에 함께
			// 있으면 배점표로 보고 건너뛴다(§22 실측 리스크 대응).
			if strings.Contains(line, "이상") && (strings.Contains(line, "미만") || strings.Contains(line, "이하")) {
				continue
			}
			op, thr, unit, hasAmount := parseTrackRecordAmount(line)
			hasYears := recentYearsRe.MatchString(line)
			// 건수형(STEP 2-C-1): 금액이 없을 때만 "N건 이상"을 구조화한다(금액이 있으면
			// 금액 임계가 우선 — 실공고에서 금액+건수 동시 문형은 미확인이라 보수적으로).
			if !hasAmount {
				if cop, cthr, cunit, hasCount := parseTrackRecordCount(line); hasCount {
					op, thr, unit = cop, cthr, cunit
					hasAmount = true // 아래 생성 조건 재사용(정량 임계 있음)
				}
			}
			if !hasAmount && !hasYears {
				continue // 정량 임계 없는 모호한 실적은 구조화하지 않음(§4/§21)
			}
			seen[line] = true
			rows = append(rows, trackRecordRuleRow{
				conditionName:  truncateRunes(line, 200),
				sourceText:     truncateRunes(line, 500),
				operator:       op,
				thresholdValue: thr,
				unit:           unit,
				reviewStatus:   reviewStatusForRule(sec.hasTableNearby),
			})
		}
	}
	return rows
}

type documentRuleRow struct {
	documentName string
	sourceText   string
	reviewStatus string
}

func buildRequiredDocumentRuleRows(sections []extractedSection) []documentRuleRow {
	var rows []documentRuleRow
	for _, sec := range sections {
		listLike := findListLikeLines(sec.sectionText)
		reviewStatus := reviewStatusForRule(sec.hasTableNearby)

		switch {
		case isReferenceOnlySection(sec.sectionText, listLike):
			rows = append(rows, documentRuleRow{
				documentName: "(본문에 목록 없음 - 별첨/타 문서 참조)",
				sourceText:   strings.TrimSpace(sec.anchorText + "\n" + sec.sectionText),
				reviewStatus: reviewStatus,
			})
		case len(listLike) > 0:
			for _, line := range listLike {
				name := stripListPrefix(line)
				if name == "" {
					name = line
				}
				rows = append(rows, documentRuleRow{
					documentName: truncateRunes(name, 500),
					sourceText:   line,
					reviewStatus: reviewStatus,
				})
			}
		case sec.sectionText != "":
			snippet := strings.TrimSpace(truncateRunes(sec.sectionText, 80))
			if utf8.RuneCountInString(sec.sectionText) > 80 {
				snippet += "..."
			}
			rows = append(rows, documentRuleRow{
				documentName: snippet,
				sourceText:   strings.TrimSpace(sec.anchorText + "\n" + sec.sectionText),
				reviewStatus: reviewStatus,
			})
			// section_text가 완전히 비어있으면 아무것도 만들어내지 않는다 —
			// 목록을 추측하지 않는다는 원칙(extract_sections.py와 동일).
		}
	}
	return rows
}

// processAttachmentForRuleExtraction — 첨부파일 1건을 규칙 기반으로 처리.
// 재실행돼도 중복이 쌓이지 않도록 이 첨부파일 기준 규칙 기반 행을 지우고
// 다시 넣는다(원본 스크립트와 동일한 멱등성 방식).
func (s *Server) processAttachmentForRuleExtraction(ctx context.Context, tx *sql.Tx, attachmentID, text string) (int, int, error) {
	var noticeVersionID string
	if err := tx.QueryRowContext(ctx, `SELECT notice_version_id FROM attachments WHERE id = $1`, attachmentID).Scan(&noticeVersionID); err != nil {
		return 0, 0, err
	}

	eligSections := extractSectionsFromText(text, eligibilityKeywords)
	eligRows := buildEligibilityRuleRows(eligSections)
	trackRows := buildTrackRecordRuleRows(eligSections)
	docRows := buildRequiredDocumentRuleRows(extractSectionsFromText(text, documentKeywords))

	// 이 첨부의 규칙기반 general/실적 행을 지우고 다시 넣는다(멱등). 실적 category도
	// 이 첨부가 만든 것만 지운다 — g2b 구조화필드 auto 조건(source_attachment_id=NULL)은
	// 건드리지 않는다.
	if _, err := tx.ExecContext(ctx, `DELETE FROM eligibility_conditions WHERE source_attachment_id = $1 AND category IN ('general','실적')`, attachmentID); err != nil {
		return 0, 0, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM required_documents WHERE source_attachment_id = $1`, attachmentID); err != nil {
		return 0, 0, err
	}

	for _, r := range eligRows {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO eligibility_conditions
				(notice_version_id, category, condition_name, operator, threshold_value, source_text, source_attachment_id, confidence, review_status)
			VALUES ($1,'general',$2,'n/a',NULL,$3,$4,$5,$6)`,
			noticeVersionID, r.conditionName, r.sourceText, attachmentID, ruleExtractionConfidence, r.reviewStatus,
		); err != nil {
			return 0, 0, err
		}
	}
	for _, r := range trackRows {
		var thr interface{}
		if r.thresholdValue != "" {
			thr = r.thresholdValue
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO eligibility_conditions
				(notice_version_id, category, condition_name, operator, threshold_value, unit, source_text, source_attachment_id, confidence, review_status, extraction_method)
			VALUES ($1,'실적',$2,$3,$4,$5,$6,$7,$8,$9,'rule')`,
			noticeVersionID, r.conditionName, r.operator, thr, r.unit, r.sourceText, attachmentID, ruleExtractionConfidence, r.reviewStatus,
		); err != nil {
			return 0, 0, err
		}
	}
	for _, r := range docRows {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO required_documents
				(notice_version_id, document_name, source_text, source_attachment_id, confidence, review_status)
			VALUES ($1,$2,$3,$4,$5,$6)`,
			noticeVersionID, r.documentName, r.sourceText, attachmentID, ruleExtractionConfidence, r.reviewStatus,
		); err != nil {
			return 0, 0, err
		}
	}
	return len(eligRows) + len(trackRows), len(docRows), nil
}

// runRuleBasedDocumentExtraction scans attachments whose text is ready but
// not yet rule-processed. Empty-text rows (run_extraction.py hasn't reached
// them yet) are left alone — no watermark set — so the next hourly batch
// picks them up automatically once the text shows up. Returns how many
// attachments were actually processed this run (for admin visibility).
func (s *Server) runRuleBasedDocumentExtraction(ctx context.Context) int {
	// 🚨 B-3(2026-08-09): 이 규칙 추출은 입찰(procurement) 어휘(참가자격/제출서류)
	// 전용이라 지원사업(support_program) 공고문에는 적용하지 않는다 — 지원사업은
	// runSupportConditionExtraction이 지원사업 어휘로 별도 처리한다(역할 분리).
	rows, err := s.db.QueryContext(ctx, `
		SELECT a.id, a.extracted_text FROM attachments a
		JOIN notice_versions nv ON nv.id = a.notice_version_id
		JOIN notices n ON n.id = nv.notice_id
		WHERE a.extraction_status = 'completed' AND a.section_extraction_processed_at IS NULL
		  AND n.notice_type <> 'support_program'
		ORDER BY a.created_at
		LIMIT `+itoa(documentExtractionBatchLimit))
	if err != nil {
		s.logger.Error("document-extraction: attachment query failed", "error", err)
		return 0
	}
	type target struct{ id, text string }
	var targets []target
	for rows.Next() {
		var t target
		var text sql.NullString
		if err := rows.Scan(&t.id, &text); err != nil {
			s.logger.Error("document-extraction: attachment scan failed", "error", err)
			continue
		}
		t.text = text.String
		targets = append(targets, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		s.logger.Error("document-extraction: attachment rows iteration failed", "error", err)
	}

	processed := 0
	for _, t := range targets {
		if strings.TrimSpace(t.text) == "" {
			continue
		}
		if err := s.runRuleExtractionForOneAttachment(ctx, t.id, t.text); err != nil {
			s.logger.Error("document-extraction: rule extraction failed", "attachmentId", t.id, "error", err)
			continue
		}
		processed++
	}
	if processed > 0 {
		s.logger.Info("document-extraction: rule-based batch finished", "attachmentsProcessed", processed)
	}
	return processed
}

func (s *Server) runRuleExtractionForOneAttachment(ctx context.Context, attachmentID, text string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	nElig, nDoc, err := s.processAttachmentForRuleExtraction(ctx, tx, attachmentID, text)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE attachments SET section_extraction_processed_at = now() WHERE id = $1`, attachmentID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if nElig > 0 || nDoc > 0 {
		s.logger.Info("document-extraction: rule-based extraction", "attachmentId", attachmentID, "eligibility", nElig, "requiredDocuments", nDoc)
	}
	return nil
}

// ============================================================
// 2b. AI 보완 추출 (analyzer/ai_extract.py 포팅)
// ============================================================

const (
	aiSupplementConfidence = 0.60
	aiContextWindowRunes   = 3000
)

const eligibilityPromptTemplate = "다음은 대한민국 공공입찰 공고문에서 발췌한 원문입니다. '<표>'는 표가 있던 자리를 나타내는 " +
	"마커이고, 그 아래 줄들은 표의 각 행을 '|'로 구분해 풀어 쓴 것입니다.\n\n" +
	"이 원문에서 실제 참가자격 요건 항목만 추출하세요.\n\n" +
	"중요한 규칙:\n" +
	"- 원문에 없는 내용은 절대 만들어내지 마세요.\n" +
	"- quotedText는 아래 원문에 실제로 등장하는 문장/구절을 그대로 복사해야 합니다. 요약하거나 바꿔 쓰지 마세요.\n" +
	"- 참가자격 요건이 아닌 일반 안내문, 유의사항, 서류 목록은 포함하지 마세요.\n" +
	"- 항목이 없으면 빈 배열을 반환하세요.\n\n" +
	"원문:\n---\n%s\n---"

const documentPromptTemplate = "다음은 대한민국 공공입찰 공고문에서 발췌한 원문입니다. '<표>'는 표가 있던 자리를 나타내는 " +
	"마커이고, 그 아래 줄들은 표의 각 행을 '|'로 구분해 풀어 쓴 것입니다.\n\n" +
	"이 원문에서 실제 제출서류 항목만 추출하세요.\n\n" +
	"중요한 규칙:\n" +
	"- 원문에 없는 내용은 절대 만들어내지 마세요.\n" +
	"- quotedText는 아래 원문에 실제로 등장하는 문장/구절을 그대로 복사해야 합니다. 요약하거나 바꿔 쓰지 마세요.\n" +
	"- 서류 항목이 아닌 일반 안내문, 유의사항, 참가자격 조건은 포함하지 마세요.\n" +
	"- 항목이 없으면 빈 배열을 반환하세요.\n\n" +
	"원문:\n---\n%s\n---"

func eligibilitySupplementTool() anthropic.ToolParam {
	return anthropic.ToolParam{
		Name:        "extract_eligibility_conditions",
		Description: anthropic.String("원문에서 실제로 발견되는 참가자격 조건만 추출합니다. 원문에 없는 내용은 절대 만들지 마세요."),
		InputSchema: anthropic.ToolInputSchemaParam{
			Properties: map[string]any{
				"conditions": map[string]any{
					"type": "array",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"conditionName": map[string]any{"type": "string", "description": "조건을 요약하는 짧은 제목 (200자 이내)"},
							"quotedText":    map[string]any{"type": "string", "description": "아래 원문에서 그대로 복사한 문장/구절. 절대 새로 만들거나 바꿔쓰지 말 것."},
						},
						"required":             []string{"conditionName", "quotedText"},
						"additionalProperties": false,
					},
				},
			},
			Required:    []string{"conditions"},
			ExtraFields: map[string]any{"additionalProperties": false},
		},
		Strict: anthropic.Bool(true),
	}
}

func documentSupplementTool() anthropic.ToolParam {
	return anthropic.ToolParam{
		Name:        "extract_required_documents",
		Description: anthropic.String("원문에서 실제로 발견되는 제출서류 항목만 추출합니다. 원문에 없는 내용은 절대 만들지 마세요."),
		InputSchema: anthropic.ToolInputSchemaParam{
			Properties: map[string]any{
				"documents": map[string]any{
					"type": "array",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"documentName": map[string]any{"type": "string", "description": "서류명 (500자 이내)"},
							"quotedText":   map[string]any{"type": "string", "description": "아래 원문에서 그대로 복사한 문장/구절. 절대 새로 만들거나 바꿔쓰지 말 것."},
						},
						"required":             []string{"documentName", "quotedText"},
						"additionalProperties": false,
					},
				},
			},
			Required:    []string{"documents"},
			ExtraFields: map[string]any{"additionalProperties": false},
		},
		Strict: anthropic.Bool(true),
	}
}

type aiSupplementItem struct {
	ConditionName string `json:"conditionName"`
	DocumentName  string `json:"documentName"`
	QuotedText    string `json:"quotedText"`
}

func normalizeWhitespace(s string) string {
	return strings.TrimSpace(whitespaceRunRe.ReplaceAllString(s, " "))
}

func firstNonEmptyLine(text string) string {
	for _, line := range strings.Split(text, "\n") {
		if l := strings.TrimSpace(line); l != "" {
			return l
		}
	}
	return ""
}

// buildAISupplementContext — anchor(source_text 첫 줄)를 extracted_text에서
// 찾아 그 위치부터 aiContextWindowRunes만큼 잘라 넘긴다(1차 규칙 기반이
// review_required로 표시한 이유의 실제 내용을 포함시키기 위해 저장된
// source_text보다 넓게 잡는다 — ai_extract.py의 build_context와 동일).
func buildAISupplementContext(extractedText, sourceText string) string {
	anchor := firstNonEmptyLine(sourceText)
	if extractedText != "" && anchor != "" {
		if idx := strings.Index(extractedText, anchor); idx != -1 {
			return truncateRunes(extractedText[idx:], aiContextWindowRunes)
		}
	}
	return sourceText
}

// verifyAndLocateQuote checks quotedText actually occurs in context
// (whitespace-insensitive) before trusting it — 환각 방지. 통과하면 원본
// context 안의 실제 표기(개행/공백 포함)를 다시 찾아 반환한다.
func verifyAndLocateQuote(quotedText, context string) (string, bool) {
	normQuote := normalizeWhitespace(quotedText)
	if normQuote == "" {
		return "", false
	}
	if !strings.Contains(normalizeWhitespace(context), normQuote) {
		return "", false
	}
	tokens := strings.Fields(normQuote)
	parts := make([]string, len(tokens))
	for i, t := range tokens {
		parts[i] = regexp.QuoteMeta(t)
	}
	pattern, err := regexp.Compile(strings.Join(parts, `\s+`))
	if err != nil {
		return quotedText, true
	}
	if m := pattern.FindString(context); m != "" {
		return m, true
	}
	return quotedText, true
}

func (s *Server) callClaudeForDocumentSupplement(ctx context.Context, kind, textContext string) ([]aiSupplementItem, error) {
	var tool anthropic.ToolParam
	var promptTemplate, toolName, itemsKey string
	if kind == "eligibility" {
		tool, promptTemplate, toolName, itemsKey = eligibilitySupplementTool(), eligibilityPromptTemplate, "extract_eligibility_conditions", "conditions"
	} else {
		tool, promptTemplate, toolName, itemsKey = documentSupplementTool(), documentPromptTemplate, "extract_required_documents", "documents"
	}

	disabledThinking := anthropic.NewThinkingConfigDisabledParam()
	resp, err := s.anthropicClient.Messages.New(ctx, anthropic.MessageNewParams{
		Model:        companyDocumentModel,
		MaxTokens:    2048,
		Thinking:     anthropic.ThinkingConfigParamUnion{OfDisabled: &disabledThinking},
		OutputConfig: anthropic.OutputConfigParam{Effort: anthropic.OutputConfigEffortLow},
		Tools:        []anthropic.ToolUnionParam{{OfTool: &tool}},
		ToolChoice:   anthropic.ToolChoiceParamOfTool(toolName),
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(fmt.Sprintf(promptTemplate, textContext))),
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
			var raw map[string]json.RawMessage
			if err := json.Unmarshal(tu.Input, &raw); err != nil {
				return nil, fmt.Errorf("parse tool input: %w", err)
			}
			var items []aiSupplementItem
			if itemsRaw, ok := raw[itemsKey]; ok {
				if err := json.Unmarshal(itemsRaw, &items); err != nil {
					return nil, fmt.Errorf("parse %s: %w", itemsKey, err)
				}
			}
			return items, nil
		}
	}
	return nil, fmt.Errorf("no tool_use block in response (stop_reason=%s)", resp.StopReason)
}

type aiSupplementTarget struct {
	id, noticeVersionID, sourceAttachmentID, sourceText string
	extractedText                                       sql.NullString
	attempts                                            int
}

func (s *Server) fetchEligibilitySupplementTargets(ctx context.Context) ([]aiSupplementTarget, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT ec.id, ec.notice_version_id, ec.source_attachment_id, ec.source_text, a.extracted_text, ec.ai_supplement_attempts
		FROM eligibility_conditions ec
		JOIN attachments a ON a.id = ec.source_attachment_id
		WHERE ec.review_status = 'review_required' AND ec.source_attachment_id IS NOT NULL
		  AND ec.ai_supplement_attempted_at IS NULL
		ORDER BY ec.created_at
		LIMIT `+itoa(documentExtractionBatchLimit))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []aiSupplementTarget
	for rows.Next() {
		var t aiSupplementTarget
		if err := rows.Scan(&t.id, &t.noticeVersionID, &t.sourceAttachmentID, &t.sourceText, &t.extractedText, &t.attempts); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// fetchDocumentSupplementTargets — required_documents엔 eligibility_
// conditions와 달리 created_at 컬럼이 없어(스키마 참고) id로 정렬한다
// (ai_extract.py의 fetch_document_targets와 동일한 제약).
func (s *Server) fetchDocumentSupplementTargets(ctx context.Context) ([]aiSupplementTarget, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT rd.id, rd.notice_version_id, rd.source_attachment_id, rd.source_text, a.extracted_text, rd.ai_supplement_attempts
		FROM required_documents rd
		JOIN attachments a ON a.id = rd.source_attachment_id
		WHERE rd.review_status = 'review_required' AND rd.source_attachment_id IS NOT NULL
		  AND rd.ai_supplement_attempted_at IS NULL
		ORDER BY rd.id
		LIMIT `+itoa(documentExtractionBatchLimit))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []aiSupplementTarget
	for rows.Next() {
		var t aiSupplementTarget
		if err := rows.Scan(&t.id, &t.noticeVersionID, &t.sourceAttachmentID, &t.sourceText, &t.extractedText, &t.attempts); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// aiSupplementResult — 규칙기반과 달리 AI 보완은 대상마다 성공/실패/포기
// (재시도 상한 도달) 세 갈래로 갈릴 수 있어 각각을 센다. 관리자 수동 실행
// 응답(handleRunDocumentExtraction)과 배치 로그 양쪽에서 재사용한다.
type aiSupplementResult struct {
	TargetCount int
	SavedCount  int
	FailedCount int
	GaveUpCount int
}

// markEligibilitySupplementAttemptFailed increments the retry counter for a
// failed AI-supplement call. Once it reaches maxAISupplementAttempts, the
// watermark is set anyway (giving up) so the hourly batch stops retrying a
// permanently-broken target (e.g. an expired API key left unnoticed) — the
// underlying review_required row itself is untouched, so a human can still
// review it manually via review.go; only the AI auto-supplement is skipped
// from then on.
func (s *Server) markEligibilitySupplementAttemptFailed(ctx context.Context, id string, attempts int) (gaveUp bool, err error) {
	newAttempts := attempts + 1
	gaveUp = newAttempts >= maxAISupplementAttempts
	if gaveUp {
		_, err = s.db.ExecContext(ctx,
			`UPDATE eligibility_conditions SET ai_supplement_attempts = $2, ai_supplement_attempted_at = now() WHERE id = $1`,
			id, newAttempts)
	} else {
		_, err = s.db.ExecContext(ctx,
			`UPDATE eligibility_conditions SET ai_supplement_attempts = $2 WHERE id = $1`,
			id, newAttempts)
	}
	return gaveUp, err
}

func (s *Server) markDocumentSupplementAttemptFailed(ctx context.Context, id string, attempts int) (gaveUp bool, err error) {
	newAttempts := attempts + 1
	gaveUp = newAttempts >= maxAISupplementAttempts
	if gaveUp {
		_, err = s.db.ExecContext(ctx,
			`UPDATE required_documents SET ai_supplement_attempts = $2, ai_supplement_attempted_at = now() WHERE id = $1`,
			id, newAttempts)
	} else {
		_, err = s.db.ExecContext(ctx,
			`UPDATE required_documents SET ai_supplement_attempts = $2 WHERE id = $1`,
			id, newAttempts)
	}
	return gaveUp, err
}

func (s *Server) runAIEligibilitySupplementExtraction(ctx context.Context) aiSupplementResult {
	targets, err := s.fetchEligibilitySupplementTargets(ctx)
	if err != nil {
		s.logger.Error("document-extraction: eligibility ai-supplement target query failed", "error", err)
		return aiSupplementResult{}
	}
	result := aiSupplementResult{TargetCount: len(targets)}
	for _, t := range targets {
		textContext := buildAISupplementContext(t.extractedText.String, t.sourceText)
		items, err := s.callClaudeForDocumentSupplement(ctx, "eligibility", textContext)
		if err != nil {
			result.FailedCount++
			gaveUp, markErr := s.markEligibilitySupplementAttemptFailed(ctx, t.id, t.attempts)
			if markErr != nil {
				s.logger.Error("document-extraction: mark eligibility ai-supplement attempt failed", "id", t.id, "error", markErr)
			}
			if gaveUp {
				result.GaveUpCount++
				s.logger.Warn("document-extraction: eligibility ai-supplement gave up after max attempts", "id", t.id, "attempts", t.attempts+1, "error", err)
			} else {
				s.logger.Warn("document-extraction: eligibility ai-supplement call failed (retry next batch)", "id", t.id, "attempts", t.attempts+1, "error", err)
			}
			continue
		}
		var verified []aiSupplementItem
		for _, it := range items {
			if located, ok := verifyAndLocateQuote(it.QuotedText, textContext); ok {
				it.QuotedText = located
				verified = append(verified, it)
			}
		}
		if err := s.saveEligibilitySupplement(ctx, t, verified); err != nil {
			s.logger.Error("document-extraction: save eligibility ai-supplement failed", "id", t.id, "error", err)
			continue
		}
		result.SavedCount += len(verified)
	}
	if result.TargetCount > 0 {
		s.logger.Info("document-extraction: eligibility ai-supplement batch finished",
			"targets", result.TargetCount, "itemsSaved", result.SavedCount, "failed", result.FailedCount, "gaveUp", result.GaveUpCount)
	}
	return result
}

func (s *Server) runAIDocumentSupplementExtraction(ctx context.Context) aiSupplementResult {
	targets, err := s.fetchDocumentSupplementTargets(ctx)
	if err != nil {
		s.logger.Error("document-extraction: document ai-supplement target query failed", "error", err)
		return aiSupplementResult{}
	}
	result := aiSupplementResult{TargetCount: len(targets)}
	for _, t := range targets {
		textContext := buildAISupplementContext(t.extractedText.String, t.sourceText)
		items, err := s.callClaudeForDocumentSupplement(ctx, "document", textContext)
		if err != nil {
			result.FailedCount++
			gaveUp, markErr := s.markDocumentSupplementAttemptFailed(ctx, t.id, t.attempts)
			if markErr != nil {
				s.logger.Error("document-extraction: mark document ai-supplement attempt failed", "id", t.id, "error", markErr)
			}
			if gaveUp {
				result.GaveUpCount++
				s.logger.Warn("document-extraction: document ai-supplement gave up after max attempts", "id", t.id, "attempts", t.attempts+1, "error", err)
			} else {
				s.logger.Warn("document-extraction: document ai-supplement call failed (retry next batch)", "id", t.id, "attempts", t.attempts+1, "error", err)
			}
			continue
		}
		var verified []aiSupplementItem
		for _, it := range items {
			if located, ok := verifyAndLocateQuote(it.QuotedText, textContext); ok {
				it.QuotedText = located
				verified = append(verified, it)
			}
		}
		if err := s.saveDocumentSupplement(ctx, t, verified); err != nil {
			s.logger.Error("document-extraction: save document ai-supplement failed", "id", t.id, "error", err)
			continue
		}
		result.SavedCount += len(verified)
	}
	if result.TargetCount > 0 {
		s.logger.Info("document-extraction: document ai-supplement batch finished",
			"targets", result.TargetCount, "itemsSaved", result.SavedCount, "failed", result.FailedCount, "gaveUp", result.GaveUpCount)
	}
	return result
}

func (s *Server) saveEligibilitySupplement(ctx context.Context, t aiSupplementTarget, items []aiSupplementItem) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for _, it := range items {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO eligibility_conditions
				(notice_version_id, category, condition_name, operator, threshold_value, source_text, source_attachment_id, confidence, review_status, extraction_method, model_version)
			VALUES ($1,'general',$2,'n/a',NULL,$3,$4,$5,'pending','ai',$6)`,
			t.noticeVersionID, truncateRunes(it.ConditionName, 200), it.QuotedText, t.sourceAttachmentID, aiSupplementConfidence, companyDocumentModel,
		); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE eligibility_conditions SET ai_supplement_attempted_at = now() WHERE id = $1`, t.id); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Server) saveDocumentSupplement(ctx context.Context, t aiSupplementTarget, items []aiSupplementItem) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for _, it := range items {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO required_documents
				(notice_version_id, document_name, source_text, source_attachment_id, confidence, review_status, extraction_method, model_version)
			VALUES ($1,$2,$3,$4,$5,'pending','ai',$6)`,
			t.noticeVersionID, truncateRunes(it.DocumentName, 500), it.QuotedText, t.sourceAttachmentID, aiSupplementConfidence, companyDocumentModel,
		); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE required_documents SET ai_supplement_attempted_at = now() WHERE id = $1`, t.id); err != nil {
		return err
	}
	return tx.Commit()
}

// documentExtractionSummary — RunDocumentExtraction's result, surfaced to
// the system_admin manual-trigger endpoint (handleRunDocumentExtraction) so
// the "시스템 상태" admin screen shows actual counts instead of just
// {"status":"completed"} (이전엔 성공/실패/대기 건수를 전혀 볼 수 없었음).
type documentExtractionSummary struct {
	Status                         string `json:"status"`
	RuleBasedProcessedCount        int    `json:"ruleBasedProcessedCount"`
	SupportConditionProcessedCount int    `json:"supportConditionProcessedCount"` // B-3: 지원사업 규칙 추출 건수

	EligibilitySupplementTargetCount int `json:"eligibilitySupplementTargetCount"`
	EligibilitySupplementSavedCount  int `json:"eligibilitySupplementSavedCount"`
	EligibilitySupplementFailedCount int `json:"eligibilitySupplementFailedCount"`
	EligibilitySupplementGaveUpCount int `json:"eligibilitySupplementGaveUpCount"`
	DocumentSupplementTargetCount    int `json:"documentSupplementTargetCount"`
	DocumentSupplementSavedCount     int `json:"documentSupplementSavedCount"`
	DocumentSupplementFailedCount    int `json:"documentSupplementFailedCount"`
	DocumentSupplementGaveUpCount    int `json:"documentSupplementGaveUpCount"`
}

// RunDocumentExtraction is the entry point cmd/apiserver calls on a
// background ticker (same 1-hour cadence as notice collection — see
// startBackgroundDocumentExtraction). Each sub-batch logs its own errors and
// keeps going, matching RunDailyNotifications' pattern.
func (s *Server) RunDocumentExtraction(ctx context.Context) documentExtractionSummary {
	ruleProcessed := s.runRuleBasedDocumentExtraction(ctx)
	// B-3(2026-08-09): 지원사업 상세조건 규칙 추출(AI 없음) — 같은 배치에 얹는다.
	supportProcessed := s.runSupportConditionExtraction(ctx)
	eligResult := s.runAIEligibilitySupplementExtraction(ctx)
	docResult := s.runAIDocumentSupplementExtraction(ctx)
	return documentExtractionSummary{
		Status:                           "completed",
		RuleBasedProcessedCount:          ruleProcessed,
		SupportConditionProcessedCount:   supportProcessed,
		EligibilitySupplementTargetCount: eligResult.TargetCount,
		EligibilitySupplementSavedCount:  eligResult.SavedCount,
		EligibilitySupplementFailedCount: eligResult.FailedCount,
		EligibilitySupplementGaveUpCount: eligResult.GaveUpCount,
		DocumentSupplementTargetCount:    docResult.TargetCount,
		DocumentSupplementSavedCount:     docResult.SavedCount,
		DocumentSupplementFailedCount:    docResult.FailedCount,
		DocumentSupplementGaveUpCount:    docResult.GaveUpCount,
	}
}

// handleRunDocumentExtraction manually fires the document-extraction batch on
// demand — same system_admin-only pattern as handleRunNotifications/
// handleRunPipelineAutoTransitions. The only other trigger is the hourly
// ticker in cmd/apiserver.
func (s *Server) handleRunDocumentExtraction(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.currentUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	role, err := s.userRole(r.Context(), userID)
	if err != nil {
		s.logger.Error("run-document-extraction: role lookup failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if role != "system_admin" {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
		return
	}
	summary := s.RunDocumentExtraction(r.Context())
	writeJSON(w, http.StatusOK, summary)
}

// ---------- 빈 상태 문구 개선(2026-08-07) ----------
// "참가자격 요건"/"제출서류"/파이프라인 체크리스트가 비어 보일 때 그
// 이유를 attachments 테이블의 실제 진행 단계로 구분한다 — 이 파일 위쪽
// 주석의 3단계 파이프라인(1. run_extraction.py 텍스트추출(사람이 수동
// cron) → 2. 이 파일의 RunDocumentExtraction(apiserver 자체 1시간
// 주기 자동배치) → 3. review.go 사람 검수)과 정확히 대응한다.
const (
	// docAnalysisNoAttachments — 이 공고 버전에 첨부파일 자체가 없다.
	// 원문에 정보가 없는 것과 같은 뜻이라 "대기"가 아니라 "정보없음"으로
	// 안내해야 한다 — 기다린다고 채워지지 않는다.
	docAnalysisNoAttachments = "no_attachments"
	// docAnalysisExtractingText — 첨부파일은 있지만 전부 아직
	// extraction_status != 'completed'(pending/processing/failed/
	// unsupported 포함). 이 단계는 사람이 수동으로 돌리는 run_extraction.py
	// 몫이라 정확한 완료 시점을 약속할 근거가 없다.
	docAnalysisExtractingText = "extracting_text"
	// docAnalysisAwaitingStructured — 텍스트 추출이 끝난(completed) 첨부가
	// 최소 하나 있는데 아직 section_extraction_processed_at이 안 찍혔다.
	// 이 다음 단계(RunDocumentExtraction)는 apiserver 자체 1시간 주기
	// 배치라 "보통 1시간 이내"라고 정직하게 약속할 수 있다.
	docAnalysisAwaitingStructured = "awaiting_structured_extraction"
	// docAnalysisAnalyzedEmpty — 첨부가 있고 전부 텍스트추출+구조화추출까지
	// 끝났는데도(section_extraction_processed_at 전부 non-null) 실제
	// eligibility_conditions/required_documents가 비어있다 — "대기 중"이
	// 아니라 "분석은 끝났지만 원문에 해당 정보가 없다"는 뜻.
	docAnalysisAnalyzedEmpty = "analyzed_empty"
)

// computeNoticeDocumentAnalysisStatus classifies why a notice version's
// eligibility_conditions/required_documents might still be empty. Callers
// should only invoke this when they've already confirmed both are empty —
// it always re-derives the 4-way state from attachments, so calling it
// otherwise would just waste a query.
func (s *Server) computeNoticeDocumentAnalysisStatus(ctx context.Context, versionID string) (string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT extraction_status = 'completed', section_extraction_processed_at IS NOT NULL
		FROM attachments WHERE notice_version_id = $1`, versionID)
	if err != nil {
		return "", err
	}
	defer rows.Close()

	count := 0
	hasCompletedUnprocessed := false
	hasIncompleteExtraction := false
	for rows.Next() {
		count++
		var textCompleted, structuredProcessed bool
		if err := rows.Scan(&textCompleted, &structuredProcessed); err != nil {
			continue
		}
		if textCompleted {
			if !structuredProcessed {
				hasCompletedUnprocessed = true
			}
		} else {
			hasIncompleteExtraction = true
		}
	}
	if err := rows.Err(); err != nil {
		return "", err
	}

	switch {
	case count == 0:
		return docAnalysisNoAttachments, nil
	case hasCompletedUnprocessed:
		return docAnalysisAwaitingStructured, nil
	case hasIncompleteExtraction:
		return docAnalysisExtractingText, nil
	default:
		return docAnalysisAnalyzedEmpty, nil
	}
}

// computeNoticeDocumentAnalysisStatusByNoticeID — handleGetPipelineEntry
// only has notice_id handy(파이프라인 엔트리는 버전이 아니라 공고 단위로
// 연결됨), so it looks up the current version itself rather than making
// every caller do that.
func (s *Server) computeNoticeDocumentAnalysisStatusByNoticeID(ctx context.Context, noticeID string) (string, error) {
	var versionID string
	err := s.db.QueryRowContext(ctx,
		`SELECT id FROM notice_versions WHERE notice_id = $1 AND is_current = true`, noticeID,
	).Scan(&versionID)
	if err != nil {
		return "", err
	}
	return s.computeNoticeDocumentAnalysisStatus(ctx, versionID)
}
