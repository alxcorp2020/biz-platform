package oauth

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
)

// KakaoClient implements Client for "카카오로 로그인"
// (https://developers.kakao.com/docs/latest/ko/kakaologin/rest-api). client_id는
// 카카오 REST API 키를 그대로 쓴다(KAKAO_REST_API_KEY) — client_secret은
// 카카오 개발자 콘솔에서 "Client Secret 코드 사용"을 활성화했을 때만 필요한
// 선택 값이라(KAKAO_CLIENT_SECRET), 비어 있으면 아예 안 보낸다.
type KakaoClient struct {
	clientID     string
	clientSecret string
	redirectURI  string
	http         *http.Client
}

func NewKakaoClient(clientID, clientSecret, redirectURI string) *KakaoClient {
	return &KakaoClient{
		clientID: clientID, clientSecret: clientSecret, redirectURI: redirectURI,
		http: &http.Client{Timeout: 10 * time.Second},
	}
}

// Configured — client_secret은 선택이라 clientID(REST API 키)만 확인한다.
func (c *KakaoClient) Configured() bool {
	return c.clientID != ""
}

func (c *KakaoClient) AuthURL(state string) string {
	v := url.Values{
		"response_type": {"code"},
		"client_id":     {c.clientID},
		"redirect_uri":  {c.redirectURI},
		"state":         {state},
	}
	return "https://kauth.kakao.com/oauth/authorize?" + v.Encode()
}

func (c *KakaoClient) Exchange(ctx context.Context, code string) (string, error) {
	form := url.Values{
		"grant_type":   {"authorization_code"},
		"client_id":    {c.clientID},
		"redirect_uri": {c.redirectURI},
		"code":         {code},
	}
	if c.clientSecret != "" {
		form.Set("client_secret", c.clientSecret)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://kauth.kakao.com/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("token request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("kakao token exchange failed: status=%d body=%s", resp.StatusCode, string(body))
	}

	var out struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("decode token response: %w", err)
	}
	if out.AccessToken == "" {
		return "", fmt.Errorf("kakao token response missing access_token")
	}
	return out.AccessToken, nil
}

func (c *KakaoClient) FetchUser(ctx context.Context, accessToken string) (email, providerUserID string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://kapi.kakao.com/v2/user/me", nil)
	if err != nil {
		return "", "", fmt.Errorf("build userinfo request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, err := c.http.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("userinfo request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return "", "", fmt.Errorf("kakao userinfo failed: status=%d body=%s", resp.StatusCode, string(body))
	}

	var out struct {
		ID           int64 `json:"id"`
		KakaoAccount struct {
			Email             string `json:"email"`
			IsEmailValid      bool   `json:"is_email_valid"`
			IsEmailVerified   bool   `json:"is_email_verified"`
			EmailNeedsConsent bool   `json:"email_needs_agreement"`
		} `json:"kakao_account"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", "", fmt.Errorf("decode userinfo response: %w", err)
	}
	if out.ID == 0 {
		return "", "", fmt.Errorf("kakao userinfo response missing id")
	}
	// 이메일 동의를 안 했거나 미인증이면 빈 문자열로 돌려준다 — 호출부
	// (resolveOAuthUser)가 "이메일 없이는 기존 계정 매칭/신규가입 불가"로
	// 처리한다(카카오 로그인 화면에서 이메일 제공 동의가 필수이도록 카카오
	// 개발자 콘솔에서 설정해두는 게 이 경로를 실질적으로 막는 방법).
	if !out.KakaoAccount.IsEmailValid || !out.KakaoAccount.IsEmailVerified {
		return "", strconv.FormatInt(out.ID, 10), nil
	}
	return out.KakaoAccount.Email, strconv.FormatInt(out.ID, 10), nil
}
