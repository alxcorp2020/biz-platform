// Package notify sends transactional email via the Resend REST API
// (https://resend.com/docs/api-reference/emails/send-email) — plain net/http,
// no SDK dependency, mirroring this project's preference for minimal external
// dependencies. Resend is chosen for its free tier (3,000 emails/month) and
// single-endpoint API, per CTO 평가 TOP10 4번(이메일 알림 최소 버전).
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

const resendEndpoint = "https://api.resend.com/emails"

// Client sends email through Resend. apiKey/from are read once at
// construction (from RESEND_API_KEY / RESEND_FROM_EMAIL) — see cmd/apiserver.
type Client struct {
	apiKey string
	from   string
	http   *http.Client
}

func NewClient(apiKey, from string) *Client {
	return &Client{apiKey: apiKey, from: from, http: &http.Client{Timeout: 10 * time.Second}}
}

// Configured reports whether an API key is set. Callers use this to decide
// whether to attempt Send at all — with no key, Send always fails, so batch
// jobs can skip the attempt (and the resulting notification_log 'failed'
// noise) entirely until RESEND_API_KEY is provisioned.
func (c *Client) Configured() bool {
	return c.apiKey != ""
}

// Send delivers one HTML email. Returns an error without making a network
// call if no API key is configured — this project's RESEND_API_KEY is
// provisioned by the user separately (실제 발송 테스트는 API 키 받은 후
// 직접 지시), so until then every call fails fast and predictably.
func (c *Client) Send(ctx context.Context, to, subject, html string) error {
	if c.apiKey == "" {
		return fmt.Errorf("RESEND_API_KEY not configured")
	}

	payload, err := json.Marshal(map[string]any{
		"from":    c.from,
		"to":      []string{to},
		"subject": subject,
		"html":    html,
	})
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, resendEndpoint, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("resend request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("resend api error: status=%d body=%s", resp.StatusCode, string(body))
	}
	return nil
}
