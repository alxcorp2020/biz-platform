package bizinfo

import (
	"context"
	"encoding/json"
	"testing"

	"biz-platform/collector/internal/collector"
)

// exampleResponseJSON is the exact response example bizinfo.go.kr publishes
// on its API detail page (https://www.bizinfo.go.kr/apiDetail.do?id=bizinfoApi),
// reconstructed field-for-field from that page — not invented. The live
// endpoint has never been called (no crtfcKey available in this environment),
// so this is the only verification available for the parsing logic until a
// real key is provisioned.
const exampleResponseJSON = `{"jsonArray":{
	"title":"기업마당 지원사업정보",
	"link":"https://www.bizinfo.go.kr/web/lay1/bbs/S1T122C128/AS/74/list.do",
	"item":[{
		"title":"착한임대인 장관 표창 신청 연장 공고",
		"link":"https://www.bizinfo.go.kr/web/lay1/bbs/S1T122C128/AS/74/view.do?pblancId=PBLN_000000000080236",
		"seq":"PBLN_000000000080236",
		"author":"중소벤처기업부",
		"excInsttNm":"지방중소벤처기업청",
		"lcategory":"경영",
		"pubDate":"2022-09-02 15:38:29",
		"reqstDt":"20220727 ~ 20220930",
		"trgetNm":"중소기업",
		"inqireCo":43,
		"flpthNm":"https://www.bizinfo.go.kr/cmm/fms/getImageFile.do?atchFileId=FILE_000000000613641&fileSn=0",
		"fileNm":"2022년 대한민국 메이커 스타 참가자모집 공고.pdf",
		"hashTags":"2022,금융,충북,대전,중소벤처기업부",
		"totCnt":1435,
		"pblancNm":"착한임대인 장관 표창 신청 연장 공고",
		"pblancUrl":"https://www.bizinfo.go.kr/web/lay1/bbs/S1T122C128/AS/74/view.do?pblancId=PBLN_000000000080236",
		"pblancId":"PBLN_000000000080236",
		"jrsdInsttNm":"중소벤처기업부",
		"bsnsSumryCn":"코로나19라는 힘든 상황속에서 소상공인에게 자발적으로 임대료를 인하한 임대인을 '착한임대인'으로 선정하는 사업입니다.",
		"reqstMthPapersCn":"",
		"refrncNm":"",
		"rceptEngnHmpgUrl":"",
		"pldirSportRealmLclasCodeNm":"경영",
		"creatPnttm":"2022-09-02 15:38:29",
		"reqstBeginEndDe":"20220727 ~ 20220930"
	}]
}}`

func TestParseExampleResponse(t *testing.T) {
	var envelope apiEnvelope
	if err := json.Unmarshal([]byte(exampleResponseJSON), &envelope); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(envelope.JsonArray.Item) != 1 {
		t.Fatalf("expected 1 item, got %d", len(envelope.JsonArray.Item))
	}
	it := envelope.JsonArray.Item[0]
	if it.PblancId != "PBLN_000000000080236" {
		t.Errorf("PblancId = %q", it.PblancId)
	}
	if it.PblancNm != "착한임대인 장관 표창 신청 연장 공고" {
		t.Errorf("PblancNm = %q", it.PblancNm)
	}
	if it.JrsdInsttNm != "중소벤처기업부" {
		t.Errorf("JrsdInsttNm = %q", it.JrsdInsttNm)
	}
	if it.TotCnt != 1435 {
		t.Errorf("TotCnt = %d, want 1435", it.TotCnt)
	}
	if it.ReqstBeginEndDe != "20220727 ~ 20220930" {
		t.Errorf("ReqstBeginEndDe = %q", it.ReqstBeginEndDe)
	}
}

func TestFlexibleInt_AcceptsStringOrNumber(t *testing.T) {
	var asNumber flexibleInt
	if err := json.Unmarshal([]byte(`1435`), &asNumber); err != nil {
		t.Fatalf("unmarshal number: %v", err)
	}
	if asNumber != 1435 {
		t.Errorf("asNumber = %d, want 1435", asNumber)
	}

	var asString flexibleInt
	if err := json.Unmarshal([]byte(`"1435"`), &asString); err != nil {
		t.Fatalf("unmarshal string: %v", err)
	}
	if asString != 1435 {
		t.Errorf("asString = %d, want 1435", asString)
	}
}

func TestParseReqstBeginEndDe(t *testing.T) {
	start, end, err := parseReqstBeginEndDe("20220727 ~ 20220930")
	if err != nil {
		t.Fatalf("parseReqstBeginEndDe: %v", err)
	}
	if start.Format("2006-01-02") != "2022-07-27" {
		t.Errorf("start = %v", start)
	}
	if end.Format("2006-01-02") != "2022-09-30" {
		t.Errorf("end = %v", end)
	}
}

func TestNormalize(t *testing.T) {
	src := New("dummy-key")
	doc := rawDocumentFromExample(t)
	n, err := src.Normalize(context.Background(), doc)
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if n.NoticeType != "support_program" {
		t.Errorf("NoticeType = %q", n.NoticeType)
	}
	if n.Title != "착한임대인 장관 표창 신청 연장 공고" {
		t.Errorf("Title = %q", n.Title)
	}
	if n.ApplicationStartAt == nil || n.ApplicationStartAt.Format("2006-01-02") != "2022-07-27" {
		t.Errorf("ApplicationStartAt = %v", n.ApplicationStartAt)
	}
	if n.ApplicationEndAt == nil || n.ApplicationEndAt.Format("2006-01-02") != "2022-09-30" {
		t.Errorf("ApplicationEndAt = %v", n.ApplicationEndAt)
	}
}

func rawDocumentFromExample(t *testing.T) collector.RawDocument {
	t.Helper()
	var envelope apiEnvelope
	if err := json.Unmarshal([]byte(exampleResponseJSON), &envelope); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	raw, err := json.Marshal(envelope.JsonArray.Item[0])
	if err != nil {
		t.Fatalf("marshal item: %v", err)
	}
	return collector.RawDocument{RawContent: string(raw)}
}
