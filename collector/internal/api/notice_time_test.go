package api

import (
	"testing"
	"time"
)

// DTO(공고 상세)와 낙찰이력의 나라장터 일시 파서가 서버 TZ와 무관하게 항상 KST(+9)로
// 해석되는지 — 운영과 같은 UTC 조건에서 — 검증한다. parseG2BDateTime은 raw_content를
// 요청마다 재파싱하므로 이 수정만으로 기존 공고 상세 시각이 즉시 정상화된다.
func TestParseG2BDateTime_KSTRegardlessOfServerTZ(t *testing.T) {
	orig := time.Local
	defer func() { time.Local = orig }()

	cases := []struct{ in, wantUTC string }{
		{"2026-08-14 10:00:00", "2026-08-14T01:00:00Z"}, // 제출마감 10:00 KST
		{"2026-08-13 18:00", "2026-08-13T09:00:00Z"},    // 자격등록마감 18:00 KST (분까지)
		{"2026-08-14 11:00:00", "2026-08-14T02:00:00Z"}, // 개찰 11:00 KST
		{"2026-08-10 09:09:57", "2026-08-10T00:09:57Z"}, // 게시
	}
	for _, serverTZ := range []*time.Location{time.UTC, time.FixedZone("KST", 9*3600)} {
		time.Local = serverTZ
		for _, c := range cases {
			got := parseG2BDateTime(c.in)
			if got == nil {
				t.Fatalf("serverTZ=%s parseG2BDateTime(%q) = nil", serverTZ, c.in)
			}
			if s := got.UTC().Format(time.RFC3339); s != c.wantUTC {
				t.Errorf("serverTZ=%s parseG2BDateTime(%q).UTC()=%s want %s", serverTZ, c.in, s, c.wantUTC)
			}
		}
	}

	// 빈값/오형식 → nil.
	time.Local = time.UTC
	if parseG2BDateTime("") != nil {
		t.Error("empty should be nil")
	}
	if parseG2BDateTime("garbage") != nil {
		t.Error("malformed should be nil")
	}

	// 낙찰이력(ScsbidInfoService)도 동일 KST 고정.
	ts, err := parseScsbidTime("2026-08-14 11:00:00")
	if err != nil {
		t.Fatalf("parseScsbidTime: %v", err)
	}
	if s := ts.UTC().Format(time.RFC3339); s != "2026-08-14T02:00:00Z" {
		t.Errorf("parseScsbidTime.UTC()=%s want 2026-08-14T02:00:00Z", s)
	}
	if _, err := parseScsbidTime(""); err == nil {
		t.Error("empty scsbid time should error")
	}
}
