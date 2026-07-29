// Package runner orchestrates a full collection job: paging through a
// source's list endpoint, fetching details, detecting changes against
// stored state, and persisting new/updated notices as versions.
package runner

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"biz-platform/collector/internal/collector"
	"biz-platform/collector/internal/collector/changedetect"
	"biz-platform/collector/internal/collector/store"
)

type JobStatus string

const (
	StatusQueued         JobStatus = "queued"
	StatusRunning        JobStatus = "running"
	StatusCompleted      JobStatus = "completed"
	StatusFailed         JobStatus = "failed"
	StatusReviewRequired JobStatus = "review_required"
)

type JobResult struct {
	Status         JobStatus
	ProcessedCount int
	SuccessCount   int
	FailedCount    int
	StartedAt      time.Time
	FinishedAt     time.Time
	Errors         []string
}

type Runner struct {
	Collector collector.Collector
	Store     store.Store
	Logger    *slog.Logger
	// MaxPages guards against runaway pagination during incremental runs.
	MaxPages int
}

func New(c collector.Collector, s store.Store, logger *slog.Logger) *Runner {
	if logger == nil {
		logger = slog.Default()
	}
	maxPages := 1000
	return &Runner{Collector: c, Store: s, Logger: logger, MaxPages: maxPages}
}

// RunIncremental pages through the source starting from `since` and
// upserts every notice it finds, recording versions only when content
// actually changed (spec: "동일 첨부파일이 재등록된 경우 ... AI 호출 안 함"
// principle — this layer is what makes that possible upstream).
func (r *Runner) RunIncremental(ctx context.Context, since time.Time) JobResult {
	result := JobResult{Status: StatusRunning, StartedAt: time.Now()}
	cursor := collector.Cursor{SinceTime: since}

	// Guards against a source whose result set shifts while we page through
	// it (e.g. new/updated rows change ordering mid-run on a live API),
	// which can surface the same ExternalNoticeID on two different pages
	// within one run and otherwise record spurious duplicate versions.
	seen := make(map[string]bool)

	for page := 0; page < r.MaxPages; page++ {
		items, next, err := r.Collector.FetchList(ctx, cursor)
		if err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("fetch list page %d: %v", page, err))
			result.Status = StatusFailed
			result.FinishedAt = time.Now()
			return result
		}

		for _, item := range items {
			if seen[item.ExternalNoticeID] {
				continue
			}
			seen[item.ExternalNoticeID] = true

			result.ProcessedCount++
			if err := r.processItem(ctx, item); err != nil {
				result.FailedCount++
				result.Errors = append(result.Errors, fmt.Sprintf("item %s: %v", item.ExternalNoticeID, err))
				r.Logger.Warn("collection item failed", "notice_id", item.ExternalNoticeID, "error", err)
				continue
			}
			result.SuccessCount++
		}

		if !next.HasMore {
			break
		}
		cursor = next
	}

	result.FinishedAt = time.Now()
	if result.FailedCount > 0 && result.SuccessCount == 0 {
		result.Status = StatusFailed
	} else {
		result.Status = StatusCompleted
	}
	return result
}

func (r *Runner) processItem(ctx context.Context, item collector.RawItem) error {
	doc, err := r.Collector.FetchDetail(ctx, item)
	if err != nil {
		return fmt.Errorf("fetch detail: %w", err)
	}

	rawDocID, _, err := r.Store.SaveRawDocument(ctx, doc) // 원본은 있는 그대로 append-only 저장
	if err != nil {
		return fmt.Errorf("save raw document: %w", err)
	}

	normalized, err := r.Collector.Normalize(ctx, doc)
	if err != nil {
		return fmt.Errorf("normalize: %w", err)
	}

	existing, found, err := r.Store.FindNoticeBySourceAndExternalID(ctx, normalized.SourceID, normalized.ExternalNoticeID)
	if err != nil {
		return fmt.Errorf("lookup existing notice: %w", err)
	}

	if !found {
		noticeID, err := r.Store.CreateNotice(ctx, normalized, rawDocID)
		if err != nil {
			return fmt.Errorf("create notice: %w", err)
		}
		r.Logger.Info("new notice collected", "notice_id", noticeID, "external_id", normalized.ExternalNoticeID)
		return nil
	}

	changes := changedetect.Compare(existing.Notice, normalized)
	if len(changes) == 0 {
		// 6.6: 내용에 변화가 없으면 새 버전을 만들지 않는다 — 불필요한 재분석/AI 호출 방지.
		return nil
	}

	changeType := changedetect.OverallChangeType(changes)
	versionID, versionNum, err := r.Store.AddNewVersion(ctx, existing.ID, normalized, rawDocID, changeType)
	if err != nil {
		return fmt.Errorf("add new version: %w", err)
	}

	var records []store.ChangeRecord
	for _, c := range changes {
		records = append(records, store.ChangeRecord{
			NoticeID:    existing.ID,
			FromVersion: existing.CurrentVersion,
			ToVersion:   versionNum,
			Field:       c.Field,
			OldValue:    c.OldValue,
			NewValue:    c.NewValue,
			Importance:  c.Importance,
		})
	}
	if err := r.Store.RecordChanges(ctx, records); err != nil {
		return fmt.Errorf("record changes: %w", err)
	}

	r.Logger.Info("notice updated",
		"notice_id", existing.ID, "version_id", versionID,
		"change_type", changeType, "field_changes", len(changes))
	return nil
}
