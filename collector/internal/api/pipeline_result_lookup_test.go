// pipeline_result_lookup_test.go — 우선순위5 개찰 결과 자동조회.
// classify(순수 판정)는 DB 없이 항상 실행. apply(상태전환/실적후보)는
// BIZ_TEST_DSN 설정 시 실제 DB로 검증.
package api

import (
	"context"
	"testing"
	"time"

	"biz-platform/collector/internal/collector/sources/scsbid"
)

func award(bizno, name, rbid, fnl, amt, rate string) *scsbid.AwardRecord {
	return &scsbid.AwardRecord{
		BidwinnrBizno: bizno, BidwinnrNm: name, RbidNo: rbid, FnlSucsfDate: fnl,
		SucsfbidAmt: amt, SucsfbidRate: rate,
	}
}

func TestClassifyAwardResult(t *testing.T) {
	ourBizno := "604-02-11138"
	ourName := "제이에스디(JSD)"
	cases := []struct {
		name       string
		m          *scsbid.AwardRecord
		method     string
		wantAction string
		wantType   string
	}{
		{"결과없음", nil, "적격심사제", awardActionNone, ""},
		{"개찰됐으나 미확정", award("6040211138", "제이에스디", "000", "", "100", "100"), "적격심사제", awardActionNone, ""},
		{"사업자번호 일치→낙찰(하이픈 무시)", award("604-02-11138", "제이에스디", "000", "2026-08-05", "48015900", "100"), "적격심사제", awardActionWin, resultTypeWon},
		{"타사+안전유형+최초→탈락", award("1112233333", "타사", "000", "2026-08-05", "1000", "95"), "적격심사제", awardActionLose, resultTypeLost},
		{"타사+최저가→탈락", award("1112233333", "타사", "000", "2026-08-05", "1000", "88"), "제한적최저가(낙찰하한율)", awardActionLose, resultTypeLost},
		{"타사+협상→보류", award("1112233333", "타사", "000", "2026-08-05", "1000", "0"), "협상에의한계약", awardActionHold, resultTypeNeedsReview},
		{"타사+공동수급→보류", award("1112233333", "타사컨소", "000", "2026-08-05", "1000", "0"), "공동수급", awardActionHold, resultTypeNeedsReview},
		{"타사+규격가격동시→보류", award("1112233333", "타사", "000", "2026-08-05", "1000", "0"), "규격가격동시입찰", awardActionHold, resultTypeNeedsReview},
		{"재입찰→보류(상태유지)", award("1112233333", "타사", "001", "2026-08-05", "1000", "95"), "적격심사제", awardActionHold, resultTypeRebid},
		{"방법미상→보수적 보류", award("1112233333", "타사", "000", "2026-08-05", "1000", "95"), "", awardActionHold, resultTypeNeedsReview},
		{"bizno없음+업체명일치→후보알림", award("", "제이에스디(JSD)", "000", "2026-08-05", "1000", "95"), "적격심사제", awardActionNameMatch, resultTypeNameMatch},
		{"bizno없음+업체명불일치→보류", award("", "전혀다른회사", "000", "2026-08-05", "1000", "95"), "적격심사제", awardActionHold, resultTypeNeedsReview},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := classifyAwardResult(c.m, ourBizno, ourName, c.method)
			if got.action != c.wantAction || got.resultType != c.wantType {
				t.Errorf("action=%s type=%s, 기대 action=%s type=%s", got.action, got.resultType, c.wantAction, c.wantType)
			}
		})
	}
}

func TestIsSafeCompetitiveMethod(t *testing.T) {
	safe := []string{"적격심사제", "최저가낙찰제", "제한적최저가(낙찰하한율)"}
	unsafe := []string{"협상에의한계약", "소액수의견적", "수의시담", "규격가격동시입찰", "다수공급자계약", "", "우선협상대상자선정"}
	for _, m := range safe {
		if !isSafeCompetitiveMethod(m) {
			t.Errorf("%q는 안전유형이어야 함", m)
		}
	}
	for _, m := range unsafe {
		if isSafeCompetitiveMethod(m) {
			t.Errorf("%q는 보류유형이어야 함", m)
		}
	}
}

func TestNormalizeBiznoAndName(t *testing.T) {
	if normalizeBizno("604-02-11138") != "6040211138" {
		t.Error("bizno 정규화 실패")
	}
	if !companyNameMatch("주식회사 제이에스디", "(주)제이에스디") {
		t.Error("회사명 정규화 매칭 실패")
	}
	if companyNameMatch("제이에스디", "케이에스디") {
		t.Error("다른 회사명이 매칭됨")
	}
}

// pickMatchingAward — 공고번호 매칭 + 재입찰차수/최종확정 우선순위.
func TestPickMatchingAward(t *testing.T) {
	recs := []scsbid.AwardRecord{
		{BidNtceNo: "AAA", RbidNo: "000", FnlSucsfDate: "2026-08-05"},
		{BidNtceNo: "BBB", RbidNo: "000", FnlSucsfDate: "2026-08-05"},
		{BidNtceNo: "AAA", RbidNo: "001", FnlSucsfDate: "2026-08-06"}, // 더 최근 차수
	}
	got := pickMatchingAward(recs, "AAA")
	if got == nil || got.RbidNo != "001" {
		t.Errorf("최근 차수(001)를 골라야 함: %+v", got)
	}
	if pickMatchingAward(recs, "ZZZ") != nil {
		t.Error("매칭 없으면 nil이어야 함")
	}
}

// --- DB 통합: 상태전환/실적후보 적용 ---

// TestApplyAwardWin_Integration — 제출완료→낙찰 전환 + 이력 + 실적후보 검증.
func TestApplyAwardWin_Integration(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	srv := testServer(db)
	ctx := context.Background()

	// 회사(사업자번호 포함) + 공고(개찰 지남) + 제출완료 엔트리 시드.
	var sourceID, userID, profileID, noticeID, entryID string
	if err := db.QueryRowContext(ctx, `SELECT id FROM data_sources LIMIT 1`).Scan(&sourceID); err != nil {
		t.Fatalf("data_sources: %v", err)
	}
	ext := "TESTWIN-" + time.Now().Format("150405.000000")
	q := func(query string, args ...any) string {
		var id string
		if err := db.QueryRowContext(ctx, query, args...).Scan(&id); err != nil {
			t.Fatalf("seed [%.40s]: %v", query, err)
		}
		return id
	}
	userID = q(`INSERT INTO users (email) VALUES ($1) RETURNING id`, ext+"@t.local")
	profileID = q(`INSERT INTO company_profiles (user_id, company_name, business_registration_number) VALUES ($1,$2,$3) RETURNING id`,
		userID, "테스트낙찰사", "604-02-11138")
	openAt := time.Now().Add(-2 * time.Hour)
	noticeID = q(`INSERT INTO notices (source_id, external_notice_id, notice_type, title, organization_name, opening_at, success_bid_method_name)
		VALUES ($1,$2,'procurement','낙찰테스트공고','테스트기관',$3,'적격심사제') RETURNING id`, sourceID, ext, openAt)
	entryID = q(`INSERT INTO notice_pipeline_entries (company_profile_id, notice_id, status) VALUES ($1,$2,'제출완료') RETURNING id`, profileID, noticeID)

	defer func() {
		db.ExecContext(ctx, `DELETE FROM pipeline_status_history WHERE pipeline_entry_id=$1`, entryID)
		db.ExecContext(ctx, `DELETE FROM in_app_notifications WHERE pipeline_entry_id=$1`, entryID)
		db.ExecContext(ctx, `DELETE FROM notification_log WHERE pipeline_entry_id=$1`, entryID)
		db.ExecContext(ctx, `DELETE FROM company_track_records WHERE company_profile_id=$1`, profileID)
		db.ExecContext(ctx, `DELETE FROM notice_pipeline_entries WHERE id=$1`, entryID)
		db.ExecContext(ctx, `DELETE FROM notices WHERE id=$1`, noticeID)
		db.ExecContext(ctx, `DELETE FROM company_profiles WHERE id=$1`, profileID)
		db.ExecContext(ctx, `DELETE FROM users WHERE id=$1`, userID)
	}()

	row := resultLookupRow{
		entryID: entryID, noticeID: noticeID, profileID: profileID, externalNoticeID: ext,
		title: "낙찰테스트공고", ourBizno: "604-02-11138",
	}
	m := award("604-02-11138", "테스트낙찰사", "000", "2026-08-05", "48015900", "100")
	srv.applyAwardWin(ctx, row, m, time.Now())

	var status, resultType string
	var awarded int64
	var finalized *time.Time
	if err := db.QueryRowContext(ctx, `SELECT status, result_type, COALESCE(awarded_amount,0), result_finalized_at
		FROM notice_pipeline_entries WHERE id=$1`, entryID).Scan(&status, &resultType, &awarded, &finalized); err != nil {
		t.Fatalf("조회: %v", err)
	}
	if status != "낙찰" || resultType != resultTypeWon || awarded != 48015900 || finalized == nil {
		t.Errorf("낙찰 전환 실패: status=%s type=%s amt=%d finalized=%v", status, resultType, awarded, finalized)
	}
	// 이력
	var histCount int
	db.QueryRowContext(ctx, `SELECT count(*) FROM pipeline_status_history WHERE pipeline_entry_id=$1 AND to_status='낙찰' AND reason='OFFICIAL_RESULT_MATCH' AND trigger_type='G2B_AWARD_RESULT'`, entryID).Scan(&histCount)
	if histCount != 1 {
		t.Errorf("낙찰 이력 %d (기대 1)", histCount)
	}
	// 실적 후보(미확정)
	var trCount, verifiedCount int
	db.QueryRowContext(ctx, `SELECT count(*), count(verified_at) FROM company_track_records WHERE company_profile_id=$1`, profileID).Scan(&trCount, &verifiedCount)
	if trCount != 1 || verifiedCount != 0 {
		t.Errorf("실적후보 수=%d verified=%d (기대 1/0)", trCount, verifiedCount)
	}
}
