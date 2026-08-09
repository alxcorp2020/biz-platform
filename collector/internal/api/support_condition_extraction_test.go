package api

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
)

const sampleSupportDoc = `2026년 소상공인 디지털 전환 지원사업 공고

본 공고는 소상공인의 디지털 전환을 지원하기 위한 사업으로, 신청을 희망하는 기업은 아래의 내용을 충분히 숙지한 후 신청하여 주시기 바랍니다.

1. 지원대상
공고일 기준 창업 7년 이내의 중소기업으로서 서울시 관내에 사업장을 두고 있는 기업을 대상으로 합니다. 최근 연도 매출액이 10억원 미만인 기업이어야 합니다.

2. 지원내용
선정된 기업에게는 기업당 최대 5천만원 이내에서 사업비를 지원합니다. 다만 총 사업비의 자부담 30% 이상을 부담하여야 합니다.

3. 제출서류
신청 시 다음의 서류를 빠짐없이 제출하여야 합니다.
- 사업계획서 1부
- 사업자등록증 사본 1부

4. 지원제외
다음에 해당하는 기업은 지원 대상에서 제외됩니다.
- 휴업 또는 폐업 중인 기업

5. 우대사항
다음에 해당하는 기업에게는 평가 시 가점을 부여합니다.
- 청년창업기업으로 인정받은 기업

6. 선정절차
제출된 서류에 대한 서류평가를 거친 후 대면평가를 통해 최종 지원 대상을 선정합니다.

문의처: 소상공인지원센터 02-000-0000 으로 문의하시기 바랍니다.`

func TestParseSupportLimitAmount(t *testing.T) {
	cases := []struct {
		in   string
		want int64
		ok   bool
	}{
		{"기업당 최대 5천만원 이내", 50000000, true},
		{"지원 한도 3억원", 300000000, true},
		{"한도 500만원", 5000000, true},
		{"지원금 1000만원 지급", 0, false}, // 최대/한도 단서 없음 → nil(추정 안 함)
		{"최대한 지원", 0, false},        // 숫자 없음
	}
	for _, c := range cases {
		got := parseSupportLimitAmount(c.in)
		if !c.ok {
			if got != nil {
				t.Errorf("%q: nil 기대, got %v", c.in, *got)
			}
			continue
		}
		if got == nil || *got != c.want {
			t.Errorf("%q: %d 기대, got %v", c.in, c.want, got)
		}
	}
}

func TestGrounded(t *testing.T) {
	text := "공고일 기준 창업 7년 이내"
	if grounded("창업 7년 이내", text) == "" {
		t.Error("원문에 있는 값이 검증 통과해야 함")
	}
	if grounded("존재하지 않는 문장", text) != "" {
		t.Error("원문에 없는 값은 빈값이어야 함(환각 방지)")
	}
	if grounded("   ", text) != "" {
		t.Error("공백은 빈값")
	}
}

func TestBuildSupportConditions_Invariants(t *testing.T) {
	row := buildSupportConditions(sampleSupportDoc)

	// 신청자격이 잡히고 원문에 근거해야 한다
	if row.eligibilityText == "" {
		t.Fatal("eligibility_text가 비어있음")
	}
	// 근거 grounding 불변식: 모든 비어있지 않은 텍스트 값은 원문의 부분문자열
	norm := strings.Join(strings.Fields(sampleSupportDoc), "")
	for name, v := range map[string]string{
		"eligibility": row.eligibilityText, "amount": row.supportAmountText,
		"limit": row.supportLimitText, "rate": row.supportRateText,
		"age": row.businessAgeCondition, "region": row.regionCondition,
		"selection": row.selectionProcess,
	} {
		if v == "" {
			continue
		}
		if !strings.Contains(norm, strings.Join(strings.Fields(v), "")) {
			t.Errorf("%s 값이 원문 근거 없음(환각): %q", name, v)
		}
	}

	// 금액/한도 추출
	if row.supportLimitAmount == nil || *row.supportLimitAmount != 50000000 {
		t.Errorf("support_limit_amount=%v (기대 50000000)", row.supportLimitAmount)
	}
	if row.supportRateText == "" {
		t.Error("자부담 30% → rate 잡혀야 함")
	}
	// 업력/지역 단서
	if row.businessAgeCondition == "" {
		t.Error("창업 7년 → business_age 잡혀야 함")
	}
	if row.regionCondition == "" {
		t.Error("관내 → region 잡혀야 함")
	}
	// confidence + needs_ai 설정
	if row.confidence == "" {
		t.Error("confidence 미설정")
	}
	if row.textPoor {
		t.Error("정상 길이인데 text_poor")
	}
}

func TestBuildSupportConditions_TextPoor(t *testing.T) {
	row := buildSupportConditions("공고문 참조") // 매우 짧음
	if !row.textPoor {
		t.Error("짧은 텍스트는 text_poor여야 함")
	}
	if row.confidence != "LOW" {
		t.Errorf("text_poor는 LOW여야 함, got %s", row.confidence)
	}
	if !row.needsAI {
		t.Error("text_poor는 needs_ai=true여야 함")
	}
}

func TestBuildSupportConditions_Documents(t *testing.T) {
	row := buildSupportConditions(sampleSupportDoc)
	// 제출서류가 최소 1건은 잡히고, 각 항목 source_text가 원문 근거를 가진다
	if len(row.requiredDocuments) == 0 {
		t.Fatal("제출서류 0건")
	}
	norm := strings.Join(strings.Fields(sampleSupportDoc), "")
	for _, d := range row.requiredDocuments {
		if d.SourceText == "" {
			t.Error("제출서류 source_text 비어있음")
		}
		if d.SourceText != "" && !strings.Contains(norm, strings.Join(strings.Fields(d.SourceText), "")) {
			t.Errorf("제출서류 source_text 원문 근거 없음: %q", d.SourceText)
		}
	}
}

// 통합 테스트(BIZ_TEST_DSN): b3populate로 support_program 공고문 extracted_text가
// 채워진 상태에서 실행 → support_program_conditions 생성 + 입찰 무영향 + 멱등성.
func TestRunSupportConditionExtraction_Integration(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	srv := testServer(db)
	srv.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := context.Background()

	// 대상이 있는지 확인(없으면 skip — 텍스트추출 선행 필요)
	var ready int
	db.QueryRowContext(ctx, `
		SELECT count(*) FROM attachments a
		JOIN notice_versions nv ON nv.id=a.notice_version_id JOIN notices n ON n.id=nv.notice_id
		WHERE n.notice_type='support_program' AND a.attachment_role='SUPPORT_PRINT_DOCUMENT'
		  AND a.extraction_status='completed'`).Scan(&ready)
	if ready == 0 {
		t.Skip("support_program 공고문 extracted_text 준비 안 됨(b3populate 선행 필요)")
	}
	// 결정성: 파생 테이블을 비우고 시작(이전 실행분이 남아 0건 처리되는 걸 방지).
	if _, err := db.ExecContext(ctx, `DELETE FROM support_program_conditions`); err != nil {
		t.Fatalf("clear conditions: %v", err)
	}

	n1 := srv.runSupportConditionExtraction(ctx)
	if n1 == 0 {
		t.Fatal("추출 0건")
	}
	var total, withConf int
	db.QueryRowContext(ctx, `SELECT count(*), count(confidence) FROM support_program_conditions`).Scan(&total, &withConf)
	if total == 0 || withConf != total {
		t.Errorf("conditions rows=%d withConfidence=%d", total, withConf)
	}
	// 입찰 공고는 support_program_conditions에 없어야(역할 분리)
	var bidLeak int
	db.QueryRowContext(ctx, `
		SELECT count(*) FROM support_program_conditions c
		JOIN notices n ON n.id=c.notice_id WHERE n.notice_type<>'support_program'`).Scan(&bidLeak)
	if bidLeak != 0 {
		t.Errorf("입찰 공고가 support_program_conditions에 유출됨: %d", bidLeak)
	}
	// 멱등성: 재실행 시 같은 hash+version이면 재처리 안 함 → 0건
	n2 := srv.runSupportConditionExtraction(ctx)
	if n2 != 0 {
		t.Errorf("재실행이 %d건 처리(기대 0 — 재분석 방지)", n2)
	}
	var totalAfter int
	db.QueryRowContext(ctx, `SELECT count(*) FROM support_program_conditions`).Scan(&totalAfter)
	if totalAfter != total {
		t.Errorf("재실행 후 행 증가: %d→%d (중복)", total, totalAfter)
	}
}
