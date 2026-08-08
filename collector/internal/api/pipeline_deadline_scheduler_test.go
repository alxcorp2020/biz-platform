// pipeline_deadline_scheduler_test.go — Phase B+ 시간단위 마감 자동화 통합 테스트.
//
// 실제 Postgres에 최소 픽스처를 심고 runDeadlineScheduleAt에 시각을 주입해
// (spec 10) 검증한다. DB가 필요하므로 BIZ_TEST_DSN이 설정된 경우에만 돈다.
//   BIZ_TEST_DSN='postgres://localhost/biz_platform?sslmode=disable' go test ./internal/api -run Deadline -v
package api

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	_ "github.com/lib/pq"
)

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("BIZ_TEST_DSN")
	if dsn == "" {
		t.Skip("BIZ_TEST_DSN 미설정 — DB 통합 테스트 건너뜀")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := db.Ping(); err != nil {
		t.Fatalf("ping db: %v", err)
	}
	return db
}

// seedEntry는 user/company_profile/notice/pipeline entry(준비중)를 한 벌 만들고
// 정리 함수를 반환한다. subDT/qualDT는 nil이면 해당 컬럼을 NULL로 둔다.
type seedResult struct {
	profileID, noticeID, entryID string
}

func seedDeadlineFixture(t *testing.T, db *sql.DB, createdAt time.Time, subDT, qualDT *time.Time) (seedResult, func()) {
	t.Helper()
	ctx := context.Background()
	var sourceID string
	if err := db.QueryRowContext(ctx, `SELECT id FROM data_sources LIMIT 1`).Scan(&sourceID); err != nil {
		t.Fatalf("data_sources 조회 실패: %v", err)
	}
	var userID, profileID, noticeID, entryID string
	must := func(q string, args ...any) string {
		var id string
		if err := db.QueryRowContext(ctx, q, args...).Scan(&id); err != nil {
			t.Fatalf("seed 실패 [%.40s]: %v", q, err)
		}
		return id
	}
	ext := "TEST-" + time.Now().Format("150405.000000")
	userID = must(`INSERT INTO users (email) VALUES ($1) RETURNING id`, ext+"@test.local")
	profileID = must(`INSERT INTO company_profiles (user_id, company_name) VALUES ($1,$2) RETURNING id`, userID, "테스트회사")
	noticeID = must(`INSERT INTO notices (source_id, external_notice_id, notice_type, title, organization_name,
		application_end_datetime, qualification_deadline_at)
		VALUES ($1,$2,'procurement','테스트공고','테스트기관',$3,$4) RETURNING id`, sourceID, ext, subDT, qualDT)
	entryID = must(`INSERT INTO notice_pipeline_entries (company_profile_id, notice_id, status, created_at)
		VALUES ($1,$2,'준비중',$3) RETURNING id`, profileID, noticeID, createdAt)

	cleanup := func() {
		// 자식(gate/in_app/checklist)은 FK ON DELETE CASCADE 또는 수동 삭제.
		db.ExecContext(ctx, `DELETE FROM pipeline_deadline_events WHERE pipeline_entry_id=$1`, entryID)
		db.ExecContext(ctx, `DELETE FROM in_app_notifications WHERE pipeline_entry_id=$1`, entryID)
		db.ExecContext(ctx, `DELETE FROM notification_log WHERE pipeline_entry_id=$1`, entryID)
		db.ExecContext(ctx, `DELETE FROM pipeline_checklist_items WHERE pipeline_entry_id=$1`, entryID)
		db.ExecContext(ctx, `DELETE FROM pipeline_status_history WHERE pipeline_entry_id=$1`, entryID)
		db.ExecContext(ctx, `DELETE FROM notice_pipeline_entries WHERE id=$1`, entryID)
		db.ExecContext(ctx, `DELETE FROM notices WHERE id=$1`, noticeID)
		db.ExecContext(ctx, `DELETE FROM company_profiles WHERE id=$1`, profileID)
		db.ExecContext(ctx, `DELETE FROM users WHERE id=$1`, userID)
	}
	return seedResult{profileID, noticeID, entryID}, cleanup
}

func testServer(db *sql.DB) *Server { return &Server{db: db} }

// firedEvents는 이 엔트리에 대해 게이트에 기록된 event_type 목록을 센다.
func firedEvents(t *testing.T, db *sql.DB, entryID string) map[string]int {
	t.Helper()
	rows, err := db.QueryContext(context.Background(),
		`SELECT event_type, count(*) FROM pipeline_deadline_events WHERE pipeline_entry_id=$1 GROUP BY 1`, entryID)
	if err != nil {
		t.Fatalf("firedEvents 조회 실패: %v", err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var et string
		var n int
		rows.Scan(&et, &n)
		out[et] = n
	}
	return out
}

// TestDeadlineSchedule_SubmissionLadder — 마감 8/13 19:00, 참여 8/1(넉넉).
// 각 시점에 정확히 해당 이벤트 1회씩 발송되는지, 여러 틱에도 중복이 없는지.
func TestDeadlineSchedule_SubmissionLadder(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	srv := testServer(db)
	ctx := context.Background()

	deadline := time.Date(2026, 8, 13, 19, 0, 0, 0, kstLocation())
	created := time.Date(2026, 8, 1, 9, 0, 0, 0, kstLocation())
	seed, cleanup := seedDeadlineFixture(t, db, created, &deadline, nil)
	defer cleanup()

	// 각 이벤트가 막 도래한 시점(+1분)에 틱을 돌린다.
	steps := []struct {
		at   time.Time
		want string
	}{
		{deadline.Add(-7*24*time.Hour + time.Minute), evtSubmissionD7},
		{deadline.Add(-3*24*time.Hour + time.Minute), evtSubmissionD3},
		{deadline.Add(-1*24*time.Hour + time.Minute), evtSubmissionD1},
		{deadline.Add(-6*time.Hour + time.Minute), evtSubmissionH6},
		{deadline.Add(-2*time.Hour + time.Minute), evtSubmissionH2},
	}
	for _, s := range steps {
		if _, err := srv.runDeadlineScheduleAt(ctx, s.at); err != nil {
			t.Fatalf("스케줄러 실행 실패(%v): %v", s.at, err)
		}
		// 같은 시점 두 번째 틱 — 중복발송 없어야 함.
		if _, err := srv.runDeadlineScheduleAt(ctx, s.at); err != nil {
			t.Fatalf("스케줄러 재실행 실패: %v", err)
		}
	}
	got := firedEvents(t, db, seed.entryID)
	for _, want := range []string{evtSubmissionD7, evtSubmissionD3, evtSubmissionD1, evtSubmissionH6, evtSubmissionH2} {
		if got[want] != 1 {
			t.Errorf("이벤트 %s: 발송횟수 %d, 기대 1", want, got[want])
		}
	}
}

// TestDeadlineSchedule_NoRetroactive — 참여 시점이 D-1이면 D-7/D-3는 소급발송
// 안 되고, H-5에 참여하면 H-6은 안 나가야 한다.
func TestDeadlineSchedule_NoRetroactive(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	srv := testServer(db)
	ctx := context.Background()

	deadline := time.Date(2026, 8, 13, 19, 0, 0, 0, kstLocation())
	created := deadline.Add(-1 * 24 * time.Hour) // D-1에 참여
	seed, cleanup := seedDeadlineFixture(t, db, created, &deadline, nil)
	defer cleanup()

	// 참여 직후(now = created+1분)에 틱.
	if _, err := srv.runDeadlineScheduleAt(ctx, created.Add(time.Minute)); err != nil {
		t.Fatalf("스케줄러 실행 실패: %v", err)
	}
	got := firedEvents(t, db, seed.entryID)
	if got[evtSubmissionD7] != 0 || got[evtSubmissionD3] != 0 {
		t.Errorf("소급발송 방지 실패: D7=%d D3=%d (기대 0/0)", got[evtSubmissionD7], got[evtSubmissionD3])
	}
	if got[evtSubmissionD1] != 1 {
		t.Errorf("D-1은 발송돼야 함: %d", got[evtSubmissionD1])
	}
}

// TestDeadlineSchedule_StatusStops — 준비중에서 벗어나면(제출완료) 이후 이벤트가
// 나가지 않는다.
func TestDeadlineSchedule_StatusStops(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	srv := testServer(db)
	ctx := context.Background()

	deadline := time.Date(2026, 8, 13, 19, 0, 0, 0, kstLocation())
	created := time.Date(2026, 8, 1, 9, 0, 0, 0, kstLocation())
	seed, cleanup := seedDeadlineFixture(t, db, created, &deadline, nil)
	defer cleanup()

	// D-7 발송.
	srv.runDeadlineScheduleAt(ctx, deadline.Add(-7*24*time.Hour+time.Minute))
	// 제출완료로 전환.
	if _, err := db.ExecContext(ctx, `UPDATE notice_pipeline_entries SET status='제출완료' WHERE id=$1`, seed.entryID); err != nil {
		t.Fatalf("상태 전환 실패: %v", err)
	}
	// D-3 시점 틱 — 더 이상 발송되면 안 됨.
	srv.runDeadlineScheduleAt(ctx, deadline.Add(-3*24*time.Hour+time.Minute))
	got := firedEvents(t, db, seed.entryID)
	if got[evtSubmissionD3] != 0 {
		t.Errorf("제출완료 후 D-3가 발송됨: %d (기대 0)", got[evtSubmissionD3])
	}
}

// TestDeadlineSchedule_DateChange — 마감이 8/13→8/18로 변경되면, 새 날짜 기준의
// "미래" 이벤트만 새로 발송되고 옛 발송은 재발송되지 않는다.
func TestDeadlineSchedule_DateChange(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	srv := testServer(db)
	ctx := context.Background()

	oldDeadline := time.Date(2026, 8, 13, 19, 0, 0, 0, kstLocation())
	newDeadline := time.Date(2026, 8, 18, 19, 0, 0, 0, kstLocation())
	created := time.Date(2026, 8, 1, 9, 0, 0, 0, kstLocation())
	seed, cleanup := seedDeadlineFixture(t, db, created, &oldDeadline, nil)
	defer cleanup()

	// 8/6 19:01 — 옛 마감 기준 D-7 발송(+ 최초 스냅샷 기록).
	tick1 := oldDeadline.Add(-7*24*time.Hour + time.Minute)
	srv.runDeadlineScheduleAt(ctx, tick1)
	before := firedEvents(t, db, seed.entryID)
	if before[evtSubmissionD7] != 1 {
		t.Fatalf("옛 D-7 발송 실패: %d", before[evtSubmissionD7])
	}

	// 마감일 변경.
	if _, err := db.ExecContext(ctx, `UPDATE notices SET application_end_datetime=$2 WHERE id=$1`, seed.noticeID, newDeadline); err != nil {
		t.Fatalf("마감 변경 실패: %v", err)
	}
	// 변경 직후 틱(tick1 + 1시간) — 정정 감지 + seen_at 갱신. 새 D-7(8/11)은
	// tick 시점(8/6)보다 미래라 아직 발송 안 됨.
	srv.runDeadlineScheduleAt(ctx, tick1.Add(time.Hour))
	// "마감 변경" 인앱 알림이 생겼는지 확인.
	var changeCount int
	db.QueryRowContext(ctx, `SELECT count(*) FROM in_app_notifications WHERE pipeline_entry_id=$1 AND event_type=$2`,
		seed.entryID, evtSubmissionDeadlineChanged).Scan(&changeCount)
	if changeCount != 1 {
		t.Errorf("마감 변경 알림 수 %d (기대 1)", changeCount)
	}
	// 새 마감 기준 D-7 시점(8/11 19:01) 틱 — 새 deadline_at 키로 발송돼야 함.
	srv.runDeadlineScheduleAt(ctx, newDeadline.Add(-7*24*time.Hour+time.Minute))
	after := firedEvents(t, db, seed.entryID)
	if after[evtSubmissionD7] != 2 {
		t.Errorf("새 마감 D-7 재발송 실패: 총 %d (기대 2 = 옛1+새1)", after[evtSubmissionD7])
	}
}

// TestDeadlineSchedule_QualificationAndFallback — 자격마감(시각)만 있는 경우
// 자격 이벤트가 나가고, 제출마감이 날짜 폴백(시각 미상)이면 H-6/H-2는 안 나간다.
func TestDeadlineSchedule_Qualification(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	srv := testServer(db)
	ctx := context.Background()

	qual := time.Date(2026, 8, 13, 3, 0, 0, 0, kstLocation())
	created := time.Date(2026, 8, 1, 9, 0, 0, 0, kstLocation())
	seed, cleanup := seedDeadlineFixture(t, db, created, nil, &qual)
	defer cleanup()

	srv.runDeadlineScheduleAt(ctx, qual.Add(-3*24*time.Hour+time.Minute))
	srv.runDeadlineScheduleAt(ctx, qual.Add(-1*24*time.Hour+time.Minute))
	srv.runDeadlineScheduleAt(ctx, qual.Add(-6*time.Hour+time.Minute))
	got := firedEvents(t, db, seed.entryID)
	for _, want := range []string{evtQualificationD3, evtQualificationD1, evtQualificationH6} {
		if got[want] != 1 {
			t.Errorf("자격 이벤트 %s: %d (기대 1)", want, got[want])
		}
	}
}
