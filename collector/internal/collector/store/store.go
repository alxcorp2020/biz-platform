// Package store defines the persistence boundary for the collector.
// InMemoryStore below is used for local development and tests without a
// database dependency. In production, implement the same Store interface
// with pgx against the schema in db/migrations/001_init.sql — no other
// package needs to change.
package store

import (
	"context"
	"sync"
	"time"

	"biz-platform/collector/internal/collector"
	"biz-platform/collector/internal/collector/common"
)

type RawDocumentRecord struct {
	ID          string
	Doc         collector.RawDocument
	ContentHash string
}

type NoticeVersionRecord struct {
	ID              string
	NoticeID        string
	VersionNumber   int
	RawDocumentID   string
	ChangeType      string
	ChangeSummary   string
	IsCurrent       bool
}

type NoticeRecord struct {
	ID                 string
	Notice             collector.NormalizedNotice
	CurrentVersion     int
	FirstCollectedAt   time.Time
	LastVerifiedAt     time.Time
}

type ChangeRecord struct {
	NoticeID     string
	FromVersion  int
	ToVersion    int
	Field        string
	OldValue     string
	NewValue     string
	Importance   string
}

// AttachmentRecord mirrors the attachments table (5.5). StoredKey is a
// storage-backend-agnostic key ("stored_filename" in the schema) — v1
// resolves it against local disk, a later object-storage backend resolves
// the same key against S3/R2 instead.
type AttachmentRecord struct {
	NoticeVersionID  string
	OriginalFilename string
	StoredKey        string
	FileType         string
	FileSizeBytes    int64
	FileHash         string // "" for a failed download — schema requires NOT NULL, not a real hash
	DownloadURL      string
	DownloadStatus   string // "completed" | "failed"
}

// Store is the persistence contract the collector runner depends on.
// 원칙: 원본은 절대 수정하지 않는다 — there is intentionally no
// UpdateRawDocument method.
type Store interface {
	SaveRawDocument(ctx context.Context, doc collector.RawDocument) (rawDocID string, contentHash string, err error)

	// FindNoticeBySourceAndExternalID returns (nil, false, nil) if not found.
	FindNoticeBySourceAndExternalID(ctx context.Context, sourceID, externalID string) (*NoticeRecord, bool, error)

	// CreateNotice inserts a brand-new notice with version 1, returning both
	// the notice id and its version-1 id (attachments FK to the version, not
	// the notice).
	CreateNotice(ctx context.Context, notice collector.NormalizedNotice, rawDocID string) (noticeID string, versionID string, err error)

	// AddNewVersion appends a new version to an existing notice and marks
	// the previous version as no longer current.
	AddNewVersion(ctx context.Context, noticeID string, notice collector.NormalizedNotice, rawDocID string, changeType string) (versionID string, versionNumber int, err error)

	RecordChanges(ctx context.Context, changes []ChangeRecord) error

	// LastRawContentHash returns the content hash of the current version's
	// raw document, used to skip re-processing unchanged content (6.6).
	LastRawContentHash(ctx context.Context, noticeID string) (string, error)

	// SaveAttachment records one attachment row (always inserted — even a
	// re-used already-downloaded file gets its own row per notice version).
	SaveAttachment(ctx context.Context, att AttachmentRecord) (id string, err error)

	// FindAttachmentByDownloadURL looks up a previously completed download
	// by source URL, so the same file linked from a re-collected/updated
	// notice doesn't get fetched over HTTP again (spec 6.6: 동일 첨부파일
	// 재등록 시 재다운로드/재분석 안 함).
	FindAttachmentByDownloadURL(ctx context.Context, downloadURL string) (*AttachmentRecord, bool, error)
}

// ---------------- In-memory implementation (dev/test only) ----------------

type InMemoryStore struct {
	mu          sync.Mutex
	rawDocs     map[string]RawDocumentRecord
	notices     map[string]*NoticeRecord
	versions    map[string][]NoticeVersionRecord // noticeID -> versions
	bySrcExt    map[string]string                // "sourceID|externalID" -> noticeID
	changes     []ChangeRecord
	attachments []AttachmentRecord
	seq         int
}

func NewInMemoryStore() *InMemoryStore {
	return &InMemoryStore{
		rawDocs:  make(map[string]RawDocumentRecord),
		notices:  make(map[string]*NoticeRecord),
		versions: make(map[string][]NoticeVersionRecord),
		bySrcExt: make(map[string]string),
	}
}

func (s *InMemoryStore) nextID(prefix string) string {
	s.seq++
	return prefix + "-" + itoa(s.seq)
}

func (s *InMemoryStore) SaveRawDocument(ctx context.Context, doc collector.RawDocument) (string, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	hash := common.Sha256Hex([]byte(doc.RawContent))
	id := s.nextID("raw")
	s.rawDocs[id] = RawDocumentRecord{ID: id, Doc: doc, ContentHash: hash}
	return id, hash, nil
}

func (s *InMemoryStore) FindNoticeBySourceAndExternalID(ctx context.Context, sourceID, externalID string) (*NoticeRecord, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := sourceID + "|" + externalID
	id, ok := s.bySrcExt[key]
	if !ok {
		return nil, false, nil
	}
	rec := s.notices[id]
	return rec, true, nil
}

func (s *InMemoryStore) CreateNotice(ctx context.Context, notice collector.NormalizedNotice, rawDocID string) (string, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.nextID("notice")
	verID := s.nextID("ver")
	now := time.Now()
	s.notices[id] = &NoticeRecord{ID: id, Notice: notice, CurrentVersion: 1, FirstCollectedAt: now, LastVerifiedAt: now}
	s.versions[id] = []NoticeVersionRecord{{
		ID: verID, NoticeID: id, VersionNumber: 1,
		RawDocumentID: rawDocID, ChangeType: "initial", IsCurrent: true,
	}}
	s.bySrcExt[notice.SourceID+"|"+notice.ExternalNoticeID] = id
	return id, verID, nil
}

func (s *InMemoryStore) AddNewVersion(ctx context.Context, noticeID string, notice collector.NormalizedNotice, rawDocID string, changeType string) (string, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	vs := s.versions[noticeID]
	for i := range vs {
		vs[i].IsCurrent = false
	}
	newVerNum := len(vs) + 1
	verID := s.nextID("ver")
	vs = append(vs, NoticeVersionRecord{
		ID: verID, NoticeID: noticeID, VersionNumber: newVerNum,
		RawDocumentID: rawDocID, ChangeType: changeType, IsCurrent: true,
	})
	s.versions[noticeID] = vs

	rec := s.notices[noticeID]
	rec.Notice = notice
	rec.CurrentVersion = newVerNum
	rec.LastVerifiedAt = time.Now()
	return verID, newVerNum, nil
}

func (s *InMemoryStore) RecordChanges(ctx context.Context, changes []ChangeRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.changes = append(s.changes, changes...)
	return nil
}

func (s *InMemoryStore) LastRawContentHash(ctx context.Context, noticeID string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	vs := s.versions[noticeID]
	if len(vs) == 0 {
		return "", nil
	}
	last := vs[len(vs)-1]
	return s.rawDocs[last.RawDocumentID].ContentHash, nil
}

func (s *InMemoryStore) SaveAttachment(ctx context.Context, att AttachmentRecord) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.nextID("att")
	s.attachments = append(s.attachments, att)
	return id, nil
}

func (s *InMemoryStore) FindAttachmentByDownloadURL(ctx context.Context, downloadURL string) (*AttachmentRecord, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.attachments) - 1; i >= 0; i-- {
		if s.attachments[i].DownloadURL == downloadURL && s.attachments[i].DownloadStatus == "completed" {
			rec := s.attachments[i]
			return &rec, true, nil
		}
	}
	return nil, false, nil
}

// Changes exposes recorded changes for inspection in tests/demo output.
func (s *InMemoryStore) Changes() []ChangeRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ChangeRecord, len(s.changes))
	copy(out, s.changes)
	return out
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	digits := []byte{}
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	if neg {
		return "-" + string(digits)
	}
	return string(digits)
}
