package scsbid

import (
	"encoding/json"
	"testing"
)

// Shapes below are copied from the real Swagger schema data.go.kr embeds on
// https://www.data.go.kr/data/15129397/openapi.do (extracted 2026-08-01) —
// not invented. The live endpoint currently returns 403 for this service key,
// so this test is the only verification available for the parsing logic
// until that access issue clears.

func TestItemsWrapper_NoData(t *testing.T) {
	var body apiBody
	if err := json.Unmarshal([]byte(`{"items":"","totalCount":"0"}`), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(body.Items.Items) != 0 {
		t.Fatalf("expected no items, got %d", len(body.Items.Items))
	}
}

func TestItemsWrapper_SingleObject(t *testing.T) {
	raw := `{"items":{"item":{"bidNtceNo":"20260101001","bidNtceOrd":"000","bidNtceNm":"테스트 용역","dminsttNm":"조달청","bidwinnrNm":"주식회사 테스트","sucsfbidAmt":"123000000","sucsfbidRate":"87.789","rlOpengDt":"2026-01-15 14:00:00"}},"totalCount":"1"}`
	var body apiBody
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(body.Items.Items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(body.Items.Items))
	}
	var rec AwardRecord
	if err := json.Unmarshal(body.Items.Items[0], &rec); err != nil {
		t.Fatalf("unmarshal record: %v", err)
	}
	if rec.BidNtceNo != "20260101001" || rec.DminsttNm != "조달청" || rec.BidwinnrNm != "주식회사 테스트" {
		t.Fatalf("unexpected record: %+v", rec)
	}
}

func TestItemsWrapper_Array(t *testing.T) {
	raw := `{"items":{"item":[{"bidNtceNo":"1"},{"bidNtceNo":"2"}]},"totalCount":"2"}`
	var body apiBody
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(body.Items.Items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(body.Items.Items))
	}
}
