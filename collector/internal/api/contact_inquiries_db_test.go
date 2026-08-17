package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"biz-platform/collector/internal/billing"
)

// 공개 문의하기(POST /api/contact) + 관리자 목록/상태 변경 통합 테스트(BIZ_TEST_DSN 필요).
// newUsageFixture(feature_usage_db_test.go)의 사용자/회사 시드·세션 헬퍼를 재사용한다.

func newContactFixture(t *testing.T) *usageFixture {
	f := newUsageFixture(t)
	f.mux.HandleFunc("POST /api/contact", f.srv.handleCreateContactInquiry)
	f.mux.HandleFunc("GET /api/admin/inquiries", f.srv.handleAdminListContactInquiries)
	f.mux.HandleFunc("PATCH /api/admin/inquiries/{id}", f.srv.handleAdminUpdateContactInquiry)
	f.mux.HandleFunc("GET /api/proposal-drafts", f.srv.handleListProposalDrafts)
	t.Cleanup(func() {
		_, _ = f.db.ExecContext(f.ctx, `DELETE FROM contact_inquiries WHERE email LIKE $1`, "%-"+f.tag+"@contact.test")
		_, _ = f.db.ExecContext(f.ctx, `DELETE FROM auth_lookup_attempts WHERE kind = 'contact_inquiry' AND identifier LIKE $1`, "10.9.%."+f.tag[len(f.tag)-3:])
	})
	return f
}

// doIP — 특정 X-Forwarded-For로 요청(rate limit 식별자 = 클라이언트 IP).
func (f *usageFixture) doIP(ip, userID, method, path string, body any) (int, map[string]any) {
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(method, path, strings.NewReader(string(b)))
	req.Header.Set("X-Forwarded-For", ip)
	if userID != "" {
		req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: f.srv.signSession(userID, time.Now().Add(time.Hour))})
	}
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req)
	var m map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &m)
	return rec.Code, m
}

func TestContactInquiry_ValidationAndStore(t *testing.T) {
	f := newContactFixture(t)
	suffix := f.tag[len(f.tag)-3:]
	ip := "10.9.1." + suffix
	valid := map[string]any{
		"name": "홍길동", "companyName": "테스트 주식회사", "email": "hong-" + f.tag + "@contact.test",
		"phone": "010-1234-5678", "inquiryType": "service", "message": "서비스 이용 방법이 궁금합니다. 자세히 알려주세요.",
		"privacyAgreed": true,
	}
	// 필수값 누락
	bad := map[string]any{"email": valid["email"], "inquiryType": "service", "message": valid["message"], "privacyAgreed": true}
	if code, m := f.doIP(ip, "", "POST", "/api/contact", bad); code != 400 || m["error"] != "missing_required" {
		t.Fatalf("missing name: %d %v", code, m)
	}
	// 이메일 형식
	bad = copyMap(valid)
	bad["email"] = "not-an-email"
	if code, m := f.doIP(ip, "", "POST", "/api/contact", bad); code != 400 || m["error"] != "invalid_email" {
		t.Fatalf("invalid email: %d %v", code, m)
	}
	// 개인정보 미동의
	bad = copyMap(valid)
	bad["privacyAgreed"] = false
	if code, m := f.doIP(ip, "", "POST", "/api/contact", bad); code != 400 || m["error"] != "privacy_agreement_required" {
		t.Fatalf("privacy: %d %v", code, m)
	}
	// 길이 제한
	bad = copyMap(valid)
	bad["message"] = strings.Repeat("가", contactInquiryMaxMessage+1)
	if code, m := f.doIP(ip, "", "POST", "/api/contact", bad); code != 400 || m["error"] != "message_too_long" {
		t.Fatalf("too long: %d %v", code, m)
	}
	bad = copyMap(valid)
	bad["message"] = "짧음"
	if code, m := f.doIP(ip, "", "POST", "/api/contact", bad); code != 400 || m["error"] != "message_too_short" {
		t.Fatalf("too short: %d %v", code, m)
	}
	// 알 수 없는 유형
	bad = copyMap(valid)
	bad["inquiryType"] = "hack"
	if code, m := f.doIP(ip, "", "POST", "/api/contact", bad); code != 400 || m["error"] != "invalid_inquiry_type" {
		t.Fatalf("type: %d %v", code, m)
	}
	// 검증 실패는 시도 횟수를 소비하지 않는다 → 정상 접수 201
	code, m := f.doIP(ip, "", "POST", "/api/contact", valid)
	if code != 201 || m["id"] == "" || m["accepted"] != true {
		t.Fatalf("create: %d %v", code, m)
	}
	id := m["id"].(string)
	// 저장 확인(HTML은 그대로 저장, 표시 단계에서 escape)
	var name, msg, status string
	var userID *string
	if err := f.db.QueryRowContext(f.ctx, `SELECT name, message, status, user_id FROM contact_inquiries WHERE id = $1`, id).Scan(&name, &msg, &status, &userID); err != nil {
		t.Fatalf("select: %v", err)
	}
	if name != "홍길동" || status != "new" || userID != nil {
		t.Fatalf("stored row: %q %q %v", name, status, userID)
	}
	// 같은 IP 1분 내 재제출 → 429
	if code, m := f.doIP(ip, "", "POST", "/api/contact", valid); code != 429 || m["error"] != "rate_limited" {
		t.Fatalf("rate limit: %d %v", code, m)
	}
	// 허니팟 채운 봇 → 201처럼 응답하지만 저장 없음
	bot := copyMap(valid)
	bot["website"] = "http://spam.example"
	bot["email"] = "bot-" + f.tag + "@contact.test"
	if code, _ := f.doIP("10.9.2."+suffix, "", "POST", "/api/contact", bot); code != 201 {
		t.Fatalf("honeypot: %d", code)
	}
	var botCount int
	_ = f.db.QueryRowContext(f.ctx, `SELECT count(*) FROM contact_inquiries WHERE email = $1`, bot["email"]).Scan(&botCount)
	if botCount != 0 {
		t.Fatalf("honeypot must not store: %d", botCount)
	}
	// 로그인 사용자 접수 → user_id 기록
	uid, _ := f.actor("contact-user", billing.PlanFree)
	loggedIn := copyMap(valid)
	loggedIn["email"] = "member-" + f.tag + "@contact.test"
	code, m = f.doIP("10.9.3."+suffix, uid, "POST", "/api/contact", loggedIn)
	if code != 201 {
		t.Fatalf("member create: %d %v", code, m)
	}
	var storedUID *string
	_ = f.db.QueryRowContext(f.ctx, `SELECT user_id FROM contact_inquiries WHERE id = $1`, m["id"]).Scan(&storedUID)
	if storedUID == nil || *storedUID != uid {
		t.Fatalf("member user_id: %v", storedUID)
	}
}

func TestContactInquiry_AdminListRequiresSystemAdmin(t *testing.T) {
	f := newContactFixture(t)
	suffix := f.tag[len(f.tag)-3:]
	// 접수 1건
	code, m := f.doIP("10.9.4."+suffix, "", "POST", "/api/contact", map[string]any{
		"name": "관리자테스트", "email": "adm-" + f.tag + "@contact.test", "inquiryType": "billing",
		"message": "요금제 결제 관련 문의드립니다. 확인 부탁드립니다.", "privacyAgreed": true,
	})
	if code != 201 {
		t.Fatalf("create: %d %v", code, m)
	}
	id := m["id"].(string)
	// 비로그인 401 / 일반 사용자 403 / system_admin 200
	if c, _, _ := f.do("", "GET", "/api/admin/inquiries", nil); c != 401 {
		t.Fatalf("anon list: %d", c)
	}
	uid, _ := f.actor("plain", billing.PlanFree)
	if c, _, _ := f.do(uid, "GET", "/api/admin/inquiries", nil); c != 403 {
		t.Fatalf("user list: %d", c)
	}
	adminID := f.must(`INSERT INTO users (email, password_hash, role, plan) VALUES ($1,'x','system_admin','free') RETURNING id`, "sa-"+f.tag+"@contact.test")
	f.users = append(f.users, adminID)
	c, lm, _ := f.do(adminID, "GET", "/api/admin/inquiries?limit=5", nil)
	if c != 200 {
		t.Fatalf("admin list: %d %v", c, lm)
	}
	items := lm["items"].([]any)
	found := false
	for _, it := range items {
		row := it.(map[string]any)
		if row["id"] == id && row["inquiryTypeLabel"] == "요금제·결제 문의" && row["status"] == "new" {
			found = true
		}
	}
	if !found {
		t.Fatalf("admin list must contain new inquiry: %v", lm)
	}
	// 상태 변경: 일반 사용자 403, 잘못된 값 400, 관리자 200, 없는 id 404
	if c, _, _ := f.do(uid, "PATCH", "/api/admin/inquiries/"+id, map[string]any{"status": "done"}); c != 403 {
		t.Fatalf("user patch: %d", c)
	}
	if c, _, _ := f.do(adminID, "PATCH", "/api/admin/inquiries/"+id, map[string]any{"status": "weird"}); c != 400 {
		t.Fatalf("bad status: %d", c)
	}
	if c, _, _ := f.do(adminID, "PATCH", "/api/admin/inquiries/"+id, map[string]any{"status": "done"}); c != 200 {
		t.Fatalf("admin patch: %d", c)
	}
	if c, _, _ := f.do(adminID, "PATCH", "/api/admin/inquiries/00000000-0000-0000-0000-000000000000", map[string]any{"status": "done"}); c != 404 {
		t.Fatalf("missing id: %d", c)
	}
	var status string
	_ = f.db.QueryRowContext(f.ctx, `SELECT status FROM contact_inquiries WHERE id = $1`, id).Scan(&status)
	if status != "done" {
		t.Fatalf("status not updated: %s", status)
	}
}

// 제안서 목록(GET /api/proposal-drafts): 로그인 401, 회사 없음 빈 목록, 소유 회사 초안만.
func TestProposalDraftsList_OwnerScoped(t *testing.T) {
	f := newContactFixture(t)
	if c, _, _ := f.do("", "GET", "/api/proposal-drafts", nil); c != 401 {
		t.Fatalf("anon: %d", c)
	}
	// 회사 없는 사용자 → 빈 목록
	noCompany := f.must(`INSERT INTO users (email, password_hash, role, plan) VALUES ($1,'x','user','free') RETURNING id`, "nocomp-"+f.tag+"@contact.test")
	f.users = append(f.users, noCompany)
	if c, m, _ := f.do(noCompany, "GET", "/api/proposal-drafts", nil); c != 200 || len(m["items"].([]any)) != 0 {
		t.Fatalf("no company: %d %v", c, m)
	}
	uidA, pidA := f.actor("draft-a", billing.PlanBasic)
	uidB, pidB := f.actor("draft-b", billing.PlanBasic)
	nid, vid := f.notice("목록 테스트 공고", false)
	// A 회사 초안 2건, B 회사 초안 1건 직접 시드
	f.must(`INSERT INTO proposal_drafts (notice_id, notice_version_id, company_profile_id, created_by_user_id, title) VALUES ($1,$2,$3,$4,'A-1') RETURNING id`, nid, vid, pidA, uidA)
	f.must(`INSERT INTO proposal_drafts (notice_id, notice_version_id, company_profile_id, created_by_user_id, title) VALUES ($1,$2,$3,$4,'A-2') RETURNING id`, nid, vid, pidA, uidA)
	f.must(`INSERT INTO proposal_drafts (notice_id, notice_version_id, company_profile_id, created_by_user_id, title) VALUES ($1,$2,$3,$4,'B-1') RETURNING id`, nid, vid, pidB, uidB)
	c, m, _ := f.do(uidA, "GET", "/api/proposal-drafts", nil)
	if c != 200 {
		t.Fatalf("A list: %d %v", c, m)
	}
	items := m["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("A must see 2 drafts, got %d", len(items))
	}
	for _, it := range items {
		row := it.(map[string]any)
		if row["noticeTitle"] != "목록 테스트 공고" || row["stale"] != false || strings.HasPrefix(row["title"].(string), "B-") {
			t.Fatalf("A row: %v", row)
		}
	}
	c, m, _ = f.do(uidB, "GET", "/api/proposal-drafts", nil)
	if c != 200 || len(m["items"].([]any)) != 1 {
		t.Fatalf("B must see 1 draft: %d %v", c, m)
	}
}

func copyMap(m map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range m {
		out[k] = v
	}
	return out
}
