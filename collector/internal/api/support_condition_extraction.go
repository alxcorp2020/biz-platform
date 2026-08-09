// support_condition_extraction.go — B-3(2026-08-09). 지원사업(support_program)
// 공고문(SUPPORT_PRINT_DOCUMENT) extracted_text에서 '상세 신청조건'을 규칙 기반으로
// 구조화해 support_program_conditions에 저장한다. AI는 쓰지 않는다(이번 단계 RULE만).
//
// 원칙:
//   - 공식 API(support_program_details)의 분류값을 절대 덮어쓰지 않는다 — 이 테이블은
//     보완(상세조건)만 담당한다(역할 분리).
//   - 원문 근거 없는 값은 저장하지 않는다 — 모든 텍스트 값은 verifyAndLocateQuote로
//     extracted_text 안에 실재하는지 검증한 뒤에만 저장한다(규칙 추출도 동일 원칙).
//   - 숫자 정밀 구조화(support_limit_amount)는 명확한 패턴일 때만. 애매하면 NULL,
//     원문 텍스트(support_limit_text)는 항상 보존.
//   - 텍스트 빈약 문서(text_poor)는 실패로 단정하지 않고 LOW/needs_ai로 표시.
//   - 재분석 방지: source_file_hash + extractor_version이 그대로면 다시 뽑지 않는다.
package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
)

// nullStr는 빈 문자열을 SQL NULL로 저장하기 위한 헬퍼(api 패키지의 nullIfEmpty는
// *string을 받으므로 값 타입용으로 따로 둔다).
func nullStr(s string) interface{} {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}

// extractor_version — 규칙 로직을 바꾸면 올린다(올리면 기존 문서도 1회 재추출).
const supportConditionExtractorVersion = "support-rule-v3"

// 텍스트가 이보다 짧으면 표지/이미지 위주로 보고 text_poor 처리(OCR/AI 강제 안 함).
const supportTextPoorThreshold = 400

// 섹션 제목/유사어. extractSectionsFromText가 공백 무시 부분일치로 앵커를 찾는다.
var (
	scEligibilityKW = []string{"지원대상", "신청자격", "지원자격", "신청요건", "지원조건", "참가자격", "응모자격"}
	scDocumentKW    = []string{"제출서류", "신청서류", "구비서류", "필수서류"}
	scSupportKW     = []string{"지원내용", "지원규모", "지원금액", "지원한도", "지원비율"} // 지원분야는 공식 분류와 겹쳐 제외
	scExclusionKW   = []string{"제외대상", "지원제외", "신청제외"}
	scPreferenceKW  = []string{"우대사항", "가점"}
	scSelectionKW   = []string{"선정절차", "평가방법", "평가기준", "심사"}
)

// 정밀 단서 정규식 — 보수적으로만 사용.
var (
	scAmountRe      = regexp.MustCompile(`억원|천만원|백만원|만원`)
	scLimitRe       = regexp.MustCompile(`최대|한도|이내|까지`)
	scRateRe        = regexp.MustCompile(`\d+\s*%|자부담|보조율|지원율`)
	scAgeRe         = regexp.MustCompile(`창업\s*\d+\s*년|업력\s*\d+\s*년|\d+\s*년\s*이내|\d+\s*년\s*미만`)
	scRevenueRe     = regexp.MustCompile(`매출`)
	scRegionRe      = regexp.MustCompile(`관내|소재지|본사|사업장|소재`)
	scLimitAmountRe = regexp.MustCompile(`(?:최대|한도)[^0-9억천백만]{0,8}([0-9]+)\s*(억|천만|백만|만)`)
)

// supportRequiredDoc — support_program_conditions.required_documents JSONB 요소.
type supportRequiredDoc struct {
	Name       string `json:"name"`
	Required   bool   `json:"required"`
	SourceText string `json:"source_text"`
}

// supportConditionRow — 한 공고의 추출 결과(규칙).
type supportConditionRow struct {
	eligibilityText      string
	requiredDocuments    []supportRequiredDoc
	supportAmountText    string
	supportLimitText     string
	supportLimitAmount   *int64
	supportRateText      string
	supportScaleText     string
	businessAgeCondition string
	revenueCondition     string
	regionCondition      string
	exclusionConditions  []string
	preferenceConditions []string
	selectionProcess     string
	textLength           int
	textPoor             bool
	needsAI              bool
	confidence           string
}

// firstMatchLine은 text의 여러 줄 중 re에 처음 매칭되는 줄(원문 근거)을 돌려준다.
func firstMatchLine(text string, re *regexp.Regexp) string {
	for _, raw := range strings.Split(text, "\n") {
		l := strings.TrimSpace(raw)
		if l != "" && re.MatchString(l) {
			return l
		}
	}
	return ""
}

// longestSection은 keywords로 찾은 섹션 중 본문이 가장 긴 것을 돌려준다(없으면 nil).
func longestSection(text string, keywords []string) *extractedSection {
	secs := extractSectionsFromText(text, keywords)
	var best *extractedSection
	for i := range secs {
		if best == nil || len(secs[i].sectionText) > len(best.sectionText) {
			best = &secs[i]
		}
	}
	return best
}

// 다음 번호 섹션 헤더(2. / 3) / ② / 나. 등)를 만나면 윈도우를 끊는다.
var nextSectionHeadRe = regexp.MustCompile(`^\s*(?:[0-9]+\s*[.)]|[①-⑳]|[가-힣]\s*[.)])`)

// windowAfterAnchor — 섹션 본문이 헤더 휴리스틱에 잘려 비는 경우(짧은 명사구
// content가 헤더로 오인됨)를 보완한다. keyword가 처음 나온 줄 다음부터 다음 번호
// 섹션 전까지 최대 maxLines개의 비어있지 않은 줄을 이어붙여 돌려준다.
func windowAfterAnchor(text string, keywords []string, maxLines int) string {
	lines := strings.Split(text, "\n")
	for i, l := range lines {
		norm := strings.ReplaceAll(strings.TrimSpace(l), " ", "")
		hit := false
		for _, kw := range keywords {
			if strings.Contains(norm, kw) {
				hit = true
				break
			}
		}
		if !hit {
			continue
		}
		var out []string
		blanks := 0
		for j := i + 1; j < len(lines) && len(out) < maxLines; j++ {
			t := strings.TrimSpace(lines[j])
			if t == "" {
				blanks++
				if blanks >= 2 && len(out) > 0 {
					break
				}
				continue
			}
			blanks = 0
			if len(out) > 0 && nextSectionHeadRe.MatchString(t) {
				break
			}
			out = append(out, t)
		}
		if len(out) > 0 {
			return strings.Join(out, " ")
		}
	}
	return ""
}

// bestBody는 섹션 본문을 돌려주되, 비어있으면(헤더로 잘림) 윈도우로 보완한다.
// fromSection=true면 진짜 섹션 본문(신뢰도 높음), false면 윈도우 보완(낮음).
func bestBody(text string, keywords []string, maxLines int) (body string, fromSection bool) {
	if sec := longestSection(text, keywords); sec != nil {
		if b := strings.TrimSpace(sec.sectionText); b != "" {
			return b, true
		}
	}
	return windowAfterAnchor(text, keywords, maxLines), false
}

// parseSupportLimitAmount는 "최대 5천만원" 같은 명확한 패턴에서만 원 단위 정수를
// 뽑는다. 애매하면 nil(원문 텍스트는 별도 보존).
func parseSupportLimitAmount(text string) *int64 {
	m := scLimitAmountRe.FindStringSubmatch(text)
	if m == nil {
		return nil
	}
	n, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return nil
	}
	var unit int64
	switch m[2] {
	case "억":
		unit = 100000000
	case "천만":
		unit = 10000000
	case "백만":
		unit = 1000000
	case "만":
		unit = 10000
	default:
		return nil
	}
	v := n * unit
	return &v
}

// grounded는 값이 실제 원문(text)에 존재할 때만 그 값을 돌려준다(근거 검증).
func grounded(value, text string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if _, ok := verifyAndLocateQuote(value, text); ok {
		return value
	}
	return ""
}

// buildSupportConditions는 공고문 텍스트에서 규칙 기반으로 상세조건을 구성한다.
func buildSupportConditions(text string) supportConditionRow {
	var row supportConditionRow
	row.textLength = len([]rune(text))
	row.textPoor = row.textLength < supportTextPoorThreshold
	row.requiredDocuments = []supportRequiredDoc{}
	row.exclusionConditions = []string{}
	row.preferenceConditions = []string{}

	// 신청자격 — 섹션 본문이 잘리면 윈도우로 보완(짧은 명사구 content 대응).
	eligFromSection := false
	if eligBody, fromSec := bestBody(text, scEligibilityKW, 6); eligBody != "" {
		if g := grounded(truncateRunes(eligBody, 800), text); g != "" {
			row.eligibilityText = g
			eligFromSection = fromSec
		}
		// 업력/매출/지역 단서(신청자격 본문 우선, 없으면 전체)
		row.businessAgeCondition = grounded(firstMatchLine(eligBody, scAgeRe), text)
		row.regionCondition = grounded(firstMatchLine(eligBody, scRegionRe), text)
	}
	if row.businessAgeCondition == "" {
		row.businessAgeCondition = grounded(firstMatchLine(text, scAgeRe), text)
	}
	if row.regionCondition == "" {
		row.regionCondition = grounded(firstMatchLine(text, scRegionRe), text)
	}
	row.revenueCondition = grounded(firstMatchLine(text, scRevenueRe), text)

	// 제출서류(목록화). 목록 없이 "별첨 참조"만이면 참조 1건으로.
	docsFromList := false
	if sec := longestSection(text, scDocumentKW); sec != nil {
		listLike := findListLikeLines(sec.sectionText)
		if isReferenceOnlySection(sec.sectionText, listLike) {
			st := grounded(strings.TrimSpace(sec.anchorText+"\n"+sec.sectionText), text)
			if st != "" {
				row.requiredDocuments = append(row.requiredDocuments, supportRequiredDoc{
					Name: "(본문에 목록 없음 - 별첨/타 문서 참조)", Required: true, SourceText: truncateRunes(st, 300)})
			}
		} else if len(listLike) > 0 {
			docsFromList = true
			for _, line := range listLike {
				name := strings.TrimSpace(stripListPrefix(line))
				if name == "" {
					name = line
				}
				if grounded(line, text) == "" {
					continue
				}
				row.requiredDocuments = append(row.requiredDocuments, supportRequiredDoc{
					Name: truncateRunes(name, 200), Required: true, SourceText: truncateRunes(line, 300)})
			}
		} else if strings.TrimSpace(sec.sectionText) != "" {
			// 목록 형태가 아니지만 서류 섹션 본문이 있으면 통째로 1건 보존(bid 파이프라인과
			// 동일한 fallback) — 목록을 추측해서 쪼개지 않고 원문 근거만 남긴다.
			if st := grounded(truncateRunes(strings.TrimSpace(sec.sectionText), 300), text); st != "" {
				name := firstNonEmptyLine(sec.sectionText)
				row.requiredDocuments = append(row.requiredDocuments, supportRequiredDoc{
					Name: truncateRunes(name, 200), Required: true, SourceText: st})
			}
		}
	}

	// 지원내용 → 금액/한도/비율(원문 텍스트만, 숫자는 명확할 때만)
	if body, _ := bestBody(text, scSupportKW, 8); body != "" {
		if scAmountRe.MatchString(body) {
			row.supportAmountText = grounded(firstMatchLine(body, scAmountRe), text)
		}
		if scLimitRe.MatchString(body) {
			row.supportLimitText = grounded(firstMatchLine(body, scLimitRe), text)
			if amt := parseSupportLimitAmount(body); amt != nil {
				row.supportLimitAmount = amt
			}
		}
		if scRateRe.MatchString(body) {
			row.supportRateText = grounded(firstMatchLine(body, scRateRe), text)
		}
		row.supportScaleText = grounded(truncateRunes(strings.TrimSpace(body), 500), text)
	}

	// 제외대상
	if sec := longestSection(text, scExclusionKW); sec != nil {
		for _, line := range findListLikeLines(sec.sectionText) {
			if g := grounded(line, text); g != "" {
				row.exclusionConditions = append(row.exclusionConditions, truncateRunes(g, 300))
			}
		}
		if len(row.exclusionConditions) == 0 {
			if g := grounded(truncateRunes(sec.sectionText, 500), text); g != "" {
				row.exclusionConditions = append(row.exclusionConditions, g)
			}
		}
	}

	// 우대사항
	if sec := longestSection(text, scPreferenceKW); sec != nil {
		for _, line := range findListLikeLines(sec.sectionText) {
			if g := grounded(line, text); g != "" {
				row.preferenceConditions = append(row.preferenceConditions, truncateRunes(g, 300))
			}
		}
	}

	// 선정절차
	if body, _ := bestBody(text, scSelectionKW, 6); body != "" {
		row.selectionProcess = grounded(truncateRunes(body, 600), text)
	}

	// confidence — 윈도우 보완(오탐 가능)은 과신하지 않는다: HIGH는 신청자격이
	// 진짜 섹션 본문에서 나오고 제출서류도 실제 목록으로 잡힌 경우만.
	switch {
	case row.textPoor:
		row.confidence = "LOW"
	case row.eligibilityText != "" && eligFromSection && docsFromList:
		row.confidence = "HIGH"
	case row.eligibilityText != "" || len(row.requiredDocuments) > 0:
		row.confidence = "MEDIUM"
	default:
		row.confidence = "LOW"
	}

	// needs_ai — 규칙이 못 채운 '정밀 조건'이 있으면 향후 AI 후보(step 19).
	row.needsAI = row.textPoor ||
		row.eligibilityText == "" ||
		(row.supportAmountText == "" && row.supportLimitText == "") ||
		row.supportLimitAmount == nil ||
		row.businessAgeCondition == "" ||
		row.regionCondition == ""

	return row
}

// upsertSupportCondition은 한 공고의 규칙 추출 결과를 저장(있으면 갱신)한다.
func (s *Server) upsertSupportCondition(ctx context.Context, noticeID, attachmentID, fileHash string, row supportConditionRow) error {
	reqDocs, _ := json.Marshal(row.requiredDocuments)
	excl, _ := json.Marshal(row.exclusionConditions)
	pref, _ := json.Marshal(row.preferenceConditions)
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO support_program_conditions
			(notice_id, source_document_id, source_file_hash, eligibility_text, required_documents,
			 support_amount_text, support_limit_text, support_limit_amount, support_rate_text, support_scale_text,
			 business_age_condition, revenue_condition, region_condition, exclusion_conditions, preference_conditions,
			 selection_process, text_length, text_poor, needs_ai, extraction_method, confidence,
			 extractor_version, ai_version, extracted_at, updated_at)
		VALUES ($1,$2,$3,$4,$5::jsonb,$6,$7,$8,$9,$10,$11,$12,$13,$14::jsonb,$15::jsonb,$16,$17,$18,$19,'RULE',$20,$21,NULL,now(),now())
		ON CONFLICT (notice_id) DO UPDATE SET
			source_document_id=EXCLUDED.source_document_id, source_file_hash=EXCLUDED.source_file_hash,
			eligibility_text=EXCLUDED.eligibility_text, required_documents=EXCLUDED.required_documents,
			support_amount_text=EXCLUDED.support_amount_text, support_limit_text=EXCLUDED.support_limit_text,
			support_limit_amount=EXCLUDED.support_limit_amount, support_rate_text=EXCLUDED.support_rate_text,
			support_scale_text=EXCLUDED.support_scale_text, business_age_condition=EXCLUDED.business_age_condition,
			revenue_condition=EXCLUDED.revenue_condition, region_condition=EXCLUDED.region_condition,
			exclusion_conditions=EXCLUDED.exclusion_conditions, preference_conditions=EXCLUDED.preference_conditions,
			selection_process=EXCLUDED.selection_process, text_length=EXCLUDED.text_length, text_poor=EXCLUDED.text_poor,
			needs_ai=EXCLUDED.needs_ai, extraction_method='RULE', confidence=EXCLUDED.confidence,
			extractor_version=EXCLUDED.extractor_version, extracted_at=now(), updated_at=now()`,
		noticeID, nullStr(attachmentID), nullStr(fileHash), nullStr(row.eligibilityText), string(reqDocs),
		nullStr(row.supportAmountText), nullStr(row.supportLimitText), row.supportLimitAmount, nullStr(row.supportRateText), nullStr(row.supportScaleText),
		nullStr(row.businessAgeCondition), nullStr(row.revenueCondition), nullStr(row.regionCondition), string(excl), string(pref),
		nullStr(row.selectionProcess), row.textLength, row.textPoor, row.needsAI, row.confidence,
		supportConditionExtractorVersion)
	return err
}

// runSupportConditionExtraction은 지원사업 공고문 중 아직(또는 파일/버전이 바뀌어)
// 추출 안 된 것을 규칙으로 처리한다. AI 없음. 처리 건수를 돌려준다.
func (s *Server) runSupportConditionExtraction(ctx context.Context) int {
	rows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT ON (n.id) a.id, COALESCE(a.file_hash,''), a.extracted_text, n.id
		FROM attachments a
		JOIN notice_versions nv ON nv.id = a.notice_version_id AND nv.is_current = true
		JOIN notices n ON n.id = nv.notice_id
		LEFT JOIN support_program_conditions c ON c.notice_id = n.id
		WHERE n.notice_type = 'support_program'
		  AND a.attachment_role = 'SUPPORT_PRINT_DOCUMENT'
		  AND a.extraction_status = 'completed'
		  AND (c.notice_id IS NULL
		       OR c.source_file_hash IS DISTINCT FROM a.file_hash
		       OR c.extractor_version IS DISTINCT FROM $1)
		ORDER BY n.id, a.created_at
		LIMIT `+itoa(documentExtractionBatchLimit), supportConditionExtractorVersion)
	if err != nil {
		s.logger.Error("support-condition-extraction: query failed", "error", err)
		return 0
	}
	type target struct{ attID, fileHash, text, noticeID string }
	var targets []target
	for rows.Next() {
		var t target
		var text sql.NullString
		if err := rows.Scan(&t.attID, &t.fileHash, &text, &t.noticeID); err != nil {
			s.logger.Error("support-condition-extraction: scan failed", "error", err)
			continue
		}
		t.text = text.String
		targets = append(targets, t)
	}
	rows.Close()

	processed := 0
	for _, t := range targets {
		row := buildSupportConditions(t.text)
		if err := s.upsertSupportCondition(ctx, t.noticeID, t.attID, t.fileHash, row); err != nil {
			s.logger.Error("support-condition-extraction: upsert failed", "noticeId", t.noticeID, "error", err)
			continue
		}
		processed++
	}
	if processed > 0 {
		s.logger.Info("support-condition-extraction: finished", "noticesProcessed", processed)
	}
	return processed
}
