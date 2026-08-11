package api

import (
	"testing"
)

// TestEnrichmentBatchSizeEnvOverride — 백필 가속 노브(NOTICE_ENRICHMENT_BATCH_SIZE)가
// 미설정/이상값이면 기본 20, 유효값이면 그 값을 쓰는지 검증한다.
func TestEnrichmentBatchSizeEnvOverride(t *testing.T) {
	cases := []struct {
		name string
		set  bool
		val  string
		want int
	}{
		{"미설정→기본", false, "", defaultEnrichmentBatchSize},
		{"유효값 100", true, "100", 100},
		{"유효값 1", true, "1", 1},
		{"0은 무시→기본", true, "0", defaultEnrichmentBatchSize},
		{"음수 무시→기본", true, "-5", defaultEnrichmentBatchSize},
		{"비정수 무시→기본", true, "abc", defaultEnrichmentBatchSize},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.set {
				t.Setenv("NOTICE_ENRICHMENT_BATCH_SIZE", c.val)
			} else {
				t.Setenv("NOTICE_ENRICHMENT_BATCH_SIZE", "")
			}
			if got := enrichmentBatchSize(); got != c.want {
				t.Errorf("enrichmentBatchSize()=%d, want %d", got, c.want)
			}
		})
	}
}
