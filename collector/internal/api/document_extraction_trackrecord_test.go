package api

import "testing"

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
