package api

import (
	"testing"
	"time"

	"biz-platform/collector/internal/collector/sources/scsbid"
)

// 개찰 상태 모델 매핑 — 실호출(2026-08-16) 값 기반 fixture. 시간 경과가 아니라 결과 행 유무로 판단한다.

func TestOpeningResult_PendingWhenNoRows(t *testing.T) {
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	past := now.Add(-2 * time.Hour)
	dto, next := buildOpeningResult(openingRawBundle{}, scsbid.BusinessService, "R26BK01680111", "000", &past, now)
	if dto.Status != openingPending || dto.StatusLabel != "개찰 결과 등록 대기" {
		t.Fatalf("status %s/%s", dto.Status, dto.StatusLabel)
	}
	if next.Sub(now) != openingTTLPending {
		t.Fatalf("ttl %v", next.Sub(now))
	}
	if len(dto.Participants) != 0 || dto.TopBidder != nil || dto.Winner != nil {
		t.Fatalf("pending must not invent bidders: %+v", dto)
	}
}

func TestOpeningResult_OpenedWaitingAward(t *testing.T) {
	now := time.Now()
	one := "1"
	b := openingRawBundle{
		List: []scsbid.OpeningListItem{{BidNtceNo: "R26BK01674070", BidNtceOrd: "000", BidClsfcNo: "0", RbidNo: "000", OpengDt: "2026-08-14 15:00:00", PrtcptCnum: "25", ProgrsDivCdNm: "개찰완료", OpengCorpInfo: "주식회사 그린환경산업^6158193504^김성원^14433930^90.069", RsrvtnPrceFileExistnceYn: "Y", InptDt: "2026-08-14 15:03:02"}},
		Bidders: []scsbid.OpeningBidder{
			{RbidNo: "000", OpengRank: "2", PrcbdrNm: "대부개발 주식회사", PrcbdrBizno: "6088119466", BidprcAmt: "14465000", Bidprcrt: "90.263", Rmrk: "정상"},
			{RbidNo: "000", OpengRank: one, PrcbdrNm: "주식회사 그린환경산업", PrcbdrBizno: "6158193504", BidprcAmt: "14433930", Bidprcrt: "90.069", Rmrk: "정상", BidprcDt: "2026-08-13 15:42:11"},
			{RbidNo: "000", OpengRank: "", PrcbdrNm: "(주)하나전설", PrcbdrBizno: "1234567890", BidprcAmt: "6860833500", Bidprcrt: "88.274", Rmrk: "낙찰하한선 미달"},
		},
		Prices: []scsbid.PreliminaryPrice{
			{RbidNo: "000", Plnprc: "16025300", Bssamt: "16119000", TotRsrvtnPrceNum: "15", CompnoRsrvtnPrceSno: "2", BsisPlnprc: "15860300", DrwtYn: "Y", DrwtNum: "6", RlOpengDt: "2026-08-14 15:03:02"},
			{RbidNo: "000", Plnprc: "16025300", Bssamt: "16119000", TotRsrvtnPrceNum: "15", CompnoRsrvtnPrceSno: "1", BsisPlnprc: "16400800", DrwtYn: "N", DrwtNum: "3", RlOpengDt: "2026-08-14 15:03:02"},
		},
	}
	dto, next := buildOpeningResult(b, scsbid.BusinessService, "R26BK01674070", "000", nil, now)
	if dto.Status != openingOpenedWaitingAward || dto.StatusLabel != "개찰완료 · 낙찰자 결정 대기" {
		t.Fatalf("status %s/%s", dto.Status, dto.StatusLabel)
	}
	if dto.Winner != nil {
		t.Fatalf("no winner before award confirmation")
	}
	if dto.TopBidder == nil || dto.TopBidder.Name != "주식회사 그린환경산업" || dto.TopBidder.Rank == nil || *dto.TopBidder.Rank != 1 || dto.TopBidder.Rate != "90.069" {
		t.Fatalf("top bidder: %+v", dto.TopBidder)
	}
	if dto.TopBidder.BusinessNumberMasked != "615-81-*****" {
		t.Fatalf("mask: %q", dto.TopBidder.BusinessNumberMasked)
	}
	// 정렬: 순위 1,2 → 부적격(순위 없음) 마지막 + 사유 유지
	if len(dto.Participants) != 3 || *dto.Participants[0].Rank != 1 || *dto.Participants[1].Rank != 2 || dto.Participants[2].Rank != nil || !dto.Participants[2].Disqualified || dto.Participants[2].Remark != "낙찰하한선 미달" {
		t.Fatalf("participants order: %+v", dto.Participants)
	}
	if dto.Participants[0].Remark != "" {
		t.Fatalf("정상 remark must be hidden")
	}
	if dto.PlannedPrice == nil || *dto.PlannedPrice != 16025300 || dto.BaseAmount == nil || *dto.BaseAmount != 16119000 || dto.PreliminaryTotal == nil || *dto.PreliminaryTotal != 15 {
		t.Fatalf("prices summary: %+v", dto)
	}
	if len(dto.PreliminaryPrices) != 2 || dto.PreliminaryPrices[0].Seq != 1 || !dto.PreliminaryPrices[1].Drawn {
		t.Fatalf("preliminary prices: %+v", dto.PreliminaryPrices)
	}
	if dto.ParticipantCount == nil || *dto.ParticipantCount != 25 || dto.RoundIndex != 1 || dto.RoundNo != "000" {
		t.Fatalf("summary: %+v", dto)
	}
	if dto.ActualOpeningAt == nil {
		t.Fatalf("actual opening at from rlOpengDt expected")
	}
	if next.Sub(now) != openingTTLWaitingAward {
		t.Fatalf("ttl %v", next.Sub(now))
	}
}

func TestOpeningResult_Awarded(t *testing.T) {
	now := time.Now()
	b := openingRawBundle{
		List:   []scsbid.OpeningListItem{{BidNtceNo: "R26BK01683359", BidNtceOrd: "000", RbidNo: "000", OpengDt: "2026-08-14 16:00:00", PrtcptCnum: "3", ProgrsDivCdNm: "개찰완료", OpengCorpInfo: "주식회사 동양기술단^1234567890^홍길동^18879000^91.048"}},
		Awards: []scsbid.AwardRecord{{BidNtceNo: "R26BK01683359", BidNtceOrd: "000", RbidNo: "000", BidwinnrNm: "주식회사 동양기술단", BidwinnrBizno: "1234567890", SucsfbidAmt: "18879000", SucsfbidRate: "91.048", RlOpengDt: "2026-08-14 16:00:00", FnlSucsfDate: "2026-08-14", PrtcptCnum: "3"}},
	}
	dto, next := buildOpeningResult(b, scsbid.BusinessService, "R26BK01683359", "000", nil, now)
	if dto.Status != openingAwarded || dto.Winner == nil || dto.Winner.Name != "주식회사 동양기술단" || dto.Winner.FinalAwardDate != "2026-08-14" || dto.Winner.Rate != "91.048" || *dto.Winner.Amount != 18879000 {
		t.Fatalf("awarded: %+v %+v", dto.Status, dto.Winner)
	}
	if dto.Winner.BusinessNumberMasked != "123-45-*****" {
		t.Fatalf("winner mask %q", dto.Winner.BusinessNumberMasked)
	}
	// 개찰순위 API가 비어도 목록 opengCorpInfo로 1순위는 채운다.
	if dto.TopBidder == nil || dto.TopBidder.Name != "주식회사 동양기술단" || dto.TopBidder.Rate != "91.048" || *dto.TopBidder.Amount != 18879000 {
		t.Fatalf("top from opengCorpInfo: %+v", dto.TopBidder)
	}
	if next.Sub(now) != openingTTLFinal {
		t.Fatalf("ttl %v", next.Sub(now))
	}
}

// 다른 차수의 낙찰 행은 무시(정정/재공고 결과 섞임 방지).
func TestOpeningResult_AwardOfOtherOrdIgnored(t *testing.T) {
	b := openingRawBundle{
		List:   []scsbid.OpeningListItem{{BidNtceNo: "X", BidNtceOrd: "001", RbidNo: "000", ProgrsDivCdNm: "개찰완료", PrtcptCnum: "2"}, {BidNtceNo: "X", BidNtceOrd: "000", RbidNo: "000", ProgrsDivCdNm: "유찰"}},
		Awards: []scsbid.AwardRecord{{BidNtceNo: "X", BidNtceOrd: "000", RbidNo: "000", BidwinnrNm: "옛차수업체"}},
	}
	dto, _ := buildOpeningResult(b, scsbid.BusinessService, "X", "001", nil, time.Now())
	if dto.Status != openingOpenedWaitingAward || dto.Winner != nil || len(dto.Rounds) != 1 {
		t.Fatalf("ord filter: %+v", dto)
	}
}

func TestOpeningResult_Failed(t *testing.T) {
	b := openingRawBundle{
		List:  []scsbid.OpeningListItem{{BidNtceNo: "R26BK01675192", BidNtceOrd: "000", RbidNo: "000", ProgrsDivCdNm: "유찰", PrtcptCnum: "1", OpengDt: "2026-08-12 11:00:00"}},
		Fails: []scsbid.FailingInfo{{RbidNo: "000", NobidRsn: "단독응찰에 따른 유찰"}},
	}
	dto, next := buildOpeningResult(b, scsbid.BusinessService, "R26BK01675192", "000", nil, time.Now())
	if dto.Status != openingFailed || dto.Failing == nil || dto.Failing.Reason != "단독응찰에 따른 유찰" || dto.Winner != nil || dto.TopBidder != nil {
		t.Fatalf("failed: %+v %+v", dto.Status, dto.Failing)
	}
	if next.Sub(time.Now()) < openingTTLFinal-time.Minute {
		t.Fatalf("failed should be long ttl")
	}
}

// 최신 회차가 재입찰이고 다음 회차 결과가 없으면 REBID.
func TestOpeningResult_RebidInProgress(t *testing.T) {
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, kstLocation())
	b := openingRawBundle{
		List:   []scsbid.OpeningListItem{{BidNtceNo: "R26BK01667341", BidNtceOrd: "000", RbidNo: "000", ProgrsDivCdNm: "재입찰", PrtcptCnum: "0", OpengDt: "2026-08-12 11:00:00"}},
		Rebids: []scsbid.RebidInfo{{RbidNo: "001", RbidRsn: "단독응찰", BidClseDt: "2026-08-12 15:00:00", OpengDt: "2026-08-12 16:00:00"}},
	}
	dto, next := buildOpeningResult(b, scsbid.BusinessConstruction, "R26BK01667341", "000", nil, now)
	if dto.Status != openingRebid || dto.StatusLabel != "재입찰 진행 중" || dto.Rebid == nil || dto.Rebid.RoundNo != "001" || dto.Rebid.Reason != "단독응찰" {
		t.Fatalf("rebid: %+v %+v", dto.Status, dto.Rebid)
	}
	// 재입찰 개찰시각(16:00)이 now+1h(13:00)보다 뒤 → next_check는 그 시각
	if !next.Equal(time.Date(2026, 8, 12, 16, 0, 0, 0, kstLocation())) {
		t.Fatalf("rebid next check %v", next)
	}
}

// 1차 재입찰 → 2차 개찰완료 → 낙찰확정: 최신 회차(001) 기준으로 AWARDED, 이전 회차는 rounds에만.
func TestOpeningResult_RebidThenAwarded_NoConfusion(t *testing.T) {
	b := openingRawBundle{
		List: []scsbid.OpeningListItem{
			{BidNtceNo: "R26BK01664554", BidNtceOrd: "000", RbidNo: "001", ProgrsDivCdNm: "개찰완료", PrtcptCnum: "2", OpengCorpInfo: "신화항공여행사 주식회사^3048119695^강석규^28500000^97.839"},
			{BidNtceNo: "R26BK01664554", BidNtceOrd: "000", RbidNo: "000", ProgrsDivCdNm: "재입찰", PrtcptCnum: "2"},
		},
		Bidders: []scsbid.OpeningBidder{{RbidNo: "001", OpengRank: "1", PrcbdrNm: "신화항공여행사 주식회사", BidprcAmt: "28500000", Bidprcrt: "97.839", Rmrk: "정상"}, {RbidNo: "001", OpengRank: "2", PrcbdrNm: "무궁화 관광(주)", BidprcAmt: "28900000", Bidprcrt: "99.212", Rmrk: "정상"}},
		Awards:  []scsbid.AwardRecord{{BidNtceOrd: "000", RbidNo: "001", BidwinnrNm: "신화항공여행사 주식회사", SucsfbidAmt: "28500000", FnlSucsfDate: "2026-08-13"}},
		Rebids:  []scsbid.RebidInfo{{RbidNo: "001", RbidRsn: "견적제출 업체 모두 예가초과로 인한 재입찰 시행"}},
	}
	dto, _ := buildOpeningResult(b, scsbid.BusinessService, "R26BK01664554", "000", nil, time.Now())
	if dto.Status != openingAwarded || dto.RoundNo != "001" || dto.RoundIndex != 2 || dto.Winner == nil || dto.Winner.Name != "신화항공여행사 주식회사" {
		t.Fatalf("rebid→awarded: %s %s %d %+v", dto.Status, dto.RoundNo, dto.RoundIndex, dto.Winner)
	}
	if len(dto.Rounds) != 2 || dto.Rounds[0].RoundNo != "000" || dto.Rounds[0].Status != "재입찰" || dto.Rounds[1].Status != "개찰완료" {
		t.Fatalf("rounds: %+v", dto.Rounds)
	}
	if len(dto.Participants) != 2 || dto.Rebid == nil {
		t.Fatalf("participants of latest round only + rebid history kept: %+v", dto)
	}
}

func TestBusinessTypeFromRaw(t *testing.T) {
	cases := map[string]scsbid.BusinessType{
		`{"bidNtceNo":"A","srvceDivNm":"일반용역"}`:                           scsbid.BusinessService,
		`{"bidNtceNo":"B","mainCnsttyNm":"전기공사","cnstrtsiteRgnNm":"대전"}`:  scsbid.BusinessConstruction,
		`{"bidNtceNo":"C","dtilPrdctClsfcNo":"4111","prdctSpecNm":"분광기"}`: scsbid.BusinessGoods,
		`{"bidNtceNo":"D","bsnsDivNm":"물품"}`:                              scsbid.BusinessGoods,
		`not json`:                                                        scsbid.BusinessService,
	}
	for raw, want := range cases {
		if got := businessTypeFromRaw(raw); got != want {
			t.Errorf("%s: got %s want %s", raw, got, want)
		}
	}
}

func TestMaskBizno(t *testing.T) {
	if maskBizno("3258701326") != "325-87-*****" || maskBizno("325-87-01326") != "325-87-*****" || maskBizno("12") != "" {
		t.Fatalf("mask")
	}
}
