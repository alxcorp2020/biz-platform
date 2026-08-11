package g2b

import (
	"testing"
	"time"
)

// 나라장터 API 일시는 타임존 표기 없는 KST 벽시계다. 파싱 결과 instant가 서버 TZ와
// 무관하게 항상 KST(+9)로 해석되는지 — 특히 운영과 같은 UTC 서버 조건에서 — 검증한다.
// (예전 time.Local 파싱은 UTC 서버에서 +9시간 어긋났다.)
func TestParseG2BTime_KSTRegardlessOfServerTZ(t *testing.T) {
	orig := time.Local
	defer func() { time.Local = orig }()

	// 실측 공고 R26BK01672209(합강1초 가연성 폐기물 처리용역)의 KST 벽시계 → 기대 UTC instant.
	cases := []struct{ in, wantUTC string }{
		{"2026-08-10 09:09:57", "2026-08-10T00:09:57Z"}, // 공고 게시
		{"2026-08-13 18:00", "2026-08-13T09:00:00Z"},    // 입찰참가자격등록 마감(분까지만)
		{"2026-08-10 10:00:00", "2026-08-10T01:00:00Z"}, // 입찰서 제출 시작
		{"2026-08-14 10:00:00", "2026-08-14T01:00:00Z"}, // 입찰서 제출 마감
		{"2026-08-14 11:00:00", "2026-08-14T02:00:00Z"}, // 개찰
		{"2026-08-14 00:30:00", "2026-08-13T15:30:00Z"}, // 자정 근처(KST 00:30 = 전날 UTC 15:30)
		{"2026-08-14 23:30:00", "2026-08-14T14:30:00Z"}, // 23:30
	}
	// 서버 TZ를 UTC / KST / 임의(PST)로 바꿔도 결과가 동일해야 한다.
	for _, serverTZ := range []*time.Location{time.UTC, time.FixedZone("KST", 9*3600), time.FixedZone("PST", -8*3600)} {
		time.Local = serverTZ
		for _, c := range cases {
			got, err := parseG2BTime(c.in)
			if err != nil {
				t.Fatalf("serverTZ=%s parse %q: %v", serverTZ, c.in, err)
			}
			if s := got.UTC().Format(time.RFC3339); s != c.wantUTC {
				t.Errorf("serverTZ=%s parseG2BTime(%q).UTC()=%s want %s", serverTZ, c.in, s, c.wantUTC)
			}
		}
	}

	// 빈값/오형식은 에러.
	time.Local = time.UTC
	if _, err := parseG2BTime(""); err == nil {
		t.Error("empty timestamp should error")
	}
	if _, err := parseG2BTime("not-a-time"); err == nil {
		t.Error("malformed timestamp should error")
	}
}
