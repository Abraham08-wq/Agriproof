package payments

import (
	"context"

	"github.com/agriproof/backend/internal/mpesa"
)

// Daraja uses Safaricom directly. Deposits are confirmed with Safaricom's STK query. Payouts need
// separate B2C approval from Safaricom, so for now they are paid by the operator and marked paid.
type Daraja struct{ C *mpesa.Client }

func (d Daraja) Name() string { return "mpesa" }
func (d Daraja) Live() bool   { return d.C.Production() } // Daraja sandbox moves no real money

func (d Daraja) Collect(ctx context.Context, phone string, amount int64, ref, narrative string) (string, error) {
	return d.C.STKPush(ctx, phone, amount, ref, narrative)
}

func (d Daraja) CollectStatus(ctx context.Context, id string) (Status, error) {
	done, ok, desc, err := d.C.STKQuery(ctx, id)
	if err != nil {
		return Status{}, err
	}
	switch {
	case !done:
		return Status{State: Pending, Reason: desc}, nil
	case ok:
		return Status{State: Paid, Reason: desc}, nil
	default:
		return Status{State: Failed, Reason: desc}, nil
	}
}

func (d Daraja) Payout(context.Context, string, string, int64, string, string) (string, error) {
	return "", ErrUnsupported
}
func (d Daraja) PayoutStatus(context.Context, string) (Status, error) {
	return Status{}, ErrUnsupported
}
func (d Daraja) Balance(context.Context) (int64, bool, error) { return 0, false, nil }
