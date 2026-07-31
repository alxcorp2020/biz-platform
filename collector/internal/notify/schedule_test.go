package notify

import (
	"testing"
	"time"
)

func TestNextDailyRun(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Seoul")
	if err != nil {
		t.Fatalf("load location: %v", err)
	}

	cases := []struct {
		name string
		now  time.Time
		want time.Time
	}{
		{
			name: "before target time today runs today",
			now:  time.Date(2026, 7, 31, 8, 0, 0, 0, loc),
			want: time.Date(2026, 7, 31, 9, 0, 0, 0, loc),
		},
		{
			name: "after target time today rolls to tomorrow",
			now:  time.Date(2026, 7, 31, 9, 30, 0, 0, loc),
			want: time.Date(2026, 8, 1, 9, 0, 0, 0, loc),
		},
		{
			name: "exact target time rolls to tomorrow (not re-fired same instant)",
			now:  time.Date(2026, 7, 31, 9, 0, 0, 0, loc),
			want: time.Date(2026, 8, 1, 9, 0, 0, 0, loc),
		},
		{
			name: "input in a different timezone is normalized",
			now:  time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC), // 09:00 KST
			want: time.Date(2026, 8, 1, 9, 0, 0, 0, loc),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := NextDailyRun(tc.now, loc, 9, 0)
			if !got.Equal(tc.want) {
				t.Errorf("NextDailyRun(%v) = %v, want %v", tc.now, got, tc.want)
			}
		})
	}
}
