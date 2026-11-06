// Package mpesa talks to Safaricom's Daraja API. Without credentials it runs in test payment mode,
// so the whole system can be built and verified at no cost.
package mpesa

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

type Config struct {
	Env            string // sandbox | production
	ConsumerKey    string
	ConsumerSecret string
	Shortcode      string
	Passkey        string
	CallbackURL    string
}

type Client struct {
	cfg     Config
	baseURL string
	http    *http.Client

	mu       sync.Mutex
	token    string
	tokenExp time.Time
}

func New(cfg Config) *Client {
	base := "https://sandbox.safaricom.co.ke"
	if cfg.Env == "production" {
		base = "https://api.safaricom.co.ke"
	}
	return &Client{cfg: cfg, baseURL: base, http: &http.Client{Timeout: 20 * time.Second}}
}

// Production is true only for Safaricom's live environment (real money).
func (c *Client) Production() bool { return c.cfg.Env == "production" }

// TestMode reports whether live Daraja calls are disabled (no Safaricom keys configured).
func (c *Client) TestMode() bool {
	return c.cfg.ConsumerKey == "" || c.cfg.ConsumerSecret == "" || c.cfg.Passkey == ""
}

// STKPush asks the member's phone to approve a payment. Returns the CheckoutRequestID.
func (c *Client) STKPush(ctx context.Context, phone string, amount int64, accountRef, desc string) (string, error) {
	msisdn, err := NormalizePhone(phone)
	if err != nil {
		return "", err
	}
	if c.TestMode() {
		return "TEST-" + randomHex(8), nil
	}
	token, err := c.accessToken(ctx)
	if err != nil {
		return "", err
	}
	ts := time.Now().In(time.FixedZone("EAT", 3*3600)).Format("20060102150405")
	password := base64.StdEncoding.EncodeToString([]byte(c.cfg.Shortcode + c.cfg.Passkey + ts))
	payload := map[string]any{
		"BusinessShortCode": c.cfg.Shortcode,
		"Password":          password,
		"Timestamp":         ts,
		"TransactionType":   "CustomerPayBillOnline",
		"Amount":            amount,
		"PartyA":            msisdn,
		"PartyB":            c.cfg.Shortcode,
		"PhoneNumber":       msisdn,
		"CallBackURL":       c.cfg.CallbackURL,
		"AccountReference":  truncate(accountRef, 12),
		"TransactionDesc":   truncate(desc, 13),
	}
	body, _ := json.Marshal(payload)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/mpesa/stkpush/v1/processrequest", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out struct {
		CheckoutRequestID string `json:"CheckoutRequestID"`
		ResponseCode      string `json:"ResponseCode"`
		ErrorMessage      string `json:"errorMessage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if out.ResponseCode != "0" || out.CheckoutRequestID == "" {
		return "", fmt.Errorf("stk push rejected: %s", out.ErrorMessage)
	}
	return out.CheckoutRequestID, nil
}

// STKQuery asks Safaricom for the final result of an STK push. done is false while the customer
// has not finished. Used to confirm every payment with Safaricom directly, so a forged callback
// can never credit a deposit.
func (c *Client) STKQuery(ctx context.Context, checkoutID string) (done, ok bool, desc string, err error) {
	if c.TestMode() {
		return false, false, "", errors.New("daraja: test mode")
	}
	token, err := c.accessToken(ctx)
	if err != nil {
		return false, false, "", err
	}
	ts := time.Now().In(time.FixedZone("EAT", 3*3600)).Format("20060102150405")
	body, _ := json.Marshal(map[string]any{
		"BusinessShortCode": c.cfg.Shortcode,
		"Password":          base64.StdEncoding.EncodeToString([]byte(c.cfg.Shortcode + c.cfg.Passkey + ts)),
		"Timestamp":         ts,
		"CheckoutRequestID": checkoutID,
	})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/mpesa/stkpushquery/v1/query", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return false, false, "", err
	}
	defer resp.Body.Close()
	var out struct {
		ResultCode   *json.RawMessage `json:"ResultCode"`
		ResultDesc   string           `json:"ResultDesc"`
		ErrorMessage string           `json:"errorMessage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return false, false, "", err
	}
	if out.ResultCode == nil {
		return false, false, out.ErrorMessage, nil // still being processed
	}
	code := strings.Trim(string(*out.ResultCode), `"`)
	return true, code == "0", out.ResultDesc, nil
}

func (c *Client) accessToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && time.Now().Before(c.tokenExp) {
		return c.token, nil
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/oauth/v1/generate?grant_type=client_credentials", nil)
	req.SetBasicAuth(c.cfg.ConsumerKey, c.cfg.ConsumerSecret)
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   string `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if out.AccessToken == "" {
		return "", errors.New("daraja: empty access token (check consumer key and secret)")
	}
	c.token = out.AccessToken
	c.tokenExp = time.Now().Add(50 * time.Minute)
	return c.token, nil
}

// CallbackResult is the useful part of Safaricom's STK callback.
type CallbackResult struct {
	CheckoutRequestID string
	Success           bool
	ResultDesc        string
	ReceiptNumber     string
	Amount            int64
}

// ParseCallback reads Safaricom's STK push callback body.
func ParseCallback(body []byte) (CallbackResult, error) {
	var cb struct {
		Body struct {
			StkCallback struct {
				CheckoutRequestID string `json:"CheckoutRequestID"`
				ResultCode        int    `json:"ResultCode"`
				ResultDesc        string `json:"ResultDesc"`
				CallbackMetadata  struct {
					Item []struct {
						Name  string `json:"Name"`
						Value any    `json:"Value"`
					} `json:"Item"`
				} `json:"CallbackMetadata"`
			} `json:"stkCallback"`
		} `json:"Body"`
	}
	if err := json.Unmarshal(body, &cb); err != nil {
		return CallbackResult{}, err
	}
	s := cb.Body.StkCallback
	if s.CheckoutRequestID == "" {
		return CallbackResult{}, errors.New("missing CheckoutRequestID")
	}
	res := CallbackResult{CheckoutRequestID: s.CheckoutRequestID, Success: s.ResultCode == 0, ResultDesc: s.ResultDesc}
	for _, it := range s.CallbackMetadata.Item {
		switch it.Name {
		case "MpesaReceiptNumber":
			res.ReceiptNumber = fmt.Sprint(it.Value)
		case "Amount":
			if f, ok := it.Value.(float64); ok {
				res.Amount = int64(f)
			}
		}
	}
	return res, nil
}

// NormalizePhone converts 07XXXXXXXX / +2547XXXXXXXX / 2547XXXXXXXX into 2547XXXXXXXX (Kenya).
// Other countries' numbers in full international format are passed through.
func NormalizePhone(p string) (string, error) {
	p = strings.TrimSpace(strings.ReplaceAll(p, " ", ""))
	p = strings.TrimPrefix(p, "+")
	if strings.HasPrefix(p, "0") && len(p) == 10 {
		p = "254" + p[1:]
	}
	if len(p) < 11 || len(p) > 15 {
		return "", fmt.Errorf("invalid phone number %q", p)
	}
	for _, ch := range p {
		if ch < '0' || ch > '9' {
			return "", fmt.Errorf("invalid phone number %q", p)
		}
	}
	return p, nil
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return strings.ToUpper(hex.EncodeToString(b))
}
