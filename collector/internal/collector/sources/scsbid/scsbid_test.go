package scsbid

import (
	"encoding/json"
	"testing"
)

// TestItemsWrapper_NoData/_SingleObject/_Array below use the shapes copied
// from data.go.kr's embedded Swagger schema (extracted 2026-08-01) — kept as
// defensive fallback coverage(itemsWrapper still parses this shape if it
// ever occurs), but 2026-08-06 실측으로 밝혀졌듯 **이 문서 스펙은 실제
// 운영 키로 호출했을 때의 진짜 응답 모양이 아니었다**(items가 "item"
// 래퍼 없이 곧장 배열로 옴, totalCount는 문자열이 아니라 숫자) — 그
// 시점엔 서비스키가 403이라 실제 응답을 볼 수 없어 문서만 믿고 짠 것이
// 원인. TestItemsWrapper_RealResponseShape가 실제 검증된 형태다.

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

// TestItemsWrapper_RealResponseShape — 2026-08-06, 운영 서비스키로
// getScsbidListSttusServcPPSSrch를 직접 호출해 확인한 진짜 응답 모양
// (curl로 원본 JSON 그대로 캡처, 값만 테스트용으로 치환). items는 "item"
// 래퍼 없이 곧장 배열, totalCount는 따옴표 없는 숫자 — 이 두 가지가
// Swagger 문서 기반 추정과 달라서 "Render에서 낙찰이력 수집이 500으로
// 실패하던" 실제 원인이었다(운영 관리자 버튼 클릭 시 500 에러 신고로
// 발견, IP 제한이 아니라 순수 파싱 버그였음).
func TestItemsWrapper_RealResponseShape(t *testing.T) {
	raw := `{"items":[{"bidNtceNo":"R26BK01659568","bidNtceOrd":"000","bidNtceNm":"2026년 송포1교 정밀안전진단 및 성능평가 용역","dminsttNm":"경상남도 사천시","bidwinnrNm":"주식회사 연암","sucsfbidAmt":"97900000","sucsfbidRate":"89.211","rlOpengDt":"2026-08-05 11:00:00"}],"numOfRows":1,"pageNo":1,"totalCount":249}`
	var body apiBody
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(body.Items.Items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(body.Items.Items))
	}
	if int(body.TotalCount) != 249 {
		t.Fatalf("expected totalCount 249, got %d", int(body.TotalCount))
	}
	var rec AwardRecord
	if err := json.Unmarshal(body.Items.Items[0], &rec); err != nil {
		t.Fatalf("unmarshal record: %v", err)
	}
	if rec.BidNtceNo != "R26BK01659568" || rec.DminsttNm != "경상남도 사천시" || rec.BidwinnrNm != "주식회사 연암" {
		t.Fatalf("unexpected record: %+v", rec)
	}
}
