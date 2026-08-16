package scsbid

// opening.go — 공고 상세 "개찰결과" 고도화 1차(2026-08-16). 조달청 나라장터
// 낙찰정보서비스(ScsbidInfoService)의 개찰결과 계열 오퍼레이션을 업무유형(용역/공사/물품)별로
// 감싼다. 기존 FetchAwards(낙찰현황 일자 수집)와 같은 Source/서비스키/레이트리밋을 공유한다.
//
// 오퍼레이션 이름과 요청 파라미터는 data.go.kr 명세(15129397 Swagger) + 2026-08-16 실호출로
// 확정한 것만 쓴다. 🚨 오퍼레이션마다 "공고번호로 조회"의 inqryDiv 값이 다르다 — 하나의 공통값으로
// 추상화하지 않고 오퍼레이션별 request builder에 상수로 박아둔다:
//
//	개찰결과 목록   getOpengResultListInfo{Servc,Cnstwk,Thng}              inqryDiv=4 + bidNtceNo (1=일자, 2·3=에러)
//	개찰순위(투찰)  getOpengResultListInfoOpengCompt                        inqryDiv 없음, bidNtceNo(+bidNtceOrd) 직접
//	복수예비가격    getOpengResultListInfo{…}PreparPcDetail                 inqryDiv=2 + bidNtceNo (4=입력범위 초과 에러)
//	최종낙찰자      getScsbidListSttus{Servc,Cnstwk,Thng}                   inqryDiv=4 + bidNtceNo (2=일자 필수·무관 결과)
//	재입찰          getOpengResultListInfoRebid                             bidNtceNo 직접
//	유찰            getOpengResultListInfoFailing                           bidNtceNo + bidClsfcNo(필수, 용역·공사 "0", 물품 "1")
//
// 응답 필드명은 전부 실응답에서 값이 확인된 것만 구조체에 둔다(추측 필드 금지). 숫자/금액은 API가
// 문자열로 주므로 그대로 문자열로 보존하고 변환은 호출부(api 패키지)가 한다.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"biz-platform/collector/internal/collector/common"
)

// BusinessType — 나라장터 업무구분. 오퍼레이션 접미어를 결정한다.
type BusinessType string

const (
	BusinessService      BusinessType = "service"      // 용역 (Servc)
	BusinessConstruction BusinessType = "construction" // 공사 (Cnstwk)
	BusinessGoods        BusinessType = "goods"        // 물품 (Thng)
)

// opSuffix — 업무구분별 오퍼레이션 접미어. 알 수 없는 값은 용역으로 폴백한다(현재 수집 대상이 용역).
func (b BusinessType) opSuffix() string {
	switch b {
	case BusinessConstruction:
		return "Cnstwk"
	case BusinessGoods:
		return "Thng"
	default:
		return "Servc"
	}
}

// Valid — 3종 중 하나인지.
func (b BusinessType) Valid() bool {
	return b == BusinessService || b == BusinessConstruction || b == BusinessGoods
}

// OpeningListItem — 개찰결과 목록 1행(공고·회차 단위). rbidNo별로 여러 행이 올 수 있다(재입찰).
type OpeningListItem struct {
	BidNtceNo                string `json:"bidNtceNo"`
	BidNtceOrd               string `json:"bidNtceOrd"`
	BidClsfcNo               string `json:"bidClsfcNo"`
	RbidNo                   string `json:"rbidNo"`     // 용역·공사. "000"=최초
	RbidNtceNo               string `json:"rbidNtceNo"` // 물품은 이 이름으로 온다(명세 차이)
	BidNtceNm                string `json:"bidNtceNm"`
	OpengDt                  string `json:"opengDt"`
	PrtcptCnum               string `json:"prtcptCnum"`
	OpengCorpInfo            string `json:"opengCorpInfo"` // "업체명^사업자번호^대표자^투찰금액^투찰률" (1순위, 개찰완료 시점)
	ProgrsDivCdNm            string `json:"progrsDivCdNm"` // 개찰완료 / 유찰 / 재입찰
	InptDt                   string `json:"inptDt"`
	RsrvtnPrceFileExistnceYn string `json:"rsrvtnPrceFileExistnceYn"` // 예비가격 파일 존재 Y/N → 예가상세 호출 여부
	NtceInsttNm              string `json:"ntceInsttNm"`
	DminsttNm                string `json:"dminsttNm"`
	OpengRsltNtcCntnts       string `json:"opengRsltNtcCntnts"`
}

// RoundNo — 회차 번호("000"=최초). 물품(rbidNtceNo)/용역·공사(rbidNo) 필드명 차이를 흡수한다.
func (it OpeningListItem) RoundNo() string {
	if r := strings.TrimSpace(it.RbidNo); r != "" {
		return r
	}
	return strings.TrimSpace(it.RbidNtceNo)
}

// OpeningBidder — 개찰순위(투찰) 1행.
type OpeningBidder struct {
	BidNtceNo      string `json:"bidNtceNo"`
	BidNtceOrd     string `json:"bidNtceOrd"`
	BidClsfcNo     string `json:"bidClsfcNo"`
	RbidNo         string `json:"rbidNo"`
	OpengRsltDivNm string `json:"opengRsltDivNm"`
	OpengRank      string `json:"opengRank"` // 부적격은 ""
	PrcbdrBizno    string `json:"prcbdrBizno"`
	PrcbdrNm       string `json:"prcbdrNm"`
	PrcbdrCeoNm    string `json:"prcbdrCeoNm"`
	BidprcAmt      string `json:"bidprcAmt"`
	Bidprcrt       string `json:"bidprcrt"` // 협상은 ""
	Rmrk           string `json:"rmrk"`     // 정상 / 낙찰하한선 미달 / 협상평가부적격자 …
	BidprcDt       string `json:"bidprcDt"`
	DrwtNo1        string `json:"drwtNo1"`
	DrwtNo2        string `json:"drwtNo2"`
	BidPrceEvlVal  string `json:"bidPrceEvlVal"`
	TechEvlVal     string `json:"techEvlVal"`
	TotalEvlAmtVal string `json:"totalEvlAmtVal"`
}

// PreliminaryPrice — 복수예비가격 1행(예가 1건).
type PreliminaryPrice struct {
	BidNtceNo                string `json:"bidNtceNo"`
	BidNtceOrd               string `json:"bidNtceOrd"`
	BidClsfcNo               string `json:"bidClsfcNo"`
	RbidNo                   string `json:"rbidNo"`
	Plnprc                   string `json:"plnprc"`              // 예정가격(전 행 동일)
	Bssamt                   string `json:"bssamt"`              // 기초금액(전 행 동일, 물품은 '' 가능)
	TotRsrvtnPrceNum         string `json:"totRsrvtnPrceNum"`    // 총 예가 건수
	CompnoRsrvtnPrceSno      string `json:"compnoRsrvtnPrceSno"` // 복수예가 순번
	BsisPlnprc               string `json:"bsisPlnprc"`          // 개별 예비가격
	DrwtYn                   string `json:"drwtYn"`              // 추첨 여부
	DrwtNum                  string `json:"drwtNum"`             // 추첨 횟수
	BssamtBssUpNum           string `json:"bssamtBssUpNum"`
	BidwinrSlctnAplBssCntnts string `json:"bidwinrSlctnAplBssCntnts"`
	RlOpengDt                string `json:"rlOpengDt"`
	CompnoRsrvtnPrceMkngDt   string `json:"compnoRsrvtnPrceMkngDt"`
	InptDt                   string `json:"inptDt"`
}

// RebidInfo — 재입찰 1행.
type RebidInfo struct {
	BidNtceNo              string `json:"bidNtceNo"`
	BidNtceOrd             string `json:"bidNtceOrd"`
	BidClsfcNo             string `json:"bidClsfcNo"`
	RbidNo                 string `json:"rbidNo"`
	OpengRsltDivNm         string `json:"opengRsltDivNm"`
	RbidRsn                string `json:"rbidRsn"`
	BidClseDt              string `json:"bidClseDt"`
	OpengDt                string `json:"opengDt"`
	CmmnSpldmdAgrmntClseDt string `json:"cmmnSpldmdAgrmntClseDt"`
}

// FailingInfo — 유찰 1행.
type FailingInfo struct {
	BidNtceNo      string `json:"bidNtceNo"`
	BidNtceOrd     string `json:"bidNtceOrd"`
	BidClsfcNo     string `json:"bidClsfcNo"`
	RbidNo         string `json:"rbidNo"`
	OpengRsltDivNm string `json:"opengRsltDivNm"`
	NobidRsn       string `json:"nobidRsn"`
}

// ---- 요청 빌더(오퍼레이션별로 분리, inqryDiv 의미가 다르므로 공통화하지 않는다) ----

func (s *Source) baseQuery(pageNo, numOfRows int) url.Values {
	q := url.Values{}
	q.Set("serviceKey", s.ServiceKey)
	q.Set("type", "json")
	q.Set("pageNo", strconv.Itoa(pageNo))
	q.Set("numOfRows", strconv.Itoa(numOfRows))
	return q
}

// buildOpeningListRequest — 개찰결과 목록: inqryDiv=4(공고번호 조회).
func (s *Source) buildOpeningListRequest(bt BusinessType, bidNtceNo string, pageNo, numOfRows int) string {
	q := s.baseQuery(pageNo, numOfRows)
	q.Set("inqryDiv", "4")
	q.Set("bidNtceNo", bidNtceNo)
	return s.base() + "/getOpengResultListInfo" + bt.opSuffix() + "?" + q.Encode()
}

// buildOpeningBiddersRequest — 개찰순위: inqryDiv 없음, bidNtceNo(+bidNtceOrd) 직접.
func (s *Source) buildOpeningBiddersRequest(bidNtceNo, bidNtceOrd string, pageNo, numOfRows int) string {
	q := s.baseQuery(pageNo, numOfRows)
	q.Set("bidNtceNo", bidNtceNo)
	if bidNtceOrd != "" {
		q.Set("bidNtceOrd", bidNtceOrd)
	}
	return s.base() + "/getOpengResultListInfoOpengCompt?" + q.Encode()
}

// buildPreliminaryPricesRequest — 복수예비가격: inqryDiv=2(공고번호 조회).
func (s *Source) buildPreliminaryPricesRequest(bt BusinessType, bidNtceNo string, pageNo, numOfRows int) string {
	q := s.baseQuery(pageNo, numOfRows)
	q.Set("inqryDiv", "2")
	q.Set("bidNtceNo", bidNtceNo)
	return s.base() + "/getOpengResultListInfo" + bt.opSuffix() + "PreparPcDetail?" + q.Encode()
}

// buildAwardByNoticeRequest — 최종낙찰자(낙찰현황): inqryDiv=4(공고번호 조회).
func (s *Source) buildAwardByNoticeRequest(bt BusinessType, bidNtceNo string, pageNo, numOfRows int) string {
	q := s.baseQuery(pageNo, numOfRows)
	q.Set("inqryDiv", "4")
	q.Set("bidNtceNo", bidNtceNo)
	return s.base() + "/getScsbidListSttus" + bt.opSuffix() + "?" + q.Encode()
}

// buildRebidRequest — 재입찰: bidNtceNo(+bidNtceOrd) 직접.
func (s *Source) buildRebidRequest(bidNtceNo, bidNtceOrd string, pageNo, numOfRows int) string {
	q := s.baseQuery(pageNo, numOfRows)
	q.Set("bidNtceNo", bidNtceNo)
	if bidNtceOrd != "" {
		q.Set("bidNtceOrd", bidNtceOrd)
	}
	return s.base() + "/getOpengResultListInfoRebid?" + q.Encode()
}

// buildFailingRequest — 유찰: bidNtceNo + bidClsfcNo(필수) (+bidNtceOrd).
func (s *Source) buildFailingRequest(bidNtceNo, bidNtceOrd, bidClsfcNo string, pageNo, numOfRows int) string {
	q := s.baseQuery(pageNo, numOfRows)
	q.Set("bidNtceNo", bidNtceNo)
	if bidNtceOrd != "" {
		q.Set("bidNtceOrd", bidNtceOrd)
	}
	if bidClsfcNo == "" {
		bidClsfcNo = "0"
	}
	q.Set("bidClsfcNo", bidClsfcNo)
	return s.base() + "/getOpengResultListInfoFailing?" + q.Encode()
}

// ---- 공통 GET + envelope 파싱 ----

// getItems — 지정 URL을 호출해 items(원본 JSON 배열)와 totalCount를 돌려준다. resultCode 03(NODATA)은
// 빈 결과로 정상 처리. 조달청 게이트웨이는 파라미터 오류를 HTTP 200 + `nkoneps.com.response.ResponseError`
// 형태로 주므로 그 형태도 에러로 승격한다.
func (s *Source) getItems(ctx context.Context, reqURL string) ([]json.RawMessage, int, error) {
	if err := s.RateLimit.Wait(ctx); err != nil {
		return nil, 0, err
	}
	var raw []byte
	err := common.Do(ctx, common.DefaultRetryConfig(), func() error {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
		resp, err := s.HTTPClient.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return &common.PermanentError{Err: fmt.Errorf("auth failed: status %d", resp.StatusCode)}
		}
		if resp.StatusCode >= 500 {
			return fmt.Errorf("server error: status %d", resp.StatusCode)
		}
		if resp.StatusCode != http.StatusOK {
			return &common.PermanentError{Err: fmt.Errorf("unexpected status %d", resp.StatusCode)}
		}
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			return err
		}
		raw = b
		return nil
	})
	if err != nil {
		return nil, 0, err
	}
	// 게이트웨이 오류 형태(HTTP 200): {"nkoneps.com.response.ResponseError":{"header":{"resultCode":"07",...}}}
	var gw struct {
		Err *struct {
			Header struct {
				ResultCode string `json:"resultCode"`
				ResultMsg  string `json:"resultMsg"`
			} `json:"header"`
		} `json:"nkoneps.com.response.ResponseError"`
	}
	if json.Unmarshal(raw, &gw) == nil && gw.Err != nil {
		return nil, 0, &common.PermanentError{Err: fmt.Errorf("scsbid gateway error %s: %s", gw.Err.Header.ResultCode, gw.Err.Header.ResultMsg)}
	}
	var envelope apiEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, 0, &common.PermanentError{Err: fmt.Errorf("parse response envelope: %w", err)}
	}
	switch code := envelope.Response.Header.ResultCode; code {
	case "00":
	case "03":
		return nil, 0, nil
	default:
		msg := fmt.Sprintf("scsbid api error %s: %s", code, envelope.Response.Header.ResultMsg)
		if isPermanentResultCode(code) {
			return nil, 0, &common.PermanentError{Err: fmt.Errorf("%s", msg)}
		}
		return nil, 0, fmt.Errorf("%s", msg)
	}
	var body apiBody
	if err := json.Unmarshal(envelope.Response.Body, &body); err != nil {
		return nil, 0, &common.PermanentError{Err: fmt.Errorf("parse response body: %w", err)}
	}
	return body.Items.Items, int(body.TotalCount), nil
}

func decodeAll[T any](items []json.RawMessage) []T {
	out := make([]T, 0, len(items))
	for _, raw := range items {
		var v T
		if err := json.Unmarshal(raw, &v); err != nil {
			continue
		}
		out = append(out, v)
	}
	return out
}

// maxOpeningRows — 개찰순위/예가는 한 공고에 수백 행(실측 323)이 있을 수 있어 넉넉히 받는다.
// 낙찰현황 numOfRows=999 정상 응답을 실측했다.
const maxOpeningRows = 999

// FetchOpeningList — 개찰결과 목록(회차별 행). 개찰 전이면 빈 슬라이스.
func (s *Source) FetchOpeningList(ctx context.Context, bt BusinessType, bidNtceNo string) ([]OpeningListItem, error) {
	items, _, err := s.getItems(ctx, s.buildOpeningListRequest(bt, bidNtceNo, 1, 50))
	if err != nil {
		return nil, err
	}
	return decodeAll[OpeningListItem](items), nil
}

// FetchOpeningBidders — 개찰순위(투찰 업체 전체). 페이지네이션 처리.
func (s *Source) FetchOpeningBidders(ctx context.Context, bidNtceNo, bidNtceOrd string) ([]OpeningBidder, error) {
	var all []OpeningBidder
	for page := 1; ; page++ {
		items, total, err := s.getItems(ctx, s.buildOpeningBiddersRequest(bidNtceNo, bidNtceOrd, page, maxOpeningRows))
		if err != nil {
			return all, err
		}
		all = append(all, decodeAll[OpeningBidder](items)...)
		if len(items) == 0 || page*maxOpeningRows >= total || page >= 5 {
			return all, nil
		}
	}
}

// FetchPreliminaryPrices — 복수예비가격 상세(예가 15행 등). 비예가/협상은 빈 슬라이스.
func (s *Source) FetchPreliminaryPrices(ctx context.Context, bt BusinessType, bidNtceNo string) ([]PreliminaryPrice, error) {
	items, _, err := s.getItems(ctx, s.buildPreliminaryPricesRequest(bt, bidNtceNo, 1, 100))
	if err != nil {
		return nil, err
	}
	return decodeAll[PreliminaryPrice](items), nil
}

// FetchAwardsByNotice — 최종낙찰자(공고번호 직접 조회). 낙찰 확정 전이면 빈 슬라이스.
func (s *Source) FetchAwardsByNotice(ctx context.Context, bt BusinessType, bidNtceNo string) ([]AwardRecord, error) {
	items, _, err := s.getItems(ctx, s.buildAwardByNoticeRequest(bt, bidNtceNo, 1, 20))
	if err != nil {
		return nil, err
	}
	out := make([]AwardRecord, 0, len(items))
	for _, raw := range items {
		var rec AwardRecord
		if err := json.Unmarshal(raw, &rec); err != nil {
			continue
		}
		rec.Raw = append(json.RawMessage(nil), raw...)
		out = append(out, rec)
	}
	return out, nil
}

// FetchRebids — 재입찰 정보.
func (s *Source) FetchRebids(ctx context.Context, bidNtceNo, bidNtceOrd string) ([]RebidInfo, error) {
	items, _, err := s.getItems(ctx, s.buildRebidRequest(bidNtceNo, bidNtceOrd, 1, 20))
	if err != nil {
		return nil, err
	}
	return decodeAll[RebidInfo](items), nil
}

// FetchFailings — 유찰 정보. bidClsfcNo는 개찰결과 목록 행의 값을 그대로 넘긴다(물품 "1").
func (s *Source) FetchFailings(ctx context.Context, bidNtceNo, bidNtceOrd, bidClsfcNo string) ([]FailingInfo, error) {
	items, _, err := s.getItems(ctx, s.buildFailingRequest(bidNtceNo, bidNtceOrd, bidClsfcNo, 1, 20))
	if err != nil {
		return nil, err
	}
	return decodeAll[FailingInfo](items), nil
}
