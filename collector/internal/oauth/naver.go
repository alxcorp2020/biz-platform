package oauth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// NaverClient implements Client for "네이버로 로그인"
// (https://developers.naver.com/docs/login/api/api.md). 네이버는 구글/카카오와
// 달리 토큰 엔드포인트가 GET(쿼리 파라미터)이라 별도 구현이 필요하다.
type NaverClient struct {
	clientID     string
	clientSecret string
	redirectURI  string
	http         *http.Client
}

func NewNaverClient(clientID, clientSecret, redirectURI string) *NaverClient {
	return &NaverClient{
		clientID: clientID, clientSecret: clientSecret, redirectURI: redirectURI,
		http: &http.Client{Timeout: 10 * time.Second},
	}
}

func (c *NaverClient) Configured() bool {
	return c.clientID != "" && c.clientSecret != ""
}

func (c *NaverClient) AuthURL(state string) string {
	v := url.Values{
		"response_type": {"code"},
		"client_id":     {c.clientID},
		"redirect_uri":  {c.redirectURI},
		"state":         {state},
	}
	return "https://nid.naver.com/oauth2.0/authorize?" + v.Encode()
}

func (c *NaverClient) Exchange(ctx context.Context, code string) (string, error) {
	v := url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {c.clientID},
		"client_secret": {c.clientSecret},
		"code":          {code},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://nid.naver.com/oauth2.0/token?"+v.Encode(), nil)
	if err != nil {
		return "", fmt.Errorf("build token request: %w", err)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("token request failed: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read token response: %w", err)
	}
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("naver token exchange failed: status=%d body=%s", resp.StatusCode, string(body))
	}

	var out struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
		ErrorDesc   string `json:"error_description"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("decode token response: %w", err)
	}
	if out.Error != "" {
		return "", fmt.Errorf("naver token exchange error: %s (%s)", out.Error, out.ErrorDesc)
	}
	if out.AccessToken == "" {
		return "", fmt.Errorf("naver token response missing access_token")
	}
	return out.AccessToken, nil
}

func (c *NaverClient) FetchUser(ctx context.Context, accessToken string) (email, providerUserID string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://openapi.naver.com/v1/nid/me", nil)
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
		return "", "", fmt.Errorf("naver userinfo failed: status=%d body=%s", resp.StatusCode, string(body))
	}

	var out struct {
		ResultCode string `json:"resultcode"`
		Message    string `json:"message"`
		Response   struct {
			ID    string `json:"id"`
			Email string `json:"email"`
		} `json:"response"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", "", fmt.Errorf("decode userinfo response: %w", err)
	}
	if out.ResultCode != "00" {
		return "", "", fmt.Errorf("naver userinfo error: %s", out.Message)
	}
	if out.Response.ID == "" {
		return "", "", fmt.Errorf("naver userinfo response missing id")
	}
	return out.Response.Email, out.Response.ID, nil
}
