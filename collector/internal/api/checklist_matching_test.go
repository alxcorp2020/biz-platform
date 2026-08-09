// checklist_matching_test.go — Step 3 서류 자동매칭.
// 순수 로직(정규화/alias/모호/양식)은 DB 없이 항상 실행. 실제 매칭은
// BIZ_TEST_DSN 설정 시 회사/문서를 심고 resolveChecklistMatch로 검증.
package api

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

func TestNormalizeDocName(t *testing.T) {
	cases := [][2]string{
		{"사업자등록증", "사업자등록증"},
		{"사업자 등록증", "사업자등록증"},
		{"사업자등록증 사본", "사업자등록증"},
		{"(주)재무제표 (원본)", "주재무제표"},
		{"ISO 9001", "iso9001"},
	}
	for _, c := range cases {
		if got := normalizeDocName(c[0]); got != c[1] {
			t.Errorf("normalizeDocName(%q)=%q, 기대 %q", c[0], got, c[1])
		}
	}
}

func TestDocNameLookup(t *testing.T) {
	cases := []struct {
		in       string
		canonKey string
		method   string
	}{
		{"사업자등록증", docTypeBusinessRegistration, matchMethodCategory},
		{"사업자 등록증", docTypeBusinessRegistration, matchMethodCategory}, // 정규화하면 표준명과 동일 → CATEGORY
		{"사업자등록증명원", docTypeBusinessRegistration, matchMethodAlias},
		{"재무제표", docTypeFinancialStatement, matchMethodCategory},
		{"수행실적증명서", docTypeTrackRecord, matchMethodAlias},
		{"4대 사회보험 사업장 가입자 명부", docTypeFourInsurance, matchMethodAlias},
	}
	for _, c := range cases {
		e, ok := docNameLookup[normalizeDocName(c.in)]
		if !ok {
			t.Errorf("%q 미해석", c.in)
			continue
		}
		if e.canonical.key != c.canonKey || e.method != c.method {
			t.Errorf("%q → key=%s method=%s, 기대 key=%s method=%s", c.in, e.canonical.key, e.method, c.canonKey, c.method)
		}
	}
	if _, ok := docNameLookup[normalizeDocName("국세완납증명서")]; ok {
		t.Error("증거원 없는 납세증명서는 lookup에 없어야 함")
	}
}

func TestAmbiguousAndForm(t *testing.T) {
	for _, n := range []string{"기타 증빙서류", "관련 증빙자료", "필요시 추가서류"} {
		if !isAmbiguousRequirement(n) {
			t.Errorf("%q는 모호해야 함", n)
		}
	}
	for _, n := range []string{"서식1 참가신청서", "서식2 청렴서약서", "기술제안서", "별지 제1호"} {
		if !isNoticeSpecificForm(n) {
			t.Errorf("%q는 공고 전용 양식이어야 함", n)
		}
	}
	// 회사 공통서류는 양식/모호로 오분류되면 안 됨.
	for _, n := range []string{"사업자등록증", "재무제표", "실적증명서"} {
		if isNoticeSpecificForm(n) || isAmbiguousRequirement(n) {
			t.Errorf("%q는 공통서류인데 양식/모호로 분류됨", n)
		}
	}
}

// --- DB 통합 ---

func TestResolveChecklistMatch_Integration(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	srv := testServer(db)
	ctx := context.Background()

	// 회사 A(사업자번호 O) + 회사 B(다른 회사, 격리 검증용).
	mk := func(email, name, bizno string) string {
		var uid, pid string
		if err := db.QueryRowContext(ctx, `INSERT INTO users(email) VALUES($1) RETURNING id`, email).Scan(&uid); err != nil {
			t.Fatalf("user: %v", err)
		}
		if err := db.QueryRowContext(ctx, `INSERT INTO company_profiles(user_id, company_name, business_registration_number) VALUES($1,$2,$3) RETURNING id`,
			uid, name, bizno).Scan(&pid); err != nil {
			t.Fatalf("profile: %v", err)
		}
		t.Cleanup(func() {
			db.ExecContext(ctx, `DELETE FROM company_documents WHERE company_profile_id=$1`, pid)
			db.ExecContext(ctx, `DELETE FROM company_certifications WHERE company_profile_id=$1`, pid)
			db.ExecContext(ctx, `DELETE FROM company_profiles WHERE id=$1`, pid)
			db.ExecContext(ctx, `DELETE FROM users WHERE id=$1`, uid)
		})
		return pid
	}
	suffix := time.Now().Format("150405.000000")
	pidA := mk("matchA-"+suffix+"@t.local", "매칭사A", "111-11-11111")
	pidB := mk("matchB-"+suffix+"@t.local", "매칭사B", "222-22-22222")

	// 회사 A: 재무제표 문서 + 만료 인증서 + 유효 인증서.
	var finDocID string
	if err := db.QueryRowContext(ctx, `INSERT INTO company_documents(company_profile_id, original_filename, stored_filename, file_type, file_size_bytes, file_hash, document_kind)
		VALUES($1,'재무제표.pdf','s.pdf','application/pdf',100,'h1',$2) RETURNING id`, pidA, documentKindFinancial).Scan(&finDocID); err != nil {
		t.Fatalf("fin doc: %v", err)
	}
	db.ExecContext(ctx, `INSERT INTO company_certifications(company_profile_id, category, name, confidence, status, expires_at)
		VALUES($1,'인증','ISO9001','A','보유',$2)`, pidA, time.Now().Add(365*24*time.Hour))
	db.ExecContext(ctx, `INSERT INTO company_certifications(company_profile_id, category, name, confidence, status, expires_at)
		VALUES($1,'인증','만료인증','A','보유',$2)`, pidA, time.Now().Add(-24*time.Hour))

	check := func(pid, docName, wantStatus, wantMethod string, wantDoc bool) {
		st, method, docID := srv.resolveChecklistMatch(ctx, pid, docName)
		if st != wantStatus || method != wantMethod {
			t.Errorf("%q → status=%s method=%s, 기대 status=%s method=%s", docName, st, method, wantStatus, wantMethod)
		}
		if wantDoc && docID == nil {
			t.Errorf("%q → matchedDocID nil, 기대 값 있음", docName)
		}
		if !wantDoc && docID != nil {
			t.Errorf("%q → matchedDocID=%v, 기대 nil", docName, *docID)
		}
	}

	// 1. 사업자등록증(프로필 값, CATEGORY, 파일 없음)
	check(pidA, "사업자등록증", "보유", matchMethodCategory, false)
	// 2. alias(정규화가 표준명과 다른 변형) → ALIAS
	check(pidA, "사업자등록증명원", "보유", matchMethodAlias, false)
	// 3. category 문서 기반(재무제표, 파일 근거)
	check(pidA, "재무제표", "보유", matchMethodCategory, true)
	// 4. 만료 인증서 → 갱신필요(EXACT)
	check(pidA, "만료인증", "갱신필요", matchMethodExact, false)
	// 4b. 유효 인증서 → 보유(EXACT, 회귀)
	check(pidA, "ISO9001", "보유", matchMethodExact, false)
	// 5. 테넌트 격리: 회사 B는 재무제표 문서 없음 → 발급필요
	check(pidB, "재무제표", "발급필요", "", false)
	// 6. 공고 전용 양식 → 신규작성
	check(pidA, "서식1 참가신청서", "신규작성", "", false)
	// 7. 모호 → 확인필요
	check(pidA, "기타 증빙서류", "확인필요", "", false)
	// 8. 재사용: 같은 회사 문서가 두 번 호출에도 동일 매칭
	_, _, d1 := srv.resolveChecklistMatch(ctx, pidA, "재무제표")
	_, _, d2 := srv.resolveChecklistMatch(ctx, pidA, "재무제표")
	if d1 == nil || d2 == nil || *d1 != *d2 || *d1 != finDocID {
		t.Errorf("재사용 매칭 불일치: %v %v (기대 %s)", d1, d2, finDocID)
	}
	// 9. 해석 불가 → 확인필요(회귀 기본값)
	check(pidA, "전혀 모르는 서류명 XYZ", "확인필요", "", false)
}

// TestReevaluateChecklistMatches_Integration — matched_document_id가 DB 외래키가
// 아니므로, 참조 문서 하드삭제/타 회사 문서/존재하지 않는 UUID여도 서버 오류 없이
// 자동 해제 후 재평가돼야 한다(테넌트 격리 포함).
func TestReevaluateChecklistMatches_Integration(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	srv := testServer(db)
	ctx := context.Background()

	var sourceID string
	if err := db.QueryRowContext(ctx, `SELECT id FROM data_sources LIMIT 1`).Scan(&sourceID); err != nil {
		t.Fatalf("data_sources: %v", err)
	}
	suffix := time.Now().Format("150405.000000")
	mkCompany := func(tag string) string {
		var uid, pid string
		db.QueryRowContext(ctx, `INSERT INTO users(email) VALUES($1) RETURNING id`, tag+suffix+"@t.local").Scan(&uid)
		db.QueryRowContext(ctx, `INSERT INTO company_profiles(user_id, company_name) VALUES($1,$2) RETURNING id`, uid, tag).Scan(&pid)
		t.Cleanup(func() {
			db.ExecContext(ctx, `DELETE FROM company_documents WHERE company_profile_id=$1`, pid)
			db.ExecContext(ctx, `DELETE FROM company_profiles WHERE id=$1`, pid)
			db.ExecContext(ctx, `DELETE FROM users WHERE id=$1`, uid)
		})
		return pid
	}
	pidA := mkCompany("재평가A")
	pidB := mkCompany("재평가B")

	// A/B 각각 재무제표 문서.
	var docA, docB string
	db.QueryRowContext(ctx, `INSERT INTO company_documents(company_profile_id, original_filename, stored_filename, file_type, file_size_bytes, file_hash, document_kind)
		VALUES($1,'a.pdf','sa.pdf','application/pdf',1,'ha',$2) RETURNING id`, pidA, documentKindFinancial).Scan(&docA)
	db.QueryRowContext(ctx, `INSERT INTO company_documents(company_profile_id, original_filename, stored_filename, file_type, file_size_bytes, file_hash, document_kind)
		VALUES($1,'b.pdf','sb.pdf','application/pdf',1,'hb',$2) RETURNING id`, pidB, documentKindFinancial).Scan(&docB)

	// A의 공고 + 파이프라인 엔트리.
	var noticeID, entryID string
	db.QueryRowContext(ctx, `INSERT INTO notices(source_id, external_notice_id, notice_type, title, organization_name)
		VALUES($1,$2,'procurement','재평가공고','기관') RETURNING id`, sourceID, "REEV-"+suffix).Scan(&noticeID)
	db.QueryRowContext(ctx, `INSERT INTO notice_pipeline_entries(company_profile_id, notice_id, status) VALUES($1,$2,'준비중') RETURNING id`, pidA, noticeID).Scan(&entryID)
	t.Cleanup(func() {
		db.ExecContext(ctx, `DELETE FROM pipeline_checklist_items WHERE pipeline_entry_id=$1`, entryID)
		db.ExecContext(ctx, `DELETE FROM notice_pipeline_entries WHERE id=$1`, entryID)
		db.ExecContext(ctx, `DELETE FROM notices WHERE id=$1`, noticeID)
	})

	mkItem := func(name, matchedDoc string) string {
		var id string
		var mdoc any
		if matchedDoc != "" {
			mdoc = matchedDoc
		}
		db.QueryRowContext(ctx, `INSERT INTO pipeline_checklist_items(pipeline_entry_id, document_name, status, matched_document_id, match_method, matched_at)
			VALUES($1,$2,'보유',$3,'CATEGORY',now()) RETURNING id`, entryID, name, mdoc).Scan(&id)
		return id
	}
	readItem := func(id string) (status string, matched *string) {
		var m sql.NullString
		db.QueryRowContext(ctx, `SELECT status, matched_document_id FROM pipeline_checklist_items WHERE id=$1`, id).Scan(&status, &m)
		if m.Valid {
			return status, &m.String
		}
		return status, nil
	}

	// 1) A 문서 정상 참조 → 재평가해도 유지(A는 financial 문서 보유).
	itemValid := mkItem("재무제표", docA)
	// 2) 타 회사(B) 문서 참조 → dangling(A 소유 아님)으로 해제 + 재평가(A는 자기 financial 있어 재매칭).
	itemCross := mkItem("재무제표", docB)
	// 3) 존재하지 않는 UUID 참조 → 해제 + 재평가, panic/500 없음.
	itemGhost := mkItem("재무제표", "00000000-0000-0000-0000-000000000000")

	srv.reevaluateChecklistMatches(ctx, entryID, pidA)

	// itemValid: A 문서 유지(여전히 보유 + docA).
	if st, m := readItem(itemValid); st != "보유" || m == nil || *m != docA {
		t.Errorf("정상참조 항목이 바뀜: status=%s matched=%v (기대 보유/%s)", st, m, docA)
	}
	// itemCross: B 문서 참조는 해제되고, A 자기 문서로 재매칭(보유 + docA).
	if st, m := readItem(itemCross); st != "보유" || m == nil || *m != docA {
		t.Errorf("타사문서 참조 미해제/재매칭 실패: status=%s matched=%v (기대 보유/%s=A문서)", st, m, docA)
	}
	// itemGhost: 유령 UUID 해제 + 재매칭(A 자기 문서 존재하므로 보유 + docA).
	if st, m := readItem(itemGhost); st != "보유" || m == nil || *m != docA {
		t.Errorf("유령 UUID 처리 실패: status=%s matched=%v", st, m)
	}

	// 4) A의 financial 문서를 하드삭제 후 재평가 → 자동 해제 + 발급필요(재매칭 대상 없음).
	if _, err := db.ExecContext(ctx, `DELETE FROM company_documents WHERE id=$1`, docA); err != nil {
		t.Fatalf("문서 삭제: %v", err)
	}
	srv.reevaluateChecklistMatches(ctx, entryID, pidA)
	for _, id := range []string{itemValid, itemCross, itemGhost} {
		st, m := readItem(id)
		if m != nil || st != "발급필요" {
			t.Errorf("문서 삭제 후 자동해제 실패: status=%s matched=%v (기대 발급필요/nil)", st, m)
		}
	}
	// 체크리스트 레코드 자체는 삭제되지 않아야 한다(무효 참조만 해제).
	var cnt int
	db.QueryRowContext(ctx, `SELECT count(*) FROM pipeline_checklist_items WHERE pipeline_entry_id=$1`, entryID).Scan(&cnt)
	if cnt != 3 {
		t.Errorf("체크리스트 레코드가 삭제됨: %d개 남음(기대 3)", cnt)
	}
}
