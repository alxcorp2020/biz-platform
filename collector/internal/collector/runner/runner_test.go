package runner

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"biz-platform/collector/internal/collector/sources/sample"
	"biz-platform/collector/internal/collector/store"
)

func newFakeServer(t *testing.T, deadline string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/notices":
			json.NewEncoder(w).Encode(map[string]any{
				"items":      []map[string]string{{"noticeId": "N0001", "title": "테스트 용역"}},
				"nextCursor": "",
			})
		case "/notices/N0001":
			json.NewEncoder(w).Encode(map[string]any{
				"noticeId": "N0001", "title": "테스트 용역",
				"organization": "테스트기관", "region": "서울", "industry": "IT",
				"publishedAt": "2026-07-01", "deadlineAt": deadline,
				"budgetAmount": 10000000, "status": "OPEN",
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func TestRunner_DuplicateRunDoesNotCreateNewVersion(t *testing.T) {
	server := newFakeServer(t, "2026-08-01")
	defer server.Close()

	src := sample.New(server.URL)
	st := store.NewInMemoryStore()
	r := New(src, st, nil)

	res1 := r.RunIncremental(context.Background(), time.Time{})
	if res1.Status != StatusCompleted || res1.SuccessCount != 1 {
		t.Fatalf("first run failed: %+v", res1)
	}

	res2 := r.RunIncremental(context.Background(), time.Time{})
	if res2.Status != StatusCompleted || res2.SuccessCount != 1 {
		t.Fatalf("second run failed: %+v", res2)
	}

	if len(st.Changes()) != 0 {
		t.Fatalf("expected no changes recorded for identical re-collection, got %d", len(st.Changes()))
	}
}

func TestRunner_ChangedDeadlineCreatesVersionAndChangeRecord(t *testing.T) {
	deadline := "2026-08-01"
	server := newFakeServer(t, deadline)
	defer server.Close()
	// override handler dynamically via closure variable
	src := sample.New(server.URL)
	st := store.NewInMemoryStore()
	r := New(src, st, nil)

	r.RunIncremental(context.Background(), time.Time{})

	// spin up a second server representing the corrected notice (정정공고)
	server2 := newFakeServer(t, "2026-08-15")
	defer server2.Close()
	src2 := sample.New(server2.URL)
	r2 := New(src2, st, nil)
	res := r2.RunIncremental(context.Background(), time.Time{})

	if res.Status != StatusCompleted {
		t.Fatalf("expected completed status, got %+v", res)
	}
	changes := st.Changes()
	if len(changes) != 1 {
		t.Fatalf("expected exactly 1 change record, got %d: %+v", len(changes), changes)
	}
	if changes[0].Field != "application_end_at" || changes[0].Importance != "critical" {
		t.Fatalf("expected critical application_end_at change, got %+v", changes[0])
	}
}

func TestRunner_PermanentErrorDoesNotHangJob(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer server.Close()

	src := sample.New(server.URL)
	st := store.NewInMemoryStore()
	r := New(src, st, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	res := r.RunIncremental(ctx, time.Time{})
	if res.Status != StatusFailed {
		t.Fatalf("expected failed status for auth error, got %+v", res)
	}
}
