// notice_opening_result.go — 공고 상세 고도화 1차(2026-08-16) "개찰결과".
//
// GET /api/notices/{id}/opening-result — 공고 상세 응답을 비대하게 만들지 않도록 읽기 전용 child
// endpoint로 분리한다. 나라장터 낙찰정보서비스(ScsbidInfoService) 개찰결과 계열 오퍼레이션
// (scsbid/opening.go)을 조합해 "개찰 상태 모델"로 정규화하고 notice_opening_results(공고당 1행)에
// 캐시한다. 공고 상세(GET /api/notices/{id})는 이 파일에 의존하지 않는다 — 여기서 API가 실패해도
// 상세 화면은 정상이고, 이 endpoint만 fetchError를 담은 200(또는 캐시된 마지막 값)을 돌려준다.
//
// 🚨 개찰 상태 모델 — 절대 혼동 금지:
//
//	BEFORE_OPENING       개찰 전(공고의 개찰 예정일시가 미래) — API 호출 없음(쿼터 절약)
//	OPENING_PENDING      개찰 예정 시각은 지났으나 결과가 아직 등록되지 않음(개찰 지연/결과 등록 대기)
//	OPENED_WAITING_AWARD 개찰 완료(1순위는 있음)이지만 최종 낙찰자 미확정(적격심사·협상 중) — "낙찰업체"라 부르지 않는다
//	AWARDED              낙찰 확정(낙찰현황 행 존재, fnlSucsfDate)
//	FAILED               유찰
//	REBID                재입찰 진행 중(최신 회차가 재입찰이고 다음 회차 결과가 아직 없음)
//
// 시간 경과만으로 "개찰 완료"를 판단하지 않는다 — 개찰결과 목록에 실제 행이 있을 때만 개찰완료다.
// 재입찰이 있으면 회차(rbidNo)별 행이 여러 개 오므로 "최신 회차"의 상태를 대표 상태로 삼고 이전
// 회차는 rounds에 보존한다(1차 재입찰 → 2차 개찰완료 → 낙찰확정 같은 이력이 섞이지 않게).
//
// 캐시(TTL) 정책 — status별 next_check_at:
//
//	BEFORE_OPENING       API 호출/DB 저장 없음(공고의 opening_at으로 즉시 계산)
//	OPENING_PENDING      30분
//	OPENED_WAITING_AWARD 3시간(적격심사 결과가 며칠 걸릴 수 있음, 표본상 당일~34일)
//	REBID                1시간(재입찰 개찰시각이 더 뒤면 그 시각)
//	AWARDED / FAILED     7일(사실상 확정 — 주 1회 재확인만)
//	fetch 실패           15분 뒤 재시도(마지막 성공 캐시가 있으면 그것을 stale로 그대로 내려준다)
//
// 개인정보: 사업자번호는 마스킹(325-87-*****), 대표자명/전화번호/주소는 화면 응답에서 제외한다.
package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"biz-platform/collector/internal/collector/sources/scsbid"
)

const (
	openingBeforeOpening      = "BEFORE_OPENING"
	openingPending            = "OPENING_PENDING"
	openingOpenedWaitingAward = "OPENED_WAITING_AWARD"
	openingAwarded            = "AWARDED"
	openingFailed             = "FAILED"
	openingRebid              = "REBID"
)

var openingStatusLabels = map[string]string{
	openingBeforeOpening:      "개찰 예정",
	openingPending:            "개찰 결과 등록 대기",
	openingOpenedWaitingAward: "개찰완료 · 낙찰자 결정 대기",
	openingAwarded:            "낙찰 확정",
	openingFailed:             "유찰",
	openingRebid:              "재입찰 진행 중",
}

// TTL
const (
	openingTTLPending      = 30 * time.Minute
	openingTTLWaitingAward = 3 * time.Hour
	openingTTLRebid        = time.Hour
	openingTTLFinal        = 7 * 24 * time.Hour
	openingTTLError        = 15 * time.Minute
	openingFetchTimeout    = 25 * time.Second
)

type openingBidderDTO struct {
	Rank                 *int   `json:"rank,omitempty"` // 부적격은 nil
	Name                 string `json:"name"`
	BusinessNumberMasked string `json:"businessNumberMasked,omitempty"`
	Amount               *int64 `json:"amount,omitempty"`
	Rate                 string `json:"rate,omitempty"` // "88.32" — API 문자열 그대로(협상은 없음)
	BidAt                string `json:"bidAt,omitempty"`
	Remark               string `json:"remark,omitempty"` // 정상 외 사유(낙찰하한선 미달 등)
	Disqualified         bool   `json:"disqualified"`
	PriceScore           string `json:"priceScore,omitempty"`
	TechScore            string `json:"techScore,omitempty"`
	TotalScore           string `json:"totalScore,omitempty"`
	RoundNo              string `json:"roundNo,omitempty"`
}

type openingWinnerDTO struct {
	Name                 string `json:"name"`
	BusinessNumberMasked string `json:"businessNumberMasked,omitempty"`
	Amount               *int64 `json:"amount,omitempty"`
	Rate                 string `json:"rate,omitempty"`
	FinalAwardDate       string `json:"finalAwardDate,omitempty"`
	RoundNo              string `json:"roundNo,omitempty"`
}

type openingPriceDTO struct {
	Seq       int    `json:"seq"`
	Price     *int64 `json:"price,omitempty"`
	Drawn     bool   `json:"drawn"`
	DrawCount *int   `json:"drawCount,omitempty"`
}

type openingRoundDTO struct {
	RoundNo          string `json:"roundNo"`
	Status           string `json:"status"` // 개찰완료/유찰/재입찰(API 원문)
	OpeningAt        string `json:"openingAt,omitempty"`
	ParticipantCount *int   `json:"participantCount,omitempty"`
}

type openingRebidDTO struct {
	RoundNo   string `json:"roundNo"`
	Reason    string `json:"reason,omitempty"`
	CloseAt   string `json:"closeAt,omitempty"`
	OpeningAt string `json:"openingAt,omitempty"`
}

type openingFailingDTO struct {
	RoundNo string `json:"roundNo"`
	Reason  string `json:"reason,omitempty"`
}

// openingResultDTO — child endpoint 응답. 실제 API가 주지 않는 값은 만들지 않는다(omitempty/nil).
type openingResultDTO struct {
	Status            string             `json:"status"`
	StatusLabel       string             `json:"statusLabel"`
	BusinessType      string             `json:"businessType"`
	BidNtceNo         string             `json:"bidNtceNo"`
	BidNtceOrd        string             `json:"bidNtceOrd,omitempty"`
	RoundNo           string             `json:"roundNo,omitempty"` // 최신 회차 rbidNo("000"=최초)
	RoundIndex        int                `json:"roundIndex"`        // 1=최초, 2=1차 재입찰…
	OpeningAt         *time.Time         `json:"openingAt,omitempty"`
	ActualOpeningAt   *time.Time         `json:"actualOpeningAt,omitempty"`
	ParticipantCount  *int               `json:"participantCount,omitempty"`
	BaseAmount        *int64             `json:"baseAmount,omitempty"`
	PlannedPrice      *int64             `json:"plannedPrice,omitempty"`
	Winner            *openingWinnerDTO  `json:"winner,omitempty"`
	TopBidder         *openingBidderDTO  `json:"topBidder,omitempty"`
	Participants      []openingBidderDTO `json:"participants"`
	PreliminaryPrices []openingPriceDTO  `json:"preliminaryPrices"`
	PreliminaryTotal  *int               `json:"preliminaryTotal,omitempty"`
	Rounds            []openingRoundDTO  `json:"rounds"`
	Rebid             *openingRebidDTO   `json:"rebid,omitempty"`
	Failing           *openingFailingDTO `json:"failing,omitempty"`
	FetchedAt         *time.Time         `json:"fetchedAt,omitempty"`
	Stale             bool               `json:"stale"`
	FetchError        string             `json:"fetchError,omitempty"`
	OfficialURL       string             `json:"officialUrl,omitempty"`
}

// openingRawBundle — API 원본 묶음(순수 함수 buildOpeningResult 입력).
type openingRawBundle struct {
	List    []scsbid.OpeningListItem
	Bidders []scsbid.OpeningBidder
	Prices  []scsbid.PreliminaryPrice
	Awards  []scsbid.AwardRecord
	Rebids  []scsbid.RebidInfo
	Fails   []scsbid.FailingInfo
}

// ---- 순수 함수(단위테스트 대상) ----

func maskBizno(s string) string {
	d := normalizeBizno(s)
	if len(d) != 10 {
		return ""
	}
	return d[0:3] + "-" + d[3:5] + "-*****"
}

func atoiPtr(s string) *int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return nil
	}
	return &n
}

func amountPtr(s string) *int64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return nil
	}
	return &n
}

// roundKey — rbidNo 문자열("000","001")을 정렬 가능한 정수로.
func roundKey(r string) int {
	n, err := strconv.Atoi(strings.TrimSpace(r))
	if err != nil {
		return 0
	}
	return n
}

// buildOpeningResult — API 원본 묶음 + 공고 개찰 예정일시 + 현재시각 → 상태 모델/DTO/다음 재조회 시각.
// 목록 행이 없으면(개찰 미등록) OPENING_PENDING. 최신 회차의 progrsDivCdNm으로 분기한다.
func buildOpeningResult(b openingRawBundle, bt scsbid.BusinessType, bidNtceNo, bidNtceOrd string, noticeOpeningAt *time.Time, now time.Time) (*openingResultDTO, time.Time) {
	dto := &openingResultDTO{
		BusinessType:      string(bt),
		BidNtceNo:         bidNtceNo,
		BidNtceOrd:        bidNtceOrd,
		Participants:      []openingBidderDTO{},
		PreliminaryPrices: []openingPriceDTO{},
		Rounds:            []openingRoundDTO{},
		OpeningAt:         noticeOpeningAt,
	}
	// 차수 필터 — 목록에 다른 차수 행이 섞여 오면(재공고) 우리 차수만 본다. 차수를 모르면(빈 문자열) 전부.
	list := make([]scsbid.OpeningListItem, 0, len(b.List))
	for _, it := range b.List {
		if bidNtceOrd != "" && strings.TrimSpace(it.BidNtceOrd) != "" && strings.TrimSpace(it.BidNtceOrd) != bidNtceOrd {
			continue
		}
		list = append(list, it)
	}
	sort.SliceStable(list, func(i, j int) bool { return roundKey(list[i].RoundNo()) < roundKey(list[j].RoundNo()) })
	for _, it := range list {
		dto.Rounds = append(dto.Rounds, openingRoundDTO{RoundNo: it.RoundNo(), Status: it.ProgrsDivCdNm, OpeningAt: it.OpengDt, ParticipantCount: atoiPtr(it.PrtcptCnum)})
	}
	if len(list) == 0 {
		dto.Status = openingPending
		dto.StatusLabel = openingStatusLabels[openingPending]
		return dto, now.Add(openingTTLPending)
	}
	latest := list[len(list)-1]
	dto.RoundNo = latest.RoundNo()
	dto.RoundIndex = len(list)
	dto.ParticipantCount = atoiPtr(latest.PrtcptCnum)
	if t := parseG2BDateTime(latest.OpengDt); t != nil {
		dto.OpeningAt = t
	}
	if t := parseG2BDateTime(latest.InptDt); t != nil {
		dto.ActualOpeningAt = t
	}
	// 예가/기초금액(전 행 동일값) — 최신 회차 우선.
	for _, p := range b.Prices {
		if dto.RoundNo != "" && strings.TrimSpace(p.RbidNo) != "" && strings.TrimSpace(p.RbidNo) != dto.RoundNo {
			continue
		}
		if dto.PlannedPrice == nil {
			dto.PlannedPrice = amountPtr(p.Plnprc)
		}
		if dto.BaseAmount == nil {
			dto.BaseAmount = amountPtr(p.Bssamt)
		}
		if dto.PreliminaryTotal == nil {
			dto.PreliminaryTotal = atoiPtr(p.TotRsrvtnPrceNum)
		}
		seq, _ := strconv.Atoi(strings.TrimSpace(p.CompnoRsrvtnPrceSno))
		if seq == 0 && strings.TrimSpace(p.BsisPlnprc) == "" {
			continue // 단일 예가(물품 표본): 복수예가 행 아님
		}
		dto.PreliminaryPrices = append(dto.PreliminaryPrices, openingPriceDTO{Seq: seq, Price: amountPtr(p.BsisPlnprc), Drawn: strings.EqualFold(strings.TrimSpace(p.DrwtYn), "Y"), DrawCount: atoiPtr(p.DrwtNum)})
	}
	for _, p := range b.Prices {
		if t := parseG2BDateTime(p.RlOpengDt); t != nil {
			dto.ActualOpeningAt = t // 실개찰일시(예가상세) — 목록의 입력일시보다 정확
			break
		}
	}
	sort.SliceStable(dto.PreliminaryPrices, func(i, j int) bool { return dto.PreliminaryPrices[i].Seq < dto.PreliminaryPrices[j].Seq })

	// 투찰 순위 — 최신 회차만. 순위 있는 행 먼저(순위 오름차순), 부적격은 뒤.
	for _, bd := range b.Bidders {
		if dto.RoundNo != "" && strings.TrimSpace(bd.RbidNo) != "" && strings.TrimSpace(bd.RbidNo) != dto.RoundNo {
			continue
		}
		remark := strings.TrimSpace(bd.Rmrk)
		disq := remark != "" && remark != "정상"
		item := openingBidderDTO{
			Rank: atoiPtr(bd.OpengRank), Name: strings.TrimSpace(bd.PrcbdrNm), BusinessNumberMasked: maskBizno(bd.PrcbdrBizno),
			Amount: amountPtr(bd.BidprcAmt), Rate: strings.TrimSpace(bd.Bidprcrt), BidAt: strings.TrimSpace(bd.BidprcDt),
			Disqualified: disq, PriceScore: strings.TrimSpace(bd.BidPrceEvlVal), TechScore: strings.TrimSpace(bd.TechEvlVal), TotalScore: strings.TrimSpace(bd.TotalEvlAmtVal),
			RoundNo: strings.TrimSpace(bd.RbidNo),
		}
		if disq {
			item.Remark = remark
		}
		dto.Participants = append(dto.Participants, item)
	}
	sort.SliceStable(dto.Participants, func(i, j int) bool {
		a, c := dto.Participants[i], dto.Participants[j]
		if (a.Rank == nil) != (c.Rank == nil) {
			return a.Rank != nil
		}
		if a.Rank != nil && c.Rank != nil && *a.Rank != *c.Rank {
			return *a.Rank < *c.Rank
		}
		return false
	})
	if len(dto.Participants) > 0 && dto.Participants[0].Rank != nil {
		top := dto.Participants[0]
		dto.TopBidder = &top
	} else if info := strings.Split(latest.OpengCorpInfo, "^"); len(info) >= 2 && strings.TrimSpace(info[0]) != "" {
		// 개찰순위 API가 비어 있어도 목록의 opengCorpInfo(업체명^사업자번호^대표자^금액^률)로 1순위 표시.
		one := 1
		top := openingBidderDTO{Rank: &one, Name: strings.TrimSpace(info[0]), BusinessNumberMasked: maskBizno(info[1])}
		if len(info) >= 4 {
			top.Amount = amountPtr(info[3])
		}
		if len(info) >= 5 && strings.TrimSpace(info[4]) != "0" {
			top.Rate = strings.TrimSpace(info[4])
		}
		dto.TopBidder = &top
	}

	// 재입찰/유찰 상세 — 최신 회차에 해당하는 행 우선.
	for _, r := range b.Rebids {
		dto.Rebid = &openingRebidDTO{RoundNo: strings.TrimSpace(r.RbidNo), Reason: strings.TrimSpace(r.RbidRsn), CloseAt: strings.TrimSpace(r.BidClseDt), OpeningAt: strings.TrimSpace(r.OpengDt)}
	}
	for _, f := range b.Fails {
		if strings.TrimSpace(f.RbidNo) == dto.RoundNo || dto.Failing == nil {
			dto.Failing = &openingFailingDTO{RoundNo: strings.TrimSpace(f.RbidNo), Reason: strings.TrimSpace(f.NobidRsn)}
		}
	}

	// 상태 분기(최신 회차 기준).
	switch strings.TrimSpace(latest.ProgrsDivCdNm) {
	case "유찰":
		dto.Status = openingFailed
		if dto.Failing == nil {
			dto.Failing = &openingFailingDTO{RoundNo: dto.RoundNo}
		}
	case "재입찰":
		dto.Status = openingRebid
	default: // 개찰완료(및 알 수 없는 값도 결과 행이 있으니 개찰은 된 것으로 본다)
		dto.Status = openingOpenedWaitingAward
		for _, a := range b.Awards {
			if bidNtceOrd != "" && strings.TrimSpace(a.BidNtceOrd) != "" && strings.TrimSpace(a.BidNtceOrd) != bidNtceOrd {
				continue
			}
			if dto.RoundNo != "" && strings.TrimSpace(a.RbidNo) != "" && strings.TrimSpace(a.RbidNo) != dto.RoundNo {
				continue
			}
			dto.Status = openingAwarded
			dto.Winner = &openingWinnerDTO{Name: strings.TrimSpace(a.BidwinnrNm), BusinessNumberMasked: maskBizno(a.BidwinnrBizno), Amount: amountPtr(a.SucsfbidAmt), Rate: strings.TrimSpace(a.SucsfbidRate), FinalAwardDate: strings.TrimSpace(a.FnlSucsfDate), RoundNo: strings.TrimSpace(a.RbidNo)}
			if dto.ParticipantCount == nil {
				dto.ParticipantCount = atoiPtr(a.PrtcptCnum)
			}
			if t, err := parseScsbidTime(a.RlOpengDt); err == nil && dto.ActualOpeningAt == nil {
				dto.ActualOpeningAt = &t
			}
			break
		}
	}
	dto.StatusLabel = openingStatusLabels[dto.Status]

	next := now.Add(openingTTLFinal)
	switch dto.Status {
	case openingOpenedWaitingAward:
		next = now.Add(openingTTLWaitingAward)
	case openingRebid:
		next = now.Add(openingTTLRebid)
		if dto.Rebid != nil {
			if t := parseG2BDateTime(dto.Rebid.OpeningAt); t != nil && t.After(next) {
				next = *t
			}
		}
	}
	return dto, next
}

// businessTypeFromRaw — g2b raw_content(JSON)에서 업무유형을 추정한다. 현재 수집기는 용역 목록만
// 쓰므로 기본은 용역이고, 공사/물품 전용 응답 키가 있을 때만 그쪽으로 본다(추측 대신 키 존재 판정).
func businessTypeFromRaw(raw string) scsbid.BusinessType {
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return scsbid.BusinessService
	}
	has := func(k string) bool { v, ok := m[k]; return ok && len(v) > 2 }
	if has("mainCnsttyNm") || has("cnstrtsiteRgnNm") || has("cnstrtnAbltyEvlAmtList") {
		return scsbid.BusinessConstruction
	}
	if has("dtilPrdctClsfcNo") || has("prdctSpecNm") || has("dlvrTmlmtDt") {
		return scsbid.BusinessGoods
	}
	if v, ok := m["bsnsDivNm"]; ok {
		var s string
		if json.Unmarshal(v, &s) == nil {
			switch strings.TrimSpace(s) {
			case "공사":
				return scsbid.BusinessConstruction
			case "물품":
				return scsbid.BusinessGoods
			}
		}
	}
	return scsbid.BusinessService
}

// ---- API 호출 조합 ----

// fetchOpeningBundle — 최소 호출: 목록 → (행 있으면) 순위 + 낙찰현황(개찰완료일 때) + 예가(예가파일 Y일 때)
// + 재입찰(재입찰 회차 있을 때) + 유찰(유찰 회차 있을 때). 개찰 전(빈 목록)은 1콜로 끝.
func fetchOpeningBundle(ctx context.Context, src *scsbid.Source, bt scsbid.BusinessType, bidNtceNo, bidNtceOrd string) (openingRawBundle, error) {
	var b openingRawBundle
	list, err := src.FetchOpeningList(ctx, bt, bidNtceNo)
	if err != nil {
		return b, err
	}
	b.List = list
	if len(list) == 0 {
		return b, nil
	}
	var hasOpened, hasRebid, hasFail, hasPrices bool
	clsfc := ""
	for _, it := range list {
		switch strings.TrimSpace(it.ProgrsDivCdNm) {
		case "개찰완료":
			hasOpened = true
		case "재입찰":
			hasRebid = true
		case "유찰":
			hasFail = true
		}
		if strings.EqualFold(strings.TrimSpace(it.RsrvtnPrceFileExistnceYn), "Y") {
			hasPrices = true
		}
		if clsfc == "" {
			clsfc = strings.TrimSpace(it.BidClsfcNo)
		}
	}
	// 개별 실패는 그 조각만 비우고 계속(부분 성공 허용) — 단 첫 오류는 기록해 next_check를 짧게 잡는다.
	var firstErr error
	keep := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if hasOpened {
		bidders, err := src.FetchOpeningBidders(ctx, bidNtceNo, bidNtceOrd)
		keep(err)
		b.Bidders = bidders
		awards, err := src.FetchAwardsByNotice(ctx, bt, bidNtceNo)
		keep(err)
		b.Awards = awards
	}
	if hasPrices {
		prices, err := src.FetchPreliminaryPrices(ctx, bt, bidNtceNo)
		keep(err)
		b.Prices = prices
	}
	if hasRebid {
		rebids, err := src.FetchRebids(ctx, bidNtceNo, bidNtceOrd)
		keep(err)
		b.Rebids = rebids
	}
	if hasFail {
		fails, err := src.FetchFailings(ctx, bidNtceNo, bidNtceOrd, clsfc)
		keep(err)
		b.Fails = fails
	}
	return b, firstErr
}

// ---- 캐시(notice_opening_results) ----

type openingCacheRow struct {
	dto          *openingResultDTO
	nextCheckAt  time.Time
	bidNtceNo    string
	bidNtceOrd   string
	businessType string
}

// matchesIdentity — 캐시 row가 "현재 공고 identity"(공고번호·차수·업무유형)와 같은지. 정정/재공고로
// 차수가 바뀌었거나(000→001) 공고번호·업무유형이 다르면 next_check_at이 미래여도 캐시를 최신 결과로
// 반환하지 않는다(과거 차수의 '낙찰 확정'이 새 차수 상세에 남는 것 차단). rbid_no(재입찰 회차)는 같은
// 차수 안에서 API가 갱신하는 값이라 identity에 넣지 않는다(회차 증가로 캐시를 폐기하는 순환 방지).
// bidClsfcNo는 개찰결과 목록 응답에서만 알 수 있어(현재 공고 raw에는 없음) identity에 포함하지 않는다.
func (c *openingCacheRow) matchesIdentity(bidNtceNo, bidNtceOrd string, bt scsbid.BusinessType) bool {
	return c != nil && c.dto != nil &&
		strings.TrimSpace(c.bidNtceNo) == strings.TrimSpace(bidNtceNo) &&
		strings.TrimSpace(c.bidNtceOrd) == strings.TrimSpace(bidNtceOrd) &&
		c.businessType == string(bt)
}

func (s *Server) loadOpeningCache(ctx context.Context, noticeID string) (*openingCacheRow, error) {
	var (
		payload      sql.NullString
		next         sql.NullTime
		no, ord, btp string
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT payload::text, next_check_at, bid_ntce_no, bid_ntce_ord, business_type
		FROM notice_opening_results WHERE notice_id = $1`, noticeID).Scan(&payload, &next, &no, &ord, &btp)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	row := &openingCacheRow{bidNtceNo: no, bidNtceOrd: ord, businessType: btp}
	if next.Valid {
		row.nextCheckAt = next.Time
	}
	// payload 컬럼에 DTO 전체를 JSONB로 보관한다(요약 컬럼은 조회/통계용 복제). 화면 응답은
	// 이 JSON을 그대로 쓰므로 컬럼 하나하나 다시 조립하지 않는다.
	if payload.Valid && payload.String != "" {
		var d openingResultDTO
		if json.Unmarshal([]byte(payload.String), &d) == nil {
			row.dto = &d
		}
	}
	return row, nil
}

func (s *Server) saveOpeningCache(ctx context.Context, noticeID string, bt scsbid.BusinessType, dto *openingResultDTO, next time.Time, fetchErr string) error {
	full, err := json.Marshal(dto)
	if err != nil {
		return err
	}
	toJSON := func(v any) []byte {
		if v == nil {
			return nil
		}
		b, _ := json.Marshal(v)
		return b
	}
	var winner, top, participants, prices, rounds, rebid, failing []byte
	if dto.Winner != nil {
		winner = toJSON(dto.Winner)
	}
	if dto.TopBidder != nil {
		top = toJSON(dto.TopBidder)
	}
	participants = toJSON(dto.Participants)
	prices = toJSON(dto.PreliminaryPrices)
	rounds = toJSON(dto.Rounds)
	if dto.Rebid != nil {
		rebid = toJSON(dto.Rebid)
	}
	if dto.Failing != nil {
		failing = toJSON(dto.Failing)
	}
	var fetchErrVal sql.NullString
	if fetchErr != "" {
		fetchErrVal = sql.NullString{String: fetchErr, Valid: true}
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO notice_opening_results
			(notice_id, bid_ntce_no, bid_ntce_ord, bid_clsfc_no, rbid_no, business_type, status,
			 opening_at, actual_opening_at, participant_count, base_amount, planned_price,
			 top_bidder, winner, participants, preliminary_prices, rounds, rebid, failing, payload,
			 fetch_error, fetched_at, next_check_at, updated_at)
		VALUES ($1,$2,$3,NULL,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,now(),$21,now())
		ON CONFLICT (notice_id) DO UPDATE SET
			bid_ntce_no = EXCLUDED.bid_ntce_no, bid_ntce_ord = EXCLUDED.bid_ntce_ord, rbid_no = EXCLUDED.rbid_no,
			business_type = EXCLUDED.business_type, status = EXCLUDED.status,
			opening_at = EXCLUDED.opening_at, actual_opening_at = EXCLUDED.actual_opening_at,
			participant_count = EXCLUDED.participant_count, base_amount = EXCLUDED.base_amount, planned_price = EXCLUDED.planned_price,
			top_bidder = EXCLUDED.top_bidder, winner = EXCLUDED.winner, participants = EXCLUDED.participants,
			preliminary_prices = EXCLUDED.preliminary_prices, rounds = EXCLUDED.rounds, rebid = EXCLUDED.rebid, failing = EXCLUDED.failing,
			payload = EXCLUDED.payload,
			fetch_error = EXCLUDED.fetch_error, fetched_at = now(), next_check_at = EXCLUDED.next_check_at, updated_at = now()`,
		noticeID, dto.BidNtceNo, dto.BidNtceOrd, nullStr(dto.RoundNo), string(bt), dto.Status,
		nullTimePtr(dto.OpeningAt), nullTimePtr(dto.ActualOpeningAt), nullIntPtr(dto.ParticipantCount), nullInt64FromPtr(dto.BaseAmount), nullInt64FromPtr(dto.PlannedPrice),
		nullBytes(top), nullBytes(winner), nullBytes(participants), nullBytes(prices), nullBytes(rounds), nullBytes(rebid), nullBytes(failing), string(full),
		fetchErrVal, next)
	return err
}

func nullTimePtr(t *time.Time) sql.NullTime {
	if t == nil {
		return sql.NullTime{}
	}
	return sql.NullTime{Time: *t, Valid: true}
}
func nullIntPtr(n *int) sql.NullInt64 {
	if n == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: int64(*n), Valid: true}
}
func nullInt64FromPtr(n *int64) sql.NullInt64 {
	if n == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: *n, Valid: true}
}
func nullBytes(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return string(b)
}

// ---- 핸들러 ----

// openingInflight — 같은 공고 동시 요청 시 API를 중복 호출하지 않도록 진행 중 표시(공유 뮤텍스).
var (
	openingInflightMu sync.Mutex
	openingInflight   = map[string]chan struct{}{}
)

func acquireOpeningInflight(noticeID string) (chan struct{}, bool) {
	openingInflightMu.Lock()
	defer openingInflightMu.Unlock()
	if ch, ok := openingInflight[noticeID]; ok {
		return ch, false
	}
	ch := make(chan struct{})
	openingInflight[noticeID] = ch
	return ch, true
}

func releaseOpeningInflight(noticeID string, ch chan struct{}) {
	openingInflightMu.Lock()
	delete(openingInflight, noticeID)
	openingInflightMu.Unlock()
	close(ch)
}

// handleGetNoticeOpeningResult — GET /api/notices/{id}/opening-result (공개, 상세와 동일 정책).
func (s *Server) handleGetNoticeOpeningResult(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ctx := r.Context()

	var (
		noticeType, externalID string
		currentVersion         int
		officialURL            sql.NullString
		openingAt              sql.NullTime
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT n.notice_type, n.external_notice_id, n.current_version, n.official_url, n.opening_at
		FROM notices n WHERE n.id = $1`, id).Scan(&noticeType, &externalID, &currentVersion, &officialURL, &openingAt)
	if err == sql.ErrNoRows {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "notice_not_found"})
		return
	}
	if err != nil {
		s.logger.Error("opening result: notice lookup failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if noticeType != "procurement" {
		writeJSON(w, http.StatusOK, map[string]any{"status": "NOT_APPLICABLE", "statusLabel": "해당 없음"})
		return
	}

	// raw_content에서 bidNtceNo/차수/업무유형을 읽는다(external_notice_id는 차수를 안 담는다).
	bidNtceNo, bidNtceOrd := strings.TrimSpace(externalID), ""
	bt := scsbid.BusinessService
	if versionID, verr := s.currentVersionID(ctx, id, currentVersion); verr == nil {
		var raw string
		if s.db.QueryRowContext(ctx, `SELECT rd.raw_content FROM notice_versions nv JOIN raw_documents rd ON rd.id = nv.raw_document_id WHERE nv.id = $1`, versionID).Scan(&raw) == nil && raw != "" {
			var f struct {
				BidNtceNo  string `json:"bidNtceNo"`
				BidNtceOrd string `json:"bidNtceOrd"`
			}
			if json.Unmarshal([]byte(raw), &f) == nil {
				if strings.TrimSpace(f.BidNtceNo) != "" {
					bidNtceNo = strings.TrimSpace(f.BidNtceNo)
				}
				bidNtceOrd = strings.TrimSpace(f.BidNtceOrd)
			}
			bt = businessTypeFromRaw(raw)
		}
	}
	var openingAtPtr *time.Time
	if openingAt.Valid {
		t := openingAt.Time
		openingAtPtr = &t
	}
	now := time.Now()

	// 개찰 전 — API/DB 없이 즉시 응답(쿼터 절약). 개찰 예정 시각을 모르면 API로 확인한다.
	if openingAtPtr != nil && openingAtPtr.After(now) {
		writeJSON(w, http.StatusOK, openingResultDTO{
			Status: openingBeforeOpening, StatusLabel: openingStatusLabels[openingBeforeOpening], BusinessType: string(bt),
			BidNtceNo: bidNtceNo, BidNtceOrd: bidNtceOrd, OpeningAt: openingAtPtr,
			Participants: []openingBidderDTO{}, PreliminaryPrices: []openingPriceDTO{}, Rounds: []openingRoundDTO{}, OfficialURL: officialURL.String,
		})
		return
	}
	if bidNtceNo == "" {
		writeJSON(w, http.StatusOK, map[string]any{"status": "UNAVAILABLE", "statusLabel": "개찰결과 조회 불가", "fetchError": "공고번호를 확인할 수 없습니다."})
		return
	}

	cached, cerr := s.loadOpeningCache(ctx, id)
	if cerr != nil {
		s.logger.Error("opening result: cache load failed", "error", cerr)
	}
	// identity(공고번호·차수·업무유형)가 현재 공고와 다르면 캐시를 통째로 무시한다 — fresh 판정·stale
	// fallback·키 미설정 경로 어디서도 과거 차수 결과를 돌려주지 않는다. row 자체는 삭제하지 않고 아래
	// 재조회 결과로 같은 notice row를 새 identity 기준으로 덮어쓴다(saveOpeningCache ON CONFLICT).
	if cached != nil && !cached.matchesIdentity(bidNtceNo, bidNtceOrd, bt) {
		s.logger.Info("opening result: cache identity mismatch, refetching", "noticeId", id,
			"cachedNo", cached.bidNtceNo, "cachedOrd", cached.bidNtceOrd, "cachedType", cached.businessType,
			"currentNo", bidNtceNo, "currentOrd", bidNtceOrd, "currentType", string(bt))
		cached = nil
	}
	fresh := cached != nil && now.Before(cached.nextCheckAt)
	if fresh {
		cached.dto.OfficialURL = officialURL.String
		writeJSON(w, http.StatusOK, cached.dto)
		return
	}
	if s.scsbidSource == nil {
		if cached != nil && cached.dto != nil {
			cached.dto.Stale = true
			cached.dto.OfficialURL = officialURL.String
			writeJSON(w, http.StatusOK, cached.dto)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "UNAVAILABLE", "statusLabel": "개찰결과 조회 불가", "fetchError": "나라장터 연동 키가 설정되지 않았습니다.", "officialUrl": officialURL.String})
		return
	}

	// 동시 요청 dedup — 먼저 온 요청이 끝날 때까지 기다렸다가 캐시를 읽는다.
	ch, mine := acquireOpeningInflight(id)
	if !mine {
		select {
		case <-ch:
		case <-ctx.Done():
			writeJSON(w, http.StatusOK, map[string]any{"status": "UNAVAILABLE", "statusLabel": "개찰결과 조회 불가", "fetchError": "요청이 취소되었습니다."})
			return
		}
		if c2, err := s.loadOpeningCache(ctx, id); err == nil && c2.matchesIdentity(bidNtceNo, bidNtceOrd, bt) {
			c2.dto.OfficialURL = officialURL.String
			writeJSON(w, http.StatusOK, c2.dto)
			return
		}
	} else {
		defer releaseOpeningInflight(id, ch)
	}

	fctx, cancel := context.WithTimeout(ctx, openingFetchTimeout)
	defer cancel()
	bundle, ferr := fetchOpeningBundle(fctx, s.scsbidSource, bt, bidNtceNo, bidNtceOrd)
	if ferr != nil && len(bundle.List) == 0 {
		// 목록 자체를 못 가져옴 — 마지막 캐시가 있으면 stale로, 없으면 fetchError만 담아 200.
		s.logger.Warn("opening result: fetch failed", "noticeId", id, "error", ferr)
		if cached != nil && cached.dto != nil {
			cached.dto.Stale = true
			cached.dto.FetchError = "개찰결과를 새로 불러오지 못했습니다."
			cached.dto.OfficialURL = officialURL.String
			// 다음 재시도를 15분 뒤로.
			_ = s.saveOpeningCache(ctx, id, bt, cached.dto, now.Add(openingTTLError), ferr.Error())
			writeJSON(w, http.StatusOK, cached.dto)
			return
		}
		writeJSON(w, http.StatusOK, openingResultDTO{
			Status: "UNAVAILABLE", StatusLabel: "개찰결과 조회 불가", BusinessType: string(bt), BidNtceNo: bidNtceNo, BidNtceOrd: bidNtceOrd,
			OpeningAt: openingAtPtr, Participants: []openingBidderDTO{}, PreliminaryPrices: []openingPriceDTO{}, Rounds: []openingRoundDTO{},
			FetchError: "개찰결과를 불러오는 중 문제가 발생했습니다.", OfficialURL: officialURL.String,
		})
		return
	}
	dto, next := buildOpeningResult(bundle, bt, bidNtceNo, bidNtceOrd, openingAtPtr, now)
	fetchErrText := ""
	if ferr != nil {
		// 부분 실패(순위/예가 등 일부만) — 결과는 내려주되 곧 재조회.
		fetchErrText = ferr.Error()
		next = now.Add(openingTTLError)
		dto.FetchError = "일부 개찰 정보를 불러오지 못했습니다."
	}
	t := now
	dto.FetchedAt = &t
	if err := s.saveOpeningCache(ctx, id, bt, dto, next, fetchErrText); err != nil {
		s.logger.Error("opening result: cache save failed", "error", err)
	}
	dto.OfficialURL = officialURL.String
	writeJSON(w, http.StatusOK, dto)
}
