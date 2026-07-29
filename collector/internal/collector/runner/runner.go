// Package runner orchestrates a full collection job: paging through a
// source's list endpoint, fetching details, detecting changes against
// stored state, and persisting new/updated notices as versions.
package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
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

	// AttachmentDir is where downloaded attachment bytes are written.
	//
	// 1차 버전: 로컬 디스크. Render 등 대부분의 무료/기본 플랜은 재배포 시
	// 디스크가 초기화되므로, 운영 전환 시 이 필드를 오브젝트 스토리지
	// (S3/R2) 클라이언트로 교체해야 한다 — attachments.stored_filename은
	// 로컬 경로가 아니라 스토리지 키로 설계되어 있어 그 전환 자체는
	// 이 필드와 downloadAttachment 함수만 바꾸면 된다.
	AttachmentDir string
	HTTPClient    *http.Client
}

func New(c collector.Collector, s store.Store, logger *slog.Logger) *Runner {
	if logger == nil {
		logger = slog.Default()
	}
	return &Runner{
		Collector:     c,
		Store:         s,
		Logger:        logger,
		MaxPages:      1000,
		AttachmentDir: "./data/attachments",
		HTTPClient:    &http.Client{Timeout: 60 * time.Second},
	}
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
		noticeID, versionID, err := r.Store.CreateNotice(ctx, normalized, rawDocID)
		if err != nil {
			return fmt.Errorf("create notice: %w", err)
		}
		r.Logger.Info("new notice collected", "notice_id", noticeID, "external_id", normalized.ExternalNoticeID)
		r.processAttachments(ctx, doc, versionID)
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
	r.processAttachments(ctx, doc, versionID)
	return nil
}

// processAttachments downloads and records every attachment the source
// reports for this document version. A single attachment failing to
// download does not fail the whole item — the notice itself already
// collected successfully; only that attachment's row is marked 'failed'
// so it can be retried or investigated later.
func (r *Runner) processAttachments(ctx context.Context, doc collector.RawDocument, versionID string) {
	attachments, err := r.Collector.FetchAttachments(ctx, doc)
	if err != nil {
		r.Logger.Warn("fetch attachments failed", "notice_external_id", doc.ExternalNoticeID, "error", err)
		return
	}

	for _, att := range attachments {
		if err := r.processAttachment(ctx, versionID, att); err != nil {
			r.Logger.Warn("attachment processing failed",
				"notice_external_id", doc.ExternalNoticeID, "filename", att.OriginalFilename, "error", err)
		}
	}
}

func (r *Runner) processAttachment(ctx context.Context, versionID string, att collector.Attachment) error {
	// 이미 (다른 공고버전에서) 완료된 다운로드면 바이트를 다시 받지 않고
	// 기존 저장 위치/해시를 그대로 재사용해 이번 버전에 연결만 한다.
	if existing, found, err := r.Store.FindAttachmentByDownloadURL(ctx, att.DownloadURL); err != nil {
		return fmt.Errorf("check existing attachment: %w", err)
	} else if found {
		_, err := r.Store.SaveAttachment(ctx, store.AttachmentRecord{
			NoticeVersionID:  versionID,
			OriginalFilename: att.OriginalFilename,
			StoredKey:        existing.StoredKey,
			FileType:         existing.FileType,
			FileSizeBytes:    existing.FileSizeBytes,
			FileHash:         existing.FileHash,
			DownloadURL:      att.DownloadURL,
			DownloadStatus:   "completed",
		})
		return err
	}

	body, err := r.downloadAttachment(ctx, att.DownloadURL)
	if err != nil {
		_, saveErr := r.Store.SaveAttachment(ctx, store.AttachmentRecord{
			NoticeVersionID:  versionID,
			OriginalFilename: att.OriginalFilename,
			StoredKey:        "",
			FileType:         att.FileType,
			DownloadURL:      att.DownloadURL,
			DownloadStatus:   "failed",
		})
		if saveErr != nil {
			return fmt.Errorf("download failed (%w) and save failed record failed: %v", err, saveErr)
		}
		return fmt.Errorf("download: %w", err)
	}

	sum := sha256.Sum256(body)
	hash := hex.EncodeToString(sum[:])
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(att.OriginalFilename), "."))
	storedKey := hash
	if ext != "" {
		storedKey = hash + "." + ext
	}

	if err := r.writeAttachmentFile(storedKey, body); err != nil {
		return fmt.Errorf("write attachment to disk: %w", err)
	}

	_, err = r.Store.SaveAttachment(ctx, store.AttachmentRecord{
		NoticeVersionID:  versionID,
		OriginalFilename: att.OriginalFilename,
		StoredKey:        storedKey,
		FileType:         ext,
		FileSizeBytes:    int64(len(body)),
		FileHash:         hash,
		DownloadURL:      att.DownloadURL,
		DownloadStatus:   "completed",
	})
	return err
}

func (r *Runner) downloadAttachment(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := r.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

// writeAttachmentFile writes body under AttachmentDir/storedKey, skipping
// the write if a file with that content hash is already on disk (the same
// bytes reachable from a different URL — e.g. an identical template file
// reused across notices).
func (r *Runner) writeAttachmentFile(storedKey string, body []byte) error {
	if err := os.MkdirAll(r.AttachmentDir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(r.AttachmentDir, storedKey)
	if _, err := os.Stat(path); err == nil {
		return nil // already on disk under this hash
	}
	return os.WriteFile(path, body, 0o644)
}
