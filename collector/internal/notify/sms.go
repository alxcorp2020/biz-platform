// SMS 발송 클라이언트 — 알리고(Aligo, https://smartsms.aligo.in) REST API.
// 이메일 알림(client.go)이 Resend를 감싸는 것과 같은 방식으로 plain
// net/http만 쓴다.
//
// 알리고 vs NHN Cloud 문자 API 중 알리고를 선택한 이유(둘 다 공식 문서
// 확인, 2026-07-31): 알리고는 단일 엔드포인트(POST https://apis.aligo.in/send/)
// + 폼인코딩 파라미터(key/user_id/sender/receiver/msg) + 평평한 JSON
// 응답이라 이 프로젝트의 기존 패턴(Resend, Toss 개별연동)과 결이 같다.
// NHN Cloud는 앱키를 URL 경로에 넣고 별도 시크릿키 헤더까지 요구하며,
// 요청 바디도 중첩 JSON(sendNo/recipientNo 등)이고 응답도 header/body로
// 나뉘어 더 복잡하다 — 발송량이 많지 않은 이 단계에서는 알리고 쪽이
// 구현/유지보수 부담이 명확히 낮다.
package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const aligoSendEndpoint = "https://apis.aligo.in/send/"

// SMSClient sends SMS through Aligo. apiKey/userID/sender are read once at
// construction (from ALIGO_API_KEY / ALIGO_USER_ID / ALIGO_SENDER — sender는
// 알리고 관리자 페이지에 사전 등록된 발신번호여야 함) — see cmd/apiserver.
type SMSClient struct {
	apiKey string
	userID string
	sender string
	http   *http.Client
}

func NewSMSClient(apiKey, userID, sender string) *SMSClient {
	return &SMSClient{apiKey: apiKey, userID: userID, sender: sender, http: &http.Client{Timeout: 10 * time.Second}}
}

// Configured reports whether Aligo credentials are set — 세 값이 다
// 있어야 실제 발송이 가능하다(하나라도 비어있으면 Send가 항상 실패).
// email.Client.Configured()와 같은 용도: 배치가 시도 자체를 건너뛰고
// notification_log에 실패 기록만 쌓이는 걸 막는다.
func (c *SMSClient) Configured() bool {
	return c.apiKey != "" && c.userID != "" && c.sender != ""
}

// aligoResponse is the flat JSON Aligo returns — result_code 1이 성공,
// 그 외는 실패(음수 코드별 의미는 알리고 문서 참고 — message 필드에
// 사람이 읽을 수 있는 설명이 함께 온다).
type aligoResponse struct {
	ResultCode int    `json:"result_code"`
	Message    string `json:"message"`
}

// Send delivers one SMS. to는 하이픈 없는 숫자만(예: 01012345678) —
// 알리고는 receiver를 그 형식으로 받는다. msg_type을 지정하지 않으면
// 알리고가 바이트 수 기준으로 SMS/LMS를 자동 판단하므로, 호출부는 짧은
// 메시지(대략 90바이트, 한글 약 45자 이내)를 유지해 SMS 단가로 발송되게
// 한다 — 강제하진 않지만 notifications.go의 메시지 조립 시 이 한도를
// 염두에 둔다.
func (c *SMSClient) Send(ctx context.Context, to, msg string) error {
	if !c.Configured() {
		return fmt.Errorf("ALIGO_API_KEY/ALIGO_USER_ID/ALIGO_SENDER not configured")
	}

	form := url.Values{}
	form.Set("key", c.apiKey)
	form.Set("user_id", c.userID)
	form.Set("sender", c.sender)
	form.Set("receiver", to)
	form.Set("msg", msg)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, aligoSendEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("aligo request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("aligo api error: status=%d body=%s", resp.StatusCode, string(body))
	}

	var result aligoResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return fmt.Errorf("parse aligo response: %w (body=%s)", err, string(body))
	}
	if result.ResultCode != 1 {
		return fmt.Errorf("aligo send failed: result_code=%d message=%s", result.ResultCode, result.Message)
	}
	return nil
}
