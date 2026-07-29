// Command collector demonstrates the full pipeline end-to-end:
//   fake source API -> Collector -> Runner -> Store -> change detection
//
// In production, replace the httptest fake server with a real source base
// URL (e.g. from data_sources.base_url) and InMemoryStore with a Postgres
// implementation of store.Store — the runner and collector code do not change.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"time"

	"biz-platform/collector/internal/collector/runner"
	"biz-platform/collector/internal/collector/sources/sample"
	"biz-platform/collector/internal/collector/store"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	// version served by the fake API on the FIRST call for notice N0001
	deadline := "2026-08-10"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch {
		case req.URL.Path == "/notices":
			resp := map[string]any{
				"items": []map[string]string{
					{"noticeId": "N0001", "title": "시청 홈페이지 유지관리 용역"},
					{"noticeId": "N0002", "title": "공공기관 홍보영상 제작"},
				},
				"nextCursor": "",
			}
			json.NewEncoder(w).Encode(resp)
		case req.URL.Path == "/notices/N0001":
			json.NewEncoder(w).Encode(map[string]any{
				"noticeId": "N0001", "title": "시청 홈페이지 유지관리 용역",
				"organization": "OO시청", "region": "광주광역시", "industry": "소프트웨어개발",
				"publishedAt": "2026-07-01", "deadlineAt": deadline,
				"budgetAmount": 45000000, "status": "OPEN",
				"attachments": []map[string]any{
					{"filename": "제안요청서.pdf", "url": "/files/rfp.pdf", "fileType": "pdf", "size": 245000},
				},
			})
		case req.URL.Path == "/notices/N0002":
			json.NewEncoder(w).Encode(map[string]any{
				"noticeId": "N0002", "title": "공공기관 홍보영상 제작",
				"organization": "OO진흥원", "region": "전국", "industry": "영상제작",
				"publishedAt": "2026-07-05", "deadlineAt": "2026-07-25",
				"budgetAmount": 20000000, "status": "OPEN",
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	src := sample.New(server.URL)
	st := store.NewInMemoryStore()
	rn := runner.New(src, st, logger)

	fmt.Println("=== 1차 수집 (신규) ===")
	res1 := rn.RunIncremental(context.Background(), time.Time{})
	printResult(res1)

	// 정정공고 시뮬레이션: 마감일과 예산이 변경됨 (critical change)
	deadline = "2026-08-20"
	fmt.Println("\n=== 2차 수집 (정정공고 발생: 마감일 변경) ===")
	res2 := rn.RunIncremental(context.Background(), time.Time{})
	printResult(res2)

	fmt.Println("\n=== 감지된 변경 이력 ===")
	for _, c := range st.Changes() {
		fmt.Printf("  notice=%s field=%s %q -> %q importance=%s\n",
			c.NoticeID, c.Field, c.OldValue, c.NewValue, c.Importance)
	}
}

func printResult(r runner.JobResult) {
	fmt.Printf("status=%s processed=%d success=%d failed=%d duration=%s\n",
		r.Status, r.ProcessedCount, r.SuccessCount, r.FailedCount, r.FinishedAt.Sub(r.StartedAt))
	for _, e := range r.Errors {
		fmt.Println("  error:", e)
	}
}
