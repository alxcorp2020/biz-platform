// Package demo provides a Collector that returns realistic-looking sample
// notices without calling any external API. It exists purely so the
// deployed service has something to show before a real government source
// (나라장터 등) is connected with an issued API key. Replace with a real
// sources/<code> package per README "새 데이터 출처 추가하는 방법" — nothing
// else in the pipeline needs to change.
package demo

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"biz-platform/collector/internal/collector"
)

type Source struct{}

func New() *Source { return &Source{} }

func (s *Source) SourceCode() string { return "demo" }

type payload struct {
	NoticeID     string `json:"noticeId"`
	Title        string `json:"title"`
	Organization string `json:"organization"`
	Region       string `json:"region"`
	Industry     string `json:"industry"`
	PublishedAt  string `json:"publishedAt"`
	DeadlineAt   string `json:"deadlineAt"`
	BudgetAmount int64  `json:"budgetAmount"`
	Status       string `json:"status"`
}

func demoData() []payload {
	today := time.Now()
	deadline := func(days int) string { return today.AddDate(0, 0, days).Format("2006-01-02") }
	return []payload{
		{"DEMO-0001", "OO시청 홈페이지 유지관리 용역", "OO시청", "광주광역시", "소프트웨어개발", today.Format("2006-01-02"), deadline(14), 45000000, "OPEN"},
		{"DEMO-0002", "공공기관 홍보영상 제작", "OO진흥원", "전국", "영상제작", today.Format("2006-01-02"), deadline(9), 20000000, "OPEN"},
		{"DEMO-0003", "지역 소상공인 실태조사 연구용역", "OO시 경제국", "부산광역시", "조사연구", today.Format("2006-01-02"), deadline(21), 32000000, "OPEN"},
		{"DEMO-0004", "정보화 시스템 구축 사업", "OO공단", "세종특별자치시", "정보화사업", today.Format("2006-01-02"), deadline(30), 180000000, "OPEN"},
	}
}

func (s *Source) FetchList(ctx context.Context, cursor collector.Cursor) ([]collector.RawItem, collector.Cursor, error) {
	if cursor.HasMore == false && cursor.Token != "" {
		// single-page demo source: nothing more after page 1
		return nil, collector.Cursor{}, nil
	}
	items := make([]collector.RawItem, 0)
	for _, d := range demoData() {
		raw, _ := json.Marshal(d)
		items = append(items, collector.RawItem{
			SourceID: s.SourceCode(), ExternalNoticeID: d.NoticeID, Title: d.Title, RawPayload: string(raw),
		})
	}
	return items, collector.Cursor{Token: "done", HasMore: false}, nil
}

func (s *Source) FetchDetail(ctx context.Context, item collector.RawItem) (collector.RawDocument, error) {
	for _, d := range demoData() {
		if d.NoticeID == item.ExternalNoticeID {
			raw, _ := json.Marshal(d)
			return collector.RawDocument{
				SourceID: s.SourceCode(), ExternalNoticeID: d.NoticeID,
				RequestURL: "demo://" + d.NoticeID, ResponseStatus: 200,
				RawContent: string(raw), CollectedAt: time.Now(),
			}, nil
		}
	}
	return collector.RawDocument{}, fmt.Errorf("demo notice %s not found", item.ExternalNoticeID)
}

func (s *Source) FetchAttachments(ctx context.Context, doc collector.RawDocument) ([]collector.Attachment, error) {
	return nil, nil // 데모 소스는 첨부파일 없음
}

func (s *Source) Normalize(ctx context.Context, doc collector.RawDocument) (collector.NormalizedNotice, error) {
	var d payload
	if err := json.Unmarshal([]byte(doc.RawContent), &d); err != nil {
		return collector.NormalizedNotice{}, err
	}
	n := collector.NormalizedNotice{
		SourceID: s.SourceCode(), ExternalNoticeID: d.NoticeID, NoticeType: "procurement",
		Title: d.Title, OrganizationName: d.Organization, Region: d.Region, Industry: d.Industry,
		Status: "open", OfficialURL: "demo://" + d.NoticeID,
	}
	if t, err := time.Parse("2006-01-02", d.PublishedAt); err == nil {
		n.PublishedAt = &t
	}
	if t, err := time.Parse("2006-01-02", d.DeadlineAt); err == nil {
		n.ApplicationEndAt = &t
	}
	if d.BudgetAmount > 0 {
		v := d.BudgetAmount
		n.BudgetAmount = &v
	}
	return n, nil
}
