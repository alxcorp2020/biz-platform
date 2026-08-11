package g2b

// enrich.go — Phase C(2026-08-11). 입찰공고 기본 목록(getBidPblancListInfoServc)에는 없는
// "투찰자격 제한" 상세를 같은 서비스(BidPublicInfoService)의 추가 공식 오퍼레이션으로 보강한다.
//
//	getBidPblancListInfoPrtcptPsblRgn  → 참가가능지역(지역명 1:N)
//	getBidPblancListInfoLicenseLimit   → 허용업종/면허 제한(명·코드 1:N)
//
// bidNtceNo + bidNtceOrd로 조회하며, 응답 envelope/resultCode 처리는 g2b.go의 것을 재사용한다.
// (기초금액 getBidPblancListInfoServcBsisAmount는 실측 표본을 확보한 뒤 별도 STEP에서 붙인다.)

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

// RegionLimit — 참가가능지역 1건.
type RegionLimit struct {
	RegionName       string
	BusinessDivision string
	SortNo           int
}

// LicenseLimit — 허용업종/면허 제한 1건. LicenseName은 "폐기물종합처분업/1143"처럼 명/코드가
// 함께 온다. LimitGroupNo는 OR/AND 그룹 힌트(같은 그룹=선택, 다른 그룹=필수 등으로 보이나 API가
// 의미를 명시하지 않아 값만 보존한다 — 추측 금지).
type LicenseLimit struct {
	LicenseName         string
	PermittedIndustries string
	IndustryField       string
	LimitGroupNo        string
	BusinessDivision    string
	SortNo              int
}

// EnrichmentClient — 보강 오퍼레이션 전용 경량 클라이언트(수집기와 별개로 API 서버 프로세스의
// 백그라운드 배치가 쓴다). g2b.Source와 키/레이트리밋 정책을 공유한다.
type EnrichmentClient struct {
	ServiceKey string
	HTTPClient *http.Client
	RateLimit  *common.RateLimiter
	BaseURL    string // 기본 baseURL. 테스트에서 모의 서버로 교체 가능.
}

// 기본 레이트리밋(보수적) — g2b/data.go.kr 실제 일일쿼터 확인 전 안전값.
// 공고당 2콜(지역+면허)이라 dailyLimit 1000 = 하루 약 500공고가 실질 상한.
const (
	defaultEnrichPerSecond = 1.0
	defaultEnrichDailyCap  = 1000
)

// NewEnrichmentClient — serviceKey는 g2b.New와 동일한 발급 키. 기본 레이트리밋 사용.
func NewEnrichmentClient(serviceKey string) *EnrichmentClient {
	return NewEnrichmentClientWithLimits(serviceKey, defaultEnrichPerSecond, defaultEnrichDailyCap)
}

// NewEnrichmentClientWithLimits — 레이트리밋을 지정해 생성한다(백필 가속 튜닝용). 백필 시
// g2b 실제 일일쿼터 범위 안에서 perSecond/perDay를 올리면 처리량이 늘어난다. 0 이하 값은
// 기본값으로 폴백해 잘못된 설정으로 레이트리밋이 사실상 무력화되는 것을 막는다.
func NewEnrichmentClientWithLimits(serviceKey string, perSecond float64, perDay int) *EnrichmentClient {
	if perSecond <= 0 {
		perSecond = defaultEnrichPerSecond
	}
	if perDay <= 0 {
		perDay = defaultEnrichDailyCap
	}
	return &EnrichmentClient{
		ServiceKey: serviceKey,
		HTTPClient: &http.Client{Timeout: 15 * time.Second},
		RateLimit:  common.NewRateLimiter(perSecond, perDay),
		BaseURL:    baseURL,
	}
}

// fetchItems — 지정 오퍼레이션을 bidNtceNo/Ord로 조회해 items 원본을 반환한다.
// resultCode "03"(NODATA)은 빈 슬라이스로 정상 처리한다.
func (c *EnrichmentClient) fetchItems(ctx context.Context, operation, bidNtceNo, bidNtceOrd string) ([]json.RawMessage, error) {
	if err := c.RateLimit.Wait(ctx); err != nil {
		return nil, err
	}
	q := url.Values{}
	q.Set("ServiceKey", c.ServiceKey)
	q.Set("inqryDiv", "2")
	q.Set("type", "json")
	q.Set("bidNtceNo", bidNtceNo)
	if bidNtceOrd != "" {
		q.Set("bidNtceOrd", bidNtceOrd)
	}
	q.Set("numOfRows", "100")
	q.Set("pageNo", "1")
	base := c.BaseURL
	if base == "" {
		base = baseURL
	}
	reqURL := base + "/" + operation + "?" + q.Encode()

	var envelope apiEnvelope
	err := common.Do(ctx, common.DefaultRetryConfig(), func() error {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
		resp, err := c.HTTPClient.Do(req)
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
			return &common.PermanentError{Err: fmt.Errorf("parse envelope: %w", err)}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	switch code := envelope.Response.Header.ResultCode; code {
	case "00":
		// success
	case "03":
		return nil, nil // NODATA — 제한 없음(정상)
	default:
		msg := fmt.Sprintf("g2b enrich %s error %s: %s", operation, code, envelope.Response.Header.ResultMsg)
		if isPermanentResultCode(code) {
			return nil, &common.PermanentError{Err: fmt.Errorf("%s", msg)}
		}
		return nil, fmt.Errorf("%s", msg)
	}

	var b apiBody
	if err := json.Unmarshal(envelope.Response.Body, &b); err != nil {
		return nil, &common.PermanentError{Err: fmt.Errorf("parse body: %w", err)}
	}
	return b.Items, nil
}

// FetchParticipationRegions — 참가가능지역 목록(중복은 호출부/DB가 아니라 여기서 dedup하지 않고
// 원본 순서대로 반환한다 — 표시 단계에서 정리).
func (c *EnrichmentClient) FetchParticipationRegions(ctx context.Context, bidNtceNo, bidNtceOrd string) ([]RegionLimit, error) {
	items, err := c.fetchItems(ctx, "getBidPblancListInfoPrtcptPsblRgn", bidNtceNo, bidNtceOrd)
	if err != nil {
		return nil, err
	}
	out := make([]RegionLimit, 0, len(items))
	for _, raw := range items {
		var it struct {
			PrtcptPsblRgnNm string `json:"prtcptPsblRgnNm"`
			BsnsDivNm       string `json:"bsnsDivNm"`
			LmtSno          string `json:"lmtSno"`
		}
		if err := json.Unmarshal(raw, &it); err != nil {
			continue
		}
		name := strings.TrimSpace(it.PrtcptPsblRgnNm)
		if name == "" {
			continue
		}
		out = append(out, RegionLimit{
			RegionName:       name,
			BusinessDivision: strings.TrimSpace(it.BsnsDivNm),
			SortNo:           atoiSafe(it.LmtSno),
		})
	}
	return out, nil
}

// FetchLicenseLimits — 허용업종/면허 제한 목록.
func (c *EnrichmentClient) FetchLicenseLimits(ctx context.Context, bidNtceNo, bidNtceOrd string) ([]LicenseLimit, error) {
	items, err := c.fetchItems(ctx, "getBidPblancListInfoLicenseLimit", bidNtceNo, bidNtceOrd)
	if err != nil {
		return nil, err
	}
	out := make([]LicenseLimit, 0, len(items))
	for _, raw := range items {
		var it struct {
			LcnsLmtNm            string `json:"lcnsLmtNm"`
			PermsnIndstrytyList  string `json:"permsnIndstrytyList"`
			IndstrytyMfrcFldList string `json:"indstrytyMfrcFldList"`
			LmtGrpNo             string `json:"lmtGrpNo"`
			BsnsDivNm            string `json:"bsnsDivNm"`
			LmtSno               string `json:"lmtSno"`
		}
		if err := json.Unmarshal(raw, &it); err != nil {
			continue
		}
		name := strings.TrimSpace(it.LcnsLmtNm)
		if name == "" {
			continue
		}
		out = append(out, LicenseLimit{
			LicenseName:         name,
			PermittedIndustries: strings.TrimSpace(it.PermsnIndstrytyList),
			IndustryField:       strings.TrimSpace(it.IndstrytyMfrcFldList),
			LimitGroupNo:        strings.TrimSpace(it.LmtGrpNo),
			BusinessDivision:    strings.TrimSpace(it.BsnsDivNm),
			SortNo:              atoiSafe(it.LmtSno),
		})
	}
	return out, nil
}

func atoiSafe(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0
	}
	return n
}
