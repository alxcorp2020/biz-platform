package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"biz-platform/collector/internal/collector/common"
	"biz-platform/collector/internal/collector/sources/scsbid"
)

// 개찰결과 child endpoint 통합 테스트(BIZ_TEST_DSN 필요) — API 실패 시 공고 상세가 아니라 이 endpoint만
// graceful degradation(200 + fetchError)하고, 성공 시 notice_opening_results에 캐시돼 두 번째 호출은 API를
// 다시 부르지 않는지 확인한다. 로컬 fixture는 전량 원복한다.
func TestOpeningResultEndpoint_FailureFallbackAndCache(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	ctx := context.Background()

	var sourceID string
	if err := db.QueryRowContext(ctx, `SELECT id FROM data_sources LIMIT 1`).Scan(&sourceID); err != nil {
		t.Fatalf("data_sources: %v", err)
	}
	ext := "OPENTEST-" + time.Now().Format("150405.000000")
	past := time.Now().Add(-2 * time.Hour)
	var noticeID string
	if err := db.QueryRowContext(ctx, `INSERT INTO notices (source_id, external_notice_id, notice_type, title, organization_name, opening_at, current_version)
		VALUES ($1,$2,'procurement','개찰테스트공고','테스트기관',$3,1) RETURNING id`, sourceID, ext, past).Scan(&noticeID); err != nil {
		t.Fatalf("seed notice: %v", err)
	}
	defer func() {
		_, _ = db.ExecContext(ctx, `DELETE FROM notice_opening_results WHERE notice_id = $1`, noticeID)
		_, _ = db.ExecContext(ctx, `DELETE FROM notices WHERE id = $1`, noticeID)
	}()

	// 1) API 500 → 200 + fetchError(공고 상세 자체는 무관), 캐시 없음.
	calls := 0
	failing := true
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		calls++
		if failing {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/getOpengResultListInfoServc") {
			_, _ = w.Write([]byte(`{"response":{"header":{"resultCode":"00","resultMsg":"정상"},"body":{"items":[{"bidNtceNo":"` + ext + `","bidNtceOrd":"000","bidClsfcNo":"0","rbidNo":"000","opengDt":"2026-08-14 15:00:00","prtcptCnum":"3","opengCorpInfo":"테스트업체^1234567890^대표^100^90.5","progrsDivCdNm":"개찰완료","rsrvtnPrceFileExistnceYn":"N"}],"totalCount":1}}}`))
			return
		}
		_, _ = w.Write([]byte(`{"response":{"header":{"resultCode":"03","resultMsg":"NODATA"},"body":{"items":"","totalCount":0}}}`))
	})
	apiSrv := httptest.NewServer(mux)
	defer apiSrv.Close()

	src := &scsbid.Source{ServiceKey: "k", HTTPClient: &http.Client{}, RateLimit: common.NewRateLimiter(1000, 1000000), PageSize: 100, BaseURL: apiSrv.URL}
	s := &Server{db: db, logger: slog.Default(), scsbidSource: src}
	routes := http.NewServeMux()
	routes.HandleFunc("GET /api/notices/{id}/opening-result", s.handleGetNoticeOpeningResult)

	get := func() (int, map[string]any) {
		rec := httptest.NewRecorder()
		routes.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/notices/"+noticeID+"/opening-result", nil))
		var body map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		return rec.Code, body
	}

	code, body := get()
	if code != http.StatusOK {
		t.Fatalf("failure path must still be 200, got %d", code)
	}
	if body["status"] != "UNAVAILABLE" || body["fetchError"] == nil {
		t.Fatalf("expected UNAVAILABLE + fetchError, got %+v", body)
	}
	if calls == 0 {
		t.Fatalf("api should have been called")
	}

	// 2) API 정상 → OPENED_WAITING_AWARD, 캐시 저장. 3) 재호출은 캐시(추가 API 호출 없음).
	failing = false
	calls = 0
	code, body = get()
	if code != http.StatusOK || body["status"] != "OPENED_WAITING_AWARD" {
		t.Fatalf("expected OPENED_WAITING_AWARD, got %d %+v", code, body)
	}
	if body["topBidder"] == nil || body["winner"] != nil {
		t.Fatalf("waiting award must have topBidder and no winner: %+v", body)
	}
	if calls == 0 {
		t.Fatalf("api should have been called on refresh")
	}
	var status string
	if err := db.QueryRowContext(ctx, `SELECT status FROM notice_opening_results WHERE notice_id = $1`, noticeID).Scan(&status); err != nil || status != "OPENED_WAITING_AWARD" {
		t.Fatalf("cache row: %v %q", err, status)
	}
	calls = 0
	code, body = get()
	if code != http.StatusOK || body["status"] != "OPENED_WAITING_AWARD" || calls != 0 {
		t.Fatalf("second call must be served from cache (calls=%d) %+v", calls, body)
	}

	// 4) 캐시 만료 + API 다시 실패 → 마지막 캐시를 stale로 반환(200), 다시 15분 뒤 재시도로 기록.
	if _, err := db.ExecContext(ctx, `UPDATE notice_opening_results SET next_check_at = now() - interval '1 minute' WHERE notice_id = $1`, noticeID); err != nil {
		t.Fatalf("expire cache: %v", err)
	}
	failing = true
	code, body = get()
	if code != http.StatusOK || body["status"] != "OPENED_WAITING_AWARD" || body["stale"] != true {
		t.Fatalf("stale fallback expected, got %d %+v", code, body)
	}
}

// 개찰 전 공고는 API/DB 없이 BEFORE_OPENING을 즉시 돌려준다(쿼터 0).
func TestOpeningResultEndpoint_BeforeOpeningNoAPICall(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	ctx := context.Background()
	var sourceID string
	if err := db.QueryRowContext(ctx, `SELECT id FROM data_sources LIMIT 1`).Scan(&sourceID); err != nil {
		t.Fatalf("data_sources: %v", err)
	}
	ext := "OPENTEST-B-" + time.Now().Format("150405.000000")
	future := time.Now().Add(48 * time.Hour)
	var noticeID string
	if err := db.QueryRowContext(ctx, `INSERT INTO notices (source_id, external_notice_id, notice_type, title, organization_name, opening_at, current_version)
		VALUES ($1,$2,'procurement','개찰전공고','테스트기관',$3,1) RETURNING id`, sourceID, ext, future).Scan(&noticeID); err != nil {
		t.Fatalf("seed notice: %v", err)
	}
	defer func() { _, _ = db.ExecContext(ctx, `DELETE FROM notices WHERE id = $1`, noticeID) }()

	calls := 0
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++ }))
	defer apiSrv.Close()
	src := &scsbid.Source{ServiceKey: "k", HTTPClient: &http.Client{}, RateLimit: common.NewRateLimiter(1000, 1000000), PageSize: 100, BaseURL: apiSrv.URL}
	s := &Server{db: db, logger: slog.Default(), scsbidSource: src}
	routes := http.NewServeMux()
	routes.HandleFunc("GET /api/notices/{id}/opening-result", s.handleGetNoticeOpeningResult)
	rec := httptest.NewRecorder()
	routes.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/notices/"+noticeID+"/opening-result", nil))
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if rec.Code != 200 || body["status"] != "BEFORE_OPENING" || calls != 0 {
		t.Fatalf("before opening: %d %+v calls=%d", rec.Code, body, calls)
	}
	var n int
	_ = db.QueryRowContext(ctx, `SELECT count(*) FROM notice_opening_results WHERE notice_id = $1`, noticeID).Scan(&n)
	if n != 0 {
		t.Fatalf("before opening must not write cache")
	}
}

// ---- 캐시 identity 안전성(정정/차수 변경) — CASE A~E ----
//
// notice_opening_results PK가 notice_id 하나라, 정정공고로 차수(bidNtceOrd)가 000→001로 바뀐 뒤 과거 차수의
// 확정 캐시(next_check_at 미래)가 새 차수 상세에 그대로 반환되면 안 된다. 현재 identity(공고번호·차수·
// 업무유형)를 raw_content에서 읽어 캐시 row와 비교하는지 검증한다. fixture는 테스트 공고/버전/원문만 만들고
// 전량 삭제한다(기존 notice_versions/원문 무수정).
func TestOpeningResultEndpoint_CacheIdentity(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	ctx := context.Background()
	var sourceID string
	if err := db.QueryRowContext(ctx, `SELECT id FROM data_sources LIMIT 1`).Scan(&sourceID); err != nil {
		t.Fatalf("data_sources: %v", err)
	}
	ext := "OPENTEST-ID-" + time.Now().Format("150405.000000")
	past := time.Now().Add(-3 * time.Hour)
	must := func(q string, args ...any) string {
		var id string
		if err := db.QueryRowContext(ctx, q, args...).Scan(&id); err != nil {
			t.Fatalf("seed [%.50s]: %v", q, err)
		}
		return id
	}
	noticeID := must(`INSERT INTO notices (source_id, external_notice_id, notice_type, title, organization_name, opening_at, current_version)
		VALUES ($1,$2,'procurement','차수변경 테스트공고','테스트기관',$3,1) RETURNING id`, sourceID, ext, past)
	// 차수별 원문/버전 — version 1 = ord 000, version 2 = ord 001 (current_version으로 전환).
	addVersion := func(ver int, ord string) {
		raw := `{"bidNtceNo":"` + ext + `","bidNtceOrd":"` + ord + `","srvceDivNm":"일반용역"}`
		rawID := must(`INSERT INTO raw_documents (source_id, external_notice_id, request_url, response_status, raw_content, content_hash, collector_version)
			VALUES ($1,$2,'test://opening',200,$3,$4,'test') RETURNING id`, sourceID, ext, raw, "h-"+ext+"-"+ord)
		must(`INSERT INTO notice_versions (notice_id, version_number, raw_document_id, change_type, is_current)
			VALUES ($1,$2,$3,'initial',$4) RETURNING id`, noticeID, ver, rawID, ver == 1)
	}
	addVersion(1, "000")
	defer func() {
		_, _ = db.ExecContext(ctx, `DELETE FROM notice_opening_results WHERE notice_id = $1`, noticeID)
		_, _ = db.ExecContext(ctx, `DELETE FROM notice_versions WHERE notice_id = $1`, noticeID)
		_, _ = db.ExecContext(ctx, `DELETE FROM raw_documents WHERE external_notice_id = $1`, ext)
		_, _ = db.ExecContext(ctx, `DELETE FROM notices WHERE id = $1`, noticeID)
	}()

	// 모의 API: 목록은 000·001 두 차수 행(둘 다 개찰완료), 낙찰현황은 000 차수만 확정.
	calls := 0
	failing := false
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		calls++
		if failing {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/getOpengResultListInfoServc"):
			_, _ = w.Write([]byte(`{"response":{"header":{"resultCode":"00","resultMsg":"정상"},"body":{"items":[
				{"bidNtceNo":"` + ext + `","bidNtceOrd":"000","bidClsfcNo":"0","rbidNo":"000","opengDt":"2026-08-10 11:00:00","prtcptCnum":"4","opengCorpInfo":"옛차수1위^1111111111^대표^100^90.1","progrsDivCdNm":"개찰완료","rsrvtnPrceFileExistnceYn":"N"},
				{"bidNtceNo":"` + ext + `","bidNtceOrd":"001","bidClsfcNo":"0","rbidNo":"000","opengDt":"2026-08-14 11:00:00","prtcptCnum":"6","opengCorpInfo":"새차수1위^2222222222^대표^200^88.8","progrsDivCdNm":"개찰완료","rsrvtnPrceFileExistnceYn":"N"}],"totalCount":2}}}`))
		case strings.HasSuffix(r.URL.Path, "/getScsbidListSttusServc"):
			_, _ = w.Write([]byte(`{"response":{"header":{"resultCode":"00","resultMsg":"정상"},"body":{"items":[
				{"bidNtceNo":"` + ext + `","bidNtceOrd":"000","rbidNo":"000","bidwinnrNm":"옛차수낙찰사","bidwinnrBizno":"1111111111","sucsfbidAmt":"100","sucsfbidRate":"90.1","rlOpengDt":"2026-08-10 11:00:00","fnlSucsfDate":"2026-08-10","prtcptCnum":"4"}],"totalCount":1}}}`))
		default:
			_, _ = w.Write([]byte(`{"response":{"header":{"resultCode":"03","resultMsg":"NODATA"},"body":{"items":"","totalCount":0}}}`))
		}
	})
	apiSrv := httptest.NewServer(mux)
	defer apiSrv.Close()
	src := &scsbid.Source{ServiceKey: "k", HTTPClient: &http.Client{}, RateLimit: common.NewRateLimiter(1000, 1000000), PageSize: 100, BaseURL: apiSrv.URL}
	s := &Server{db: db, logger: slog.Default(), scsbidSource: src}
	routes := http.NewServeMux()
	routes.HandleFunc("GET /api/notices/{id}/opening-result", s.handleGetNoticeOpeningResult)
	get := func() (int, map[string]any) {
		rec := httptest.NewRecorder()
		routes.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/notices/"+noticeID+"/opening-result", nil))
		var body map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		return rec.Code, body
	}
	cacheRow := func() (no, ord, status string, next time.Time) {
		if err := db.QueryRowContext(ctx, `SELECT bid_ntce_no, bid_ntce_ord, status, next_check_at FROM notice_opening_results WHERE notice_id = $1`, noticeID).Scan(&no, &ord, &status, &next); err != nil {
			t.Fatalf("cache row: %v", err)
		}
		return
	}

	// 준비: ord 000으로 첫 조회 → AWARDED 캐시(next_check_at +7일).
	code, body := get()
	if code != 200 || body["status"] != "AWARDED" || body["bidNtceOrd"] != "000" {
		t.Fatalf("seed fetch (ord 000): %d %+v", code, body)
	}
	if _, ord, st, next := cacheRow(); ord != "000" || st != "AWARDED" || !next.After(time.Now().Add(6*24*time.Hour)) {
		t.Fatalf("seed cache: ord=%s status=%s next=%v", ord, st, next)
	}

	// CASE A: current Ord=000, cache Ord=000, next_check_at 미래 → API 재호출 없이 캐시 반환.
	calls = 0
	code, body = get()
	if code != 200 || body["status"] != "AWARDED" || calls != 0 {
		t.Fatalf("CASE A: expected cache hit without API, got %d status=%v calls=%d", code, body["status"], calls)
	}

	// CASE B(핵심): 정정공고 → current Ord=001, cache Ord=000(AWARDED, 미래) → 과거 캐시 반환 금지, API 재조회, 001 결과 반환/저장.
	addVersion(2, "001")
	if _, err := db.ExecContext(ctx, `UPDATE notice_versions SET is_current = (version_number = 2) WHERE notice_id = $1`, noticeID); err != nil {
		t.Fatalf("switch version: %v", err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE notices SET current_version = 2 WHERE id = $1`, noticeID); err != nil {
		t.Fatalf("switch current_version: %v", err)
	}
	calls = 0
	code, body = get()
	if code != 200 {
		t.Fatalf("CASE B: code %d", code)
	}
	if calls == 0 {
		t.Fatalf("CASE B: API must be re-queried on ord change")
	}
	if body["bidNtceOrd"] != "001" || body["status"] != "OPENED_WAITING_AWARD" || body["winner"] != nil {
		t.Fatalf("CASE B: stale ord-000 award must not leak; got ord=%v status=%v winner=%v", body["bidNtceOrd"], body["status"], body["winner"])
	}
	if tb, _ := body["topBidder"].(map[string]any); tb == nil || tb["name"] != "새차수1위" {
		t.Fatalf("CASE B: top bidder must be from ord 001: %+v", body["topBidder"])
	}
	if no, ord, st, _ := cacheRow(); no != ext || ord != "001" || st != "OPENED_WAITING_AWARD" {
		t.Fatalf("CASE B: cache row must be overwritten with new identity: %s %s %s", no, ord, st)
	}
	// 재호출은 새 identity 캐시 적중(API 0콜).
	calls = 0
	if _, body = get(); body["bidNtceOrd"] != "001" || calls != 0 {
		t.Fatalf("CASE B follow-up: expected ord-001 cache hit, calls=%d", calls)
	}

	// CASE C: bidNtceNo 자체 mismatch(캐시 row의 공고번호가 다름) → 캐시 반환 금지, 재조회.
	if _, err := db.ExecContext(ctx, `UPDATE notice_opening_results SET bid_ntce_no = 'OTHER-NO', status = 'AWARDED', next_check_at = now() + interval '7 days' WHERE notice_id = $1`, noticeID); err != nil {
		t.Fatalf("simulate no mismatch: %v", err)
	}
	calls = 0
	code, body = get()
	if code != 200 || calls == 0 || body["bidNtceNo"] != ext || body["status"] != "OPENED_WAITING_AWARD" {
		t.Fatalf("CASE C: bidNtceNo mismatch must refetch; calls=%d body=%v/%v", calls, body["bidNtceNo"], body["status"])
	}
	if no, _, _, _ := cacheRow(); no != ext {
		t.Fatalf("CASE C: cache row must be rewritten with current bidNtceNo, got %s", no)
	}

	// CASE D: identity 일치 + expired → 정상 재조회.
	if _, err := db.ExecContext(ctx, `UPDATE notice_opening_results SET next_check_at = now() - interval '1 minute' WHERE notice_id = $1`, noticeID); err != nil {
		t.Fatalf("expire: %v", err)
	}
	calls = 0
	code, body = get()
	if code != 200 || calls == 0 || body["status"] != "OPENED_WAITING_AWARD" || body["stale"] == true {
		t.Fatalf("CASE D: expired cache must refetch (calls=%d stale=%v)", calls, body["stale"])
	}
	if _, _, _, next := cacheRow(); !next.After(time.Now()) {
		t.Fatalf("CASE D: next_check_at must be renewed")
	}

	// CASE E: identity 일치 + expired + API 실패 → 기존 stale fallback 유지(마지막 캐시 stale:true, 200).
	if _, err := db.ExecContext(ctx, `UPDATE notice_opening_results SET next_check_at = now() - interval '1 minute' WHERE notice_id = $1`, noticeID); err != nil {
		t.Fatalf("expire: %v", err)
	}
	failing = true
	code, body = get()
	if code != 200 || body["status"] != "OPENED_WAITING_AWARD" || body["stale"] != true || body["bidNtceOrd"] != "001" {
		t.Fatalf("CASE E: stale fallback expected: %d %+v", code, body)
	}
	// identity mismatch + API 실패 → 과거 캐시로도 폴백하지 않고 UNAVAILABLE(과거 차수 노출 금지).
	if _, err := db.ExecContext(ctx, `UPDATE notice_opening_results SET bid_ntce_ord = '000', status = 'AWARDED', next_check_at = now() + interval '7 days' WHERE notice_id = $1`, noticeID); err != nil {
		t.Fatalf("simulate stale ord: %v", err)
	}
	code, body = get()
	if code != 200 || body["status"] != "UNAVAILABLE" {
		t.Fatalf("mismatch + API failure must not fall back to old-ord cache: %d %+v", code, body)
	}
}
