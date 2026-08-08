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

	// ProcurementClass* / IndustryRestricted — 공공조달분류(대/중/세 계층 +
	// 코드)와 업종제한 여부(2026-08-08 추가, Phase 0). g2b가 목록 응답에
	// 이미 주던 값인데 그동안 Industry(중분류명)만 쓰고 버리던 것을 살렸다.
	// Industry는 그대로 중분류명을 유지하고(판정엔진 호환), 여기에 코드·
	// 계층·제한플래그를 추가로 담는다. g2b 외 소스는 채우지 않으므로 빈값/nil.
	ProcurementClassCode   string // 공공조달분류 코드(8자리, pubPrcrmntClsfcNo)
	ProcurementClassLarge  string // 대분류명(pubPrcrmntLrgClsfcNm)
	ProcurementClassDetail string // 세분류명(pubPrcrmntClsfcNm)
	IndustryRestricted     *bool  // 업종제한 여부(indstrytyLmtYn Y/N). RegionRestricted와 동일하게 nil=미상
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
