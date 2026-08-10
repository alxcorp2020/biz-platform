package api

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/lib/pq"
)

func TestRecommendationJudgmentFrom(t *testing.T) {
	// 전부 PASS → 참여 가능, 이유 없음
	ready := &participationJudgment{Grade: "ready", GradeLabel: "참여 가능", Conditions: []conditionResult{
		{ConditionType: "지역", Result: condPASS}, {ConditionType: "면허", Result: condPASS},
	}}
	r := recommendationJudgmentFrom(ready)
	if r.Grade != "ready" || len(r.Reasons) != 0 {
		t.Errorf("ready: grade=%q reasons=%v", r.Grade, r.Reasons)
	}
	// REVIEW 2개 초과 → 최대 2개, FAIL/REVIEW/UNKNOWN 순
	j := &participationJudgment{Grade: "needsReview", GradeLabel: "확인 필요", Conditions: []conditionResult{
		{ConditionType: "지역", Result: condPASS},
		{ConditionType: "면허", Result: condREVIEW},
		{ConditionType: "직접생산확인", Result: condREVIEW},
		{ConditionType: "인증", Result: condREVIEW},
	}}
	r = recommendationJudgmentFrom(j)
	if len(r.Reasons) != 2 {
		t.Fatalf("reasons len=%d want 2", len(r.Reasons))
	}
	if r.Reasons[0] != "면허 확인 필요" || r.Reasons[1] != "직접생산 세부품명 확인" {
		t.Errorf("reasons=%v", r.Reasons)
	}

	// 홈 HERO 질문형 해소(2026-08-10): 면허/인증 REVIEW의 Question이 그대로 실려야 한다
	// (상세와 동일 데이터). 질문 없는 조건(직접생산 등)은 제외.
	jq := &participationJudgment{Grade: "needsReview", GradeLabel: "확인 필요", Conditions: []conditionResult{
		{ConditionType: "면허", Result: condREVIEW, Reason: "면허 확인 필요",
			Question: &conditionQuestion{Kind: "license", Category: "면허", Targets: []string{"정보통신공사업 면허"}}},
		{ConditionType: "직접생산확인", Result: condREVIEW}, // Question 없음 → 홈 질문에서 제외
	}}
	rq := recommendationJudgmentFrom(jq)
	if len(rq.Questions) != 1 {
		t.Fatalf("questions len=%d want 1(면허만)", len(rq.Questions))
	}
	q0 := rq.Questions[0]
	if q0.ConditionType != "면허" || q0.Kind != "license" || q0.Category != "면허" ||
		len(q0.Targets) != 1 || q0.Targets[0] != "정보통신공사업 면허" || q0.Reason == "" {
		t.Errorf("question=%+v", q0)
	}
}

func TestMapCategoryResult(t *testing.T) {
	cases := map[string]string{
		"met": condPASS, "not_met": condFAIL, "needs_confirmation": condREVIEW,
		"insufficient_data": condUNKNOWN, "anything_else": condUNKNOWN,
	}
	for in, want := range cases {
		if got := mapCategoryResult(in); got != want {
			t.Errorf("mapCategoryResult(%q)=%q want %q", in, got, want)
		}
	}
}

func TestFinalizeJudgment_GradeRules(t *testing.T) {
	mk := func(conds ...conditionResult) *participationJudgment {
		j := &participationJudgment{Conditions: conds}
		finalizeJudgment(j)
		return j
	}
	P := conditionResult{Result: condPASS, Severity: sevHARD}
	hardFail := conditionResult{Result: condFAIL, Severity: sevHARD}
	softFail := conditionResult{Result: condFAIL, Severity: sevSOFT}
	review := conditionResult{Result: condREVIEW, Severity: sevHARD}
	unknown := conditionResult{Result: condUNKNOWN, Severity: sevHARD}

	// HARD FAIL 존재 → 참여 어려움
	if j := mk(P, P, hardFail); j.Grade != "notRecommended" {
		t.Errorf("HARD FAIL → %q want notRecommended", j.Grade)
	}
	// FAIL 없음 + REVIEW → 확인 필요
	if j := mk(P, P, review); j.Grade != "needsReview" {
		t.Errorf("REVIEW → %q want needsReview", j.Grade)
	}
	// FAIL 없음 + UNKNOWN → 확인 필요
	if j := mk(P, unknown); j.Grade != "needsReview" {
		t.Errorf("UNKNOWN → %q want needsReview", j.Grade)
	}
	// 전부 PASS → 참여 가능
	if j := mk(P, P, P); j.Grade != "ready" {
		t.Errorf("all PASS → %q want ready", j.Grade)
	}
	// SOFT FAIL만(나머지 PASS) → 참여 어려움이 아니어야 한다(§2)
	if j := mk(P, P, softFail); j.Grade == "notRecommended" {
		t.Errorf("SOFT FAIL이 참여어려움을 만들면 안 됨: %q", j.Grade)
	}
	// 카운트 검증
	j := mk(P, review, hardFail, unknown)
	if j.PassCount != 1 || j.ReviewCount != 1 || j.FailCount != 1 || j.UnknownCount != 1 {
		t.Errorf("counts p%d r%d f%d u%d", j.PassCount, j.ReviewCount, j.FailCount, j.UnknownCount)
	}
	if j.GradeLabel != "참여 어려움" {
		t.Errorf("GradeLabel=%q", j.GradeLabel)
	}
}

// 통합(BIZ_TEST_DSN): 면허/인증/직접생산 실제 매칭 + 종합 grade.
func TestBuildParticipationJudgment_Integration(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	srv := testServer(db)
	srv.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := context.Background()

	must := func(q string, args ...any) string {
		var id string
		if err := db.QueryRowContext(ctx, q, args...).Scan(&id); err != nil {
			t.Fatalf("seed [%.40s]: %v", q, err)
		}
		return id
	}
	ext := "JUDGE-" + time.Now().Format("150405.000000")
	userID := must(`INSERT INTO users (email) VALUES ($1) RETURNING id`, ext+"@t.local")
	profileID := must(`INSERT INTO company_profiles (user_id, company_name, direct_production_cert)
		VALUES ($1,'판정테스트사',false) RETURNING id`, userID)
	// 회사가 보유한 면허 하나(정확 이름 일치, 유효)
	must(`INSERT INTO company_licenses (company_profile_id, category, name, confidence, status)
		VALUES ($1,'면허',$2,'A','보유') RETURNING id`, profileID, "정보통신공사업 면허")
	defer func() {
		db.ExecContext(ctx, `DELETE FROM company_licenses WHERE company_profile_id=$1`, profileID)
		db.ExecContext(ctx, `DELETE FROM company_profiles WHERE id=$1`, profileID)
		db.ExecContext(ctx, `DELETE FROM users WHERE id=$1`, userID)
	}()

	score := &participationScore{Categories: []categoryScore{
		{Category: "지역", Result: "met"}, {Category: "업종", Result: "met"}, {Category: "예산 규모", Result: "met"},
	}}

	byType := func(j *participationJudgment) map[string]conditionResult {
		m := map[string]conditionResult{}
		for _, c := range j.Conditions {
			m[c.ConditionType] = c
		}
		return m
	}

	// 케이스 1: 보유 면허 요구 + 미보유 인증 요구 + 직접생산 요구
	docs := []requiredDocumentItem{
		{DocumentName: "정보통신공사업 면허"},
		{DocumentName: "ISO 9001 인증서"},
		{DocumentName: "직접생산확인증명서"},
	}
	j := srv.buildParticipationJudgment(ctx, "", profileID, score, docs)
	if j == nil {
		t.Fatal("judgment nil")
	}
	m := byType(j)
	if m["면허"].Result != condPASS {
		t.Errorf("면허=%q want PASS", m["면허"].Result)
	}
	if m["인증"].Result != condREVIEW || m["인증"].Severity != sevSOFT {
		t.Errorf("인증=%q/%q want REVIEW/SOFT", m["인증"].Result, m["인증"].Severity)
	}
	if m["직접생산확인"].Result != condREVIEW {
		t.Errorf("직접생산=%q want REVIEW", m["직접생산확인"].Result)
	}
	// 3요소 PASS + 면허 PASS + 인증/직접생산 REVIEW → 확인 필요
	if j.Grade != "needsReview" {
		t.Errorf("grade=%q want needsReview", j.Grade)
	}

	// 케이스 2(FALSE HARD FAIL 방지): 회사가 보유하지 않은/정확명 불일치 면허
	// 서류명 → HARD FAIL로 단정하지 않고 REVIEW → 참여 어려움이 아니라 확인 필요.
	j2 := srv.buildParticipationJudgment(ctx, "", profileID, score, []requiredDocumentItem{{DocumentName: "건설업 면허"}})
	if byType(j2)["면허"].Result != condREVIEW {
		t.Errorf("미매칭 면허=%q want REVIEW(FALSE FAIL 방지)", byType(j2)["면허"].Result)
	}
	if j2.Grade == "notRecommended" {
		t.Errorf("면허 미매칭이 참여어려움을 만들면 안 됨: grade=%q", j2.Grade)
	}
	if j2.Grade != "needsReview" {
		t.Errorf("grade=%q want needsReview", j2.Grade)
	}

	// 케이스 3: 서류 분석 전(빈 목록) → UNKNOWN 안전장치 → 확인 필요(참여 가능 아님)
	j3 := srv.buildParticipationJudgment(ctx, "", profileID, score, nil)
	if j3.Grade != "needsReview" {
		t.Errorf("빈 서류 grade=%q want needsReview(안전장치)", j3.Grade)
	}

	// 케이스 4: 3요소 중 하나가 not_met(구조화 판정) → 정상 HARD FAIL → 참여 어려움
	failScore := &participationScore{Categories: []categoryScore{
		{Category: "지역", Result: "not_met"}, {Category: "업종", Result: "met"}, {Category: "예산 규모", Result: "met"},
	}}
	j4 := srv.buildParticipationJudgment(ctx, "", profileID, failScore, []requiredDocumentItem{{DocumentName: "사업자등록증"}})
	if j4.Grade != "notRecommended" {
		t.Errorf("지역 not_met grade=%q want notRecommended(3요소 정상 FAIL)", j4.Grade)
	}
}

// 질문형 해소(Human-in-the-loop): REVIEW 면허/인증에 미답변 요구명이 있으면 Question이
// 붙고, 사용자가 보유로 답하면(company_licenses 저장) PASS로 바뀌며 Question이 사라진다.
// 미보유로 답하면 REVIEW는 유지하되 같은 질문을 다시 내지 않는다(§9).
func TestParticipationJudgmentQuestion_Integration(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	srv := testServer(db)
	srv.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := context.Background()

	must := func(q string, args ...any) string {
		var id string
		if err := db.QueryRowContext(ctx, q, args...).Scan(&id); err != nil {
			t.Fatalf("seed [%.40s]: %v", q, err)
		}
		return id
	}
	ext := "QUEST-" + time.Now().Format("150405.000000")
	userID := must(`INSERT INTO users (email) VALUES ($1) RETURNING id`, ext+"@t.local")
	profileID := must(`INSERT INTO company_profiles (user_id, company_name) VALUES ($1,'질문테스트사') RETURNING id`, userID)
	defer func() {
		db.ExecContext(ctx, `DELETE FROM company_licenses WHERE company_profile_id=$1`, profileID)
		db.ExecContext(ctx, `DELETE FROM company_profiles WHERE id=$1`, profileID)
		db.ExecContext(ctx, `DELETE FROM users WHERE id=$1`, userID)
	}()
	score := &participationScore{Categories: []categoryScore{
		{Category: "지역", Result: "met"}, {Category: "업종", Result: "met"}, {Category: "예산 규모", Result: "met"},
	}}
	docs := []requiredDocumentItem{{DocumentName: "정보통신공사업 면허"}}
	licCond := func(j *participationJudgment) conditionResult {
		for _, c := range j.Conditions {
			if c.ConditionType == "면허" {
				return c
			}
		}
		return conditionResult{}
	}

	// 1) 미답변 → REVIEW + Question(target=요구명, kind=license, category=면허)
	c1 := licCond(srv.buildParticipationJudgment(ctx, "", profileID, score, docs))
	if c1.Result != condREVIEW {
		t.Fatalf("초기 면허=%q want REVIEW", c1.Result)
	}
	if c1.Question == nil || c1.Question.Kind != "license" || c1.Question.Category != "면허" ||
		len(c1.Question.Targets) != 1 || c1.Question.Targets[0] != "정보통신공사업 면허" {
		t.Fatalf("Question=%+v want license/면허/[정보통신공사업 면허]", c1.Question)
	}

	// 2) 사용자가 "보유" 저장 → PASS + Question 사라짐
	db.ExecContext(ctx, `INSERT INTO company_licenses (company_profile_id, category, name, confidence, status)
		VALUES ($1,'면허','정보통신공사업 면허','C','보유')`, profileID)
	c2 := licCond(srv.buildParticipationJudgment(ctx, "", profileID, score, docs))
	if c2.Result != condPASS {
		t.Errorf("보유 후 면허=%q want PASS", c2.Result)
	}
	if c2.Question != nil {
		t.Errorf("보유 후 Question이 남으면 안 됨: %+v", c2.Question)
	}

	// 3) 미보유로 답한 경우: REVIEW 유지하되 재질문 없음(§9)
	db.ExecContext(ctx, `DELETE FROM company_licenses WHERE company_profile_id=$1`, profileID)
	db.ExecContext(ctx, `INSERT INTO company_licenses (company_profile_id, category, name, confidence, status)
		VALUES ($1,'면허','정보통신공사업 면허','C','미보유')`, profileID)
	c3 := licCond(srv.buildParticipationJudgment(ctx, "", profileID, score, docs))
	if c3.Result != condREVIEW {
		t.Errorf("미보유 후 면허=%q want REVIEW(안전정책 유지)", c3.Result)
	}
	if c3.Question != nil {
		t.Errorf("미보유 후 같은 질문 반복 금지: %+v", c3.Question)
	}
}

// 대시보드 배치 경로와 상세 경로가 같은 공고/회사에 대해 동일한 judgment를 내는지
// (일치 보장의 핵심: 배치 SQL이 listRequiredDocuments와 같은 필터로 같은 서류명을
// 가져오는지) 검증한다.
func TestDashboardDetailJudgmentConsistency_Integration(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	srv := testServer(db)
	srv.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := context.Background()

	var sourceID string
	if err := db.QueryRowContext(ctx, `SELECT id FROM data_sources LIMIT 1`).Scan(&sourceID); err != nil {
		t.Fatalf("data_sources: %v", err)
	}
	must := func(q string, args ...any) string {
		var id string
		if err := db.QueryRowContext(ctx, q, args...).Scan(&id); err != nil {
			t.Fatalf("seed [%.40s]: %v", q, err)
		}
		return id
	}
	ext := "CONSIST-" + time.Now().Format("150405.000000")
	noticeID := must(`INSERT INTO notices (source_id, external_notice_id, notice_type, title, organization_name, current_version)
		VALUES ($1,$2,'procurement','일치테스트','기관',1) RETURNING id`, sourceID, ext)
	rawID := must(`INSERT INTO raw_documents (source_id, external_notice_id, request_url, response_status, raw_content, content_hash, collector_version)
		VALUES ($1,$2,'x',200,'{}','h','t') RETURNING id`, sourceID, ext)
	versionID := must(`INSERT INTO notice_versions (notice_id, version_number, raw_document_id, change_type, is_current)
		VALUES ($1,1,$2,'initial',true) RETURNING id`, noticeID, rawID)
	must(`INSERT INTO required_documents (notice_version_id, document_name, is_required, confidence, review_status, extraction_method, ai_supplement_attempts)
		VALUES ($1,'정보통신공사업 면허',true,0.8,'pending','rule',0) RETURNING id`, versionID)
	must(`INSERT INTO required_documents (notice_version_id, document_name, is_required, confidence, review_status, extraction_method, ai_supplement_attempts)
		VALUES ($1,'제외될 서류',true,0.8,'rejected','rule',0) RETURNING id`, versionID) // rejected → 양쪽 모두 제외돼야
	userID := must(`INSERT INTO users (email) VALUES ($1) RETURNING id`, ext+"@t.local")
	profileID := must(`INSERT INTO company_profiles (user_id, company_name) VALUES ($1,'일치테스트사') RETURNING id`, userID)
	must(`INSERT INTO company_licenses (company_profile_id, category, name, confidence, status)
		VALUES ($1,'면허','정보통신공사업 면허','A','보유') RETURNING id`, profileID)
	defer func() {
		db.ExecContext(ctx, `DELETE FROM required_documents WHERE notice_version_id=$1`, versionID)
		db.ExecContext(ctx, `DELETE FROM company_licenses WHERE company_profile_id=$1`, profileID)
		db.ExecContext(ctx, `DELETE FROM company_profiles WHERE id=$1`, profileID)
		db.ExecContext(ctx, `DELETE FROM users WHERE id=$1`, userID)
		db.ExecContext(ctx, `DELETE FROM notice_versions WHERE id=$1`, versionID)
		db.ExecContext(ctx, `DELETE FROM raw_documents WHERE id=$1`, rawID)
		db.ExecContext(ctx, `DELETE FROM notices WHERE id=$1`, noticeID)
	}()

	score := &participationScore{Categories: []categoryScore{
		{Category: "지역", Result: "met"}, {Category: "업종", Result: "met"}, {Category: "예산 규모", Result: "met"},
	}}

	// 상세 경로: currentVersionID → listRequiredDocuments → judgment
	vID, err := srv.currentVersionID(ctx, noticeID, 1)
	if err != nil {
		t.Fatalf("currentVersionID: %v", err)
	}
	reqDetail, err := srv.listRequiredDocuments(ctx, vID, profileID)
	if err != nil {
		t.Fatalf("listRequiredDocuments: %v", err)
	}
	jDetail := srv.buildParticipationJudgment(ctx, vID, profileID, score, reqDetail)

	// 대시보드 경로: dashboard.go와 동일한 배치 SQL
	reqBatch := []requiredDocumentItem{}
	rows, err := db.QueryContext(ctx, `
		SELECT nv.notice_id, rd.document_name FROM required_documents rd
		JOIN notice_versions nv ON nv.id = rd.notice_version_id
		JOIN notices n ON n.id = nv.notice_id AND nv.version_number = n.current_version
		WHERE nv.notice_id = ANY($1) AND rd.review_status != 'rejected'`, pq.Array([]string{noticeID}))
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	for rows.Next() {
		var nid, dname string
		rows.Scan(&nid, &dname)
		reqBatch = append(reqBatch, requiredDocumentItem{DocumentName: dname})
	}
	rows.Close()
	jDash := srv.buildParticipationJudgment(ctx, "", profileID, score, reqBatch)

	// rejected 제외 → 양쪽 모두 면허 1건만
	if len(reqDetail) != 1 || len(reqBatch) != 1 {
		t.Fatalf("서류 수 불일치: detail=%d batch=%d (rejected 제외 후 각 1이어야)", len(reqDetail), len(reqBatch))
	}
	if jDetail.Grade != jDash.Grade {
		t.Errorf("grade 불일치: detail=%q dashboard=%q", jDetail.Grade, jDash.Grade)
	}
	if jDetail.Grade != "ready" { // 3요소 met + 면허 보유 PASS → 참여 가능
		t.Errorf("grade=%q want ready", jDetail.Grade)
	}
}

// 진행 중 사업 상세(buildJudgmentForNotice)가 공고 상세와 동일한 judgment를 내는지,
// 그리고 지원사업은 nil인지 검증한다.
func TestBuildJudgmentForNotice_Integration(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	srv := testServer(db)
	srv.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := context.Background()

	var sourceID string
	if err := db.QueryRowContext(ctx, `SELECT id FROM data_sources LIMIT 1`).Scan(&sourceID); err != nil {
		t.Fatalf("data_sources: %v", err)
	}
	must := func(q string, args ...any) string {
		var id string
		if err := db.QueryRowContext(ctx, q, args...).Scan(&id); err != nil {
			t.Fatalf("seed [%.40s]: %v", q, err)
		}
		return id
	}
	ext := "PIPEJ-" + time.Now().Format("150405.000000")
	noticeID := must(`INSERT INTO notices (source_id, external_notice_id, notice_type, title, organization_name,
		region, industry, budget_amount, industry_restricted, current_version)
		VALUES ($1,$2,'procurement','파이프판정','기관','서울특별시','소프트웨어개발',100000000,false,1) RETURNING id`, sourceID, ext)
	rawID := must(`INSERT INTO raw_documents (source_id, external_notice_id, request_url, response_status, raw_content, content_hash, collector_version)
		VALUES ($1,$2,'x',200,'{}','h','t') RETURNING id`, sourceID, ext)
	versionID := must(`INSERT INTO notice_versions (notice_id, version_number, raw_document_id, change_type, is_current)
		VALUES ($1,1,$2,'initial',true) RETURNING id`, noticeID, rawID)
	must(`INSERT INTO required_documents (notice_version_id, document_name, is_required, confidence, review_status, extraction_method, ai_supplement_attempts)
		VALUES ($1,'정보통신공사업 면허',true,0.8,'pending','rule',0) RETURNING id`, versionID)
	extS := "PIPEJS-" + time.Now().Format("150405.000000")
	supportID := must(`INSERT INTO notices (source_id, external_notice_id, notice_type, title, organization_name, current_version)
		VALUES ($1,$2,'support_program','지원사업','기관',1) RETURNING id`, sourceID, extS)
	userID := must(`INSERT INTO users (email) VALUES ($1) RETURNING id`, ext+"@t.local")
	profileID := must(`INSERT INTO company_profiles (user_id, company_name, region, industry, company_size)
		VALUES ($1,'파이프테스트사','서울특별시', ARRAY['소프트웨어개발'], '소기업') RETURNING id`, userID)
	must(`INSERT INTO company_licenses (company_profile_id, category, name, confidence, status)
		VALUES ($1,'면허','정보통신공사업 면허','A','보유') RETURNING id`, profileID)
	defer func() {
		db.ExecContext(ctx, `DELETE FROM required_documents WHERE notice_version_id=$1`, versionID)
		db.ExecContext(ctx, `DELETE FROM company_licenses WHERE company_profile_id=$1`, profileID)
		db.ExecContext(ctx, `DELETE FROM company_profiles WHERE id=$1`, profileID)
		db.ExecContext(ctx, `DELETE FROM users WHERE id=$1`, userID)
		db.ExecContext(ctx, `DELETE FROM notice_versions WHERE id=$1`, versionID)
		db.ExecContext(ctx, `DELETE FROM raw_documents WHERE id=$1`, rawID)
		db.ExecContext(ctx, `DELETE FROM notices WHERE id IN ($1,$2)`, noticeID, supportID)
	}()

	company := companyScoringInput{
		Region:   sql.NullString{String: "서울특별시", Valid: true},
		Industry: []string{"소프트웨어개발"},
		Size:     sql.NullString{String: "소기업", Valid: true},
	}

	// 파이프라인 경로
	jPipe := srv.buildJudgmentForNotice(ctx, noticeID, profileID, company)
	if jPipe == nil {
		t.Fatal("buildJudgmentForNotice nil (procurement인데)")
	}

	// 공고 상세 경로(같은 공개 함수들로 수동 재현)
	score := scoreNoticeForCompany(noticeScoringInput{
		NoticeType:   "procurement",
		Region:       sql.NullString{String: "서울특별시", Valid: true},
		Industry:     sql.NullString{String: "소프트웨어개발", Valid: true},
		BudgetAmount: sql.NullInt64{Int64: 100000000, Valid: true},
	}, company)
	reqDocs, _ := srv.listRequiredDocuments(ctx, versionID, profileID)
	jNotice := srv.buildParticipationJudgment(ctx, versionID, profileID, &score, reqDocs)

	if jPipe.Grade != jNotice.Grade {
		t.Errorf("grade 불일치: pipeline=%q notice=%q", jPipe.Grade, jNotice.Grade)
	}
	if len(jPipe.Conditions) != len(jNotice.Conditions) {
		t.Errorf("조건 수 불일치: pipeline=%d notice=%d", len(jPipe.Conditions), len(jNotice.Conditions))
	}

	// 지원사업은 nil
	if got := srv.buildJudgmentForNotice(ctx, supportID, profileID, company); got != nil {
		t.Errorf("support_program은 nil이어야 함, got grade=%q", got.Grade)
	}
}
