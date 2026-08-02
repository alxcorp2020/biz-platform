package oauth

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

// GoogleClient implements Client for "Google로 로그인" using the standard
// OAuth 2.0 / OpenID Connect endpoints (https://developers.google.com/identity/protocols/oauth2/web-server).
type GoogleClient struct {
	clientID     string
	clientSecret string
	redirectURI  string
	http         *http.Client
}

func NewGoogleClient(clientID, clientSecret, redirectURI string) *GoogleClient {
	return &GoogleClient{
		clientID: clientID, clientSecret: clientSecret, redirectURI: redirectURI,
		http: &http.Client{Timeout: 10 * time.Second},
	}
}

func (c *GoogleClient) Configured() bool {
	return c.clientID != "" && c.clientSecret != ""
}

func (c *GoogleClient) AuthURL(state string) string {
	v := url.Values{
		"client_id":     {c.clientID},
		"redirect_uri":  {c.redirectURI},
		"response_type": {"code"},
		"scope":         {"openid email profile"},
		"state":         {state},
	}
	return "https://accounts.google.com/o/oauth2/v2/auth?" + v.Encode()
}

func (c *GoogleClient) Exchange(ctx context.Context, code string) (string, error) {
	form := url.Values{
		"client_id":     {c.clientID},
		"client_secret": {c.clientSecret},
		"redirect_uri":  {c.redirectURI},
		"code":          {code},
		"grant_type":    {"authorization_code"},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://oauth2.googleapis.com/token", strings.NewReader(form.Encode()))
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
		return "", fmt.Errorf("google token exchange failed: status=%d body=%s", resp.StatusCode, string(body))
	}

	var out struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("decode token response: %w", err)
	}
	if out.AccessToken == "" {
		return "", fmt.Errorf("google token response missing access_token")
	}
	return out.AccessToken, nil
}

func (c *GoogleClient) FetchUser(ctx context.Context, accessToken string) (email, providerUserID string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://www.googleapis.com/oauth2/v3/userinfo", nil)
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
		return "", "", fmt.Errorf("google userinfo failed: status=%d body=%s", resp.StatusCode, string(body))
	}

	var out struct {
		Sub   string `json:"sub"`
		Email string `json:"email"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", "", fmt.Errorf("decode userinfo response: %w", err)
	}
	if out.Sub == "" {
		return "", "", fmt.Errorf("google userinfo response missing sub")
	}
	return out.Email, out.Sub, nil
}
