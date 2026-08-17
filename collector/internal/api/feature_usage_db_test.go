package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"biz-platform/collector/internal/billing"
	"biz-platform/collector/internal/migrate"
)

// 플랜 정책 + 사용량 quota 통합 테스트(BIZ_TEST_DSN 필요).
//   - Consumption usage(feature_usage): 참여 가능 여부(월·공고 dedup·동시성·월 경계), 제안서(Free 평생
//     체험 1회 → 이후 403, 체험 초안 GET/PATCH/DOCX 유지, Basic 5/6번째 차단, Pro 30 정책), SMS(Free 0,
//     Basic 10 dedup·release), OCR 파일 해시 dedup, 업그레이드 시 같은 달 사용량 일관성.
//   - Capacity(row count): 맞춤공고 Free 1/Basic 5/Pro 20, 삭제 후 재생성.

type usageFixture struct {
	t     *testing.T
	db    *sql.DB
	ctx   context.Context
	srv   *Server
	mux   *http.ServeMux
	tag   string
	users []string
	profs []string
	nids  []string
}

func newUsageFixture(t *testing.T) *usageFixture {
	db := openTestDB(t)
	ctx := context.Background()
	t.Setenv("ANTHROPIC_API_KEY", "")
	if err := migrate.Apply(ctx, db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	srv := &Server{db: db, logger: slog.New(slog.NewTextHandler(io.Discard, nil)), sessionSecret: []byte("usage-test-secret-0123456789abcdef")}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/me", srv.handleMe)
	mux.HandleFunc("GET /api/notices/{id}", srv.handleGetNotice)
	mux.HandleFunc("GET /api/notices/{id}/proposal-readiness", srv.handleProposalReadiness)
	mux.HandleFunc("POST /api/notices/{id}/proposal-drafts", srv.handleCreateProposalDraft)
	mux.HandleFunc("GET /api/proposal-drafts/{id}", srv.handleGetProposalDraft)
	mux.HandleFunc("PATCH /api/proposal-drafts/{id}", srv.handlePatchProposalDraft)
	mux.HandleFunc("GET /api/proposal-drafts/{id}/docx", srv.handleProposalDraftDocx)
	mux.HandleFunc("POST /api/me/saved-searches", srv.handleCreateSavedSearch)
	mux.HandleFunc("DELETE /api/me/saved-searches/{id}", srv.handleDeleteSavedSearch)
	f := &usageFixture{t: t, db: db, ctx: ctx, srv: srv, mux: mux, tag: time.Now().Format("150405.000000")}
	t.Cleanup(func() {
		for _, n := range f.nids {
			_, _ = db.ExecContext(ctx, `DELETE FROM proposal_drafts WHERE notice_id = $1`, n)
			_, _ = db.ExecContext(ctx, `DELETE FROM attachments WHERE notice_version_id IN (SELECT id FROM notice_versions WHERE notice_id = $1)`, n)
			_, _ = db.ExecContext(ctx, `DELETE FROM notice_versions WHERE notice_id = $1`, n)
			_, _ = db.ExecContext(ctx, `DELETE FROM notices WHERE id = $1`, n)
		}
		_, _ = db.ExecContext(ctx, `DELETE FROM raw_documents WHERE external_notice_id LIKE $1`, "USAGETEST-%-"+f.tag)
		for _, p := range f.profs {
			_, _ = db.ExecContext(ctx, `DELETE FROM feature_usage WHERE company_profile_id = $1`, p)
			_, _ = db.ExecContext(ctx, `DELETE FROM subscriptions WHERE company_profile_id = $1`, p)
			_, _ = db.ExecContext(ctx, `DELETE FROM saved_searches WHERE user_id IN (SELECT user_id FROM company_members WHERE company_profile_id = $1)`, p)
			_, _ = db.ExecContext(ctx, `DELETE FROM company_members WHERE company_profile_id = $1`, p)
			_, _ = db.ExecContext(ctx, `DELETE FROM company_profiles WHERE id = $1`, p)
		}
		for _, u := range f.users {
			_, _ = db.ExecContext(ctx, `DELETE FROM users WHERE id = $1`, u)
		}
		db.Close()
	})
	return f
}

func (f *usageFixture) must(q string, args ...any) string {
	var id string
	if err := f.db.QueryRowContext(f.ctx, q, args...).Scan(&id); err != nil {
		f.t.Fatalf("seed [%.70s]: %v", q, err)
	}
	return id
}

// actor: 사용자+회사(+구독). plan "free"면 구독 없음.
func (f *usageFixture) actor(name string, plan billing.Plan) (userID, profileID string) {
	userID = f.must(`INSERT INTO users (email, password_hash, role, plan) VALUES ($1,'x','user','free') RETURNING id`, name+"-"+f.tag+"@usage.test")
	profileID = f.must(`INSERT INTO company_profiles (user_id, company_name, representative_name, address, region, industry, business_type) VALUES ($1,$2,'홍길동','서울시 테스트구','서울','{"행사기획"}','{"서비스업"}') RETURNING id`, userID, name+" 주식회사")
	f.must(`INSERT INTO company_members (company_profile_id, user_id, role) VALUES ($1,$2,'owner') RETURNING id`, profileID, userID)
	if plan != billing.PlanFree {
		f.must(`INSERT INTO subscriptions (company_profile_id, plan, status, started_at, expires_at, amount) VALUES ($1,$2,'active', now(), now() + interval '30 days', 19900) RETURNING id`, profileID, string(plan))
	}
	f.users = append(f.users, userID)
	f.profs = append(f.profs, profileID)
	return
}

func (f *usageFixture) setPlan(profileID string, plan billing.Plan) {
	if _, err := f.db.ExecContext(f.ctx, `DELETE FROM subscriptions WHERE company_profile_id = $1`, profileID); err != nil {
		f.t.Fatal(err)
	}
	if plan != billing.PlanFree {
		f.must(`INSERT INTO subscriptions (company_profile_id, plan, status, started_at, expires_at, amount) VALUES ($1,$2,'active', now(), now() + interval '30 days', 19900) RETURNING id`, profileID, string(plan))
	}
}

// notice: procurement + 현재 버전(+옵션 RFP 첨부 completed 텍스트).
func (f *usageFixture) notice(title string, withRFP bool) (nid, vid string) {
	var sourceID string
	if err := f.db.QueryRowContext(f.ctx, `SELECT id FROM data_sources LIMIT 1`).Scan(&sourceID); err != nil {
		f.t.Fatalf("data_sources: %v", err)
	}
	ext := fmt.Sprintf("USAGETEST-%d-%s", len(f.nids), f.tag)
	nid = f.must(`INSERT INTO notices (source_id, external_notice_id, notice_type, title, organization_name, budget_amount, current_version, success_bid_method_name) VALUES ($1,$2,'procurement',$3,'테스트발주기관',50000000,1,'협상에 의한 계약') RETURNING id`, sourceID, ext, title)
	rawID := f.must(`INSERT INTO raw_documents (source_id, external_notice_id, request_url, response_status, raw_content, content_hash, collector_version) VALUES ($1,$2,'test://usage',200,'{}',$3,'test') RETURNING id`, sourceID, ext, "h-"+ext)
	vid = f.must(`INSERT INTO notice_versions (notice_id, version_number, raw_document_id, change_type, is_current) VALUES ($1,1,$2,'initial',true) RETURNING id`, nid, rawID)
	if withRFP {
		f.must(`INSERT INTO attachments (notice_version_id, original_filename, stored_filename, file_hash, download_status, extraction_status, analysis_status, extracted_text) VALUES ($1,'제안요청서.pdf','x.pdf',$2,'completed','completed','completed',$3) RETURNING id`, vid, "fh-"+ext,
			"제안요청서\n3. 평가항목 및 배점기준\n1) 사업 이해도 (20점)\n2) 수행계획의 적정성 (25점)\n3) 전문인력 (20점)\n4) 유사사업 수행실적 (20점)\n5) 사후관리 (15점)\n계 100\n")
	}
	f.nids = append(f.nids, nid)
	return
}

func (f *usageFixture) do(userID, method, path string, body any) (int, map[string]any, []byte) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, rd)
	if userID != "" {
		req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: f.srv.signSession(userID, time.Now().Add(time.Hour))})
	}
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req)
	var m map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &m)
	return rec.Code, m, rec.Body.Bytes()
}

func TestFeatureUsage_ParticipationReview(t *testing.T) {
	f := newUsageFixture(t)
	ctx := f.ctx
	uid, pid := f.actor("free-pr", billing.PlanFree)
	limit := billing.Plans[billing.PlanFree].MonthlyLimit(billing.UsageParticipationReview)
	if limit != 3 {
		t.Fatalf("policy: free participation review limit = %d", limit)
	}
	period := usagePeriodMonth(time.Now())
	// 3개의 서로 다른 공고 → 허용, 4번째 → 차단, 같은 공고 재조회 → 추가 소비 없음.
	var nids []string
	for i := 0; i < 4; i++ {
		nid, _ := f.notice(fmt.Sprintf("참여검토 %d", i), false)
		nids = append(nids, nid)
	}
	for i := 0; i < 3; i++ {
		d, err := f.srv.consumeFeatureUsage(ctx, nil, pid, billing.UsageParticipationReview, period, nids[i], limit)
		if err != nil || !d.Allowed || !d.NewlyCounted || d.Used != i+1 {
			t.Fatalf("notice %d: %+v %v", i, d, err)
		}
	}
	d, err := f.srv.consumeFeatureUsage(ctx, nil, pid, billing.UsageParticipationReview, period, nids[3], limit)
	if err != nil || d.Allowed || d.Used != 3 {
		t.Fatalf("4th notice must be denied: %+v %v", d, err)
	}
	d, err = f.srv.consumeFeatureUsage(ctx, nil, pid, billing.UsageParticipationReview, period, nids[0], limit)
	if err != nil || !d.Allowed || !d.AlreadyCounted || d.NewlyCounted || d.Used != 3 {
		t.Fatalf("same notice again must not consume: %+v %v", d, err)
	}
	// HTTP: 상세 API — 4번째 공고는 participationReview.locked=true, judgment null; 기존 공고는 locked=false.
	code, m, _ := f.do(uid, "GET", "/api/notices/"+nids[3], nil)
	if code != 200 {
		t.Fatalf("detail: %d", code)
	}
	pr, _ := m["participationReview"].(map[string]any)
	if pr == nil || pr["locked"] != true || pr["used"].(float64) != 3 || pr["limit"].(float64) != 3 || m["participationJudgment"] != nil {
		t.Fatalf("locked expected: %v judgment=%v", pr, m["participationJudgment"])
	}
	code, m, _ = f.do(uid, "GET", "/api/notices/"+nids[0], nil)
	pr, _ = m["participationReview"].(map[string]any)
	if code != 200 || pr == nil || pr["locked"] != false || pr["used"].(float64) != 3 {
		t.Fatalf("already-counted notice must stay unlocked: %d %v", code, pr)
	}
	// 월 경계: 다음 달 기간키는 새로 0부터.
	next := usagePeriodMonth(time.Now().AddDate(0, 1, 0))
	d, err = f.srv.consumeFeatureUsage(ctx, nil, pid, billing.UsageParticipationReview, next, nids[3], limit)
	if err != nil || !d.Allowed || d.Used != 1 {
		t.Fatalf("new period must start fresh: %+v %v", d, err)
	}
	// 동시성: 새 회사, 한도 3, 서로 다른 공고 10개 동시 → 정확히 3개만 승인.
	_, pid2 := f.actor("free-cc", billing.PlanFree)
	var subjects []string
	for i := 0; i < 10; i++ {
		nid, _ := f.notice(fmt.Sprintf("동시성 %d", i), false)
		subjects = append(subjects, nid)
	}
	var allowed int32
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(sub string) {
			defer wg.Done()
			d, err := f.srv.consumeFeatureUsage(ctx, nil, pid2, billing.UsageParticipationReview, period, sub, limit)
			if err == nil && d.Allowed {
				atomic.AddInt32(&allowed, 1)
			}
		}(subjects[i])
	}
	wg.Wait()
	if allowed != 3 {
		t.Fatalf("concurrent: exactly 3 must be allowed, got %d", allowed)
	}
	used, _ := f.srv.countFeatureUsage(ctx, pid2, billing.UsageParticipationReview, period)
	if used != 3 {
		t.Fatalf("concurrent stored used=%d", used)
	}
	// 업그레이드: Free에서 3개 쓴 뒤 Basic → 같은 달 사용량 유지(3/30), 4번째 허용.
	f.setPlan(pid, billing.PlanBasic)
	blimit := billing.Plans[billing.PlanBasic].MonthlyLimit(billing.UsageParticipationReview)
	d, err = f.srv.consumeFeatureUsage(ctx, nil, pid, billing.UsageParticipationReview, period, nids[3], blimit)
	if err != nil || !d.Allowed || d.Used != 4 || d.Limit != 30 {
		t.Fatalf("after upgrade: %+v %v", d, err)
	}
	// /api/me usage 표시.
	code, me, _ := f.do(uid, "GET", "/api/me", nil)
	u := me["usage"].(map[string]any)["participation_review"].(map[string]any)
	if code != 200 || u["used"].(float64) != 4 || u["limit"].(float64) != 30 || me["effectivePlan"] != "basic" {
		t.Fatalf("/api/me usage: %d %v", code, me["usage"])
	}
}

func TestFeatureUsage_ProposalTrialAndMonthly(t *testing.T) {
	f := newUsageFixture(t)
	ctx := f.ctx
	// ---- Free: 평생 체험 1회 ----
	uid, pid := f.actor("free-pd", billing.PlanFree)
	n1, _ := f.notice("체험 공고 1", true)
	n2, _ := f.notice("체험 공고 2", true)
	code, me, _ := f.do(uid, "GET", "/api/me", nil)
	if code != 200 || me["entitlements"].(map[string]any)["proposal_draft_docx"] != true {
		t.Fatalf("free with trial remaining must be entitled: %d %v", code, me["entitlements"])
	}
	pu := me["usage"].(map[string]any)["proposal_draft"].(map[string]any)
	if pu["period"] != usagePeriodLifetime || pu["limit"].(float64) != 1 || pu["used"].(float64) != 0 {
		t.Fatalf("free proposal usage: %v", pu)
	}
	code, rd, _ := f.do(uid, "GET", "/api/notices/"+n1+"/proposal-readiness", nil)
	if code != 200 || rd["status"] != "ready" {
		t.Fatalf("free readiness(trial): %d %v", code, rd)
	}
	code, cr, _ := f.do(uid, "POST", "/api/notices/"+n1+"/proposal-drafts", map[string]any{})
	if code != 201 {
		t.Fatalf("free first draft: %d %v", code, cr)
	}
	draftID := cr["id"].(string)
	// 두 번째(다른 공고) → 403(체험 소진 = entitlement false → 기존 계약 paid_feature_required → 결제 안내
	// 모달), 초안 미생성. (한도 게이트가 먼저 잡히는 경로도 403 quota_exceeded로 같은 결과.)
	code, cr2, _ := f.do(uid, "POST", "/api/notices/"+n2+"/proposal-drafts", map[string]any{})
	if code != 403 || (cr2["error"] != errorPaidFeatureRequired && cr2["error"] != errorQuotaExceeded) {
		t.Fatalf("free second draft must be blocked: %d %v", code, cr2)
	}
	var cnt int
	_ = f.db.QueryRowContext(ctx, `SELECT count(*) FROM proposal_drafts WHERE company_profile_id = $1`, pid).Scan(&cnt)
	if cnt != 1 {
		t.Fatalf("draft count after blocked create: %d", cnt)
	}
	// 체험 소진 후 entitlement false, readiness 403 paid_feature_required.
	_, me, _ = f.do(uid, "GET", "/api/me", nil)
	if me["entitlements"].(map[string]any)["proposal_draft_docx"] != false {
		t.Fatalf("free after trial must not be entitled: %v", me["entitlements"])
	}
	code, rd, _ = f.do(uid, "GET", "/api/notices/"+n2+"/proposal-readiness", nil)
	if code != 403 || rd["error"] != errorPaidFeatureRequired {
		t.Fatalf("free readiness after trial: %d %v", code, rd)
	}
	// 기존 체험 초안: GET/PATCH/DOCX 계속 가능.
	code, g, _ := f.do(uid, "GET", "/api/proposal-drafts/"+draftID, nil)
	if code != 200 || g["id"] != draftID {
		t.Fatalf("trial draft GET: %d %v", code, g)
	}
	code, _, _ = f.do(uid, "PATCH", "/api/proposal-drafts/"+draftID, map[string]any{"title": "수정된 제목"})
	if code != 200 {
		t.Fatalf("trial draft PATCH: %d", code)
	}
	req := httptest.NewRequest("GET", "/api/proposal-drafts/"+draftID+"/docx", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: f.srv.signSession(uid, time.Now().Add(time.Hour))})
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req)
	if rec.Code != 200 || rec.Header().Get("Content-Type") == "" || rec.Body.Len() < 100 {
		t.Fatalf("trial draft DOCX: %d %s len=%d", rec.Code, rec.Header().Get("Content-Type"), rec.Body.Len())
	}
	// ---- Basic: 월 5, 6번째 차단(같은 공고 force 재생성도 새 초안 = 1건) ----
	buid, bpid := f.actor("basic-pd", billing.PlanBasic)
	bn, _ := f.notice("Basic 공고", true)
	for i := 0; i < 5; i++ {
		code, cr, _ := f.do(buid, "POST", "/api/notices/"+bn+"/proposal-drafts", map[string]any{"force": true})
		if code != 201 {
			t.Fatalf("basic draft %d: %d %v", i+1, code, cr)
		}
	}
	code, cr6, _ := f.do(buid, "POST", "/api/notices/"+bn+"/proposal-drafts", map[string]any{"force": true})
	if code != 403 || cr6["error"] != errorQuotaExceeded || cr6["used"].(float64) != 5 || cr6["limit"].(float64) != 5 {
		t.Fatalf("basic 6th must be blocked: %d %v", code, cr6)
	}
	_ = f.db.QueryRowContext(ctx, `SELECT count(*) FROM proposal_drafts WHERE company_profile_id = $1`, bpid).Scan(&cnt)
	if cnt != 5 {
		t.Fatalf("basic drafts after blocked create: %d (rollback expected)", cnt)
	}
	// 기존 초안 재조회/수정/DOCX는 소비 없음.
	used, _ := f.srv.countFeatureUsage(ctx, bpid, billing.UsageProposalDraft, usagePeriodMonth(time.Now()))
	if used != 5 {
		t.Fatalf("basic used=%d", used)
	}
	// ---- Pro: 30 정책(헬퍼로 30 소비 후 31번째 차단) ----
	_, ppid := f.actor("pro-pd", billing.PlanPro)
	plimit := billing.Plans[billing.PlanPro].MonthlyLimit(billing.UsageProposalDraft)
	if plimit != 30 {
		t.Fatalf("pro policy limit=%d", plimit)
	}
	period := usagePeriodMonth(time.Now())
	for i := 0; i < 30; i++ {
		d, err := f.srv.consumeFeatureUsage(ctx, nil, ppid, billing.UsageProposalDraft, period, fmt.Sprintf("draft-%d", i), plimit)
		if err != nil || !d.Allowed {
			t.Fatalf("pro %d: %+v %v", i, d, err)
		}
	}
	if d, _ := f.srv.consumeFeatureUsage(ctx, nil, ppid, billing.UsageProposalDraft, period, "draft-31", plimit); d.Allowed {
		t.Fatalf("pro 31st must be blocked")
	}
	// Business: 무제한(-1)이어도 기록은 남고 항상 허용.
	if billing.Plans[billing.PlanBusiness].MonthlyLimit(billing.UsageProposalDraft) != -1 {
		t.Fatalf("business proposal must be unlimited")
	}
}

func TestFeatureUsage_SavedSearchCapacity(t *testing.T) {
	f := newUsageFixture(t)
	ctx := f.ctx
	uid, pid := f.actor("free-ss", billing.PlanFree)
	// 온보딩 자동생성 1개가 이미 있다고 가정.
	f.must(`INSERT INTO saved_searches (user_id, name, region, industry, alert_enabled, origin) VALUES ($1,'온보딩','서울','{"행사기획"}',true,'onboarding') RETURNING id`, uid)
	body := map[string]any{"name": "추가 조건", "region": "부산", "industry": "행사기획", "keywordsInclude": []string{"축제"}}
	code, m, _ := f.do(uid, "POST", "/api/me/saved-searches", body)
	if code != 403 || m["error"] != errorQuotaExceeded || m["feature"] != "saved_search" || m["used"].(float64) != 1 || m["limit"].(float64) != 1 {
		t.Fatalf("free second saved search must be blocked: %d %v", code, m)
	}
	// Basic 5: 4개 더 만들 수 있고 6번째 차단.
	f.setPlan(pid, billing.PlanBasic)
	var ids []string
	for i := 0; i < 4; i++ {
		b := map[string]any{"name": fmt.Sprintf("조건 %d", i), "region": "부산", "industry": "행사기획", "keywordsInclude": []string{fmt.Sprintf("키워드%d", i)}}
		code, m, _ := f.do(uid, "POST", "/api/me/saved-searches", b)
		if code != 201 && code != 200 {
			t.Fatalf("basic create %d: %d %v", i, code, m)
		}
		if id, ok := m["id"].(string); ok {
			ids = append(ids, id)
		} else if item, ok := m["item"].(map[string]any); ok {
			ids = append(ids, item["id"].(string))
		}
	}
	code, m, _ = f.do(uid, "POST", "/api/me/saved-searches", map[string]any{"name": "6번째", "region": "대구", "industry": "행사기획", "keywordsInclude": []string{"여섯"}})
	if code != 403 || m["error"] != errorQuotaExceeded || m["limit"].(float64) != 5 {
		t.Fatalf("basic 6th must be blocked: %d %v", code, m)
	}
	// 삭제 후 재생성 가능.
	if len(ids) == 0 {
		t.Fatalf("no created ids captured")
	}
	code, _, _ = f.do(uid, "DELETE", "/api/me/saved-searches/"+ids[0], nil)
	if code != 200 && code != 204 {
		t.Fatalf("delete: %d", code)
	}
	code, m, _ = f.do(uid, "POST", "/api/me/saved-searches", map[string]any{"name": "재생성", "region": "대구", "industry": "행사기획", "keywordsInclude": []string{"재생성"}})
	if code != 201 && code != 200 {
		t.Fatalf("recreate after delete: %d %v", code, m)
	}
	// Pro 20 / Business 무제한 정책값.
	if billing.Plans[billing.PlanPro].MaxSavedSearches != 20 || billing.Plans[billing.PlanBusiness].MaxSavedSearches != -1 {
		t.Fatalf("policy: pro=%d business=%d", billing.Plans[billing.PlanPro].MaxSavedSearches, billing.Plans[billing.PlanBusiness].MaxSavedSearches)
	}
	f.setPlan(pid, billing.PlanPro)
	ok, used, limit, err := f.srv.checkSavedSearchCapacity(ctx, pid)
	if err != nil || !ok || used != 5 || limit != 20 {
		t.Fatalf("pro capacity: ok=%v used=%d limit=%d err=%v", ok, used, limit, err)
	}
	// /api/me capacities.
	code, me, _ := f.do(uid, "GET", "/api/me", nil)
	c := me["capacities"].(map[string]any)["saved_search"].(map[string]any)
	if code != 200 || c["used"].(float64) != 5 || c["limit"].(float64) != 20 {
		t.Fatalf("/api/me capacities: %v", me["capacities"])
	}
}

func TestFeatureUsage_SMSAndOCR(t *testing.T) {
	f := newUsageFixture(t)
	ctx := f.ctx
	period := usagePeriodMonth(time.Now())
	// SMS Free: 한도 0 → 항상 거부(기록 없음).
	_, fpid := f.actor("free-sms", billing.PlanFree)
	if !f.srv.smsAllowedForPlan(ctx, fpid) == false {
		t.Fatalf("free must be blocked by smsAllowedForPlan")
	}
	d, err := f.srv.reserveSMSUsage(ctx, fpid, period, "evt|e1|n1|c1|010")
	if err != nil || d.Allowed {
		t.Fatalf("free sms must be denied: %+v %v", d, err)
	}
	// Basic 10: 서로 다른 알림 10건 허용, 11번째 거부, 같은 알림 재시도는 추가 소비 없음, 실패 release.
	_, bpid := f.actor("basic-sms", billing.PlanBasic)
	for i := 0; i < 10; i++ {
		d, err := f.srv.reserveSMSUsage(ctx, bpid, period, fmt.Sprintf("evt|e%d|n1|c1|010", i))
		if err != nil || !d.Allowed {
			t.Fatalf("basic sms %d: %+v %v", i, d, err)
		}
	}
	if d, _ := f.srv.reserveSMSUsage(ctx, bpid, period, "evt|e99|n1|c1|010"); d.Allowed {
		t.Fatalf("basic 11th sms must be denied")
	}
	d, _ = f.srv.reserveSMSUsage(ctx, bpid, period, "evt|e3|n1|c1|010")
	if !d.Allowed || !d.AlreadyCounted || d.Used != 10 {
		t.Fatalf("retry same notification must not double count: %+v", d)
	}
	f.srv.releaseFeatureUsage(ctx, bpid, billing.UsageSMS, period, "evt|e3|n1|c1|010")
	if used, _ := f.srv.countFeatureUsage(ctx, bpid, billing.UsageSMS, period); used != 9 {
		t.Fatalf("release: used=%d", used)
	}
	// Pro 100 / Business 무제한 정책.
	if billing.Plans[billing.PlanPro].MonthlyLimit(billing.UsageSMS) != 100 || billing.Plans[billing.PlanBusiness].MonthlyLimit(billing.UsageSMS) != -1 {
		t.Fatalf("sms policy mismatch")
	}
	// OCR: 같은 파일 해시 반복 → 1건, Free 월 5.
	olimit := billing.Plans[billing.PlanFree].MonthlyLimit(billing.UsageBusinessRegistrationOCR)
	if olimit != 5 {
		t.Fatalf("ocr free limit=%d", olimit)
	}
	for i := 0; i < 3; i++ {
		d, err := f.srv.consumeFeatureUsage(ctx, nil, fpid, billing.UsageBusinessRegistrationOCR, period, "hash-same", olimit)
		if err != nil || !d.Allowed || d.Used != 1 {
			t.Fatalf("ocr same hash %d: %+v %v", i, d, err)
		}
	}
	for i := 0; i < 4; i++ {
		if d, _ := f.srv.consumeFeatureUsage(ctx, nil, fpid, billing.UsageBusinessRegistrationOCR, period, fmt.Sprintf("hash-%d", i), olimit); !d.Allowed {
			t.Fatalf("ocr distinct %d must be allowed", i)
		}
	}
	if d, _ := f.srv.consumeFeatureUsage(ctx, nil, fpid, billing.UsageBusinessRegistrationOCR, period, "hash-x", olimit); d.Allowed {
		t.Fatalf("ocr 6th distinct must be denied")
	}
}
