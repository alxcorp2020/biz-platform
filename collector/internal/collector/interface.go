// Package collector defines the common contract every data-source collector
// must implement. Keeping this interface stable lets us add/replace
// source-specific collectors without touching the scheduler, rate limiter,
// retry logic, or storage layer.
package collector

import (
	"context"
	"time"
)

// Cursor represents pagination/incremental state for a source.
// Opaque to the caller; each collector interprets its own cursor format.
type Cursor struct {
	Token       string    // opaque paging token (page number, offset, next-link, etc.)
	SinceTime   time.Time // for incremental collection: only items changed after this time
	HasMore     bool
}

// RawItem is a single list-row before detail/attachments have been fetched.
type RawItem struct {
	SourceID         string
	ExternalNoticeID string
	Title            string
	RawPayload       string // raw JSON/HTML fragment for this list row, kept as-is
}

// RawDocument is the full, unmodified detail document for one notice.
// This is what gets persisted verbatim into raw_documents (원본 저장 계층, 4.2).
type RawDocument struct {
	SourceID         string
	ExternalNoticeID string
	RequestURL       string
	ResponseStatus   int
	RawContent       string // full API JSON or HTML body, never mutated after save
	CollectedAt      time.Time
}

// Attachment describes one file linked to a RawDocument, before download.
type Attachment struct {
	OriginalFilename string
	DownloadURL      string
	FileType         string
	FileSizeBytes    int64
}

// NormalizedNotice is the common schema all source-specific fields map into
// (4.3 정규화 계층). Field names intentionally mirror the notices table.
type NormalizedNotice struct {
	SourceID            string
	ExternalNoticeID     string
	NoticeType           string // "procurement" | "support_program"
	Title                string
	OrganizationName     string
	DepartmentName       string
	Region               string
	Industry             string
	PublishedAt          *time.Time
	ApplicationStartAt   *time.Time
	ApplicationEndAt     *time.Time
	BudgetAmount         *int64
	SupportAmount        *int64
	Status               string
	OfficialURL          string
	// RegionRestricted — 지역제한 여부(2026-08-06 추가). 신뢰할 수 있는
	// 소스만 채운다 — nil이면 "정보 없음"(이 소스가 애초에 이 값을 안
	// 주거나 판단 불가), 지어내지 않는다.
	RegionRestricted *bool
}

// Collector is the contract every source package (g2b, bizinfo, ...) implements.
// See spec section 6.1.
type Collector interface {
	// FetchList returns one page of list items plus the cursor for the next page.
	FetchList(ctx context.Context, cursor Cursor) ([]RawItem, Cursor, error)

	// FetchDetail retrieves the full document for a single list item.
	FetchDetail(ctx context.Context, item RawItem) (RawDocument, error)

	// FetchAttachments lists (but does not necessarily download) attachments
	// referenced by a detail document.
	FetchAttachments(ctx context.Context, doc RawDocument) ([]Attachment, error)

	// Normalize maps a source-specific RawDocument into the common schema.
	Normalize(ctx context.Context, doc RawDocument) (NormalizedNotice, error)

	// SourceCode returns the stable identifier matching data_sources.code.
	SourceCode() string
}
