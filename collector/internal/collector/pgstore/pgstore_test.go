// pgstore_test.go — B-2 support_program_details 저장 통합 테스트.
// DB 필요(BIZ_TEST_DSN 설정 시만 실행).
//
//	BIZ_TEST_DSN='postgres://localhost/biz_platform?sslmode=disable' go test ./internal/collector/pgstore -run Support -v
package pgstore

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	"biz-platform/collector/internal/collector"
)

func TestSupportProgramDetail_Upsert(t *testing.T) {
	dsn := os.Getenv("BIZ_TEST_DSN")
	if dsn == "" {
		t.Skip("BIZ_TEST_DSN 미설정")
	}
	ctx := context.Background()
	st, err := Open(ctx, dsn, "bizinfo", "기업마당", "support_program", "https://www.bizinfo.go.kr")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	db, _ := sql.Open("postgres", dsn)
	defer db.Close()

	ext := "SPD-TEST-" + time.Now().Format("150405.000000")
	raw, _, err := st.SaveRawDocument(ctx, collector.RawDocument{ExternalNoticeID: ext, RawContent: "{}"})
	if err != nil {
		t.Fatalf("saveraw: %v", err)
	}
	iq := int64(4103)
	upd := time.Now()
	nid, _, err := st.CreateNotice(ctx, collector.NormalizedNotice{
		ExternalNoticeID: ext, NoticeType: "support_program", Title: "지원사업테스트", Status: "open",
		SupportDetail: &collector.SupportProgramDetail{
			SupportTarget: "창업벤처", BusinessSummaryHTML: "<p>요약</p>", BusinessSummaryText: "요약",
			ApplicationMethod: "온라인 접수", ReferenceContact: "042-000", ApplicationURL: "https://x",
			CategoryMajor: "수출", CategoryMiddle: "해외진출준비", Hashtags: "수출,창업",
			InquiryCount: &iq, SourceUpdatedAt: &upd,
		},
	}, raw)
	if err != nil {
		t.Fatalf("createnotice: %v", err)
	}
	t.Cleanup(func() {
		db.ExecContext(ctx, `DELETE FROM support_program_details WHERE notice_id=$1`, nid)
		db.ExecContext(ctx, `DELETE FROM notice_versions WHERE notice_id=$1`, nid)
		db.ExecContext(ctx, `DELETE FROM notices WHERE id=$1`, nid)
		db.ExecContext(ctx, `DELETE FROM raw_documents WHERE id=$1`, raw)
	})

	// 저장 확인
	var target, method, cat2 string
	var inquiry int64
	if err := db.QueryRowContext(ctx,
		`SELECT support_target, application_method, support_category_middle, inquiry_count FROM support_program_details WHERE notice_id=$1`, nid).
		Scan(&target, &method, &cat2, &inquiry); err != nil {
		t.Fatalf("detail 조회 실패(저장 안 됨?): %v", err)
	}
	if target != "창업벤처" || method != "온라인 접수" || cat2 != "해외진출준비" || inquiry != 4103 {
		t.Errorf("detail 값 오류: target=%s method=%s cat2=%s iq=%d", target, method, cat2, inquiry)
	}

	// 재수집(AddNewVersion)으로 값 갱신 = UPSERT
	raw2, _, _ := st.SaveRawDocument(ctx, collector.RawDocument{ExternalNoticeID: ext, RawContent: "{}"})
	if _, _, err := st.AddNewVersion(ctx, nid, collector.NormalizedNotice{
		ExternalNoticeID: ext, NoticeType: "support_program", Title: "지원사업테스트", Status: "open",
		SupportDetail: &collector.SupportProgramDetail{SupportTarget: "갱신됨", ApplicationMethod: "방문접수"},
	}, raw2, "minor_update"); err != nil {
		t.Fatalf("addnewversion: %v", err)
	}
	t.Cleanup(func() { db.ExecContext(ctx, `DELETE FROM raw_documents WHERE id=$1`, raw2) })
	var target2 string
	db.QueryRowContext(ctx, `SELECT support_target FROM support_program_details WHERE notice_id=$1`, nid).Scan(&target2)
	if target2 != "갱신됨" {
		t.Errorf("UPSERT 갱신 실패: %q", target2)
	}
	// row 1개만(중복 아님)
	var cnt int
	db.QueryRowContext(ctx, `SELECT count(*) FROM support_program_details WHERE notice_id=$1`, nid).Scan(&cnt)
	if cnt != 1 {
		t.Errorf("detail row %d개(기대 1)", cnt)
	}
}

func TestSupportProgramDetail_NoneForProcurement(t *testing.T) {
	dsn := os.Getenv("BIZ_TEST_DSN")
	if dsn == "" {
		t.Skip("BIZ_TEST_DSN 미설정")
	}
	ctx := context.Background()
	st, err := Open(ctx, dsn, "g2b", "나라장터", "procurement", "https://x")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	db, _ := sql.Open("postgres", dsn)
	defer db.Close()

	ext := "PROC-TEST-" + time.Now().Format("150405.000000")
	raw, _, _ := st.SaveRawDocument(ctx, collector.RawDocument{ExternalNoticeID: ext, RawContent: "{}"})
	// 입찰 공고: SupportDetail nil
	nid, _, err := st.CreateNotice(ctx, collector.NormalizedNotice{
		ExternalNoticeID: ext, NoticeType: "procurement", Title: "입찰테스트", Status: "open",
	}, raw)
	if err != nil {
		t.Fatalf("createnotice: %v", err)
	}
	t.Cleanup(func() {
		db.ExecContext(ctx, `DELETE FROM notice_versions WHERE notice_id=$1`, nid)
		db.ExecContext(ctx, `DELETE FROM notices WHERE id=$1`, nid)
		db.ExecContext(ctx, `DELETE FROM raw_documents WHERE id=$1`, raw)
	})
	var cnt int
	db.QueryRowContext(ctx, `SELECT count(*) FROM support_program_details WHERE notice_id=$1`, nid).Scan(&cnt)
	if cnt != 0 {
		t.Errorf("입찰 공고에 detail이 생성됨: %d (기대 0)", cnt)
	}
}
