package api

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"
)

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
