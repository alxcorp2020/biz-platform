package g2b

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"biz-platform/collector/internal/collector/common"
)

func newTestEnrichClient(url string) *EnrichmentClient {
	return &EnrichmentClient{
		ServiceKey: "testkey",
		HTTPClient: &http.Client{},
		RateLimit:  common.NewRateLimiter(1000, 1000000),
		BaseURL:    url,
	}
}

// 실측 응답 형태(참가가능지역 4건, 면허 2건 + NODATA)를 모의 서버로 재현해 파싱을 검증한다.
func TestEnrichmentClient_ParsesOfficialResponses(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/getBidPblancListInfoPrtcptPsblRgn", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("bidNtceNo") != "R26BK01672209" {
			t.Errorf("missing bidNtceNo: %q", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"response":{"header":{"resultCode":"00","resultMsg":"정상"},"body":{"totalCount":4,"items":[
			{"prtcptPsblRgnNm":"대전광역시","bsnsDivNm":"용역","lmtSno":"1"},
			{"prtcptPsblRgnNm":"세종특별자치시","bsnsDivNm":"용역","lmtSno":"2"},
			{"prtcptPsblRgnNm":"충청북도","bsnsDivNm":"용역","lmtSno":"3"},
			{"prtcptPsblRgnNm":"","bsnsDivNm":"용역","lmtSno":"4"}
		]}}}`))
	})
	mux.HandleFunc("/getBidPblancListInfoLicenseLimit", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"response":{"header":{"resultCode":"00","resultMsg":"정상"},"body":{"totalCount":2,"items":[
			{"lcnsLmtNm":"폐기물종합처분업/1143","permsnIndstrytyList":"","indstrytyMfrcFldList":"","lmtGrpNo":"2","bsnsDivNm":"용역","lmtSno":"3"},
			{"lcnsLmtNm":"건설폐기물 수집·운반업/6728","lmtGrpNo":"1","bsnsDivNm":"용역","lmtSno":"5"}
		]}}}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := newTestEnrichClient(srv.URL)

	regions, err := c.FetchParticipationRegions(context.Background(), "R26BK01672209", "000")
	if err != nil {
		t.Fatalf("FetchParticipationRegions: %v", err)
	}
	// 빈 지역명은 걸러져 3건.
	if len(regions) != 3 || regions[0].RegionName != "대전광역시" || regions[0].SortNo != 1 {
		t.Errorf("regions parsed wrong: %+v", regions)
	}

	lics, err := c.FetchLicenseLimits(context.Background(), "R26BK01672209", "000")
	if err != nil {
		t.Fatalf("FetchLicenseLimits: %v", err)
	}
	if len(lics) != 2 || !strings.HasPrefix(lics[0].LicenseName, "폐기물종합처분업") || lics[0].LimitGroupNo != "2" {
		t.Errorf("licenses parsed wrong: %+v", lics)
	}
}

// resultCode 03(NODATA) = 제한 없음 → 빈 슬라이스(정상).
func TestEnrichmentClient_NoData(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"response":{"header":{"resultCode":"03","resultMsg":"NODATA_ERROR"},"body":{"totalCount":0,"items":""}}}`))
	}))
	defer srv.Close()
	c := newTestEnrichClient(srv.URL)
	regions, err := c.FetchParticipationRegions(context.Background(), "X", "000")
	if err != nil || len(regions) != 0 {
		t.Errorf("NODATA should be empty+nil err: %v %v", regions, err)
	}
}
