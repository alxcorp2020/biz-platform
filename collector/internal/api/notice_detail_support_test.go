// notice_detail_support_test.go — B-2 Commit 4. 지원사업 상세 응답이 기업마당
// 공식 데이터(support_program_details)와 역할(role)이 붙은 첨부를 그대로
// 실어내는지 실제 Postgres에 최소 픽스처를 심어 검증한다. DB가 필요하므로
// BIZ_TEST_DSN이 설정된 경우에만 돈다.
//
//	BIZ_TEST_DSN='postgres://localhost/biz_platform?sslmode=disable' go test ./internal/api -run SupportDetail -v
package api

import (
	"context"
	"testing"
	"time"
)

func TestFetchSupportProgramDetail_And_AttachmentRoles(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	srv := testServer(db)
	ctx := context.Background()

	var sourceID string
	if err := db.QueryRowContext(ctx, `SELECT id FROM data_sources LIMIT 1`).Scan(&sourceID); err != nil {
		t.Fatalf("data_sources 조회 실패: %v", err)
	}
	must := func(q string, args ...any) string {
		var id string
		if err := db.QueryRowContext(ctx, q, args...).Scan(&id); err != nil {
			t.Fatalf("seed 실패 [%.50s]: %v", q, err)
		}
		return id
	}

	ext := "SUPPORTTEST-" + time.Now().Format("150405.000000")
	noticeID := must(`INSERT INTO notices (source_id, external_notice_id, notice_type, title, organization_name)
		VALUES ($1,$2,'support_program','지원사업테스트','중소벤처기업부') RETURNING id`, sourceID, ext)
	rawID := must(`INSERT INTO raw_documents
			(source_id, external_notice_id, request_url, response_status, raw_content, content_hash, collector_version)
		VALUES ($1,$2,'https://example.test',200,'{}','deadbeef','test') RETURNING id`, sourceID, ext)
	versionID := must(`INSERT INTO notice_versions (notice_id, version_number, raw_document_id, change_type, is_current)
		VALUES ($1,1,$2,'initial',true) RETURNING id`, noticeID, rawID)

	updated := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	must(`INSERT INTO support_program_details
			(notice_id, support_target, business_summary_html, business_summary_text,
			 application_method, reference_contact, application_url,
			 support_category_major, support_category_middle, hashtags, inquiry_count, source_updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12) RETURNING notice_id`,
		noticeID, "중소기업 및 소상공인", "<p>요약 HTML</p>", "요약 평문",
		"온라인 신청(누리집)", "1234-5678", "https://www.bizinfo.go.kr/apply",
		"금융", "융자", "#융자 #소상공인", int64(42), updated)

	// 역할별 첨부 3건(공고문/별첨/역할없음).
	must(`INSERT INTO attachments (notice_version_id, original_filename, stored_filename, file_type, file_size_bytes,
			file_hash, download_url, download_status, attachment_role)
		VALUES ($1,'공고문.hwp','k1','hwp',100,'h1','https://d/1','completed','SUPPORT_PRINT_DOCUMENT') RETURNING id`, versionID)
	must(`INSERT INTO attachments (notice_version_id, original_filename, stored_filename, file_type, file_size_bytes,
			file_hash, download_url, download_status, attachment_role)
		VALUES ($1,'별첨양식.pdf','k2','pdf',200,'h2','https://d/2','completed','SUPPORT_ATTACHMENT') RETURNING id`, versionID)
	must(`INSERT INTO attachments (notice_version_id, original_filename, stored_filename, file_type, file_size_bytes,
			file_hash, download_url, download_status, attachment_role)
		VALUES ($1,'역할없음.txt','k3','txt',50,'h3','https://d/3','completed',NULL) RETURNING id`, versionID)

	// 다른 소관: 입찰(procurement) 공고 하나 — supportDetail가 nil이어야 한다.
	extProc := "PROCTEST-" + time.Now().Format("150405.000000")
	procID := must(`INSERT INTO notices (source_id, external_notice_id, notice_type, title, organization_name)
		VALUES ($1,$2,'procurement','입찰테스트','조달청') RETURNING id`, sourceID, extProc)

	defer func() {
		db.ExecContext(ctx, `DELETE FROM attachments WHERE notice_version_id=$1`, versionID)
		db.ExecContext(ctx, `DELETE FROM support_program_details WHERE notice_id=$1`, noticeID)
		db.ExecContext(ctx, `DELETE FROM notice_versions WHERE id=$1`, versionID)
		db.ExecContext(ctx, `DELETE FROM raw_documents WHERE id=$1`, rawID)
		db.ExecContext(ctx, `DELETE FROM notices WHERE id IN ($1,$2)`, noticeID, procID)
	}()

	// 1) 공식 데이터가 그대로 실려나오는지.
	d := srv.fetchSupportProgramDetail(ctx, noticeID)
	if d == nil {
		t.Fatal("fetchSupportProgramDetail가 nil을 반환 — 행이 있어야 함")
	}
	if d.SupportTarget != "중소기업 및 소상공인" {
		t.Errorf("supportTarget=%q", d.SupportTarget)
	}
	if d.BusinessSummaryText != "요약 평문" {
		t.Errorf("businessSummaryText=%q", d.BusinessSummaryText)
	}
	if d.ApplicationMethod != "온라인 신청(누리집)" {
		t.Errorf("applicationMethod=%q", d.ApplicationMethod)
	}
	if d.ReferenceContact != "1234-5678" {
		t.Errorf("referenceContact=%q", d.ReferenceContact)
	}
	if d.ApplicationURL != "https://www.bizinfo.go.kr/apply" {
		t.Errorf("applicationUrl=%q", d.ApplicationURL)
	}
	if d.CategoryMajor != "금융" || d.CategoryMiddle != "융자" {
		t.Errorf("category=%q/%q", d.CategoryMajor, d.CategoryMiddle)
	}
	if d.InquiryCount == nil || *d.InquiryCount != 42 {
		t.Errorf("inquiryCount=%v (기대 42)", d.InquiryCount)
	}
	if d.SourceUpdatedAt == nil || *d.SourceUpdatedAt != "2026-08-01" {
		t.Errorf("sourceUpdatedAt=%v (기대 2026-08-01)", d.SourceUpdatedAt)
	}

	// 2) 입찰 공고는 nil.
	if got := srv.fetchSupportProgramDetail(ctx, procID); got != nil {
		t.Errorf("입찰 공고는 supportDetail이 nil이어야 함, got=%+v", got)
	}

	// 3) 첨부 role이 응답에 실리는지.
	atts, err := srv.listAttachments(ctx, versionID)
	if err != nil {
		t.Fatalf("listAttachments: %v", err)
	}
	if len(atts) != 3 {
		t.Fatalf("첨부 수=%d (기대 3)", len(atts))
	}
	roles := map[string]string{}
	for _, a := range atts {
		roles[a.OriginalFilename] = a.Role
	}
	if roles["공고문.hwp"] != "SUPPORT_PRINT_DOCUMENT" {
		t.Errorf("공고문 role=%q", roles["공고문.hwp"])
	}
	if roles["별첨양식.pdf"] != "SUPPORT_ATTACHMENT" {
		t.Errorf("별첨 role=%q", roles["별첨양식.pdf"])
	}
	if roles["역할없음.txt"] != "" {
		t.Errorf("역할없음 role=%q (기대 빈값)", roles["역할없음.txt"])
	}
}
