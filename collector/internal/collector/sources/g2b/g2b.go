// Package g2b implements collector.Collector against the real 조달청
// 나라장터 입찰공고정보서비스 (BidPublicInfoService), operation
// getBidPblancListInfoServc (용역 부문 입찰공고 목록).
//
// Endpoint verified by hand against the live API on 2026-07-29:
//
//	https://apis.data.go.kr/1230000/ad/BidPublicInfoService/getBidPblancListInfoServc
//
// Note the "/ad/" path segment — data.go.kr's own docs and most blog posts
// omit it, but every request to .../1230000/BidPublicInfoService/... (without
// /ad/) returns a generic gateway-level "Unexpected errors" 500 regardless of
// service key or operation, while the same request under .../1230000/ad/...
// gets a normal response (401 for a bad key, 200 + real data for a valid one).
//
// The list operation already returns full notice detail per row (no separate
// detail endpoint exists for this service), so FetchDetail does not make a
// second HTTP call — it just wraps the row captured during FetchList.
package g2b

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"biz-platform/collector/internal/collector"
	"biz-platform/collector/internal/collector/common"
)

const (
	baseURL   = "https://apis.data.go.kr/1230000/ad/BidPublicInfoService"
	operation = "getBidPblancListInfoServc"
	// regionNationwide — internal/api/eligibility.go의 regionNationwide와
	// 반드시 같은 문자열이어야 한다(별도 패키지라 상수 공유 대신 문자열
	// 리터럴을 이 코드베이스 기존 관례대로 중복 정의 — REGION_OPTIONS류가
	// 프론트/백엔드 여러 곳에 이미 각자 정의돼 있는 것과 같은 패턴).
	regionNationwide = "전국"
)

type Source struct {
	ServiceKey string
	HTTPClient *http.Client
	RateLimit  *common.RateLimiter
	PageSize   int
	now        func() time.Time
}

// New creates a g2b Source. serviceKey is the decoded (raw) service key
// issued by data.go.kr for 조달청_나라장터 입찰공고정보서비스.
//
// Rate limit defaults to 1 req/sec and 1,000 calls/day, matching the
// "개발계정" (development account) default quota data.go.kr grants on
// first issuance — raise the daily figure once the account is approved
// for higher production traffic.
func New(serviceKey string) *Source {
	return &Source{
		ServiceKey: serviceKey,
		HTTPClient: &http.Client{Timeout: 15 * time.Second},
		RateLimit:  common.NewRateLimiter(1, 1000),
		PageSize:   100,
		now:        time.Now,
	}
}

func (s *Source) SourceCode() string { return "g2b" }

// bidItem is the subset of getBidPblancListInfoServc's ~90 response fields
// this collector actually maps. Field names are the API's own (verified
// against a live response), not a guess.
type bidItem struct {
	BidNtceNo  string `json:"bidNtceNo"`  // 입찰공고번호
	BidNtceOrd string `json:"bidNtceOrd"` // 입찰공고차수 (재공고 시 증가)
	ReNtceYn   string `json:"reNtceYn"`   // 재공고 여부 Y/N

	BidNtceNm   string `json:"bidNtceNm"`   // 입찰공고명
	NtceInsttNm string `json:"ntceInsttNm"` // 공고기관명
	DminsttNm   string `json:"dminsttNm"`   // 수요기관명

	BidNtceDt  string `json:"bidNtceDt"`  // 입찰공고일시
	BidBeginDt string `json:"bidBeginDt"` // 입찰개시일시
	BidClseDt  string `json:"bidClseDt"`  // 입찰마감일시

	AsignBdgtAmt string `json:"asignBdgtAmt"` // 배정예산액

	PubPrcrmntMidClsfcNm string `json:"pubPrcrmntMidClsfcNm"` // 공공조달분류 중분류명 (업종 근사치)

	// BidPrtcptLmtYn/CmmnSpldmdCorpRgnLmtYn — 지역제한 여부(Y/N). 2026-08-05
	// 추가 — Region을 아예 안 채우던 걸 발견(온보딩 화면의 "지역" 질문이
	// eligibility.go의 scoreRegion을 거쳐 항상 "공고 쪽 데이터 부족"으로만
	// 잡혀 회사 지역 미입력이 결코 감지되지 못하던 근본 원인). 실측(raw_documents
	// 원본 JSON) 확인 결과 이 목록 조회(용역 부문) 응답은 지역제한이 있을 때도
	// RgnLmtBidLocplcJdgmBssNm(제한 지역명) 자체를 채워주지 않는다 — 그래서
	// 그 필드는 신뢰할 수 없어 아예 안 쓴다. 대신 "제한이 있는지 없는지"는
	// 신뢰할 수 있으므로, 제한이 없으면(실측상 99%+) "전국"으로 매핑한다.
	BidPrtcptLmtYn         string `json:"bidPrtcptLmtYn"`         // 입찰참가 지역제한 여부
	CmmnSpldmdCorpRgnLmtYn string `json:"cmmnSpldmdCorpRgnLmtYn"` // 공동수급 법인 지역제한 여부

	BidNtceDtlUrl string `json:"bidNtceDtlUrl"` // 공고 상세 링크
	BidNtceUrl    string `json:"bidNtceUrl"`

	NtceSpecDocUrl1  string `json:"ntceSpecDocUrl1"`
	NtceSpecDocUrl2  string `json:"ntceSpecDocUrl2"`
	NtceSpecDocUrl3  string `json:"ntceSpecDocUrl3"`
	NtceSpecDocUrl4  string `json:"ntceSpecDocUrl4"`
	NtceSpecDocUrl5  string `json:"ntceSpecDocUrl5"`
	NtceSpecDocUrl6  string `json:"ntceSpecDocUrl6"`
	NtceSpecDocUrl7  string `json:"ntceSpecDocUrl7"`
	NtceSpecDocUrl8  string `json:"ntceSpecDocUrl8"`
	NtceSpecDocUrl9  string `json:"ntceSpecDocUrl9"`
	NtceSpecDocUrl10 string `json:"ntceSpecDocUrl10"`

	NtceSpecFileNm1  string `json:"ntceSpecFileNm1"`
	NtceSpecFileNm2  string `json:"ntceSpecFileNm2"`
	NtceSpecFileNm3  string `json:"ntceSpecFileNm3"`
	NtceSpecFileNm4  string `json:"ntceSpecFileNm4"`
	NtceSpecFileNm5  string `json:"ntceSpecFileNm5"`
	NtceSpecFileNm6  string `json:"ntceSpecFileNm6"`
	NtceSpecFileNm7  string `json:"ntceSpecFileNm7"`
	NtceSpecFileNm8  string `json:"ntceSpecFileNm8"`
	NtceSpecFileNm9  string `json:"ntceSpecFileNm9"`
	NtceSpecFileNm10 string `json:"ntceSpecFileNm10"`
}

// apiEnvelope decodes only the header eagerly; body is kept raw because
// data.go.kr returns "items":"" (a string, not an array) on NODATA_ERROR,
// which would otherwise break a single-pass strict unmarshal into a typed
// items slice.
type apiEnvelope struct {
	Response struct {
		Header struct {
			ResultCode string `json:"resultCode"`
			ResultMsg  string `json:"resultMsg"`
		} `json:"header"`
		Body json.RawMessage `json:"body"`
	} `json:"response"`
}

type apiBody struct {
	Items      []json.RawMessage `json:"items"`
	TotalCount int               `json:"totalCount"`
}

func (s *Source) FetchList(ctx context.Context, cursor collector.Cursor) ([]collector.RawItem, collector.Cursor, error) {
	if err := s.RateLimit.Wait(ctx); err != nil {
		return nil, cursor, err
	}

	pageNo := 1
	if cursor.Token != "" {
		if n, err := strconv.Atoi(cursor.Token); err == nil {
			pageNo = n
		}
	}

	end := s.now()
	begin := end.Add(-24 * time.Hour)
	if !cursor.SinceTime.IsZero() && cursor.SinceTime.Before(end) {
		begin = cursor.SinceTime
	}

	q := url.Values{}
	q.Set("ServiceKey", s.ServiceKey)
	q.Set("inqryDiv", "1")
	q.Set("type", "json")
	q.Set("inqryBgnDt", begin.Format("200601021504"))
	q.Set("inqryEndDt", end.Format("200601021504"))
	q.Set("pageNo", strconv.Itoa(pageNo))
	q.Set("numOfRows", strconv.Itoa(s.PageSize))
	reqURL := baseURL + "/" + operation + "?" + q.Encode()

	var envelope apiEnvelope
	err := common.Do(ctx, common.DefaultRetryConfig(), func() error {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
		resp, err := s.HTTPClient.Do(req)
		if err != nil {
			return err // network/timeout — retryable
		}
		defer resp.Body.Close()

		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return &common.PermanentError{Err: fmt.Errorf("auth failed: status %d", resp.StatusCode)}
		}
		if resp.StatusCode >= 500 {
			return fmt.Errorf("server error: status %d", resp.StatusCode)
		}
		if resp.StatusCode != http.StatusOK {
			return &common.PermanentError{Err: fmt.Errorf("unexpected status %d", resp.StatusCode)}
		}

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(body, &envelope); err != nil {
			return &common.PermanentError{Err: fmt.Errorf("parse response envelope: %w", err)}
		}
		return nil
	})
	if err != nil {
		return nil, cursor, err
	}

	switch code := envelope.Response.Header.ResultCode; code {
	case "00":
		// success — fall through
	case "03":
		// NODATA_ERROR: no notices in the requested window, not a failure.
		return nil, collector.Cursor{HasMore: false}, nil
	default:
		msg := fmt.Sprintf("g2b api error %s: %s", code, envelope.Response.Header.ResultMsg)
		if isPermanentResultCode(code) {
			return nil, cursor, &common.PermanentError{Err: fmt.Errorf("%s", msg)}
		}
		return nil, cursor, fmt.Errorf("%s", msg)
	}

	var body apiBody
	if err := json.Unmarshal(envelope.Response.Body, &body); err != nil {
		return nil, cursor, &common.PermanentError{Err: fmt.Errorf("parse response body: %w", err)}
	}

	items := make([]collector.RawItem, 0, len(body.Items))
	for _, raw := range body.Items {
		var it bidItem
		if err := json.Unmarshal(raw, &it); err != nil {
			continue // skip a malformed row rather than failing the whole page
		}
		items = append(items, collector.RawItem{
			SourceID:         s.SourceCode(),
			ExternalNoticeID: it.BidNtceNo,
			Title:            it.BidNtceNm,
			RawPayload:       string(raw),
		})
	}

	fetchedSoFar := pageNo * s.PageSize
	nextCursor := collector.Cursor{
		Token:     strconv.Itoa(pageNo + 1),
		SinceTime: cursor.SinceTime,
		HasMore:   fetchedSoFar < body.TotalCount,
	}
	return items, nextCursor, nil
}

// isPermanentResultCode reports data.go.kr resultCodes that will not resolve
// by retrying: bad/unregistered key, expired application, IP not allow-listed,
// or the daily quota already spent for today.
func isPermanentResultCode(code string) bool {
	switch code {
	case "20", "22", "30", "31", "32":
		return true
	default:
		return false
	}
}

// FetchDetail does not make a second HTTP call: getBidPblancListInfoServc
// already returns full notice detail per list row.
func (s *Source) FetchDetail(ctx context.Context, item collector.RawItem) (collector.RawDocument, error) {
	return collector.RawDocument{
		SourceID:         s.SourceCode(),
		ExternalNoticeID: item.ExternalNoticeID,
		RequestURL:       baseURL + "/" + operation,
		ResponseStatus:   http.StatusOK,
		RawContent:       item.RawPayload,
		CollectedAt:      s.now(),
	}, nil
}

func (s *Source) FetchAttachments(ctx context.Context, doc collector.RawDocument) ([]collector.Attachment, error) {
	var it bidItem
	if err := json.Unmarshal([]byte(doc.RawContent), &it); err != nil {
		return nil, fmt.Errorf("parse detail for attachments: %w", err)
	}

	urls := [10]string{
		it.NtceSpecDocUrl1, it.NtceSpecDocUrl2, it.NtceSpecDocUrl3, it.NtceSpecDocUrl4, it.NtceSpecDocUrl5,
		it.NtceSpecDocUrl6, it.NtceSpecDocUrl7, it.NtceSpecDocUrl8, it.NtceSpecDocUrl9, it.NtceSpecDocUrl10,
	}
	names := [10]string{
		it.NtceSpecFileNm1, it.NtceSpecFileNm2, it.NtceSpecFileNm3, it.NtceSpecFileNm4, it.NtceSpecFileNm5,
		it.NtceSpecFileNm6, it.NtceSpecFileNm7, it.NtceSpecFileNm8, it.NtceSpecFileNm9, it.NtceSpecFileNm10,
	}

	var out []collector.Attachment
	for i := range urls {
		if urls[i] == "" || names[i] == "" {
			continue
		}
		out = append(out, collector.Attachment{
			OriginalFilename: names[i],
			DownloadURL:      urls[i],
			FileType:         fileExt(names[i]),
		})
	}
	return out, nil
}

func fileExt(name string) string {
	idx := strings.LastIndex(name, ".")
	if idx == -1 || idx == len(name)-1 {
		return ""
	}
	return strings.ToLower(name[idx+1:])
}

func (s *Source) Normalize(ctx context.Context, doc collector.RawDocument) (collector.NormalizedNotice, error) {
	var it bidItem
	if err := json.Unmarshal([]byte(doc.RawContent), &it); err != nil {
		return collector.NormalizedNotice{}, fmt.Errorf("normalize: %w", err)
	}

	status := "open"
	if it.ReNtceYn == "Y" {
		status = "reannounced"
	}

	officialURL := it.BidNtceDtlUrl
	if officialURL == "" {
		officialURL = it.BidNtceUrl
	}

	// region — 지역제한이 없으면(대다수) "전국", 있으면 실제 지역명을 이
	// API가 안 줘서(위 bidItem 필드 주석 참고) 빈 문자열로 남긴다(공고 쪽
	// 데이터 부족으로 정직하게 처리됨 — 지어내지 않음).
	region := ""
	regionRestricted := it.BidPrtcptLmtYn == "Y" || it.CmmnSpldmdCorpRgnLmtYn == "Y"
	if !regionRestricted {
		region = regionNationwide
	}

	n := collector.NormalizedNotice{
		SourceID:         s.SourceCode(),
		ExternalNoticeID: it.BidNtceNo,
		NoticeType:       "procurement",
		Title:            it.BidNtceNm,
		OrganizationName: it.NtceInsttNm,
		DepartmentName:   it.DminsttNm,
		Region:           region,
		Industry:         it.PubPrcrmntMidClsfcNm,
		Status:           status,
		OfficialURL:      officialURL,
		RegionRestricted: &regionRestricted,
	}
	if t, err := parseG2BTime(it.BidNtceDt); err == nil {
		n.PublishedAt = &t
	}
	if t, err := parseG2BTime(it.BidBeginDt); err == nil {
		n.ApplicationStartAt = &t
	}
	if t, err := parseG2BTime(it.BidClseDt); err == nil {
		n.ApplicationEndAt = &t
	}
	if amt, err := strconv.ParseInt(it.AsignBdgtAmt, 10, 64); err == nil && amt > 0 {
		n.BudgetAmount = &amt
	}
	return n, nil
}

func parseG2BTime(v string) (time.Time, error) {
	if v == "" {
		return time.Time{}, fmt.Errorf("empty timestamp")
	}
	return time.ParseInLocation("2006-01-02 15:04:05", v, time.Local)
}
