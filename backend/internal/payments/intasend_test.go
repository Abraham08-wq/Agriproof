package payments

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func fake(t *testing.T, routes map[string]func(body map[string]any) (int, any)) *IntaSend {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sk" || r.Header.Get("INTASEND_PUBLIC_API_KEY") != "pk" {
			w.WriteHeader(401)
			return
		}
		h, ok := routes[r.Method+" "+r.URL.Path]
		if !ok {
			w.WriteHeader(404)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		code, out := h(body)
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(srv.Close)
	p := NewIntaSend("sk", "pk", "sandbox", "")
	p.base = srv.URL + "/api/v1/"
	return p
}

func TestIntaSendCollect(t *testing.T) {
	p := fake(t, map[string]func(map[string]any) (int, any){
		"POST /api/v1/payment/mpesa-stk-push/": func(b map[string]any) (int, any) {
			if b["phone_number"] != "254712345678" || b["amount"].(float64) != 500 || b["method"] != "M-PESA" {
				return 400, map[string]any{"error": "bad"}
			}
			return 200, map[string]any{"invoice": map[string]any{"invoice_id": "INV1", "state": "PENDING"}}
		},
		"POST /api/v1/payment/status/": func(b map[string]any) (int, any) {
			return 200, map[string]any{"invoice": map[string]any{"invoice_id": b["invoice_id"], "state": "COMPLETE",
				"value": "500.00", "net_amount": "485.00", "charges": "15.00", "mpesa_reference": "QAB12CD"}}
		},
	})
	id, err := p.Collect(context.Background(), "254712345678", 500, "CHAMA", "x")
	if err != nil || id != "INV1" {
		t.Fatalf("collect: %v %q", err, id)
	}
	st, err := p.CollectStatus(context.Background(), id)
	if err != nil || st.State != Paid || st.Amount != 500 || st.Net != 485 || st.Fee != 15 || st.Receipt != "QAB12CD" {
		t.Fatalf("status: %+v %v", st, err)
	}
}

func TestIntaSendStates(t *testing.T) {
	for state, want := range map[string]string{"PENDING": Pending, "PROCESSING": Pending, "FAILED": Failed, "COMPLETE": Paid} {
		s := state
		p := fake(t, map[string]func(map[string]any) (int, any){"POST /api/v1/payment/status/": func(map[string]any) (int, any) {
			return 200, map[string]any{"invoice": map[string]any{"state": s}}
		}})
		st, err := p.CollectStatus(context.Background(), "X")
		if err != nil || st.State != want {
			t.Fatalf("%s: got %q %v", s, st.State, err)
		}
	}
}

func TestIntaSendRejectedIsDefinite(t *testing.T) {
	p := fake(t, map[string]func(map[string]any) (int, any){"POST /api/v1/send-money/initiate/": func(map[string]any) (int, any) {
		return 400, map[string]any{"detail": "insufficient balance"}
	}})
	if _, err := p.Payout(context.Background(), "254712345678", "A B", 100, "WD-1", "n"); !errors.Is(err, ErrRejected) {
		t.Fatalf("want ErrRejected, got %v", err)
	}
	p.base = "http://127.0.0.1:1/api/v1/" // nothing listening: outcome unknown, must not look like a refusal
	if _, err := p.Payout(context.Background(), "254712345678", "A B", 100, "WD-1", "n"); err == nil || errors.Is(err, ErrRejected) {
		t.Fatalf("network failure must be 'unknown', got %v", err)
	}
}

func TestIntaSendPayout(t *testing.T) {
	p := fake(t, map[string]func(map[string]any) (int, any){
		"POST /api/v1/send-money/initiate/": func(b map[string]any) (int, any) {
			tx := b["transactions"].([]any)[0].(map[string]any)
			if b["provider"] != "MPESA-B2C" || b["requires_approval"] != "NO" || tx["account"] != "254712345678" {
				return 400, nil
			}
			return 200, map[string]any{"tracking_id": "TRK1", "status": "BP101"}
		},
		"POST /api/v1/send-money/status/": func(map[string]any) (int, any) {
			return 200, map[string]any{"status": "BC100", "transactions": []any{map[string]any{"status_code": "TS100", "charge": "10"}}}
		},
	})
	id, err := p.Payout(context.Background(), "254712345678", "A B", 100, "WD-1", "n")
	if err != nil || id != "TRK1" {
		t.Fatalf("payout: %v %q", err, id)
	}
	st, err := p.PayoutStatus(context.Background(), id)
	if err != nil || st.State != Paid || st.Fee != 10 {
		t.Fatalf("payout status: %+v %v", st, err)
	}
}

func TestPayoutStatusCodes(t *testing.T) {
	cases := []struct {
		in   map[string]any
		want string
	}{
		{map[string]any{"status": "BP109", "transactions": []any{map[string]any{"status_code": "TP102"}}}, Pending},
		{map[string]any{"status": "BC100", "transactions": []any{map[string]any{"status_code": "TF106", "failed_reason": "invalid"}}}, Failed},
		{map[string]any{"status": "BF105"}, Failed},
		{map[string]any{"status": "BC100", "transactions": []any{map[string]any{"status_code": "TF105"}}}, Pending},
	}
	for i, c := range cases {
		if got := parsePayoutStatus(c.in).State; got != c.want {
			t.Fatalf("case %d: got %s want %s", i, got, c.want)
		}
	}
}

func TestIntaSendBalance(t *testing.T) {
	p := fake(t, map[string]func(map[string]any) (int, any){"GET /api/v1/wallets/": func(map[string]any) (int, any) {
		return 200, map[string]any{"results": []any{
			map[string]any{"wallet_id": "W1", "currency": "KES", "available_balance": "1200.00"},
			map[string]any{"wallet_id": "W2", "currency": "KES", "available_balance": 300.0},
			map[string]any{"wallet_id": "W3", "currency": "USD", "available_balance": "50"},
		}}
	}})
	if b, ok, err := p.Balance(context.Background()); err != nil || !ok || b != 1500 {
		t.Fatalf("balance %d %v %v", b, ok, err)
	}
	p.WalletID = "W2"
	if b, _, _ := p.Balance(context.Background()); b != 300 {
		t.Fatalf("wallet balance %d", b)
	}
}
