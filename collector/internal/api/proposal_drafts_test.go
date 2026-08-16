package api

import (
	"archive/zip"
	"bytes"
	"io"
	"strings"
	"testing"
	"time"
)

func f64(v float64) *float64 { return &v }

// 규칙 추출: "항목명(N점)" 형태 + anchor 창 안 "항목명 N" 형태만, 합계/구분 행 제외,
// 배점 없는 항목은 만들지 않음(0점 추측 금지는 AI 경로에서 null 유지로 검증).
func TestExtractEvaluationCriteriaRule(t *testing.T) {
	text := `3. 평가항목 및 배점기준
가. 기술능력평가(90점) : 정량적 평가(20점) + 정성적 평가(70점)
1) 사업 이해도 (20점)
2) 수행계획의 적정성 (25점)
3) 전문인력 (20점)
4) 유사사업 수행실적 (20점)
5) 사후관리 (15점)
가격평가(10점)
계 100
- 42 -`
	set := extractEvaluationCriteriaRule(text)
	if set.Method != "rule" {
		t.Fatalf("method")
	}
	titles := []string{}
	for _, c := range set.Criteria {
		titles = append(titles, c.Title)
	}
	joined := strings.Join(titles, "|")
	for _, want := range []string{"사업 이해도", "수행계획의 적정성", "전문인력", "유사사업 수행실적", "사후관리", "가격평가"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in %v", want, titles)
		}
	}
	if strings.Contains(joined, "계") && !strings.Contains(joined, "수행계획") {
		t.Fatalf("total row must be excluded: %v", titles)
	}
	for _, c := range set.Criteria {
		if c.Title == "사업 이해도" && (c.Score == nil || *c.Score != 20) {
			t.Fatalf("score parse: %+v", c)
		}
	}
	finalizeCriteriaSet(set)
	if set.Criteria[0].ID != "c1" {
		t.Fatalf("ids")
	}
}

// 대응표: 실적/인력/인증/재무/사후관리/이해도/계획/가격 분류와 회사정보 유무에 따른 상태·질문.
func TestBuildReadiness_MappingAndQuestions(t *testing.T) {
	set := &evaluationCriteriaSet{Criteria: []evaluationCriterion{
		{ID: "c1", Title: "사업 이해도", Score: f64(20)},
		{ID: "c2", Title: "수행계획", Score: f64(25), SubCriteria: []string{"추진전략", "단계별 수행방법"}},
		{ID: "c3", Title: "전문인력", Score: f64(20)},
		{ID: "c4", Title: "유사사업 수행실적", Score: f64(20)},
		{ID: "c5", Title: "사후관리", Score: f64(15)},
		{ID: "c6", Title: "가격평가", Score: f64(10), Category: "price"},
		{ID: "c7", Title: "경영상태", Score: nil},
	}}
	finalizeCriteriaSet(set)
	if set.TotalScore != nil {
		t.Fatalf("total must be nil when a score is unknown")
	}
	factsEmpty := &companyFacts{}
	items, questions, sum := buildReadiness(set, factsEmpty)
	byID := map[string]readinessItem{}
	for _, it := range items {
		byID[it.CriterionID] = it
	}
	if byID["c1"].Kind != kindUnderstanding || byID["c1"].Status != readyStatusReady {
		t.Fatalf("c1: %+v", byID["c1"])
	}
	if byID["c2"].Kind != kindPlan || byID["c2"].Status != readyStatusReady {
		t.Fatalf("c2: %+v", byID["c2"])
	}
	if byID["c3"].Kind != kindPersonnel || byID["c3"].Status != readyStatusInput || byID["c3"].Question == nil || !strings.Contains(byID["c3"].Question.Prompt, "책임자") {
		t.Fatalf("c3: %+v", byID["c3"])
	}
	if byID["c4"].Kind != kindTrackRecord || byID["c4"].Status != readyStatusInput || byID["c4"].Question == nil || byID["c4"].Question.LinkHref == "" {
		t.Fatalf("c4: %+v", byID["c4"])
	}
	if byID["c5"].Kind != kindManagement || byID["c5"].Status != readyStatusInput || len(byID["c5"].Question.Options) < 2 {
		t.Fatalf("c5: %+v", byID["c5"])
	}
	if byID["c6"].Kind != kindPrice || byID["c6"].Status != readyStatusNA {
		t.Fatalf("c6: %+v", byID["c6"])
	}
	if byID["c7"].Kind != kindFinancial || byID["c7"].Status != readyStatusInput {
		t.Fatalf("c7: %+v", byID["c7"])
	}
	if sum.CriteriaCount != 7 || sum.ReadyCount != 2 || sum.NeedsInput != 4 {
		t.Fatalf("summary: %+v", sum)
	}
	if len(questions) != 4 {
		t.Fatalf("questions: %d", len(questions))
	}
	// 회사정보가 있으면 다시 묻지 않는다(실적 3건 → 작성 준비 완료, 질문 없음).
	factsFull := &companyFacts{
		TrackRecords: []trackRecordFact{{ProjectName: "A"}, {ProjectName: "B"}, {ProjectName: "C"}},
		Personnel:    []personnelFact{{Role: "PM", CareerYears: f64(10), Qualifications: []string{}}},
		Financials:   []financialFact{{FiscalYear: 2025}},
	}
	items, _, sum = buildReadiness(set, factsFull)
	for _, it := range items {
		if it.CriterionID == "c4" && (it.Status != readyStatusReady || it.Question != nil || !strings.Contains(it.Evidence[0], "3건")) {
			t.Fatalf("track record with facts: %+v", it)
		}
		if it.CriterionID == "c3" && it.Status != readyStatusPartial {
			t.Fatalf("personnel with facts should be partial(책임자 선택): %+v", it)
		}
		if it.CriterionID == "c7" && it.Status != readyStatusReady {
			t.Fatalf("financial with facts: %+v", it)
		}
	}
	if sum.NeedsInput != 1 { // 사후관리만
		t.Fatalf("needsInput with facts: %+v", sum)
	}
}

// 사실성: 회사 DB에 없는 사실은 생성하지 않고 [확인 필요]로 남긴다. 등록된 실적은
// 그대로(금액 포함) 반영. 평가기준 순서 유지. 답변은 해당 섹션에만 반영.
func TestComposeProposalDraft_NoFabrication(t *testing.T) {
	notice := &proposalNoticeFacts{Title: "테스트 행사 대행 용역", OrganizationName: "테스트기관", ExternalNoticeID: "R26TEST", BudgetAmount: func() *int64 { v := int64(50000000); return &v }(), AwardMethod: "협상에 의한 계약", SummaryLines: []string{"요약1"}}
	set := &evaluationCriteriaSet{Criteria: []evaluationCriterion{
		{ID: "c1", Title: "사업 이해도", Score: f64(20)},
		{ID: "c2", Title: "수행계획", Score: f64(25), SubCriteria: []string{"추진전략"}},
		{ID: "c3", Title: "전문인력", Score: f64(20)},
		{ID: "c4", Title: "유사사업 수행실적", Score: f64(20)},
		{ID: "c5", Title: "사후관리", Score: f64(15)},
	}, Requirements: []proposalRequirement{{Label: "분량", Value: "30페이지 이내"}}}
	amt := int64(120000000)
	done := true
	facts := &companyFacts{CompanyName: "테스트 주식회사", TrackRecords: []trackRecordFact{{ProjectName: "실제 실적 사업", ClientName: "실제 발주처", PeriodStart: "2025-01-01", PeriodEnd: "2025-06-30", ContractAmount: &amt, IsCompleted: &done}}}
	answers := map[string]answerValue{"q_c5": {Value: "24h", Text: "전담 콜센터 운영"}}
	content := composeProposalDraft(notice, set, facts, answers)

	if len(content.Sections) != 6 { // 개요 + 5
		t.Fatalf("sections: %d", len(content.Sections))
	}
	if content.Sections[1].Title != "사업 이해도" || content.Sections[5].Title != "사후관리" {
		t.Fatalf("order not preserved")
	}
	all := ""
	for _, s := range content.Sections {
		all += s.Title + "\n" + s.Body + "\n" + strings.Join(s.Bullets, "\n") + "\n" + strings.Join(s.Missing, "\n") + "\n"
		for _, tb := range s.Tables {
			for _, r := range tb.Rows {
				all += strings.Join(r, "|") + "\n"
			}
		}
	}
	// 실제 실적만(이름·발주처·금액 그대로).
	if !strings.Contains(all, "실제 실적 사업|실제 발주처|2025-01-01 ~ 2025-06-30|120,000,000원|완료") {
		t.Fatalf("real track record must be reflected verbatim:\n%s", all)
	}
	// 인력 없음 → 가짜 인물/경력 없이 [확인 필요].
	pers := content.Sections[3]
	if len(pers.Tables) != 0 || len(pers.Missing) == 0 || !strings.Contains(pers.Missing[0], "[확인 필요: 본 사업에 투입할 책임자(PM)") {
		t.Fatalf("personnel must be [확인 필요]: %+v", pers)
	}
	// 개요: 대표자/주소/직원수/매출 등 없는 값은 표 셀에 [확인 필요]로(부록 목록에도 포함).
	intro := content.Sections[0]
	introFlat := ""
	for _, r := range intro.Tables[0].Rows {
		introFlat += strings.Join(r, "|") + "\n"
	}
	if !strings.Contains(introFlat, "대표자|[확인 필요: 대표자명") || !strings.Contains(strings.Join(content.Missing, "\n"), "대표자명") {
		t.Fatalf("intro missing markers: %s / %v", introFlat, content.Missing)
	}
	// 답변은 사후관리 섹션에만.
	mgmt := content.Sections[5]
	if !strings.Contains(strings.Join(mgmt.Bullets, "\n"), "24시간") || !strings.Contains(strings.Join(mgmt.Bullets, "\n"), "전담 콜센터 운영") {
		t.Fatalf("answer not applied: %+v", mgmt)
	}
	if strings.Contains(content.Sections[1].Body+strings.Join(content.Sections[1].Bullets, ""), "전담 콜센터") {
		t.Fatalf("answer leaked into another section")
	}
	// 공고 사실은 공고에서만.
	if !strings.Contains(all, "발주기관: 테스트기관") || !strings.Contains(all, "50,000,000원") {
		t.Fatalf("notice facts missing")
	}
	// 확인 필요 목록·면책 문구·AI 문구 금지.
	if len(content.Missing) == 0 || content.Disclaimer == "" || strings.Contains(all, "AI") {
		t.Fatalf("missing list/disclaimer/AI text: %d %q", len(content.Missing), content.Disclaimer)
	}
	// 예상 평가점수 같은 표현 금지.
	if strings.Contains(all, "예상 평가점수") || strings.Contains(all, "예상 점수") {
		t.Fatalf("must not show expected score")
	}

	// DOCX: 유효한 zip + document.xml에 제목/[확인 필요]/실적/순서/면책.
	b, err := buildProposalDocx(content, notice, facts, time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("docx: %v", err)
	}
	zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatalf("zip: %v", err)
	}
	var doc string
	for _, f := range zr.File {
		if f.Name == "word/document.xml" {
			rc, _ := f.Open()
			bb, _ := io.ReadAll(rc)
			rc.Close()
			doc = string(bb)
		}
	}
	if doc == "" {
		t.Fatalf("document.xml missing")
	}
	for _, want := range []string{"테스트 행사 대행 용역", "1. 사업 이해도 (20점)", "2. 수행계획 (25점)", "3. 전문인력 (20점)", "4. 유사사업 수행실적 (20점)", "5. 사후관리 (15점)", "[확인 필요: 본 사업에 투입할 책임자(PM)", "실제 실적 사업", "120,000,000원", "부록. 확인 필요 항목", proposalDisclaimer, "30페이지 이내"} {
		if !strings.Contains(doc, want) {
			t.Fatalf("docx missing %q", want)
		}
	}
	i1, i2, i5 := strings.Index(doc, "1. 사업 이해도"), strings.Index(doc, "2. 수행계획"), strings.Index(doc, "5. 사후관리")
	if !(i1 < i2 && i2 < i5) {
		t.Fatalf("docx order")
	}
	if strings.Contains(doc, "AI가") {
		t.Fatalf("docx must not claim AI authorship")
	}
}

func TestProposalDocxFilename(t *testing.T) {
	name := proposalDocxFilename(`2026년 "테스트"/행사: 대행<용역>?`, `주식회사 테스트|회사`, time.Date(2026, 8, 16, 3, 0, 0, 0, time.UTC))
	if strings.ContainsAny(name, `\/:*?"<>|`) {
		t.Fatalf("unsafe chars: %s", name)
	}
	if !strings.HasSuffix(name, "_20260816.docx") || !strings.Contains(name, "_제안서초안_") {
		t.Fatalf("format: %s", name)
	}
	if strings.Contains(name, "  ") {
		t.Fatalf("spaces: %s", name)
	}
}

func TestClassifyCriterionKind(t *testing.T) {
	cases := map[string]string{
		"사업 이해도": kindUnderstanding, "수행계획의 적정성": kindPlan, "전문인력": kindPersonnel, "수행조직 및 인력": kindPersonnel,
		"유사사업 수행실적": kindTrackRecord, "사후관리": kindManagement, "가격평가": kindPrice, "경영상태": kindFinancial,
		"보유 인증 및 특허": kindCredential, "품질관리 방안": kindPlan, "기타 제안": kindOther,
	}
	for title, want := range cases {
		if got := classifyCriterionKind(evaluationCriterion{Title: title}); got != want {
			t.Errorf("%s: got %s want %s", title, got, want)
		}
	}
}
