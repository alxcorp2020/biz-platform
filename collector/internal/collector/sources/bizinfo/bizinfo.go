// Package bizinfo implements collector.Collector against 중소벤처기업부
// 기업마당(bizinfo.go.kr)의 지원사업 정보 Open API — notice_type=
// "support_program"(나라장터/g2b는 "procurement"). 기업마당은 중앙부처뿐
// 아니라 지방자치단체가 주관하는 지원사업도 같은 피드로 함께 제공한다
// (jrsdInsttNm에 "OO시청"/"OO도청"류 지자체 기관명이 함께 섞여 나옴) —
// 그래서 "기업마당 수집기"와 "지자체 지원사업 수집기"를 별도로 만들지
// 않고 이 패키지 하나로 둘 다 충족한다.
//
// 엔드포인트/파라미터/응답 필드는 bizinfo.go.kr이 공개한 API 안내
// 페이지(https://www.bizinfo.go.kr/apiDetail.do?id=bizinfoApi)에 실린
// 필드표 + XML/JSON 응답 예시 3곳을 교차 확인해 확정했다(추측 아님).
// data.go.kr에는 이 API의 fileData(스냅샷 다운로드) 항목만 있고 라이브
// OpenAPI는 없음을 확인함 — 기업마당 자체 도메인의 API가 유일한 경로.
//
// 엔드포인트/파라미터명(crtfcKey 포함)은 2026-08-01 실제 curl로도
// 재확인했다: 키 없이 호출 시 HTTP 200 + {"reqErr":"인증키를 입력해주세요."},
// 더미 키로는 HTTP 200 + {"reqErr":"존재하지 않는 인증키 입니다."} — 이
// 두 응답 모두 서버가 crtfcKey 파라미터 자체는 정확히 인식한다는 뜻이라
// 엔드포인트/파라미터명은 라이브로 검증됨. 단, 이 API는 유효한 키가 있어야만
// 정상 응답(성공 시 실제 필드 형태)을 볼 수 있어, 목록 수집 자체는 아직
// 실제 200 성공 응답으로 검증하지 못했다(사용자가 crtfcKey 발급 신청 중).
// totCnt 등 숫자 필드가 성공 응답에서 실제로 JSON number로 오는지
// string으로 오는지는 문서 예시의 HTML 렌더링이 애매해 확신할 수 없어
// 방어적으로(flexibleInt) 둘 다 받아들인다.
//
// 이 API는 g2b(BidPublicInfoService)와 달리 조회 기간(inqryBgnDt/
// inqryEndDt) 파라미터가 없다 — 서버 쪽 날짜 필터가 불가능하다. 대신
// 매 수집 주기마다 앞쪽 페이지(최신순으로 가정)를 maxItemsPerRun까지
// 다시 훑고, 이미 수집된 항목은 store 계층의 내용-해시 기반 변경감지가
// 자연스럽게 걸러낸다(재분석/재저장 없음) — g2b의 24시간 롤링 윈도우와
// 같은 목적을 서버 필터 대신 클라이언트 측 상한으로 달성한다.
package bizinfo

import (
	"bytes"
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
	baseURL = "https://www.bizinfo.go.kr/uss/rss/bizinfoApi.do"
	// 서버 날짜 필터가 없어 안전장치로 매 주기 상한을 둔다. 2026-08-08: 실 문서
	// 확인 결과 전체 지원사업이 ~1,400여 건이라, 정렬 순서와 무관하게 전량을
	// 확실히 담도록 1000→2000으로 상향(PageSize=100 기준 회당 최대 20호출, 여전히
	// 하루 총 480호출 이하로 매우 낮음).
	maxItemsPerRun = 2000
)

type Source struct {
	ServiceKey string // crtfcKey
	HTTPClient *http.Client
	RateLimit  *common.RateLimiter
	PageSize   int
	now        func() time.Time
}

// New creates a bizinfo Source. serviceKey is the crtfcKey issued by
// bizinfo.go.kr's own key-request form (이메일로 발급) — not a data.go.kr
// ServiceKey, this API doesn't go through that gateway.
func New(serviceKey string) *Source {
	return &Source{
		ServiceKey: serviceKey,
		HTTPClient: &http.Client{Timeout: 15 * time.Second},
		RateLimit:  common.NewRateLimiter(1, 2000),
		PageSize:   100,
		now:        time.Now,
	}
}

func (s *Source) SourceCode() string { return "bizinfo" }

// flexibleInt unmarshals from either a JSON number or a JSON string — see
// package doc comment for why this defensive handling exists (never
// verified against a live response).
type flexibleInt int

func (f *flexibleInt) UnmarshalJSON(data []byte) error {
	trimmed := strings.Trim(string(data), `"`)
	if trimmed == "" || trimmed == "null" {
		*f = 0
		return nil
	}
	n, err := strconv.Atoi(trimmed)
	if err != nil {
		*f = 0
		return nil // 카운트 파싱 실패는 페이지네이션만 보수적으로 만들 뿐 — 전체 실패로 만들지 않는다
	}
	*f = flexibleInt(n)
	return nil
}

// bizinfoItem is the subset of fields this collector maps, using the API's
// own field names (verified against bizinfo.go.kr's field table + XML/JSON
// response examples — see package doc comment).
type bizinfoItem struct {
	PblancId                   string      `json:"pblancId"`                   // 공고ID
	PblancNm                   string      `json:"pblancNm"`                   // 공고명
	PblancUrl                  string      `json:"pblancUrl"`                  // 공고URL
	JrsdInsttNm                string      `json:"jrsdInsttNm"`                // 소관기관명(중앙부처 또는 지자체)
	ExcInsttNm                 string      `json:"excInsttNm"`                 // 수행기관명
	BsnsSumryCn                string      `json:"bsnsSumryCn"`                // 사업개요내용
	ReqstBeginEndDe            string      `json:"reqstBeginEndDe"`            // 신청기간, "YYYYMMDD ~ YYYYMMDD"
	PldirSportRealmLclasCodeNm string      `json:"pldirSportRealmLclasCodeNm"` // 지원분야 대분류(g2b의 조달 업종 분류와는 다른 체계 — industryRawToGroup에 없는 값이라 자동으로 "확인필요"로만 처리되고 강제 매칭되지 않는다)
	CreatPnttm                 string      `json:"creatPnttm"`                 // 등록일자, "YYYY-MM-DD HH:MM:SS"
	FlpthNm                    string      `json:"flpthNm"`                    // 첨부파일 경로
	FileNm                     string      `json:"fileNm"`                     // 첨부파일명
	TotCnt                     flexibleInt `json:"totCnt"`                     // 전체건수(페이지마다 각 item에 반복됨)
}

// apiEnvelope.ReqErr — verified 2026-08-01 via live curl against the real
// endpoint (no key / a dummy key): unlike data.go.kr's gateway, bizinfo
// returns HTTP 200 with {"reqErr":"인증키를 입력해주세요."} or
// {"reqErr":"존재하지 않는 인증키 입니다."} for auth failures, never a
// 401/403 status. Without checking this field, a bad/expired key would
// silently look like "0 items collected" forever instead of failing loud.
// apiEnvelope — reqErr(인증 실패 등)는 항상 최상위. jsonArray는 형태가 소스마다
// 다르다: 2026-08-08 실 엔드포인트는 **배열**로 내려주는데(운영 로그로 확인:
// "cannot unmarshal array into ... jsonArray of type struct{Item ...}"), API 문서의
// JSON 예시는 **객체**({item:[...]})였다. 그래서 RawMessage로 받고 extractBizinfoItems가
// 배열/객체 양쪽과 item의 배열/단일까지 모두 흡수한다.
type apiEnvelope struct {
	ReqErr    string          `json:"reqErr"`
	JsonArray json.RawMessage `json:"jsonArray"`
}

// extractBizinfoItems는 jsonArray의 형태 변형을 하나로 정규화한다:
//   - 배열이고 원소가 채널 래퍼({..., "item": ...})면 각 원소의 item을 모아 펼친다
//   - 배열이고 원소가 공고 항목 자체(pblancId 보유)면 그대로 담는다
//   - 객체({item:[...]})면 그 item을 쓴다(문서 예시 형태)
//   - item은 배열 또는 (단일 결과일 때) 객체 하나일 수 있다
func extractBizinfoItems(raw json.RawMessage) []bizinfoItem {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	switch raw[0] {
	case '[':
		var arr []json.RawMessage
		if json.Unmarshal(raw, &arr) != nil {
			return nil
		}
		var out []bizinfoItem
		for _, el := range arr {
			var wrap struct {
				Item json.RawMessage `json:"item"`
			}
			if json.Unmarshal(el, &wrap) == nil && len(bytes.TrimSpace(wrap.Item)) > 0 {
				out = append(out, decodeBizinfoItemList(wrap.Item)...)
				continue
			}
			var it bizinfoItem
			if json.Unmarshal(el, &it) == nil && it.PblancId != "" {
				out = append(out, it)
			}
		}
		return out
	case '{':
		var wrap struct {
			Item json.RawMessage `json:"item"`
		}
		if json.Unmarshal(raw, &wrap) == nil && len(bytes.TrimSpace(wrap.Item)) > 0 {
			return decodeBizinfoItemList(wrap.Item)
		}
	}
	return nil
}

func decodeBizinfoItemList(raw json.RawMessage) []bizinfoItem {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil
	}
	if raw[0] == '[' {
		var arr []bizinfoItem
		if json.Unmarshal(raw, &arr) == nil {
			return arr
		}
		return nil
	}
	var single bizinfoItem
	if json.Unmarshal(raw, &single) == nil && single.PblancId != "" {
		return []bizinfoItem{single}
	}
	return nil
}

func (s *Source) FetchList(ctx context.Context, cursor collector.Cursor) ([]collector.RawItem, collector.Cursor, error) {
	if err := s.RateLimit.Wait(ctx); err != nil {
		return nil, cursor, err
	}

	pageIndex := 1
	if cursor.Token != "" {
		if n, err := strconv.Atoi(cursor.Token); err == nil {
			pageIndex = n
		}
	}

	q := url.Values{}
	q.Set("crtfcKey", s.ServiceKey)
	q.Set("dataType", "json")
	q.Set("pageUnit", strconv.Itoa(s.PageSize))
	q.Set("pageIndex", strconv.Itoa(pageIndex))
	reqURL := baseURL + "?" + q.Encode()

	var envelope apiEnvelope
	err := common.Do(ctx, common.DefaultRetryConfig(), func() error {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
		resp, err := s.HTTPClient.Do(req)
		if err != nil {
			return err
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
			return &common.PermanentError{Err: fmt.Errorf("parse response: %w", err)}
		}
		if envelope.ReqErr != "" {
			// bizinfo always answers HTTP 200 even on auth failure — see
			// apiEnvelope.ReqErr doc comment. Not retryable: a bad key won't
			// fix itself.
			return &common.PermanentError{Err: fmt.Errorf("bizinfo api error: %s", envelope.ReqErr)}
		}
		return nil
	})
	if err != nil {
		return nil, cursor, err
	}

	itemList := extractBizinfoItems(envelope.JsonArray)
	items := make([]collector.RawItem, 0, len(itemList))
	for _, it := range itemList {
		raw, err := json.Marshal(it)
		if err != nil {
			continue
		}
		items = append(items, collector.RawItem{
			SourceID:         s.SourceCode(),
			ExternalNoticeID: it.PblancId,
			Title:            it.PblancNm,
			RawPayload:       string(raw),
		})
	}

	var totCnt int
	if len(itemList) > 0 {
		totCnt = int(itemList[0].TotCnt)
	}
	fetchedSoFar := pageIndex * s.PageSize
	nextCursor := collector.Cursor{
		Token:   strconv.Itoa(pageIndex + 1),
		HasMore: len(items) > 0 && fetchedSoFar < totCnt && fetchedSoFar < maxItemsPerRun,
	}
	return items, nextCursor, nil
}

// FetchDetail does not make a second HTTP call: the list operation already
// returns full detail per row (same as g2b).
func (s *Source) FetchDetail(ctx context.Context, item collector.RawItem) (collector.RawDocument, error) {
	return collector.RawDocument{
		SourceID:         s.SourceCode(),
		ExternalNoticeID: item.ExternalNoticeID,
		RequestURL:       baseURL,
		ResponseStatus:   http.StatusOK,
		RawContent:       item.RawPayload,
		CollectedAt:      s.now(),
	}, nil
}

func (s *Source) FetchAttachments(ctx context.Context, doc collector.RawDocument) ([]collector.Attachment, error) {
	var it bizinfoItem
	if err := json.Unmarshal([]byte(doc.RawContent), &it); err != nil {
		return nil, fmt.Errorf("parse detail for attachments: %w", err)
	}
	if it.FlpthNm == "" || it.FileNm == "" {
		return nil, nil
	}
	return []collector.Attachment{{
		OriginalFilename: it.FileNm,
		DownloadURL:      it.FlpthNm,
		FileType:         fileExt(it.FileNm),
	}}, nil
}

func fileExt(name string) string {
	idx := strings.LastIndex(name, ".")
	if idx == -1 || idx == len(name)-1 {
		return ""
	}
	return strings.ToLower(name[idx+1:])
}

func (s *Source) Normalize(ctx context.Context, doc collector.RawDocument) (collector.NormalizedNotice, error) {
	var it bizinfoItem
	if err := json.Unmarshal([]byte(doc.RawContent), &it); err != nil {
		return collector.NormalizedNotice{}, fmt.Errorf("normalize: %w", err)
	}

	n := collector.NormalizedNotice{
		SourceID:         s.SourceCode(),
		ExternalNoticeID: it.PblancId,
		NoticeType:       "support_program",
		Title:            it.PblancNm,
		OrganizationName: it.JrsdInsttNm,
		DepartmentName:   it.ExcInsttNm,
		Industry:         it.PldirSportRealmLclasCodeNm,
		// Region은 이 API에 별도 필드가 없다(hashTags에 지역명이 섞여
		// 나오지만 연도/분야/기관명과 뒤섞인 비정형 문자열이라 신뢰성
		// 있게 파싱할 수 없음) — 비워두면 scoreRegion이 "지역 정보
		// 없음"으로 정직하게 처리한다(오매칭보다 낫다).
		Status:      "open", // g2b와 같은 관례: 수집 시점엔 항상 open, 마감 여부는 조회 시 application_end_at으로 판단
		OfficialURL: it.PblancUrl,
	}
	if start, end, err := parseReqstBeginEndDe(it.ReqstBeginEndDe); err == nil {
		n.ApplicationStartAt = &start
		n.ApplicationEndAt = &end
	}
	if t, err := parseBizinfoTimestamp(it.CreatPnttm); err == nil {
		n.PublishedAt = &t
	}
	return n, nil
}

func parseReqstBeginEndDe(v string) (start, end time.Time, err error) {
	parts := strings.Split(v, "~")
	if len(parts) != 2 {
		return time.Time{}, time.Time{}, fmt.Errorf("unexpected reqstBeginEndDe format: %q", v)
	}
	start, err = time.ParseInLocation("20060102", strings.TrimSpace(parts[0]), time.Local)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	end, err = time.ParseInLocation("20060102", strings.TrimSpace(parts[1]), time.Local)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	return start, end, nil
}

func parseBizinfoTimestamp(v string) (time.Time, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return time.Time{}, fmt.Errorf("empty timestamp")
	}
	return time.ParseInLocation("2006-01-02 15:04:05", v, time.Local)
}
