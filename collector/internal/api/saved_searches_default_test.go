package api

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"
)

// legacy 계정 기본 맞춤조건 정상화(A안): origin='onboarding'이 없는 계정에 회사정보로
// 기본 조건을 지연 생성하되, 기존 활성 조건과 업종이 겹치면 생성하지 않고 사용자 데이터를
// 절대 자동 변경하지 않는다. 멱등성도 확인.
func TestEnsureDefaultSavedSearch_Integration(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	srv := testServer(db)
	srv.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := context.Background()

	must := func(q string, args ...any) string {
		var id string
		if err := db.QueryRowContext(ctx, q, args...).Scan(&id); err != nil {
			t.Fatalf("seed [%.40s]: %v", q, err)
		}
		return id
	}
	ext := "SSDEF-" + time.Now().Format("150405.000000")
	userID := must(`INSERT INTO users (email) VALUES ($1) RETURNING id`, ext+"@t.local")
	defer db.ExecContext(ctx, `DELETE FROM saved_searches WHERE user_id=$1`, userID)
	defer db.ExecContext(ctx, `DELETE FROM users WHERE id=$1`, userID)

	count := func() (total, onboarding, active int) {
		db.QueryRowContext(ctx, `SELECT count(*), count(*) FILTER (WHERE origin='onboarding'), count(*) FILTER (WHERE is_active) FROM saved_searches WHERE user_id=$1`, userID).Scan(&total, &onboarding, &active)
		return
	}

	// 1) legacy(기본 없음) + 회사정보 지역/업종, 활성 충돌 없음 → 기본 생성(origin='onboarding', active).
	created, skipped, _, err := srv.ensureDefaultSavedSearch(ctx, userID, "광주광역시", "IT/SW")
	if err != nil {
		t.Fatalf("ensure(1): %v", err)
	}
	if !created || skipped {
		t.Fatalf("case1 created=%v skipped=%v want created", created, skipped)
	}
	if tot, ob, act := count(); tot != 1 || ob != 1 || act != 1 {
		t.Fatalf("case1 counts total=%d onboarding=%d active=%d want 1/1/1", tot, ob, act)
	}

	// 2) 멱등: 이미 기본 있으면 아무것도 안 함.
	created2, _, _, err := srv.ensureDefaultSavedSearch(ctx, userID, "광주광역시", "IT/SW")
	if err != nil || created2 {
		t.Fatalf("case2 idempotent created=%v err=%v", created2, err)
	}
	if tot, ob, _ := count(); tot != 1 || ob != 1 {
		t.Fatalf("case2 counts total=%d onboarding=%d want 1/1(중복 생성 금지)", tot, ob)
	}

	// 3) 충돌 케이스: 기본 없는 다른 계정 + 같은 업종의 활성 추가조건 → skip, 미생성, 데이터 무변경.
	userID2 := must(`INSERT INTO users (email) VALUES ($1) RETURNING id`, ext+"-2@t.local")
	defer db.ExecContext(ctx, `DELETE FROM saved_searches WHERE user_id=$1`, userID2)
	defer db.ExecContext(ctx, `DELETE FROM users WHERE id=$1`, userID2)
	addID := must(`INSERT INTO saved_searches (user_id, name, region, industry, alert_enabled, is_active) VALUES ($1,'건설 조건','서울','건설',true,true) RETURNING id`, userID2)

	created3, skipped3, conflict3, err := srv.ensureDefaultSavedSearch(ctx, userID2, "서울", "건설")
	if err != nil {
		t.Fatalf("ensure(3): %v", err)
	}
	if created3 || !skipped3 || conflict3 != "건설 조건" {
		t.Errorf("case3 created=%v skipped=%v conflict=%q want skip/conflict='건설 조건'", created3, skipped3, conflict3)
	}
	// 기본 미생성 + 기존 추가조건 그대로(무변경): origin='onboarding' 0개, 추가조건 active 유지.
	var ob int
	db.QueryRowContext(ctx, `SELECT count(*) FILTER (WHERE origin='onboarding') FROM saved_searches WHERE user_id=$1`, userID2).Scan(&ob)
	if ob != 0 {
		t.Errorf("case3 기본 생성됨(want 0): onboarding=%d", ob)
	}
	var stillActive bool
	db.QueryRowContext(ctx, `SELECT is_active FROM saved_searches WHERE id=$1`, addID).Scan(&stillActive)
	if !stillActive {
		t.Error("case3 기존 추가조건이 자동 비활성화됨(A안 위반 — 사용자 데이터 무변경이어야 함)")
	}
}
