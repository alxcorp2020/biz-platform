package bizinfo

import (
	"context"
	"encoding/json"
	"strings"
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

// TestParseReqErr verifies parsing of the real error envelope bizinfo
// returned for a live curl against the actual endpoint on 2026-08-01 (no
// key, then a dummy key) — see package doc comment.
func TestParseReqErr(t *testing.T) {
	cases := []string{
		`{"reqErr":"인증키를 입력해주세요."}`,
		`{"reqErr":"존재하지 않는 인증키 입니다."}`,
	}
	for _, raw := range cases {
		var envelope apiEnvelope
		if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
			t.Fatalf("unmarshal %q: %v", raw, err)
		}
		if envelope.ReqErr == "" {
			t.Errorf("expected ReqErr to be set for %q", raw)
		}
	}
}

// exampleResponseArrayJSON — 2026-08-08 실 엔드포인트가 내려주는 형태: jsonArray가
// **배열**(문서 예시의 객체가 아니라)이고, 첫 원소가 채널 메타 + item을 담는다.
// 운영 로그 "cannot unmarshal array into ... jsonArray of type struct{Item ...}"로
// 확인된 실제 형태를 반영한다.
const exampleResponseArrayJSON = `{"jsonArray":[{
	"title":"기업마당 지원사업정보",
	"link":"https://www.bizinfo.go.kr/web/lay1/bbs/S1T122C128/AS/74/list.do",
	"item":[{
		"totCnt":1435,
		"pblancNm":"착한임대인 장관 표창 신청 연장 공고",
		"pblancId":"PBLN_000000000080236",
		"jrsdInsttNm":"중소벤처기업부",
		"reqstBeginEndDe":"20220727 ~ 20220930"
	}]
}]}`

func TestParseExampleResponse(t *testing.T) {
	// 객체 형태(문서 예시)와 배열 형태(실 엔드포인트) 둘 다 동일하게 파싱돼야 한다.
	for name, body := range map[string]string{"object": exampleResponseJSON, "array": exampleResponseArrayJSON} {
		var envelope apiEnvelope
		if err := json.Unmarshal([]byte(body), &envelope); err != nil {
			t.Fatalf("%s: unmarshal: %v", name, err)
		}
		items := extractBizinfoItems(envelope.JsonArray)
		if len(items) != 1 {
			t.Fatalf("%s: expected 1 item, got %d", name, len(items))
		}
		var it bizinfoItem
		if err := json.Unmarshal(items[0], &it); err != nil {
			t.Fatalf("%s: decode item: %v", name, err)
		}
		if it.PblancId != "PBLN_000000000080236" {
			t.Errorf("%s: PblancId = %q", name, it.PblancId)
		}
		if it.PblancNm != "착한임대인 장관 표창 신청 연장 공고" {
			t.Errorf("%s: PblancNm = %q", name, it.PblancNm)
		}
		if it.JrsdInsttNm != "중소벤처기업부" {
			t.Errorf("%s: JrsdInsttNm = %q", name, it.JrsdInsttNm)
		}
		if it.TotCnt != 1435 {
			t.Errorf("%s: TotCnt = %d, want 1435", name, it.TotCnt)
		}
		if it.ReqstBeginEndDe != "20220727 ~ 20220930" {
			t.Errorf("%s: ReqstBeginEndDe = %q", name, it.ReqstBeginEndDe)
		}
	}

	// 방어: jsonArray 원소가 채널 래퍼 없이 공고 항목 자체인 배열도 흡수한다.
	direct := `{"jsonArray":[{"pblancId":"PBLN_1","pblancNm":"직접배열","totCnt":7}]}`
	var env2 apiEnvelope
	if err := json.Unmarshal([]byte(direct), &env2); err != nil {
		t.Fatalf("direct: unmarshal: %v", err)
	}
	items := extractBizinfoItems(env2.JsonArray)
	var d0 bizinfoItem
	if len(items) != 1 || json.Unmarshal(items[0], &d0) != nil || d0.PblancId != "PBLN_1" {
		t.Fatalf("direct-array form not parsed: %+v", items)
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
	items := extractBizinfoItems(envelope.JsonArray)
	if len(items) == 0 {
		t.Fatalf("no items extracted from example")
	}
	// 원본 RawMessage를 그대로 raw_content로 쓴다(raw 보존 방식과 동일).
	return collector.RawDocument{RawContent: string(items[0])}
}

// TestBizinfo_RawPreservation — struct에 없는 필드도 extractBizinfoItems가
// 원본 RawMessage로 보존해야 한다(json.Marshal(struct) 소실 문제 방지, 22.3).
func TestBizinfo_RawPreservation(t *testing.T) {
	body := `{"jsonArray":[{"pblancId":"PBLN_RAW","pblancNm":"보존테스트","totCnt":1,"미래신규필드":"보존되어야함","trgetNm":"창업"}]}`
	var env apiEnvelope
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	items := extractBizinfoItems(env.JsonArray)
	if len(items) != 1 {
		t.Fatalf("expected 1, got %d", len(items))
	}
	raw := string(items[0])
	if !strings.Contains(raw, "미래신규필드") || !strings.Contains(raw, "보존되어야함") {
		t.Errorf("struct에 없는 필드가 raw에서 소실됨: %s", raw)
	}
}

// TestBizinfo_NewFieldsNormalize — 100건 실측 기반 신규 10필드가 SupportDetail로
// 매핑되고, HTML→평문/조회수/신청기간(YYYY-MM-DD)이 올바르게 처리되는지(22.4).
func TestBizinfo_NewFieldsNormalize(t *testing.T) {
	item := `{
      "pblancId":"PBLN_NEW","pblancNm":"신규필드공고","pblancUrl":"https://www.bizinfo.go.kr/x",
      "jrsdInsttNm":"중소벤처기업부","excInsttNm":"창업진흥원","pldirSportRealmLclasCodeNm":"수출",
      "pldirSportRealmMlsfcCodeNm":"해외진출준비","reqstBeginEndDe":"2026-08-07 ~ 2026-08-12",
      "creatPnttm":"2026-08-07 14:43:49","updtPnttm":"2026-08-07 15:22:48",
      "trgetNm":"창업벤처","bsnsSumryCn":"<p>해외시장 &amp; 진출 <br>지원사업</p>",
      "reqstMthPapersCn":"온라인 접수 (K-Startup)","refrncNm":"창업진흥원 042-720-4561",
      "rceptEngnHmpgUrl":"https://www.k-startup.go.kr/x","hashtags":"수출,창업,서울,부산","inqireCo":"4103"
    }`
	n, err := New("dummy").Normalize(context.Background(), collector.RawDocument{RawContent: item})
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	// 기존 매핑 회귀(22.5)
	if n.OrganizationName != "중소벤처기업부" || n.DepartmentName != "창업진흥원" || n.Industry != "수출" {
		t.Errorf("기존 매핑 회귀: org=%q dept=%q ind=%q", n.OrganizationName, n.DepartmentName, n.Industry)
	}
	if n.ApplicationEndAt == nil || n.ApplicationEndAt.Format("2006-01-02") != "2026-08-12" {
		t.Errorf("신청기간(하이픈 형식) 파싱 실패: %v", n.ApplicationEndAt)
	}
	d := n.SupportDetail
	if d == nil {
		t.Fatal("SupportDetail nil")
	}
	if d.SupportTarget != "창업벤처" || d.ApplicationMethod != "온라인 접수 (K-Startup)" ||
		d.ReferenceContact != "창업진흥원 042-720-4561" || d.CategoryMajor != "수출" || d.CategoryMiddle != "해외진출준비" ||
		d.ApplicationURL != "https://www.k-startup.go.kr/x" || d.Hashtags != "수출,창업,서울,부산" {
		t.Errorf("SupportDetail 매핑 오류: %+v", d)
	}
	if d.InquiryCount == nil || *d.InquiryCount != 4103 {
		t.Errorf("InquiryCount = %v", d.InquiryCount)
	}
	if d.SourceUpdatedAt == nil || d.SourceUpdatedAt.Format("2006-01-02 15:04") != "2026-08-07 15:22" {
		t.Errorf("SourceUpdatedAt = %v", d.SourceUpdatedAt)
	}
	// HTML 원본 보존 + 평문 변환(태그 제거, 엔티티 복원)
	if d.BusinessSummaryHTML != "<p>해외시장 &amp; 진출 <br>지원사업</p>" {
		t.Errorf("HTML 원본 미보존: %q", d.BusinessSummaryHTML)
	}
	if strings.Contains(d.BusinessSummaryText, "<") || !strings.Contains(d.BusinessSummaryText, "해외시장 & 진출") {
		t.Errorf("HTML→평문 변환 오류: %q", d.BusinessSummaryText)
	}
}

// TestBizinfo_FetchAttachments_BothRoles — 별첨(flpthNm)과 본문출력(printFlpthNm)이
// 서로 다른 파일로 각각 역할과 함께 반환되는지(22.10~22.12).
func TestBizinfo_FetchAttachments_BothRoles(t *testing.T) {
	item := `{"pblancId":"P","flpthNm":"https://x/att","fileNm":"신청서.hwp",
	          "printFlpthNm":"https://x/doc","printFileNm":"공고문.pdf"}`
	atts, err := New("x").FetchAttachments(context.Background(), collector.RawDocument{RawContent: item})
	if err != nil {
		t.Fatalf("FetchAttachments: %v", err)
	}
	if len(atts) != 2 {
		t.Fatalf("첨부 %d개(기대 2)", len(atts))
	}
	byRole := map[string]collector.Attachment{}
	for _, a := range atts {
		byRole[a.Role] = a
	}
	att := byRole[RoleSupportAttachment]
	doc := byRole[RoleSupportPrintDocument]
	if att.DownloadURL != "https://x/att" || att.OriginalFilename != "신청서.hwp" || att.FileType != "hwp" {
		t.Errorf("별첨 오류: %+v", att)
	}
	if doc.DownloadURL != "https://x/doc" || doc.OriginalFilename != "공고문.pdf" || doc.FileType != "pdf" {
		t.Errorf("본문출력 오류: %+v", doc)
	}
	if att.DownloadURL == doc.DownloadURL {
		t.Error("별첨과 본문출력이 같은 URL로 반환됨(중복)")
	}

	// 별첨만 있고 본문출력 없는 경우 → 1개.
	only := `{"pblancId":"P","flpthNm":"https://x/att","fileNm":"a.zip"}`
	atts2, _ := New("x").FetchAttachments(context.Background(), collector.RawDocument{RawContent: only})
	if len(atts2) != 1 || atts2[0].Role != RoleSupportAttachment {
		t.Errorf("별첨만 케이스 오류: %+v", atts2)
	}
}

// TestBizinfo_FetchAttachments_MultiFile — 🚨 2026-08-09 다중 별첨 '@' 분할 버그 수정 검증.
func TestBizinfo_FetchAttachments_MultiFile(t *testing.T) {
	ctx := context.Background()

	// Case 2: URL 3개@ / 파일명 3개@ → 첨부 3건 정확히 pairing.
	item := `{"pblancId":"P","flpthNm":"https://x/a@https://x/b@https://x/c","fileNm":"신청서.hwp@계획서.hwpx@참고.xlsx"}`
	atts, err := New("x").FetchAttachments(ctx, collector.RawDocument{RawContent: item})
	if err != nil {
		t.Fatalf("FetchAttachments: %v", err)
	}
	if len(atts) != 3 {
		t.Fatalf("Case2 첨부 %d개(기대 3)", len(atts))
	}
	want := []struct{ url, name, ext string }{
		{"https://x/a", "신청서.hwp", "hwp"},
		{"https://x/b", "계획서.hwpx", "hwpx"},
		{"https://x/c", "참고.xlsx", "xlsx"},
	}
	for i, w := range want {
		if atts[i].DownloadURL != w.url || atts[i].OriginalFilename != w.name || atts[i].FileType != w.ext || atts[i].Role != RoleSupportAttachment {
			t.Errorf("Case2[%d] 오류: %+v", i, atts[i])
		}
	}

	// Case 3: URL 3개 / 파일명 2개 → panic 없이 3건, 3번째는 fallback 파일명.
	item = `{"pblancId":"P","flpthNm":"https://x/a@https://x/b@https://x/c","fileNm":"신청서.hwp@계획서.hwpx"}`
	atts, err = New("x").FetchAttachments(ctx, collector.RawDocument{RawContent: item})
	if err != nil {
		t.Fatalf("Case3 FetchAttachments: %v", err)
	}
	if len(atts) != 3 {
		t.Fatalf("Case3 첨부 %d개(기대 3)", len(atts))
	}
	if atts[2].DownloadURL != "https://x/c" || atts[2].OriginalFilename != "첨부파일" || atts[2].FileType != "" {
		t.Errorf("Case3 fallback 오류: %+v", atts[2])
	}

	// Case 4: 중간 빈 URL(@@) → 빈 항목 skip, 2건만.
	item = `{"pblancId":"P","flpthNm":"https://x/a@@https://x/c","fileNm":"a.hwp@무시@c.pdf"}`
	atts, err = New("x").FetchAttachments(ctx, collector.RawDocument{RawContent: item})
	if err != nil {
		t.Fatalf("Case4 FetchAttachments: %v", err)
	}
	if len(atts) != 2 {
		t.Fatalf("Case4 첨부 %d개(기대 2): %+v", len(atts), atts)
	}
	if atts[0].DownloadURL != "https://x/a" || atts[1].DownloadURL != "https://x/c" {
		t.Errorf("Case4 URL 오류: %+v", atts)
	}

	// Case 5: printFlpthNm 다중 → SUPPORT_PRINT_DOCUMENT 다건.
	item = `{"pblancId":"P","printFlpthNm":"https://x/d1@https://x/d2","printFileNm":"공고1.pdf@공고2.hwp"}`
	atts, err = New("x").FetchAttachments(ctx, collector.RawDocument{RawContent: item})
	if err != nil {
		t.Fatalf("Case5 FetchAttachments: %v", err)
	}
	if len(atts) != 2 {
		t.Fatalf("Case5 첨부 %d개(기대 2)", len(atts))
	}
	for i, a := range atts {
		if a.Role != RoleSupportPrintDocument {
			t.Errorf("Case5[%d] role=%s(기대 %s)", i, a.Role, RoleSupportPrintDocument)
		}
	}

	// dedup: 같은 필드에 동일 URL이 두 번 → url+role 조합 1건만.
	item = `{"pblancId":"P","flpthNm":"https://x/dup@https://x/dup","fileNm":"a.hwp@b.hwp"}`
	atts, err = New("x").FetchAttachments(ctx, collector.RawDocument{RawContent: item})
	if err != nil {
		t.Fatalf("dedup FetchAttachments: %v", err)
	}
	if len(atts) != 1 {
		t.Errorf("dedup 첨부 %d개(기대 1): %+v", len(atts), atts)
	}
}

// TestBizinfo_ReqstBeginEndDe_BothFormats — 신형(YYYY-MM-DD)·구형(YYYYMMDD) 모두.
func TestBizinfo_ReqstBeginEndDe_BothFormats(t *testing.T) {
	for _, v := range []string{"2026-08-07 ~ 2026-08-12", "20260807 ~ 20260812"} {
		s, e, err := parseReqstBeginEndDe(v)
		if err != nil || s.Format("2006-01-02") != "2026-08-07" || e.Format("2006-01-02") != "2026-08-12" {
			t.Errorf("%q → s=%v e=%v err=%v", v, s, e, err)
		}
	}
}
