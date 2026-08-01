package billing

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

const tossConfirmEndpoint = "https://api.tosspayments.com/v1/payments/confirm"
const tossCancelEndpointFmt = "https://api.tosspayments.com/v1/payments/%s/cancel"

// TossClient calls Toss's server-side payment-confirm API. Per Toss's
// security guidance, payment approval must always happen server-side with
// the secret key — the client only ever sees the (public) client key used
// to open the checkout widget.
type TossClient struct {
	secretKey string
	http      *http.Client
}

func NewTossClient(secretKey string) *TossClient {
	return &TossClient{secretKey: secretKey, http: &http.Client{Timeout: 15 * time.Second}}
}

func (c *TossClient) Configured() bool {
	return c.secretKey != ""
}

// ConfirmResult mirrors the subset of Toss's Payment response object this
// project persists (payment_log.raw_response keeps the full JSON separately).
type ConfirmResult struct {
	Status      string    `json:"status"`
	PaymentKey  string    `json:"paymentKey"`
	OrderID     string    `json:"orderId"`
	Method      string    `json:"method"`
	TotalAmount int64     `json:"totalAmount"`
	ApprovedAt  time.Time `json:"approvedAt"`
}

// TossError is returned when Toss rejects a request — confirm (e.g. amount
// mismatch, expired payment, already-processed key) or cancel (e.g. payment
// not found, already cancelled). Shared between both since Toss's error body
// shape is identical either way. Callers persist raw_response for confirm
// errors too — see handleBillingConfirm.
type TossError struct {
	StatusCode int
	Code       string `json:"code"`
	Message    string `json:"message"`
}

func (e *TossError) Error() string {
	return fmt.Sprintf("toss api error: status=%d code=%s message=%s", e.StatusCode, e.Code, e.Message)
}

// Confirm calls POST /v1/payments/confirm. rawBody is always returned (even
// on error) so the caller can store it verbatim in payment_log.raw_response —
// never fabricate what Toss actually said.
func (c *TossClient) Confirm(ctx context.Context, paymentKey, orderID string, amount int64) (result *ConfirmResult, rawBody []byte, err error) {
	if c.secretKey == "" {
		return nil, nil, fmt.Errorf("TOSS_SECRET_KEY not configured")
	}

	payload, err := json.Marshal(map[string]any{
		"paymentKey": paymentKey,
		"orderId":    orderID,
		"amount":     amount,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tossConfirmEndpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(c.secretKey+":")))
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("toss request failed: %w", err)
	}
	defer resp.Body.Close()

	rawBody, err = io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode >= 300 {
		var tErr TossError
		tErr.StatusCode = resp.StatusCode
		_ = json.Unmarshal(rawBody, &tErr) // best-effort — raw body preserved regardless
		return nil, rawBody, &tErr
	}

	var res ConfirmResult
	if err := json.Unmarshal(rawBody, &res); err != nil {
		return nil, rawBody, fmt.Errorf("unmarshal response: %w", err)
	}
	return &res, rawBody, nil
}

// CancelResult mirrors the subset of Toss's cancel response this project
// cares about — Toss returns the full updated Payment object (status becomes
// "CANCELED"), but only the cancellation timestamp is currently persisted
// separately (payment_log.refunded_at); the raw body isn't stored for cancel
// the way it is for confirm(no raw_response column slot reserved for it —
// 환불 정책 자체가 부분환불 없이 전액/불가 둘뿐이라 재현성 감사 목적의
// 원본 보존 필요성이 confirm만큼 크지 않다고 판단).
type CancelResult struct {
	Status     string    `json:"status"`
	PaymentKey string    `json:"paymentKey"`
	CanceledAt time.Time `json:"-"`
}

// tossCancelRawResult exists only to parse the one nested field(cancels[0].
// canceledAt) CancelResult needs — Toss's cancel response embeds the
// cancellation record inside a "cancels" array (a payment can have multiple
// partial cancels; this project only ever does one full cancel per payment).
type tossCancelRawResult struct {
	Status  string `json:"status"`
	Cancels []struct {
		CanceledAt time.Time `json:"canceledAt"`
	} `json:"cancels"`
}

// Cancel calls POST /v1/payments/{paymentKey}/cancel — a full cancellation
// (this project's refund policy is all-or-nothing, no partial cancelAmount).
// rawBody is always returned so the caller can log/inspect it even though it
// isn't persisted to payment_log today.
func (c *TossClient) Cancel(ctx context.Context, paymentKey, cancelReason string) (result *CancelResult, rawBody []byte, err error) {
	if c.secretKey == "" {
		return nil, nil, fmt.Errorf("TOSS_SECRET_KEY not configured")
	}

	payload, err := json.Marshal(map[string]any{
		"cancelReason": cancelReason,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("marshal request: %w", err)
	}

	endpoint := fmt.Sprintf(tossCancelEndpointFmt, paymentKey)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(c.secretKey+":")))
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("toss request failed: %w", err)
	}
	defer resp.Body.Close()

	rawBody, err = io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode >= 300 {
		var tErr TossError
		tErr.StatusCode = resp.StatusCode
		_ = json.Unmarshal(rawBody, &tErr)
		return nil, rawBody, &tErr
	}

	var raw tossCancelRawResult
	if err := json.Unmarshal(rawBody, &raw); err != nil {
		return nil, rawBody, fmt.Errorf("unmarshal response: %w", err)
	}
	res := CancelResult{Status: raw.Status, PaymentKey: paymentKey}
	if len(raw.Cancels) > 0 {
		res.CanceledAt = raw.Cancels[len(raw.Cancels)-1].CanceledAt
	}
	return &res, rawBody, nil
}
