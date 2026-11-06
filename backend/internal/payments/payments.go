// Package payments moves real money in and out through a licensed provider (IntaSend or
// Safaricom Daraja), or through a simulator in test mode. Every provider is used the same way:
// a notification from the provider is only a hint; the money is confirmed by asking the provider.
package payments

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
)

const (
	Pending = "pending"
	Paid    = "paid"
	Failed  = "failed"
)

var (
	// ErrUnsupported: the provider cannot do this automatically (for example Daraja payouts
	// without B2C approval). The operator pays by hand and marks it paid.
	ErrUnsupported = errors.New("not supported by this payment provider")
	// ErrRejected: the provider definitely refused the request and no money moved, so it is
	// safe to try again later.
	ErrRejected = errors.New("rejected by the payment provider")
)

// Status is the provider's answer about one payment.
type Status struct {
	State   string // pending | paid | failed
	Amount  int64  // gross amount, when the provider reports it
	Net     int64  // amount received after the provider's fee, when reported
	Fee     int64  // provider fee, when reported
	Receipt string // e.g. the M-Pesa receipt number
	Reason  string
}

type Provider interface {
	Name() string // test | mpesa | intasend
	Live() bool   // true when real money moves
	Collect(ctx context.Context, phone string, amount int64, ref, narrative string) (id string, err error)
	CollectStatus(ctx context.Context, id string) (Status, error)
	Payout(ctx context.Context, phone, name string, amount int64, ref, narrative string) (id string, err error)
	PayoutStatus(ctx context.Context, id string) (Status, error)
	// Balance returns the money in the provider account; ok is false if it cannot be read.
	Balance(ctx context.Context) (amount int64, ok bool, err error)
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// ---------- simulator ----------

// Simulator is test payment mode: no real money moves. Deposits wait for the operator to confirm
// them in the console; payouts succeed straight away.
type Simulator struct{}

func (Simulator) Name() string { return "test" }
func (Simulator) Live() bool   { return false }
func (Simulator) Collect(context.Context, string, int64, string, string) (string, error) {
	return "TEST-" + randomHex(8), nil
}
func (Simulator) CollectStatus(context.Context, string) (Status, error) {
	return Status{State: Pending}, nil // confirmed by the operator, never automatically
}
func (Simulator) Payout(context.Context, string, string, int64, string, string) (string, error) {
	return "SIMPAY-" + randomHex(6), nil
}
func (Simulator) PayoutStatus(context.Context, string) (Status, error) {
	return Status{State: Paid, Receipt: "SIMULATED"}, nil
}
func (Simulator) Balance(context.Context) (int64, bool, error) { return 0, false, nil }
