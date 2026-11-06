// Package sms sends text messages through Africa's Talking, which covers most African networks.
// Without an API key it runs in test mode and writes messages to the server log instead.
package sms

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

type Config struct {
	Env      string // sandbox | production
	Username string
	APIKey   string
	SenderID string // optional alphanumeric sender ID approved by Africa's Talking
}

type Client struct {
	cfg  Config
	url  string
	http *http.Client
	log  *slog.Logger

	mu       sync.Mutex
	day      string
	sentDay  int
	codesDay int
	dayLimit int
}

// SetDailyLimit caps messages per day so a flood of sign-ups cannot drain the SMS budget.
func (c *Client) SetDailyLimit(n int) { c.dayLimit = n }

// allow counts sign-in codes and other messages (receipts, recalls) separately, so a flood of
// sign-in requests can never use up the budget that farmers' receipts and recall warnings need.
// Sign-in codes may use half of the daily limit.
func (c *Client) allow(code bool) bool {
	if c.dayLimit <= 0 {
		return true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	today := time.Now().Format("2006-01-02")
	if today != c.day {
		c.day, c.sentDay, c.codesDay = today, 0, 0
	}
	if code {
		if c.codesDay >= (c.dayLimit+1)/2 {
			return false
		}
		c.codesDay++
		return true
	}
	if c.sentDay >= c.dayLimit {
		return false
	}
	c.sentDay++
	return true
}

func New(cfg Config, log *slog.Logger) *Client {
	u := "https://api.sandbox.africastalking.com/version1/messaging"
	if cfg.Env == "production" {
		u = "https://api.africastalking.com/version1/messaging"
	}
	return &Client{cfg: cfg, url: u, http: &http.Client{Timeout: 15 * time.Second}, log: log}
}

func (c *Client) TestMode() bool { return c.cfg.APIKey == "" || c.cfg.Username == "" }

// Send delivers one message to an international-format number (e.g. 2547XXXXXXXX).
func (c *Client) Send(ctx context.Context, phone, message string) error {
	return c.send(ctx, phone, message, false)
}

// SendCode delivers a sign-in code, from its own daily budget.
func (c *Client) SendCode(ctx context.Context, phone, message string) error {
	return c.send(ctx, phone, message, true)
}

func (c *Client) send(ctx context.Context, phone, message string, code bool) error {
	to := "+" + strings.TrimPrefix(phone, "+")
	if !c.allow(code) {
		c.log.Error("daily SMS limit reached; message not sent", "limit", c.dayLimit)
		return fmt.Errorf("SMS service is busy; try again later")
	}
	if c.TestMode() {
		c.log.Warn("TEST SMS (not sent: add Africa's Talking keys to .env)", "to", to, "message", message)
		return nil
	}
	form := url.Values{}
	form.Set("username", c.cfg.Username)
	form.Set("to", to)
	form.Set("message", message)
	if c.cfg.SenderID != "" {
		form.Set("from", c.cfg.SenderID)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("apiKey", c.cfg.APIKey)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var out struct {
		SMSMessageData struct {
			Message    string `json:"Message"`
			Recipients []struct {
				Status     string `json:"status"`
				StatusCode int    `json:"statusCode"`
			} `json:"Recipients"`
		} `json:"SMSMessageData"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return fmt.Errorf("sms: unexpected response (%d)", resp.StatusCode)
	}
	rs := out.SMSMessageData.Recipients
	if len(rs) == 0 || (rs[0].StatusCode != 100 && rs[0].StatusCode != 101 && rs[0].StatusCode != 102) {
		return fmt.Errorf("sms not accepted: %s", out.SMSMessageData.Message)
	}
	return nil
}
