// notice_detail.go — GET /api/notices/{id} 응답에 g2b 원본 JSON
// (raw_documents.raw_content)에서 뽑은 상세 필드와 첨부파일 목록을 얹어준다.
// notices 테이블에 컬럼을 계속 추가하는 대신, 필터/정렬에 쓰이지 않고
// 상세 화면 표시 전용인 필드들은 조회 시점에 그때그때 파싱한다.
//
// 주의: g2bRawFields는 g2b(나라장터) 응답 JSON 구조 전용이다. 현재 실제
// 출처가 g2b뿐이라 범위를 좁혔다 — 다른 출처가 추가되면 출처별 분기가
// 필요하다.
package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"strconv"
	"time"
)

type g2bRawFields struct {
	NtceKindNm               string `json:"ntceKindNm"`
	CntrctCnclsMthdNm        string `json:"cntrctCnclsMthdNm"`
	SucsfbidMthdNm           string `json:"sucsfbidMthdNm"`
	SucsfbidMthdAppStd       string `json:"sucsfbidMthdAppStd"`
	SucsfbidLwltRate         string `json:"sucsfbidLwltRate"`
	RbidPermsnYn             string `json:"rbidPermsnYn"`
	BidQlfctRgstDt           string `json:"bidQlfctRgstDt"`
	BidBeginDt               string `json:"bidBeginDt"`
	BidClseDt                string `json:"bidClseDt"`
	OpengDt                  string `json:"opengDt"`
	PresmptPrce              string `json:"presmptPrce"`
	VAT                      string `json:"VAT"`
	NtceInsttOfclNm          string `json:"ntceInsttOfclNm"`
	NtceInsttOfclTelNo       string `json:"ntceInsttOfclTelNo"`
	IndstrytyLmtYn           string `json:"indstrytyLmtYn"`
	RgnLmtBidLocplcJdgmBssNm string `json:"rgnLmtBidLocplcJdgmBssNm"`
}

type noticeRawDetail struct {
	NoticeKind              string     `json:"noticeKind"`
	ContractMethod          string     `json:"contractMethod"`
	AwardMethod             string     `json:"awardMethod"`
	AwardMethodDetail       string     `json:"awardMethodDetail"`
	AwardLowerLimitRate     string     `json:"awardLowerLimitRate"`
	RebidAllowed            *bool      `json:"rebidAllowed"`
	QualificationDeadlineAt *time.Time `json:"qualificationDeadlineAt"`
	ApplicationStartAt      *time.Time `json:"applicationStartAt"`
	ApplicationEndAt        *time.Time `json:"applicationEndAt"`
	BidOpeningAt            *time.Time `json:"bidOpeningAt"`
	EstimatedPrice          *int64     `json:"estimatedPrice"`
	Vat                     *int64     `json:"vat"`
	OfficerName             string     `json:"officerName"`
	OfficerPhone            string     `json:"officerPhone"`
	IndustryRestricted      *bool      `json:"industryRestricted"`
	RegionRestrictionBasis  string     `json:"regionRestrictionBasis"`
}

// fetchNoticeRawDetail loads and parses the raw g2b JSON for a notice
// version. Returns (nil, nil) when there's nothing to show — no raw
// document, or content that doesn't parse as g2b's shape (e.g. the bundled
// demo source) — callers should treat that as "no detail available", not
// as an error.
func (s *Server) fetchNoticeRawDetail(ctx context.Context, versionID string) (*noticeRawDetail, error) {
	var rawContent string
	err := s.db.QueryRowContext(ctx, `
		SELECT rd.raw_content
		FROM notice_versions nv
		JOIN raw_documents rd ON rd.id = nv.raw_document_id
		WHERE nv.id = $1`, versionID,
	).Scan(&rawContent)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	var f g2bRawFields
	if err := json.Unmarshal([]byte(rawContent), &f); err != nil {
		return nil, nil
	}

	return &noticeRawDetail{
		NoticeKind:              f.NtceKindNm,
		ContractMethod:          f.CntrctCnclsMthdNm,
		AwardMethod:             f.SucsfbidMthdNm,
		AwardMethodDetail:       f.SucsfbidMthdAppStd,
		AwardLowerLimitRate:     f.SucsfbidLwltRate,
		RebidAllowed:            ynToBool(f.RbidPermsnYn),
		QualificationDeadlineAt: parseG2BDateTime(f.BidQlfctRgstDt),
		ApplicationStartAt:      parseG2BDateTime(f.BidBeginDt),
		ApplicationEndAt:        parseG2BDateTime(f.BidClseDt),
		BidOpeningAt:            parseG2BDateTime(f.OpengDt),
		EstimatedPrice:          parseG2BAmount(f.PresmptPrce),
		Vat:                     parseG2BAmount(f.VAT),
		OfficerName:             f.NtceInsttOfclNm,
		OfficerPhone:            f.NtceInsttOfclTelNo,
		IndustryRestricted:      ynToBool(f.IndstrytyLmtYn),
		RegionRestrictionBasis:  f.RgnLmtBidLocplcJdgmBssNm,
	}, nil
}

func ynToBool(v string) *bool {
	switch v {
	case "Y":
		t := true
		return &t
	case "N":
		f := false
		return &f
	default:
		return nil
	}
}

// parseG2BDateTime tries both observed g2b timestamp layouts — most fields
// carry seconds ("2006-01-02 15:04:05"), but bidQlfctRgstDt has been
// observed without them ("2006-01-02 15:04").
func parseG2BDateTime(v string) *time.Time {
	if v == "" {
		return nil
	}
	for _, layout := range []string{"2006-01-02 15:04:05", "2006-01-02 15:04"} {
		if t, err := time.ParseInLocation(layout, v, time.Local); err == nil {
			return &t
		}
	}
	return nil
}

func parseG2BAmount(v string) *int64 {
	if v == "" {
		return nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return nil
	}
	return &n
}

type attachmentItem struct {
	OriginalFilename string `json:"originalFilename"`
	FileType         string `json:"fileType"`
	FileSizeBytes    *int64 `json:"fileSizeBytes"`
	DownloadURL      string `json:"downloadUrl"`
	DownloadStatus   string `json:"downloadStatus"`
}

// listAttachments surfaces the download_url g2b originally served the file
// from (verified to work via plain GET — see g2b Collector) rather than
// proxying through a new local-file-serving endpoint, since that's simpler
// and the attachments table already tracks whether the download completed.
func (s *Server) listAttachments(ctx context.Context, versionID string) ([]attachmentItem, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT original_filename, COALESCE(file_type, ''), file_size_bytes, COALESCE(download_url, ''), download_status
		FROM attachments
		WHERE notice_version_id = $1
		ORDER BY created_at`, versionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []attachmentItem{}
	for rows.Next() {
		var it attachmentItem
		var size sql.NullInt64
		if err := rows.Scan(&it.OriginalFilename, &it.FileType, &size, &it.DownloadURL, &it.DownloadStatus); err != nil {
			continue
		}
		if size.Valid {
			it.FileSizeBytes = &size.Int64
		}
		out = append(out, it)
	}
	return out, nil
}
