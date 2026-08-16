package api

import (
	"archive/zip"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"biz-platform/collector/internal/migrate"
)

// 평가기준 맞춤 제안서 통합 테스트(BIZ_TEST_DSN 필요).
//
//	CASE B  무료 사용자 직접 API 호출 → 403 paid_feature_required (readiness/create/get/docx)
//	CASE C  유료 사용자: readiness(평가기준·대응·질문) → 초안 생성 201 → 같은 버전 재생성 409 draft_exists
//	        → force → 새 초안 → GET → PATCH → DOCX(유효 zip, 한글/[확인 필요]/순서)
//	CASE D  인력 없음 → [확인 필요], 가짜 인물 없음 / CASE E 실제 실적만 반영
//	CASE F  평가기준 없는 공고 → readiness status=no_criteria, 생성 409 no_evaluation_criteria
//	CASE G  정정공고(current_version 변경) → 기존 초안 stale=true
//	CASE H  IDOR: 다른 회사 사용자 → 404 (GET/PATCH/DOCX)
//	+ /api/me entitlements 반영, 마이그레이션 멱등(migrate.Apply 2회)
func TestProposalDrafts_EndToEnd(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	ctx := context.Background()
	t.Setenv("ANTHROPIC_API_KEY", "") // 규칙 추출 경로 강제(외부 호출 없음)

	if err := migrate.Apply(ctx, db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := migrate.Apply(ctx, db); err != nil { // 멱등
		t.Fatalf("migrate 2nd: %v", err)
	}
	srv := &Server{db: db, logger: slog.New(slog.NewTextHandler(io.Discard, nil)), sessionSecret: []byte("proposal-test-secret-0123456789abcdef")}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/me", srv.handleMe)
	mux.HandleFunc("GET /api/notices/{id}/proposal-readiness", srv.handleProposalReadiness)
	mux.HandleFunc("POST /api/notices/{id}/proposal-drafts", srv.handleCreateProposalDraft)
	mux.HandleFunc("GET /api/proposal-drafts/{id}", srv.handleGetProposalDraft)
	mux.HandleFunc("PATCH /api/proposal-drafts/{id}", srv.handlePatchProposalDraft)
	mux.HandleFunc("GET /api/proposal-drafts/{id}/docx", srv.handleProposalDraftDocx)

	must := func(q string, args ...any) string {
		var id string
		if err := db.QueryRowContext(ctx, q, args...).Scan(&id); err != nil {
			t.Fatalf("seed [%.60s]: %v", q, err)
		}
		return id
	}
	tag := time.Now().Format("150405.000000")
	// 사용자/회사 3세트: free(A), paid(B), paid other company(C, IDOR)
	type actor struct{ userID, profileID string }
	var actors []actor
	var noticeIDs []string
	// seed 도중 실패해도 잔여 fixture가 남지 않도록 cleanup을 먼저 등록한다.
	defer func() {
		for _, n := range noticeIDs {
			_, _ = db.ExecContext(ctx, `DELETE FROM proposal_drafts WHERE notice_id = $1`, n)
			_, _ = db.ExecContext(ctx, `DELETE FROM attachments WHERE notice_version_id IN (SELECT id FROM notice_versions WHERE notice_id = $1)`, n)
			_, _ = db.ExecContext(ctx, `DELETE FROM notice_versions WHERE notice_id = $1`, n)
			_, _ = db.ExecContext(ctx, `DELETE FROM notices WHERE id = $1`, n)
		}
		_, _ = db.ExecContext(ctx, `DELETE FROM raw_documents WHERE external_notice_id LIKE $1`, "PROPTEST-%-"+tag)
		for _, a := range actors {
			_, _ = db.ExecContext(ctx, `DELETE FROM subscriptions WHERE company_profile_id = $1`, a.profileID)
			_, _ = db.ExecContext(ctx, `DELETE FROM company_track_records WHERE company_profile_id = $1`, a.profileID)
			_, _ = db.ExecContext(ctx, `DELETE FROM company_members WHERE company_profile_id = $1`, a.profileID)
			_, _ = db.ExecContext(ctx, `DELETE FROM company_profiles WHERE id = $1`, a.profileID)
			_, _ = db.ExecContext(ctx, `DELETE FROM users WHERE id = $1`, a.userID)
		}
	}()
	mk := func(name, plan string) actor {
		u := must(`INSERT INTO users (email, password_hash, role, plan) VALUES ($1,'x','user','free') RETURNING id`, name+"-"+tag+"@proposal.test")
		p := must(`INSERT INTO company_profiles (user_id, company_name, representative_name, address, region, industry, business_type) VALUES ($1,$2,'홍길동','서울시 테스트구','서울','{"행사기획"}','{"서비스업"}') RETURNING id`, u, name+" 주식회사")
		must(`INSERT INTO company_members (company_profile_id, user_id, role) VALUES ($1,$2,'owner') RETURNING id`, p, u)
		if plan != "free" {
			must(`INSERT INTO subscriptions (company_profile_id, plan, status, started_at, expires_at, amount) VALUES ($1,$2,'active', now(), now() + interval '30 days', 19900) RETURNING id`, p, plan)
		}
		a := actor{u, p}
		actors = append(actors, a)
		return a
	}
	free := mk("free", "free")
	paid := mk("paid", "basic")
	other := mk("other", "pro")
	// 만료된 유료 구독은 Free 취급(effectivePlanFromRow) — 게이트가 구독 만료를 존중하는지.
	expired := mk("expired", "free")
	must(`INSERT INTO subscriptions (company_profile_id, plan, status, started_at, expires_at, amount) VALUES ($1,'pro','active', now() - interval '60 days', now() - interval '1 day', 49000) RETURNING id`, expired.profileID)

	// paid 회사에 실제 실적 1건(가짜 생성 금지 검증용). 인력은 없음(CASE D).
	must(`INSERT INTO company_track_records (company_profile_id, project_name, client_name, period_start, period_end, contract_amount, is_completed, confidence) VALUES ($1,'실제 등록 실적 사업','실제 발주처','2025-01-01','2025-06-30',120000000,true,'A') RETURNING id`, paid.profileID)

	// 공고 2개: N1(평가기준 있는 첨부) / N2(평가기준 없음)
	var sourceID string
	if err := db.QueryRowContext(ctx, `SELECT id FROM data_sources LIMIT 1`).Scan(&sourceID); err != nil {
		t.Fatalf("data_sources: %v", err)
	}
	newNotice := func(title, ext string) (string, string) {
		nid := must(`INSERT INTO notices (source_id, external_notice_id, notice_type, title, organization_name, budget_amount, current_version, success_bid_method_name) VALUES ($1,$2,'procurement',$3,'테스트발주기관',50000000,1,'협상에 의한 계약') RETURNING id`, sourceID, ext, title)
		rawID := must(`INSERT INTO raw_documents (source_id, external_notice_id, request_url, response_status, raw_content, content_hash, collector_version) VALUES ($1,$2,'test://proposal',200,'{}',$3,'test') RETURNING id`, sourceID, ext, "h-"+ext)
		vid := must(`INSERT INTO notice_versions (notice_id, version_number, raw_document_id, change_type, is_current) VALUES ($1,1,$2,'initial',true) RETURNING id`, nid, rawID)
		noticeIDs = append(noticeIDs, nid)
		return nid, vid
	}
	n1, v1 := newNotice("제안서 테스트 행사 대행 용역", "PROPTEST-1-"+tag)
	n2, _ := newNotice("평가기준 없는 공고", "PROPTEST-2-"+tag)
	rfpText := "제안요청서\n3. 평가항목 및 배점기준\n1) 사업 이해도 (20점)\n2) 수행계획의 적정성 (25점)\n3) 전문인력 (20점)\n4) 유사사업 수행실적 (20점)\n5) 사후관리 (15점)\n계 100\n제안서는 30페이지 이내로 작성\n"
	must(`INSERT INTO attachments (notice_version_id, original_filename, stored_filename, file_hash, download_status, extraction_status, analysis_status, extracted_text) VALUES ($1,'제안요청서.pdf','x.pdf',$2,'completed','completed','completed',$3) RETURNING id`, v1, "fh-"+tag, rfpText)

	do := func(a actor, method, path string, body any) (int, map[string]any, []byte) {
		var rd io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rd = bytes.NewReader(b)
		}
		req := httptest.NewRequest(method, path, rd)
		if a.userID != "" {
			req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: srv.signSession(a.userID, time.Now().Add(time.Hour))})
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		var m map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &m)
		return rec.Code, m, rec.Body.Bytes()
	}

	// /api/me entitlements
	code, me, _ := do(free, "GET", "/api/me", nil)
	if code != 200 || me["entitlements"].(map[string]any)["proposal_draft_docx"] != false {
		t.Fatalf("free entitlements: %d %v", code, me["entitlements"])
	}
	code, me, _ = do(paid, "GET", "/api/me", nil)
	if code != 200 || me["entitlements"].(map[string]any)["proposal_draft_docx"] != true {
		t.Fatalf("paid entitlements: %d %v", code, me["entitlements"])
	}
	code, me, _ = do(expired, "GET", "/api/me", nil)
	if code != 200 || me["entitlements"].(map[string]any)["proposal_draft_docx"] != false {
		t.Fatalf("expired sub must be free: %v", me["entitlements"])
	}

	// CASE B: 무료 사용자 직접 호출 → 403 paid_feature_required
	for _, c := range []struct{ m, p string }{{"GET", "/api/notices/" + n1 + "/proposal-readiness"}, {"POST", "/api/notices/" + n1 + "/proposal-drafts"}} {
		code, m, _ := do(free, c.m, c.p, map[string]any{})
		if code != 403 || m["error"] != errorPaidFeatureRequired || m["feature"] != "proposal_draft_docx" {
			t.Fatalf("CASE B free %s %s: %d %v", c.m, c.p, code, m)
		}
	}
	code, m, _ := do(expired, "POST", "/api/notices/"+n1+"/proposal-drafts", map[string]any{})
	if code != 403 || m["error"] != errorPaidFeatureRequired {
		t.Fatalf("expired must be denied: %d %v", code, m)
	}
	code, m, _ = do(actor{}, "GET", "/api/notices/"+n1+"/proposal-readiness", nil)
	if code != 401 {
		t.Fatalf("anon: %d %v", code, m)
	}
	var draftCount int
	_ = db.QueryRowContext(ctx, `SELECT count(*) FROM proposal_drafts WHERE notice_id = $1`, n1).Scan(&draftCount)
	if draftCount != 0 {
		t.Fatalf("free user must not create drafts")
	}

	// CASE C: 유료 readiness
	code, rd, _ := do(paid, "GET", "/api/notices/"+n1+"/proposal-readiness", nil)
	if code != 200 || rd["status"] != "ready" {
		t.Fatalf("readiness: %d %v", code, rd)
	}
	items := rd["items"].([]any)
	if len(items) < 5 {
		t.Fatalf("items: %d", len(items))
	}
	sum := rd["summary"].(map[string]any)
	if sum["criteriaCount"].(float64) < 5 || sum["needsInputCount"].(float64) < 2 {
		t.Fatalf("summary: %v", sum)
	}
	// 실적 항목은 실제 실적 1건 반영(ready), 인력은 needs_input + 책임자 질문
	var sawTrack, sawPersonnel bool
	for _, it := range items {
		m := it.(map[string]any)
		if m["kind"] == kindTrackRecord {
			sawTrack = true
			if m["status"] != readyStatusReady || !strings.Contains(m["evidence"].([]any)[0].(string), "1건") {
				t.Fatalf("track item: %v", m)
			}
		}
		if m["kind"] == kindPersonnel {
			sawPersonnel = true
			if m["status"] != readyStatusInput || m["question"] == nil {
				t.Fatalf("personnel item: %v", m)
			}
		}
	}
	if !sawTrack || !sawPersonnel {
		t.Fatalf("mapping kinds missing")
	}
	if len(rd["drafts"].([]any)) != 0 {
		t.Fatalf("no drafts yet")
	}
	// readiness 자체는 초안을 만들지 않는다(성능/비용 원칙)
	_ = db.QueryRowContext(ctx, `SELECT count(*) FROM proposal_drafts WHERE notice_id = $1`, n1).Scan(&draftCount)
	if draftCount != 0 {
		t.Fatalf("readiness must not generate drafts")
	}
	// 평가기준이 notice_versions에 저장됐는지(재조회 시 재추출 없음)
	var evalStatus string
	_ = db.QueryRowContext(ctx, `SELECT evaluation_criteria_status FROM notice_versions WHERE id = $1`, v1).Scan(&evalStatus)
	if evalStatus != evalStatusFound {
		t.Fatalf("criteria not stored: %q", evalStatus)
	}

	// 초안 생성(답변: 사후관리 24시간)
	code, d1, _ := do(paid, "POST", "/api/notices/"+n1+"/proposal-drafts", map[string]any{"answers": map[string]any{"q_c5": map[string]string{"value": "24h", "text": "전담 콜센터 운영"}}})
	if code != 201 || d1["id"] == nil {
		t.Fatalf("create: %d %v", code, d1)
	}
	draftID := d1["id"].(string)
	if d1["stale"] != false {
		t.Fatalf("new draft must not be stale")
	}
	content := d1["content"].(map[string]any)
	secs := content["sections"].([]any)
	if len(secs) < 6 {
		t.Fatalf("sections: %d", len(secs))
	}
	flat, _ := json.Marshal(content)
	if !strings.Contains(string(flat), "실제 등록 실적 사업") || !strings.Contains(string(flat), "120,000,000원") {
		t.Fatalf("CASE E: real track record must appear")
	}
	if !strings.Contains(string(flat), "[확인 필요: 본 사업에 투입할 책임자(PM)") {
		t.Fatalf("CASE D: personnel must be [확인 필요]")
	}
	if strings.Contains(string(flat), "AI") {
		t.Fatalf("no AI wording in content")
	}
	// 같은 버전 재생성 → 409 draft_exists
	code, m, _ = do(paid, "POST", "/api/notices/"+n1+"/proposal-drafts", map[string]any{})
	if code != 409 || m["error"] != "draft_exists" || m["draftId"] != draftID {
		t.Fatalf("dup: %d %v", code, m)
	}
	// force → 새 초안
	code, d2, _ := do(paid, "POST", "/api/notices/"+n1+"/proposal-drafts", map[string]any{"force": true})
	if code != 201 || d2["id"] == draftID {
		t.Fatalf("force: %d %v", code, d2)
	}
	// readiness에 초안 2건 노출
	code, rd, _ = do(paid, "GET", "/api/notices/"+n1+"/proposal-readiness", nil)
	if code != 200 || len(rd["drafts"].([]any)) != 2 {
		t.Fatalf("drafts list: %d %v", code, rd["drafts"])
	}
	// GET/PATCH
	code, g, _ := do(paid, "GET", "/api/proposal-drafts/"+draftID, nil)
	if code != 200 || g["id"] != draftID {
		t.Fatalf("get: %d %v", code, g)
	}
	code, p, _ := do(paid, "PATCH", "/api/proposal-drafts/"+draftID, map[string]any{"title": "수정된 제목", "sections": []map[string]any{{"id": "s1", "body": "사용자가 직접 수정한 본문"}}})
	if code != 200 || p["title"] != "수정된 제목" {
		t.Fatalf("patch: %d %v", code, p)
	}
	pflat, _ := json.Marshal(p["content"])
	if !strings.Contains(string(pflat), "사용자가 직접 수정한 본문") {
		t.Fatalf("patch body not applied")
	}
	// DOCX
	req := httptest.NewRequest("GET", "/api/proposal-drafts/"+draftID+"/docx", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: srv.signSession(paid.userID, time.Now().Add(time.Hour))})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Header().Get("Content-Type"), "wordprocessingml") || !strings.Contains(rec.Header().Get("Content-Disposition"), "filename*=UTF-8''") {
		t.Fatalf("docx: %d %s %s", rec.Code, rec.Header().Get("Content-Type"), rec.Header().Get("Content-Disposition"))
	}
	if !strings.Contains(rec.Header().Get("Content-Disposition"), "%EC%A0%9C%EC%95%88%EC%84%9C%EC%B4%88%EC%95%88") { // "제안서초안"
		t.Fatalf("filename: %s", rec.Header().Get("Content-Disposition"))
	}
	zb := rec.Body.Bytes()
	zr, err := zip.NewReader(bytes.NewReader(zb), int64(len(zb)))
	if err != nil {
		t.Fatalf("docx not a zip: %v", err)
	}
	var docXML string
	for _, f := range zr.File {
		if f.Name == "word/document.xml" {
			rc, _ := f.Open()
			bb, _ := io.ReadAll(rc)
			rc.Close()
			docXML = string(bb)
		}
	}
	for _, want := range []string{"수정된 제목", "사용자가 직접 수정한 본문", "실제 등록 실적 사업", "[확인 필요: 본 사업에 투입할 책임자(PM)", "1. 사업 이해도 (20점)", "5. 사후관리 (15점)", proposalDisclaimer, "30페이지 이내"} {
		if !strings.Contains(docXML, want) {
			t.Fatalf("docx missing %q", want)
		}
	}
	// 무료 사용자 DOCX 직접 호출 → 403 (자기 회사 초안이어도 유료 아님)
	code, m, _ = do(free, "GET", "/api/proposal-drafts/"+draftID+"/docx", nil)
	if code != 403 || m["error"] != errorPaidFeatureRequired {
		t.Fatalf("free docx: %d %v", code, m)
	}

	// CASE H: IDOR — 다른 회사(유료) 사용자 → 404, 수정/다운로드 불가
	for _, c := range []struct{ m, p string }{{"GET", "/api/proposal-drafts/" + draftID}, {"PATCH", "/api/proposal-drafts/" + draftID}, {"GET", "/api/proposal-drafts/" + draftID + "/docx"}} {
		code, m, _ := do(other, c.m, c.p, map[string]any{"title": "해킹"})
		if code != 404 || m["error"] != "draft_not_found" {
			t.Fatalf("IDOR %s %s: %d %v", c.m, c.p, code, m)
		}
	}
	code, g, _ = do(paid, "GET", "/api/proposal-drafts/"+draftID, nil)
	if g["title"] != "수정된 제목" {
		t.Fatalf("IDOR patch must not have changed title: %v", g["title"])
	}
	// 존재하지 않는/잘못된 id → 404
	code, _, _ = do(paid, "GET", "/api/proposal-drafts/not-a-uuid", nil)
	if code != 404 {
		t.Fatalf("bad id: %d", code)
	}

	// CASE F: 평가기준 없는 공고
	code, rd, _ = do(paid, "GET", "/api/notices/"+n2+"/proposal-readiness", nil)
	if code != 200 || rd["status"] != "no_criteria" {
		t.Fatalf("no criteria: %d %v", code, rd)
	}
	code, m, _ = do(paid, "POST", "/api/notices/"+n2+"/proposal-drafts", map[string]any{})
	if code != 409 || m["error"] != "no_evaluation_criteria" {
		t.Fatalf("no criteria create: %d %v", code, m)
	}

	// CASE G: 정정공고 — current_version 2 → 기존 초안 stale
	rawID2 := must(`INSERT INTO raw_documents (source_id, external_notice_id, request_url, response_status, raw_content, content_hash, collector_version) VALUES ($1,$2,'test://proposal',200,'{}',$3,'test') RETURNING id`, sourceID, "PROPTEST-1-"+tag, "h2-"+tag)
	must(`INSERT INTO notice_versions (notice_id, version_number, raw_document_id, change_type, is_current) VALUES ($1,2,$2,'correction',true) RETURNING id`, n1, rawID2)
	if _, err := db.ExecContext(ctx, `UPDATE notice_versions SET is_current = false WHERE notice_id = $1 AND version_number = 1`, n1); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE notices SET current_version = 2 WHERE id = $1`, n1); err != nil {
		t.Fatal(err)
	}
	code, g, _ = do(paid, "GET", "/api/proposal-drafts/"+draftID, nil)
	if code != 200 || g["stale"] != true || g["currentVersionNumber"].(float64) != 2 || g["noticeVersionNumber"].(float64) != 1 {
		t.Fatalf("stale: %d %v", code, g)
	}
	// 새 버전은 첨부 없음 → readiness no_criteria(과거 버전 평가기준으로 새 문서를 만들지 않는다)
	code, rd, _ = do(paid, "GET", "/api/notices/"+n1+"/proposal-readiness", nil)
	if code != 200 || rd["status"] != "no_criteria" {
		t.Fatalf("new version must not reuse old criteria: %d %v", code, rd["status"])
	}
	drafts := rd["drafts"].([]any)
	if len(drafts) != 2 || drafts[0].(map[string]any)["stale"] != true {
		t.Fatalf("existing drafts must be flagged stale: %v", drafts)
	}
	_ = sql.ErrNoRows
}

// 동시성: 같은 notice_version에 동시 요청 N개 → 실제 추출(모델/규칙 호출) 1회, 나머지는 결과
// 재사용. 이미 found면 이후 요청은 추출 0회. 강제 재추출이 무결과여도 기존 found 결과는 유지.
func TestEvaluationCriteria_ConcurrentExtractionDedup(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	ctx := context.Background()
	t.Setenv("ANTHROPIC_API_KEY", "")
	if err := migrate.Apply(ctx, db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	srv := &Server{db: db, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	must := func(q string, args ...any) string {
		var id string
		if err := db.QueryRowContext(ctx, q, args...).Scan(&id); err != nil {
			t.Fatalf("seed [%.60s]: %v", q, err)
		}
		return id
	}
	var sourceID string
	if err := db.QueryRowContext(ctx, `SELECT id FROM data_sources LIMIT 1`).Scan(&sourceID); err != nil {
		t.Fatalf("data_sources: %v", err)
	}
	tag := time.Now().Format("150405.000000")
	ext := "PROPTEST-CC-" + tag
	nid := must(`INSERT INTO notices (source_id, external_notice_id, notice_type, title, organization_name, current_version) VALUES ($1,$2,'procurement','동시성 테스트','기관',1) RETURNING id`, sourceID, ext)
	defer func() {
		_, _ = db.ExecContext(ctx, `DELETE FROM attachments WHERE notice_version_id IN (SELECT id FROM notice_versions WHERE notice_id = $1)`, nid)
		_, _ = db.ExecContext(ctx, `DELETE FROM notice_versions WHERE notice_id = $1`, nid)
		_, _ = db.ExecContext(ctx, `DELETE FROM raw_documents WHERE external_notice_id = $1`, ext)
		_, _ = db.ExecContext(ctx, `DELETE FROM notices WHERE id = $1`, nid)
	}()
	rawID := must(`INSERT INTO raw_documents (source_id, external_notice_id, request_url, response_status, raw_content, content_hash, collector_version) VALUES ($1,$2,'test://cc',200,'{}',$3,'test') RETURNING id`, sourceID, ext, "h-"+ext)
	vid := must(`INSERT INTO notice_versions (notice_id, version_number, raw_document_id, change_type, is_current) VALUES ($1,1,$2,'initial',true) RETURNING id`, nid, rawID)
	attID := must(`INSERT INTO attachments (notice_version_id, original_filename, stored_filename, file_hash, download_status, extraction_status, analysis_status, extracted_text) VALUES ($1,'평가표.pdf','x.pdf',$2,'completed','completed','completed', E'평가항목 및 배점\n1) 사업 이해도 (20점)\n2) 수행계획 (30점)\n3) 수행실적 (50점)\n') RETURNING id`, vid, "fh-cc-"+tag)

	var runs int32
	evalExtractionObserver = func(v string) {
		if v == vid {
			atomic.AddInt32(&runs, 1)
			time.Sleep(150 * time.Millisecond) // 추출 작업 시간(경합 창을 넓힌다)
		}
	}
	defer func() { evalExtractionObserver = nil }()

	const n = 12
	var wg sync.WaitGroup
	statuses := make([]string, n)
	counts := make([]int, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			set, st, err := srv.getOrExtractEvaluationCriteria(ctx, vid, false)
			statuses[i], errs[i] = st, err
			if set != nil {
				counts[i] = len(set.Criteria)
			}
		}(i)
	}
	wg.Wait()
	for i := 0; i < n; i++ {
		if errs[i] != nil || statuses[i] != evalStatusFound || counts[i] != 3 {
			t.Fatalf("caller %d: status=%s count=%d err=%v", i, statuses[i], counts[i], errs[i])
		}
	}
	if got := atomic.LoadInt32(&runs); got != 1 {
		t.Fatalf("extraction must run once for %d concurrent callers, ran %d", n, got)
	}
	// 이미 found → 이후 요청은 추출 0회.
	for i := 0; i < 5; i++ {
		if _, st, _ := srv.getOrExtractEvaluationCriteria(ctx, vid, false); st != evalStatusFound {
			t.Fatalf("cached: %s", st)
		}
	}
	if got := atomic.LoadInt32(&runs); got != 1 {
		t.Fatalf("found result must not re-extract, ran %d", got)
	}
	// 강제 재추출인데 원문이 사라짐(anchor 없음) → 기존 found 결과 유지, 덮어쓰기 없음.
	if _, err := db.ExecContext(ctx, `UPDATE attachments SET extracted_text = '본문에 평가 관련 표 없음' WHERE id = $1`, attID); err != nil {
		t.Fatal(err)
	}
	set, st, err := srv.getOrExtractEvaluationCriteria(ctx, vid, true)
	if err != nil || st != evalStatusFound || set == nil || len(set.Criteria) != 3 {
		t.Fatalf("forced re-extract must keep previous found: %s %v %v", st, err, set)
	}
	var stored string
	_ = db.QueryRowContext(ctx, `SELECT evaluation_criteria_status FROM notice_versions WHERE id = $1`, vid).Scan(&stored)
	if stored != evalStatusFound {
		t.Fatalf("stored status overwritten: %s", stored)
	}
	// not_found TTL: 평가기준 없는 새 버전은 첫 요청만 추출, TTL 안 재요청은 추출 0회.
	rawID2 := must(`INSERT INTO raw_documents (source_id, external_notice_id, request_url, response_status, raw_content, content_hash, collector_version) VALUES ($1,$2,'test://cc2',200,'{}',$3,'test') RETURNING id`, sourceID, ext, "h2-"+ext)
	vid2 := must(`INSERT INTO notice_versions (notice_id, version_number, raw_document_id, change_type, is_current) VALUES ($1,2,$2,'correction',false) RETURNING id`, nid, rawID2)
	var runs2 int32
	evalExtractionObserver = func(v string) {
		if v == vid2 {
			atomic.AddInt32(&runs2, 1)
		}
	}
	for i := 0; i < 4; i++ {
		if _, st, _ := srv.getOrExtractEvaluationCriteria(ctx, vid2, false); st != evalStatusNotFound {
			t.Fatalf("not_found expected: %s", st)
		}
	}
	if got := atomic.LoadInt32(&runs2); got != 1 {
		t.Fatalf("not_found within TTL must not re-extract, ran %d", got)
	}
}
