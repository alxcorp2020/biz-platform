package main

import "testing"

// TestPositiveIntEnv — 운영 첫 수집 상한(BIZINFO_PAGE_SIZE/MAX_PAGES) 파싱.
// 미설정/비정상/0이하는 0(=기본값 유지), 양의 정수만 그 값을 반환한다.
func TestPositiveIntEnv(t *testing.T) {
	const k = "BIZINFO_TEST_CAP_ENV"
	cases := []struct {
		set  bool
		val  string
		want int
	}{
		{false, "", 0},       // 미설정 → 0(기본 유지)
		{true, "", 0},        // 빈값 → 0
		{true, "  ", 0},      // 공백 → 0
		{true, "20", 20},     // 정상
		{true, " 100 ", 100}, // 공백 trim
		{true, "0", 0},       // 0 → 0
		{true, "-5", 0},      // 음수 → 0
		{true, "abc", 0},     // 비숫자 → 0
	}
	for _, c := range cases {
		if c.set {
			t.Setenv(k, c.val)
		} else {
			t.Setenv(k, "")
		}
		if got := positiveIntEnv(k); got != c.want {
			t.Errorf("positiveIntEnv(set=%v,val=%q)=%d, want %d", c.set, c.val, got, c.want)
		}
	}
}
