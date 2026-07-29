// Package sample implements collector.Collector against a generic JSON
// list/detail API shape. It stands in for a real source (e.g. 나라장터,
// bizinfo) during development — swap BaseURL to the real endpoint and
// adjust field mapping in Normalize() to onboard an actual source without
// touching the runner, rate limiter, retry, or storage code.
package sample

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"biz-platform/collector/internal/collector"
	"biz-platform/collector/internal/collector/common"
)

type listResponse struct {
	Items      []listItem `json:"items"`
	NextCursor string     `json:"nextCursor"`
}

type listItem struct {
	NoticeID string `json:"noticeId"`
	Title    string `json:"title"`
}

type detailResponse struct {
	NoticeID     string `json:"noticeId"`
	Title        string `json:"title"`
	Organization string `json:"organization"`
	Region       string `json:"region"`
	Industry     string `json:"industry"`
	PublishedAt  string `json:"publishedAt"`  // "2026-07-01"
	DeadlineAt   string `json:"deadlineAt"`   // "2026-07-20"
	BudgetAmount int64  `json:"budgetAmount"`
	Status       string `json:"status"`
	Attachments  []struct {
		Filename string `json:"filename"`
		URL      string `json:"url"`
		FileType string `json:"fileType"`
		Size     int64  `json:"size"`
	} `json:"attachments"`
}

type Source struct {
	BaseURL    string
	HTTPClient *http.Client
	RateLimit  *common.RateLimiter
}

func New(baseURL string) *Source {
	return &Source{
		BaseURL:    baseURL,
		HTTPClient: &http.Client{Timeout: 10 * time.Second},
		RateLimit:  common.NewRateLimiter(2, 5000), // 2 req/sec, 5000/day — 출처별 실제 한도로 조정
	}
}

func (s *Source) SourceCode() string { return "sample" }

func (s *Source) FetchList(ctx context.Context, cursor collector.Cursor) ([]collector.RawItem, collector.Cursor, error) {
	if err := s.RateLimit.Wait(ctx); err != nil {
		return nil, cursor, err
	}

	url := fmt.Sprintf("%s/notices?cursor=%s", s.BaseURL, cursor.Token)
	var parsed listResponse

	err := common.Do(ctx, common.DefaultRetryConfig(), func() error {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		resp, err := s.HTTPClient.Do(req)
		if err != nil {
			return err // network/timeout -> retryable by default
		}
		defer resp.Body.Close()

		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return &common.PermanentError{Err: fmt.Errorf("auth failed: status %d", resp.StatusCode)}
		}
		if resp.StatusCode == http.StatusNotFound {
			return &common.PermanentError{Err: fmt.Errorf("endpoint not found: %s", url)}
		}
		if resp.StatusCode >= 500 {
			return fmt.Errorf("server error: status %d", resp.StatusCode) // retryable
		}
		if resp.StatusCode != http.StatusOK {
			return &common.PermanentError{Err: fmt.Errorf("unexpected status %d", resp.StatusCode)}
		}

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return err
		}
		return json.Unmarshal(body, &parsed)
	})
	if err != nil {
		return nil, cursor, err
	}

	items := make([]collector.RawItem, 0, len(parsed.Items))
	for _, it := range parsed.Items {
		raw, _ := json.Marshal(it)
		items = append(items, collector.RawItem{
			SourceID:         s.SourceCode(),
			ExternalNoticeID: it.NoticeID,
			Title:            it.Title,
			RawPayload:       string(raw),
		})
	}

	nextCursor := collector.Cursor{Token: parsed.NextCursor, HasMore: parsed.NextCursor != ""}
	return items, nextCursor, nil
}

func (s *Source) FetchDetail(ctx context.Context, item collector.RawItem) (collector.RawDocument, error) {
	if err := s.RateLimit.Wait(ctx); err != nil {
		return collector.RawDocument{}, err
	}

	url := fmt.Sprintf("%s/notices/%s", s.BaseURL, item.ExternalNoticeID)
	var body []byte
	var status int

	err := common.Do(ctx, common.DefaultRetryConfig(), func() error {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		resp, err := s.HTTPClient.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		status = resp.StatusCode

		if status == http.StatusUnauthorized || status == http.StatusForbidden || status == http.StatusNotFound {
			return &common.PermanentError{Err: fmt.Errorf("non-retryable status %d for %s", status, url)}
		}
		if status >= 500 {
			return fmt.Errorf("server error %d", status)
		}
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			return err
		}
		body = b
		return nil
	})
	if err != nil {
		return collector.RawDocument{}, err
	}

	return collector.RawDocument{
		SourceID:         s.SourceCode(),
		ExternalNoticeID: item.ExternalNoticeID,
		RequestURL:       url,
		ResponseStatus:   status,
		RawContent:       string(body),
		CollectedAt:      time.Now(),
	}, nil
}

func (s *Source) FetchAttachments(ctx context.Context, doc collector.RawDocument) ([]collector.Attachment, error) {
	var d detailResponse
	if err := json.Unmarshal([]byte(doc.RawContent), &d); err != nil {
		return nil, fmt.Errorf("parse detail for attachments: %w", err)
	}
	out := make([]collector.Attachment, 0, len(d.Attachments))
	for _, a := range d.Attachments {
		out = append(out, collector.Attachment{
			OriginalFilename: a.Filename,
			DownloadURL:      a.URL,
			FileType:         a.FileType,
			FileSizeBytes:    a.Size,
		})
	}
	return out, nil
}

func (s *Source) Normalize(ctx context.Context, doc collector.RawDocument) (collector.NormalizedNotice, error) {
	var d detailResponse
	if err := json.Unmarshal([]byte(doc.RawContent), &d); err != nil {
		return collector.NormalizedNotice{}, fmt.Errorf("normalize: %w", err)
	}

	n := collector.NormalizedNotice{
		SourceID:         s.SourceCode(),
		ExternalNoticeID: d.NoticeID,
		NoticeType:       "procurement",
		Title:            d.Title,
		OrganizationName: d.Organization,
		Region:           d.Region,
		Industry:         d.Industry,
		Status:           mapStatus(d.Status),
		OfficialURL:      fmt.Sprintf("%s/notices/%s", s.BaseURL, d.NoticeID),
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

func mapStatus(raw string) string {
	switch raw {
	case "OPEN", "ONGOING":
		return "open"
	case "CLOSED", "ENDED":
		return "closed"
	case "CANCELLED":
		return "cancelled"
	default:
		return "open"
	}
}

var _ = strconv.Itoa // reserved for future numeric field parsing
