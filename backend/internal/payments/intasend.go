package payments

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// IntaSend is a CBK-licensed payment company: M-Pesa and card collections, and payouts to M-Pesa.
// Endpoints follow IntaSend's official SDKs (github.com/IntaSend/intasend-python).
type IntaSend struct {
	SecretKey      string
	PublishableKey string
	Env            string // sandbox | live
	WalletID       string // optional: collect into and pay out of this wallet
	http           *http.Client
	base           string
}

func NewIntaSend(secret, publishable, env, wallet string) *IntaSend {
	base := "https://sandbox.intasend.com/api/v1/"
	if env == "live" {
		base = "https://payment.intasend.com/api/v1/"
	}
	if b := os.Getenv("INTASEND_BASE_URL"); b != "" && env != "live" { // test harness only; never for live money
		base = strings.TrimSuffix(b, "/") + "/"
	}
	return &IntaSend{SecretKey: secret, PublishableKey: publishable, Env: env, WalletID: wallet,
		http: &http.Client{Timeout: 30 * time.Second}, base: base}
}

func (p *IntaSend) Name() string { return "intasend" }

// Live is true only for the live environment; the IntaSend sandbox moves no real money.
func (p *IntaSend) Live() bool { return p.Env == "live" }

func (p *IntaSend) call(ctx context.Context, method, path string, payload any, out any) error {
	var body io.Reader
	if payload != nil {
		b, _ := json.Marshal(payload)
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, p.base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+p.SecretKey)
	req.Header.Set("INTASEND_PUBLIC_API_KEY", p.PublishableKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := p.http.Do(req)
	if err != nil {
		return err // no answer: the outcome is unknown
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == 400 || resp.StatusCode == 401 || resp.StatusCode == 403 || resp.StatusCode == 404 {
		return fmt.Errorf("%w: intasend %d: %s", ErrRejected, resp.StatusCode, truncate(string(raw), 300))
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("intasend %d: %s", resp.StatusCode, truncate(string(raw), 300))
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("intasend: unexpected reply: %w", err)
		}
	}
	return nil
}

func (p *IntaSend) Collect(ctx context.Context, phone string, amount int64, ref, narrative string) (string, error) {
	payload := map[string]any{
		"public_key": p.PublishableKey, "currency": "KES", "method": "M-PESA",
		"amount": amount, "phone_number": phone, "api_ref": ref, "narrative": narrative,
	}
	if p.WalletID != "" {
		payload["wallet_id"] = p.WalletID
	}
	var out struct {
		Invoice struct {
			InvoiceID string `json:"invoice_id"`
		} `json:"invoice"`
	}
	if err := p.call(ctx, http.MethodPost, "payment/mpesa-stk-push/", payload, &out); err != nil {
		return "", err
	}
	if out.Invoice.InvoiceID == "" {
		return "", fmt.Errorf("%w: intasend returned no invoice id", ErrRejected)
	}
	return out.Invoice.InvoiceID, nil
}

func (p *IntaSend) CollectStatus(ctx context.Context, id string) (Status, error) {
	var out struct {
		Invoice map[string]any `json:"invoice"`
	}
	if err := p.call(ctx, http.MethodPost, "payment/status/", map[string]any{"invoice_id": id, "public_key": p.PublishableKey}, &out); err != nil {
		return Status{}, err
	}
	inv := out.Invoice
	st := Status{Amount: num(inv["value"]), Net: num(inv["net_amount"]), Fee: num(inv["charges"]),
		Receipt: str(inv["mpesa_reference"]), Reason: str(inv["failed_reason"])}
	switch strings.ToUpper(str(inv["state"])) {
	case "COMPLETE", "COMPLETED":
		st.State = Paid
	case "FAILED", "CANCELLED", "CANCELED":
		st.State = Failed
	default:
		st.State = Pending
	}
	return st, nil
}

func (p *IntaSend) Payout(ctx context.Context, phone, name string, amount int64, ref, narrative string) (string, error) {
	payload := map[string]any{
		"provider": "MPESA-B2C", "currency": "KES", "requires_approval": "NO",
		"transactions": []map[string]any{{"account": phone, "amount": amount, "name": truncate(name, 60), "narrative": truncate(narrative, 22)}},
	}
	if p.WalletID != "" {
		payload["wallet_id"] = p.WalletID
	}
	var out struct {
		TrackingID string `json:"tracking_id"`
	}
	if err := p.call(ctx, http.MethodPost, "send-money/initiate/", payload, &out); err != nil {
		return "", err
	}
	if out.TrackingID == "" {
		return "", fmt.Errorf("intasend returned no tracking id") // outcome unknown: do not retry blindly
	}
	return out.TrackingID, nil
}

// Send-money status codes, from IntaSend's status reference.
var (
	batchFailed = map[string]bool{"BF102": true, "BF105": true, "BF107": true, "BE111": true}
	txFailed    = map[string]bool{"TF103": true, "TF106": true, "TC108": true}
)

func (p *IntaSend) PayoutStatus(ctx context.Context, id string) (Status, error) {
	var out map[string]any
	if err := p.call(ctx, http.MethodPost, "send-money/status/", map[string]any{"tracking_id": id}, &out); err != nil {
		return Status{}, err
	}
	return parsePayoutStatus(out), nil
}

func parsePayoutStatus(out map[string]any) Status {
	st := Status{State: Pending}
	batch := strings.ToUpper(firstNonEmpty(str(out["status_code"]), str(out["status"])))
	if txs, ok := out["transactions"].([]any); ok && len(txs) > 0 {
		if tx, ok := txs[0].(map[string]any); ok {
			code := strings.ToUpper(firstNonEmpty(str(tx["status_code"]), str(tx["status"])))
			st.Reason = firstNonEmpty(str(tx["failed_reason"]), str(tx["status_description"]))
			st.Fee = num(tx["charge"])
			st.Receipt = str(tx["mpesa_reference"])
			switch {
			case code == "TS100" || code == "SUCCESSFUL" || code == "SUCCESS":
				st.State = Paid
				return st
			case txFailed[code] || code == "FAILED":
				st.State = Failed
				return st
			}
		}
	}
	if batchFailed[batch] {
		st.State = Failed
	}
	return st
}

func (p *IntaSend) Balance(ctx context.Context) (int64, bool, error) {
	var out struct {
		Results []map[string]any `json:"results"`
	}
	if err := p.call(ctx, http.MethodGet, "wallets/", nil, &out); err != nil {
		return 0, false, err
	}
	var total int64
	found := false
	for _, w := range out.Results {
		if p.WalletID != "" && str(w["wallet_id"]) != p.WalletID {
			continue
		}
		if c := str(w["currency"]); c != "" && c != "KES" {
			continue
		}
		total += num(w["available_balance"])
		found = true
	}
	return total, found, nil
}

// ---------- helpers ----------

func num(v any) int64 {
	switch x := v.(type) {
	case float64:
		return int64(math.Round(x))
	case string:
		f, err := strconv.ParseFloat(strings.ReplaceAll(x, ",", ""), 64)
		if err == nil {
			return int64(math.Round(f))
		}
	}
	return 0
}

func str(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	}
	return ""
}

func firstNonEmpty(a ...string) string {
	for _, s := range a {
		if s != "" {
			return s
		}
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
