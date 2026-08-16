package scsbid

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"biz-platform/collector/internal/collector/common"
)

func newTestSource(url string) *Source {
	return &Source{
		ServiceKey: "testkey",
		HTTPClient: &http.Client{},
		RateLimit:  common.NewRateLimiter(1000, 1000000),
		PageSize:   100,
		BaseURL:    url,
	}
}

func envelope(items string, total int) string {
	return `{"response":{"header":{"resultCode":"00","resultMsg":"정상"},"body":{"items":` + items + `,"numOfRows":999,"pageNo":1,"totalCount":` + itoa(total) + `}}}`
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	s := ""
	for n > 0 {
		s = string(rune('0'+n%10)) + s
		n /= 10
	}
	return s
}

// 실응답 축약 fixture(2026-08-16 실호출 값 기반, 필드명 그대로).
const (
	fxServcList  = `[{"bidNtceNo":"R26BK01674070","bidNtceOrd":"000","bidClsfcNo":"0","rbidNo":"000","bidNtceNm":"대곡고 폐기물처리 용역","opengDt":"2026-08-14 15:00:00","prtcptCnum":"25","opengCorpInfo":"주식회사 그린환경산업^6158193504^김성원^14433930^90.069","progrsDivCdNm":"개찰완료","inptDt":"2026-08-14 15:03:02","rsrvtnPrceFileExistnceYn":"Y","ntceInsttCd":"9010000","ntceInsttNm":"경상남도교육청","dminsttCd":"9010000","dminsttNm":"경상남도교육청","opengRsltNtcCntnts":""}]`
	fxCnstwkList = `[{"bidNtceNo":"R26BK01667341","bidNtceOrd":"000","bidClsfcNo":"0","rbidNo":"000","bidNtceNm":"자동채수기 구조물설치 공사","opengDt":"2026-08-12 11:00:00","prtcptCnum":"0","opengCorpInfo":"","progrsDivCdNm":"재입찰","inptDt":"2026-08-12 11:05:00","rsrvtnPrceFileExistnceYn":"N","ntceInsttNm":"춘천시","dminsttNm":"춘천시","opengRsltNtcCntnts":""},{"bidNtceNo":"R26BK01667341","bidNtceOrd":"000","bidClsfcNo":"0","rbidNo":"001","bidNtceNm":"자동채수기 구조물설치 공사","opengDt":"2026-08-12 16:00:00","prtcptCnum":"0","opengCorpInfo":"","progrsDivCdNm":"유찰","inptDt":"2026-08-12 16:05:00","rsrvtnPrceFileExistnceYn":"N","ntceInsttNm":"춘천시","dminsttNm":"춘천시","opengRsltNtcCntnts":""}]`
	// 물품은 rbidNo 대신 rbidNtceNo로 온다(명세 차이).
	fxThngList = `[{"bidNtceNo":"R26BK01660544","bidNtceOrd":"000","bidClsfcNo":"1","rbidNtceNo":"000","bidNtceNm":"원자방출분광기 구매","opengDt":"2026-08-12 13:00:00","prtcptCnum":"1","opengCorpInfo":"베이스테크 주식회사^3258701326^이명석^120350000^99.462","progrsDivCdNm":"개찰완료","inptDt":"2026-08-12 13:30:38","rsrvtnPrceFileExistnceYn":"Y","ntceInsttNm":"조달청 부산지방조달청","dminsttNm":"(재)한국원자력환경복원연구원","opengRsltNtcCntnts":""}]`
	fxBidders  = `[{"opengRsltDivNm":"개찰완료","bidNtceNo":"R26BK01674070","bidNtceOrd":"000","bidClsfcNo":"0","rbidNo":"000","opengRank":"1","prcbdrBizno":"6158193504","prcbdrNm":"주식회사 그린환경산업","prcbdrCeoNm":"김성원","bidprcAmt":"14433930","bidprcrt":"90.069","rmrk":"정상","cnsttyAccotBidAmtUrl":"","drwtNo1":" 12","drwtNo2":" 11","bidprcDt":"2026-08-13 15:42:11","bidPrceEvlVal":"","techEvlVal":"","totalEvlAmtVal":"","techEvlNaturVal":""},{"opengRsltDivNm":"개찰완료","bidNtceNo":"R26BK01674070","bidNtceOrd":"000","bidClsfcNo":"0","rbidNo":"000","opengRank":"","prcbdrBizno":"1234567890","prcbdrNm":"(주)하나전설","prcbdrCeoNm":"홍길동","bidprcAmt":"6860833500","bidprcrt":"88.274","rmrk":"낙찰하한선 미달","cnsttyAccotBidAmtUrl":"","drwtNo1":"","drwtNo2":"","bidprcDt":"2026-08-13 08:00:00","bidPrceEvlVal":"44.96","techEvlVal":"","totalEvlAmtVal":"","techEvlNaturVal":""}]`
	fxPrices   = `[{"bidNtceNo":"R26BK01674070","bidNtceOrd":"000","bidClsfcNo":"0","rbidNo":"000","bidNtceNm":"대곡고 폐기물처리 용역","plnprc":"16025300","bssamt":"16119000","totRsrvtnPrceNum":"15","compnoRsrvtnPrceSno":"1","bsisPlnprc":"16400800","drwtYn":"N","drwtNum":"3","bidwinrSlctnAplBssCntnts":"행자부","rlOpengDt":"2026-08-14 15:03:02","bssamtBssUpNum":"7","compnoRsrvtnPrceMkngDt":"2026-08-14 15:02:00","inptDt":"2026-08-14 15:03:02","PrearngPrcePurcnstcst":""},{"bidNtceNo":"R26BK01674070","bidNtceOrd":"000","bidClsfcNo":"0","rbidNo":"000","bidNtceNm":"대곡고 폐기물처리 용역","plnprc":"16025300","bssamt":"16119000","totRsrvtnPrceNum":"15","compnoRsrvtnPrceSno":"2","bsisPlnprc":"15860300","drwtYn":"Y","drwtNum":"6","bidwinrSlctnAplBssCntnts":"행자부","rlOpengDt":"2026-08-14 15:03:02","bssamtBssUpNum":"7","compnoRsrvtnPrceMkngDt":"2026-08-14 15:02:00","inptDt":"2026-08-14 15:03:02","PrearngPrcePurcnstcst":""}]`
	fxAward    = `[{"bidNtceNo":"R26BK01683359","bidNtceOrd":"000","bidClsfcNo":"0","rbidNo":"000","ntceDivCd":"통050001","bidNtceNm":"테스트 용역","prtcptCnum":"3","bidwinnrNm":"주식회사 동양기술단","bidwinnrBizno":"1234567890","bidwinnrCeoNm":"홍길동","bidwinnrAdrs":"서울","bidwinnrTelNo":"***********","sucsfbidAmt":"18879000","sucsfbidRate":"91.048","rlOpengDt":"2026-08-14 16:00:00","dminsttCd":"1","dminsttNm":"기관","rgstDt":"2026-08-14 16:10:00","fnlSucsfDate":"2026-08-14","fnlSucsfCorpOfcl":""}]`
	fxRebid    = `[{"opengRsltDivNm":"재입찰","bidNtceNo":"R26BK01664554","bidNtceOrd":"000","bidClsfcNo":"0","rbidNo":"001","bidClseDt":"2026-08-12 14:30:00","opengDt":"2026-08-12 15:30:00","rbidRsn":"견적제출 업체 모두 예가초과로 인한 재입찰 시행","cmmnSpldmdAgrmntClseDt":""}]`
	fxFailing  = `[{"opengRsltDivNm":"유찰","bidNtceNo":"R26BK01675192","bidNtceOrd":"000","bidClsfcNo":"0","rbidNo":"000","nobidRsn":"단독응찰에 따른 유찰"}]`
	fxGwError  = `{"nkoneps.com.response.ResponseError": {"header": {"resultCode": "07","resultMsg": "입력범위값 초과 에러"}}}`
	fxNoData   = `{"response":{"header":{"resultCode":"03","resultMsg":"NODATA_ERROR"},"body":{"items":"","numOfRows":5,"pageNo":1,"totalCount":0}}}`
)

func fixtureServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	// 업무유형별 목록: 각 오퍼레이션이 inqryDiv=4를 쓰는지 검증.
	list := func(fx string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("inqryDiv") != "4" || r.URL.Query().Get("bidNtceNo") == "" {
				t.Errorf("list op wrong params: %s", r.URL.RawQuery)
			}
			_, _ = w.Write([]byte(envelope(fx, 1)))
		}
	}
	mux.HandleFunc("/getOpengResultListInfoServc", list(fxServcList))
	mux.HandleFunc("/getOpengResultListInfoCnstwk", list(fxCnstwkList))
	mux.HandleFunc("/getOpengResultListInfoThng", list(fxThngList))
	mux.HandleFunc("/getOpengResultListInfoOpengCompt", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Has("inqryDiv") {
			t.Errorf("OpengCompt must not send inqryDiv: %s", r.URL.RawQuery)
		}
		if r.URL.Query().Get("bidNtceOrd") != "000" {
			t.Errorf("OpengCompt must forward bidNtceOrd: %s", r.URL.RawQuery)
		}
		_, _ = w.Write([]byte(envelope(fxBidders, 2)))
	})
	prices := func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("inqryDiv") != "2" {
			t.Errorf("PreparPcDetail must use inqryDiv=2: %s", r.URL.RawQuery)
		}
		_, _ = w.Write([]byte(envelope(fxPrices, 2)))
	}
	mux.HandleFunc("/getOpengResultListInfoServcPreparPcDetail", prices)
	mux.HandleFunc("/getOpengResultListInfoCnstwkPreparPcDetail", prices)
	mux.HandleFunc("/getOpengResultListInfoThngPreparPcDetail", prices)
	award := func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("inqryDiv") != "4" {
			t.Errorf("ScsbidListSttus by notice must use inqryDiv=4: %s", r.URL.RawQuery)
		}
		_, _ = w.Write([]byte(envelope(fxAward, 1)))
	}
	mux.HandleFunc("/getScsbidListSttusServc", award)
	mux.HandleFunc("/getScsbidListSttusCnstwk", award)
	mux.HandleFunc("/getScsbidListSttusThng", award)
	mux.HandleFunc("/getOpengResultListInfoRebid", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(envelope(fxRebid, 1)))
	})
	mux.HandleFunc("/getOpengResultListInfoFailing", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("bidClsfcNo") == "" {
			t.Errorf("Failing requires bidClsfcNo: %s", r.URL.RawQuery)
		}
		if r.URL.Query().Get("bidClsfcNo") == "9" {
			_, _ = w.Write([]byte(fxGwError))
			return
		}
		_, _ = w.Write([]byte(envelope(fxFailing, 1)))
	})
	return httptest.NewServer(mux)
}

func TestOpening_ThreeBusinessTypes(t *testing.T) {
	srv := fixtureServer(t)
	defer srv.Close()
	s := newTestSource(srv.URL)
	ctx := context.Background()

	svc, err := s.FetchOpeningList(ctx, BusinessService, "R26BK01674070")
	if err != nil || len(svc) != 1 || svc[0].ProgrsDivCdNm != "개찰완료" || svc[0].RoundNo() != "000" || svc[0].RsrvtnPrceFileExistnceYn != "Y" {
		t.Fatalf("service list: %v %+v", err, svc)
	}
	cn, err := s.FetchOpeningList(ctx, BusinessConstruction, "R26BK01667341")
	if err != nil || len(cn) != 2 || cn[0].ProgrsDivCdNm != "재입찰" || cn[1].RoundNo() != "001" || cn[1].ProgrsDivCdNm != "유찰" {
		t.Fatalf("construction list: %v %+v", err, cn)
	}
	th, err := s.FetchOpeningList(ctx, BusinessGoods, "R26BK01660544")
	if err != nil || len(th) != 1 || th[0].RoundNo() != "000" || th[0].BidClsfcNo != "1" {
		t.Fatalf("goods list (rbidNtceNo → RoundNo): %v %+v", err, th)
	}
}

func TestOpening_BiddersPricesAwardRebidFailing(t *testing.T) {
	srv := fixtureServer(t)
	defer srv.Close()
	s := newTestSource(srv.URL)
	ctx := context.Background()

	bidders, err := s.FetchOpeningBidders(ctx, "R26BK01674070", "000")
	if err != nil || len(bidders) != 2 {
		t.Fatalf("bidders: %v %d", err, len(bidders))
	}
	if bidders[0].OpengRank != "1" || bidders[0].Bidprcrt != "90.069" || bidders[1].Rmrk != "낙찰하한선 미달" || bidders[1].OpengRank != "" {
		t.Fatalf("bidder fields: %+v", bidders)
	}
	prices, err := s.FetchPreliminaryPrices(ctx, BusinessService, "R26BK01674070")
	if err != nil || len(prices) != 2 || prices[0].Plnprc != "16025300" || prices[0].Bssamt != "16119000" || prices[1].DrwtYn != "Y" {
		t.Fatalf("prices: %v %+v", err, prices)
	}
	awards, err := s.FetchAwardsByNotice(ctx, BusinessService, "R26BK01683359")
	if err != nil || len(awards) != 1 || awards[0].BidwinnrNm != "주식회사 동양기술단" || awards[0].FnlSucsfDate != "2026-08-14" || len(awards[0].Raw) == 0 {
		t.Fatalf("awards: %v %+v", err, awards)
	}
	rebids, err := s.FetchRebids(ctx, "R26BK01664554", "000")
	if err != nil || len(rebids) != 1 || rebids[0].RbidNo != "001" || !strings.Contains(rebids[0].RbidRsn, "예가초과") {
		t.Fatalf("rebids: %v %+v", err, rebids)
	}
	fails, err := s.FetchFailings(ctx, "R26BK01675192", "000", "0")
	if err != nil || len(fails) != 1 || fails[0].NobidRsn != "단독응찰에 따른 유찰" {
		t.Fatalf("failings: %v %+v", err, fails)
	}
}

// 조달청 게이트웨이는 파라미터 오류를 HTTP 200 + ResponseError 형태로 준다 → 에러로 승격돼야 한다.
func TestOpening_GatewayErrorIsError(t *testing.T) {
	srv := fixtureServer(t)
	defer srv.Close()
	s := newTestSource(srv.URL)
	if _, err := s.FetchFailings(context.Background(), "X", "", "9"); err == nil || !strings.Contains(err.Error(), "07") {
		t.Fatalf("expected gateway error, got %v", err)
	}
}

// NODATA(03)는 에러가 아니라 빈 결과.
func TestOpening_NoDataIsEmpty(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(fxNoData)) })
	srv := httptest.NewServer(mux)
	defer srv.Close()
	s := newTestSource(srv.URL)
	items, err := s.FetchOpeningList(context.Background(), BusinessService, "R26BK01680111")
	if err != nil || len(items) != 0 {
		t.Fatalf("nodata: %v %d", err, len(items))
	}
}
