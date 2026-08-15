package api

import (
	"strings"
	"testing"
)

// STEP 2-C: 참가필수 실적요건 규칙 추출 검증. 입력 문자열은 로컬 실데이터
// (attachments.extracted_text)에서 뽑은 실제 공고 원문 형태를 사용한다.
// 핵심은 "실적"이라는 단어가 있다고 다 참가요건으로 만들지 않는 것(§3/§22).

func trackRows(t *testing.T, anchor, section string) []trackRecordRuleRow {
	t.Helper()
	return buildTrackRecordRuleRows([]extractedSection{{anchorText: anchor, sectionText: section}})
}

func TestTrackRecord_ParticipationThresholdExtracted(t *testing.T) {
	// 참가자격 섹션 안, 단일 하한 금액 + 최근 N년 → 구조화되어야 함.
	rows := trackRows(t, "참가자격", "○ 최근 3년간 유사 용역 수행실적 1억원 이상인 자")
	if len(rows) != 1 {
		t.Fatalf("want 1 track-record row, got %d: %+v", len(rows), rows)
	}
	r := rows[0]
	if r.operator != ">=" || r.thresholdValue != "1" || r.unit != "억원" {
		t.Errorf("amount parse wrong: op=%q thr=%q unit=%q", r.operator, r.thresholdValue, r.unit)
	}
}

func TestTrackRecord_AmountUnits(t *testing.T) {
	cases := []struct{ line, thr, unit string }{
		{"최근 3년 유사실적 5천만원 이상", "5", "천만원"},
		{"최근 3년 실적 50,000,000원 이상", "50000000", "원"},
		{"최근 5년 이내 실적 3억원 이상", "3", "억원"},
	}
	for _, c := range cases {
		rows := trackRows(t, "참가자격", c.line)
		if len(rows) != 1 {
			t.Fatalf("%q: want 1 row, got %d", c.line, len(rows))
		}
		if rows[0].thresholdValue != c.thr || rows[0].unit != c.unit {
			t.Errorf("%q: thr=%q unit=%q want thr=%q unit=%q", c.line, rows[0].thresholdValue, rows[0].unit, c.thr, c.unit)
		}
	}
}

func TestTrackRecord_PeriodOnlyExtractedWithoutAmount(t *testing.T) {
	// 금액 없이 "최근 N년"만 명확 → 구조화하되 금액 임계는 비운다(§7).
	rows := trackRows(t, "참가자격", "○ 최근 3년간 동종 용역 수행실적을 보유한 업체")
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(rows))
	}
	if rows[0].thresholdValue != "" {
		t.Errorf("period-only should have empty threshold, got %q", rows[0].thresholdValue)
	}
}

func TestTrackRecord_EvaluationScoreBandNotExtracted(t *testing.T) {
	// 실제 배점표 줄(0390a132): "이상 ... 미만" 구간 + 점수 → 참가요건 아님.
	line := "①-1. [중소기업] 최근 3년간 50억원 이상 10억원 미만 50억원 미만 10 연평균 매출액(‘23~’25) 10 8 6"
	if rows := trackRows(t, "참가자격", line); len(rows) != 0 {
		t.Errorf("evaluation score-band must NOT be extracted, got %+v", rows)
	}
}

func TestTrackRecord_EvaluationTokenNotExtracted(t *testing.T) {
	// "실적 평가/가점/제출/증빙" 문맥은 참가요건 아님(§22).
	for _, line := range []string{
		"○ 해외 전시회 참여 실적 (평가 25점)",
		"○ 관련 분야 주요 수행 및 지원 실적 등 (필요시 증빙서류 제출)",
		"가점 서류: 수출 증빙자료, 수출실적증명서 등 1억원 이상",
	} {
		if rows := trackRows(t, "참가자격", line); len(rows) != 0 {
			t.Errorf("eval/doc context must NOT be extracted: %q -> %+v", line, rows)
		}
	}
}

func TestTrackRecord_ForeignCurrencyNotExtracted(t *testing.T) {
	// 실제 참가제한(0060a551): "수출실적 1,000만불 이상" — 외화라 원 비교 불가 → 미구조화.
	line := "* (참가제한) ① 전년도(2025년) 수출실적 1,000만불 이상 기업"
	if rows := trackRows(t, "참가자격", line); len(rows) != 0 {
		t.Errorf("foreign-currency amount must NOT be structured, got %+v", rows)
	}
}

func TestTrackRecord_CompoundAmountNotStructured(t *testing.T) {
	// 복합금액(1억 5천만원)은 과소파싱 위험 → 금액 임계 비움(최근 N년으로만 구조화).
	rows := trackRows(t, "참가자격", "최근 3년 유사실적 1억 5천만원 이상")
	if len(rows) != 1 {
		t.Fatalf("want 1 row (period), got %d", len(rows))
	}
	if rows[0].thresholdValue != "" {
		t.Errorf("compound amount should be UNKNOWN threshold, got %q %q", rows[0].thresholdValue, rows[0].unit)
	}
}

func TestTrackRecord_BareMentionNotExtracted(t *testing.T) {
	// 정량 임계 없는 모호한 실적은 구조화 안 함(제출서류 경로에 위임, §4/§21).
	if rows := trackRows(t, "참가자격", "○ 유사 용역 수행실적이 있는 업체"); len(rows) != 0 {
		t.Errorf("bare 실적 (no threshold) must NOT be extracted, got %+v", rows)
	}
}

// ───────── STEP 2-C-1: 실공고 스팟 검증(2026-08-15)에서 확인된 문형 보정 ─────────

func TestTrackRecord_CountThresholdExtracted(t *testing.T) {
	// 실제 공고 ①(방사선환경 로봇 실증센터 설계용역, eb8c87db) 참가자격 원문.
	line := "공고일 기준 방사선 조사시설의 차폐설계 및 인허가 기술지원 관련 실적 1건 이상 보유하여 업무 를 수행할 수 있는 역량을 갖춘 업체 또는 해당 역량을 갖춘 업체를 포함한 공동수급체"
	rows := trackRows(t, "3. 입찰참가자격", line)
	if len(rows) != 1 {
		t.Fatalf("건수형 참가필수 실적이 추출돼야 함, got %d: %+v", len(rows), rows)
	}
	r := rows[0]
	if r.operator != ">=" || r.thresholdValue != "1" || r.unit != "건" {
		t.Errorf("count parse wrong: op=%q thr=%q unit=%q (want >=,1,건)", r.operator, r.thresholdValue, r.unit)
	}
	if r.sourceText == "" || !contains(r.sourceText, "실적 1건 이상") {
		t.Errorf("source_text 원문 보존 실패: %q", r.sourceText)
	}
}

func TestTrackRecord_CountVariants(t *testing.T) {
	for _, line := range []string{
		"수행실적 2건 이상인 업체",
		"유사용역 실적 3건 이상 보유한 자",
		"관련 실적이 1건 이상 있을 것",
	} {
		rows := trackRows(t, "참가자격", line)
		if len(rows) != 1 || rows[0].unit != "건" {
			t.Errorf("%q: want 1 count row(unit=건), got %+v", line, rows)
		}
	}
}

func TestTrackRecord_PQSectionAnchorNotExtracted(t *testing.T) {
	// 실제 공고 ③(신현배수지 PQ, dc99e8a7): 섹션 제목이 평가 문맥("사업수행능력 평가서
	// 제출 참가자격")이면 줄에 '평가'가 없어도 섹션 전체를 제외해야 한다(LOW 방어).
	line := "3) 유사용역수행실적은 입찰공고일을 기준으로 최근 3년간 발주청에서 시행한 “상하수도분야”건설 공사의 건설사업관리용역을 전체 준공하였거나, 수행중인 실적으로"
	if rows := trackRows(t, "사업수행능력 평가서 제출 참가자격", line); len(rows) != 0 {
		t.Errorf("PQ 평가 섹션의 실적 인정기준이 참가필수로 오탐됨: %+v", rows)
	}
	// 같은 줄이 PQ 토큰을 담고 있으면 라인 단위로도 방어된다.
	if rows := trackRows(t, "참가자격", "PQ 심사를 위한 유사용역수행실적은 최근 3년간 실적으로 한다"); len(rows) != 0 {
		t.Errorf("PQ 라인 토큰 방어 실패: %+v", rows)
	}
}

func TestTrackRecord_RealEvalPhraseNotExtracted(t *testing.T) {
	// 실제 공고 ②(논현동, bc395ac6) 적격심사 문형 — '평가' 토큰으로 제외되어야 한다.
	line := "▢ 이행실적 평가 : 공고일 기준 최근 3년간 준공 완료된 공공기관 용역실적의 합계 - 당해용역 추정가격 : 163,636,364원"
	if rows := trackRows(t, "참가자격", line); len(rows) != 0 {
		t.Errorf("적격심사 이행실적 평가가 참가필수로 오탐됨: %+v", rows)
	}
}

func TestRequiredDocs_ParenNumberPrefix(t *testing.T) {
	// 실제 공고 ① 제출서류 원문 — "(4)" 괄호숫자 목록이 문서명으로 인식되어야 한다(MEDIUM-2).
	section := "(3) 건축사 면허 소지를 확인할 수 있는 증빙서류(건축사자격등록증 등) 1부.\n(4) 방사선 조사시설의 차폐설계 및 인허가 기술지원 관련 실적을 확인할 수 있는 증빙서류 1부."
	rows := buildRequiredDocumentRuleRows([]extractedSection{{anchorText: "3-1. 제출서류", sectionText: section}})
	var names []string
	for _, r := range rows {
		names = append(names, r.documentName)
	}
	foundTrack := false
	for _, n := range names {
		if contains(n, "실적") {
			foundTrack = true
		}
	}
	if !foundTrack {
		t.Fatalf("괄호숫자 목록의 실적 증빙서류가 문서명으로 인식돼야 함, got %v", names)
	}
	// 기존 prefix(1. / 가. / -)도 계속 동작해야 한다.
	rows2 := buildRequiredDocumentRuleRows([]extractedSection{{anchorText: "제출서류", sectionText: "1. 사업자등록증 사본 1부\n가. 법인등기부등본 1부\n- 인감증명서 1부"}})
	if len(rows2) != 3 {
		t.Errorf("기존 목록 prefix 회귀: got %d rows %+v", len(rows2), rows2)
	}
}

func TestTrackRecord_CountUnitNotTreatedAsAmount(t *testing.T) {
	// unit='건'은 금액 환산에 절대 들어가면 안 된다(§3) — 건수 임계로만 읽혀야 한다.
	reqs := []noticeRequirement{{Type: reqTypeTrackRecord, ThresholdValue: "1", Unit: "건", SourceText: "실적 1건 이상"}}
	if amt, ok := trackRecordAmountThreshold(reqs); ok {
		t.Errorf("unit=건이 금액 임계로 오해됨: %d", amt)
	}
	if n, ok := trackRecordCountThreshold(reqs); !ok || n != 1 {
		t.Errorf("건수 임계 파싱 실패: n=%d ok=%v", n, ok)
	}
}

func contains(s, sub string) bool { return len(s) >= len(sub) && strings.Contains(s, sub) }
