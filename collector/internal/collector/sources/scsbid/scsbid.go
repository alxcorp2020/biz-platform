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
// ✅ 2026-08-06 최종 확인: 이 경로/서비스키는 정상이다(운영 승인 완료,
// 403 아님) — 예전엔 이 지점에서 403 Forbidden이 났었는데, 실제 원인은
// URL/키/승인 문제가 아니라 **응답 파싱 버그**였다(itemsWrapper 주석
// 참고 — items 필드가 예상과 달리 "item" 래퍼 없이 배열로 바로 옴). 운영
// 키로 실제 프로덕션 코드를 직접 재현해 확정했다. 이 주석은 예전 진단이
// 틀렸었다는 걸 남겨두는 기록용 — 더 이상 IP 등록/활용신청 승인 문제를
// 의심할 필요 없음.
package scsbid

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
	PrtcptCnum   string `json:"prtcptCnum"`   // 참가업체수(2026-08-06 추가, 실측 응답에 이미 존재 확인됨)
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
	TotalCount flexibleInt  `json:"totalCount"`
}

// flexibleInt — 2026-08-06 실측 확인: totalCount는 문자열이 아니라 맨
// 숫자로 온다(다른 조달청 계열 API는 문자열로 오는 경우가 있어 처음엔
// string으로 추정했다가 파싱 에러로 잡힘). 숫자/따옴표 문자열 양쪽 다
// 받아주는 방어적 타입 — 이 API군이 필드마다 타입 표기가 일관되지
// 않는다는 걸 두 번째로 확인했으니(items 래퍼 건과 함께) 앞으로 이
// 패키지에 필드를 더 추가할 때도 숫자 필드는 이 타입을 우선 검토할 것.
type flexibleInt int

func (n *flexibleInt) UnmarshalJSON(data []byte) error {
	s := strings.Trim(string(data), `"`)
	if s == "" {
		*n = 0
		return nil
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return err
	}
	*n = flexibleInt(v)
	return nil
}

// itemsWrapper handles the shapes this API's "items" field can take.
//
// 🚨 2026-08-06 실측 정정: 이 필드가 나라장터 계열 API 공통 관행대로
// {"item": {...}}/{"item": [...]}로 감싸져 올 것이라 추정했었으나(아래
// 주석은 그 추정의 흔적), 운영 키로 실제 프로덕션 코드(scsbid.FetchAwards)를
// 직접 호출해 재현한 결과 이 오퍼레이션(getScsbidListSttusServcPPSSrch)은
// **"item" 래퍼 없이 items 필드에 곧장 배열을 내려준다**(numOfRows=1이어도
// [{...}] 형태) — 이게 "Render에서만 낙찰이력 수집이 500으로 실패하던"
// 진짜 원인이었다(IP 제한이 아니라 순수 JSON 파싱 버그, 로컬에서도 이
// 코드를 실제로 돌려보기 전까진 안 잡혔음 — curl로 눈으로 확인할 때는
// resultCode만 보고 실제 struct 파싱까지는 확인 안 했던 게 놓친 이유).
// 배열 형태를 최우선으로 시도하고, 혹시 이 API가 상황에 따라 예전
// 추정대로 {"item":...}로 감싸 올 수도 있으니(다른 페이지/기간에서
// 다르게 동작할 가능성을 배제 못 해) 그 형태도 계속 폴백으로 처리한다.
//   - [...] — 실측된 실제 형태(최우선)
//   - "" (string) — NODATA_ERROR로 추정되는 경우
//   - {"item": {...}} / {"item": [...]} — 예전 추정, 폴백으로 유지
type itemsWrapper struct {
	Items []json.RawMessage
}

func (w *itemsWrapper) UnmarshalJSON(data []byte) error {
	var asArray []json.RawMessage
	if err := json.Unmarshal(data, &asArray); err == nil {
		w.Items = asArray
		return nil
	}
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

		totalCount := int(body.TotalCount)
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
