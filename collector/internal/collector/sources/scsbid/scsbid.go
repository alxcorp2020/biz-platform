// Package scsbid fetches 조달청 나라장터 낙찰정보서비스(ScsbidInfoService)의
// 용역 부문 낙찰현황(getScsbidListSttusServcPPSSrch) — g2b 패키지가 수집하는
// notices가 전부 용역 부문이라 범위를 맞췄다(공사/물품/외자는 이번 범위 밖).
//
// 엔드포인트/파라미터/응답 필드는 추측이 아니라 data.go.kr의 API 상세
// 페이지(https://www.data.go.kr/data/15129397/openapi.do)에 내장된 Swagger
// 스펙(JSON)을 직접 파싱해 확인했다:
//
//	host: apis.data.go.kr/1230000/as/ScsbidInfoService
//	path: /getScsbidListSttusServcPPSSrch
//
// g2b.go가 검증한 "/ad/" 세그먼트와 마찬가지로 이 서비스는 "/as/"가 맞는
// 경로다 — 실제로 이 경로에 대해서만 403 Forbidden(순수 텍스트, Kong 프록시
// latency 헤더 없음)이 오고 나머지 조합(다른 접두사/오퍼레이션명)은 전부
// 404/500이라, 게이트웨이가 이 경로 자체는 인식한다는 뜻이다. 즉 URL이
// 틀린 게 아니라 이 서비스키에 대한 활용신청 승인이 아직 게이트웨이에
// 반영되지 않았거나, 사용자가 승인받은 게 이 데이터셋(15129397)이 아닌
// 유사한 다른 낙찰 관련 API일 가능성이 있다 — data.go.kr 마이페이지 >
// 활용신청현황에서 정확히 이 데이터셋명("조달청_나라장터 낙찰정보서비스")의
// 승인 상태를 재확인해야 한다. 이 파일의 코드는 그 문제와 무관하게 실제
// Swagger 스키마 그대로 작성했으므로, 403이 풀리는 즉시 별도 코드 수정 없이
// 동작해야 한다.
package scsbid

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"biz-platform/collector/internal/collector/common"
)

const (
	baseURL   = "https://apis.data.go.kr/1230000/as/ScsbidInfoService"
	operation = "getScsbidListSttusServcPPSSrch"
)

type Source struct {
	ServiceKey string
	HTTPClient *http.Client
	RateLimit  *common.RateLimiter
	PageSize   int
}

// New creates a scsbid Source. serviceKey is the decoded (raw) data.go.kr
// service key — the same key used for g2b.New, since data.go.kr issues one
// key per account that covers every API that account has been approved for.
func New(serviceKey string) *Source {
	return &Source{
		ServiceKey: serviceKey,
		HTTPClient: &http.Client{Timeout: 15 * time.Second},
		RateLimit:  common.NewRateLimiter(1, 1000),
		PageSize:   100,
	}
}

// AwardRecord is the subset of getScsbidListSttusServcPPSSrch's response
// fields this ingestion stores, using the API's own field names (verified
// against data.go.kr's embedded Swagger spec — not guessed).
//
// Note DminsttNm(수요기관명) is the only agency-name field this operation
// returns — 공고기관명(ntceInsttNm)은 요청 파라미터로만 받고 응답에는 없다.
// notices.organization_name은 ntceInsttNm 기반이라, 매칭 시 이 값은
// notices.department_name(같은 dminsttNm 출처)과 비교해야 한다 — 자세한
// 이유는 award_history.go의 fetchOrganizationAwardHistory 주석 참고.
type AwardRecord struct {
	BidNtceNo    string `json:"bidNtceNo"`
	BidNtceOrd   string `json:"bidNtceOrd"`
	BidNtceNm    string `json:"bidNtceNm"`    // 입찰공고명
	DminsttNm    string `json:"dminsttNm"`    // 수요기관명
	BidwinnrNm   string `json:"bidwinnrNm"`   // 최종낙찰업체명
	SucsfbidAmt  string `json:"sucsfbidAmt"`  // 최종낙찰금액
	SucsfbidRate string `json:"sucsfbidRate"` // 최종낙찰률(%, 예: "87.789")
	RlOpengDt    string `json:"rlOpengDt"`    // 실개찰일시
}

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
	Items      itemsWrapper `json:"items"`
	TotalCount string       `json:"totalCount"`
}

// itemsWrapper handles the three shapes this API's "items" field can take:
//   - "" (string) on NODATA_ERROR
//   - {"item": {...}} when there's exactly one result (조달청 계열 API 공통
//     XML→JSON 변환 특성 — 결과가 1건이면 배열이 아니라 단일 객체로 온다)
//   - {"item": [...]} when there are multiple results
type itemsWrapper struct {
	Items []json.RawMessage
}

func (w *itemsWrapper) UnmarshalJSON(data []byte) error {
	var asString string
	if err := json.Unmarshal(data, &asString); err == nil {
		w.Items = nil
		return nil
	}
	var raw struct {
		Item json.RawMessage `json:"item"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if len(raw.Item) == 0 || string(raw.Item) == "null" {
		w.Items = nil
		return nil
	}
	if raw.Item[0] == '[' {
		return json.Unmarshal(raw.Item, &w.Items)
	}
	w.Items = []json.RawMessage{raw.Item}
	return nil
}

// FetchAwards returns every award record whose 실개찰일시(RlOpengDt) falls
// within [begin, end), paging through the full result set.
func (s *Source) FetchAwards(ctx context.Context, begin, end time.Time) ([]AwardRecord, error) {
	var all []AwardRecord
	pageNo := 1
	for {
		if err := s.RateLimit.Wait(ctx); err != nil {
			return all, err
		}

		q := url.Values{}
		q.Set("serviceKey", s.ServiceKey)
		q.Set("pageNo", strconv.Itoa(pageNo))
		q.Set("numOfRows", strconv.Itoa(s.PageSize))
		q.Set("type", "json")
		q.Set("inqryDiv", "1")
		q.Set("inqryBgnDt", begin.Format("200601021504"))
		q.Set("inqryEndDt", end.Format("200601021504"))
		reqURL := baseURL + "/" + operation + "?" + q.Encode()

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
				return &common.PermanentError{Err: fmt.Errorf("parse response envelope: %w", err)}
			}
			return nil
		})
		if err != nil {
			return all, err
		}

		switch code := envelope.Response.Header.ResultCode; code {
		case "00":
			// success — fall through
		case "03":
			return all, nil // NODATA_ERROR
		default:
			msg := fmt.Sprintf("scsbid api error %s: %s", code, envelope.Response.Header.ResultMsg)
			if isPermanentResultCode(code) {
				return all, &common.PermanentError{Err: fmt.Errorf("%s", msg)}
			}
			return all, fmt.Errorf("%s", msg)
		}

		var body apiBody
		if err := json.Unmarshal(envelope.Response.Body, &body); err != nil {
			return all, &common.PermanentError{Err: fmt.Errorf("parse response body: %w", err)}
		}

		for _, raw := range body.Items.Items {
			var rec AwardRecord
			if err := json.Unmarshal(raw, &rec); err != nil {
				continue // skip a malformed row rather than failing the whole page
			}
			all = append(all, rec)
		}

		totalCount, _ := strconv.Atoi(body.TotalCount)
		if pageNo*s.PageSize >= totalCount || len(body.Items.Items) == 0 {
			return all, nil
		}
		pageNo++
	}
}

// isPermanentResultCode mirrors g2b's list: bad/unregistered key, expired
// application, IP not allow-listed, or the daily quota already spent today.
func isPermanentResultCode(code string) bool {
	switch code {
	case "20", "22", "30", "31", "32":
		return true
	default:
		return false
	}
}
