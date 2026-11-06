package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/agriproof/backend/internal/mpesa"
	"github.com/agriproof/backend/internal/payments"
)

// Money: a cooperative puts money into the payment account (funding), a manager prepares a pay run
// from unpaid deliveries, a different person approves it, and a background worker pays each farmer
// exactly once. A daily check compares the account with the records.

func (s *Service) PaymentsMode() string {
	switch {
	case s.pay.Live():
		return "live"
	case s.pay.Name() == "test":
		return "test"
	}
	return "sandbox"
}

func (s *Service) PaymentProvider() string { return s.pay.Name() }

// ---------- funding ----------

type Funding struct {
	ID        string    `json:"id"`
	CoopID    string    `json:"coop_id"`
	CoopName  string    `json:"coop_name"`
	Phone     string    `json:"phone"`
	Amount    int64     `json:"amount"`
	Net       *int64    `json:"net_amount,omitempty"`
	Provider  string    `json:"provider"`
	Status    string    `json:"status"`
	Note      string    `json:"note"`
	StartedBy string    `json:"started_by"`
	Test      bool      `json:"test"`
	CreatedAt time.Time `json:"created_at"`
}

// StartFunding sends an M-Pesa prompt to the payer's phone to move cooperative money into the account.
// Nothing counts until the payment company confirms it (or, in test mode, the operator does).
func (s *Service) StartFunding(ctx context.Context, coopID, phone string, amount int64, by string) (Funding, error) {
	if amount <= 0 || amount > 10_000_000 {
		return Funding{}, invalid("enter an amount between 1 and 10,000,000")
	}
	p, err := mpesa.NormalizePhone(phone)
	if err != nil {
		return Funding{}, invalid("enter the phone number that will pay")
	}
	if _, err := s.GetCoop(ctx, coopID); err != nil {
		return Funding{}, err
	}
	id, err := s.pay.Collect(ctx, p, amount, "AGRIPROOF", "Cooperative funding")
	if err != nil {
		s.log.Error("funding could not be started", "err", err)
		return Funding{}, fmt.Errorf("%w: the M-Pesa prompt could not be sent right now; try again in a few minutes", ErrUnavailable)
	}
	if _, err := s.db.Exec(ctx, `INSERT INTO fundings(id, coop_id, phone, amount, provider, started_by) VALUES ($1,$2,$3,$4,$5,$6)`,
		id, coopID, p, amount, s.pay.Name(), by); err != nil {
		return Funding{}, err
	}
	s.wake()
	return s.getFunding(ctx, id)
}

const fundingCols = `x.id, x.coop_id, c.name, x.phone, x.amount, x.net_amount, x.provider, x.status, x.result_desc, x.started_by, x.created_at`

func scanFunding(row pgx.Row) (Funding, error) {
	var f Funding
	err := row.Scan(&f.ID, &f.CoopID, &f.CoopName, &f.Phone, &f.Amount, &f.Net, &f.Provider, &f.Status, &f.Note, &f.StartedBy, &f.CreatedAt)
	f.Test = f.Provider == "test"
	return f, err
}

func (s *Service) getFunding(ctx context.Context, id string) (Funding, error) {
	f, err := scanFunding(s.db.QueryRow(ctx, `SELECT `+fundingCols+` FROM fundings x JOIN cooperatives c ON c.id=x.coop_id WHERE x.id=$1`, id))
	return f, notFoundIf(err, "funding")
}

func (s *Service) Fundings(ctx context.Context, limit int) ([]Funding, error) {
	rows, err := s.db.Query(ctx, `SELECT `+fundingCols+` FROM fundings x JOIN cooperatives c ON c.id=x.coop_id
		ORDER BY (x.status='pending') DESC, x.created_at DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Funding{}
	for rows.Next() {
		f, err := scanFunding(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// DecideTestFunding lets the operator confirm a test-mode funding. Real money is only ever confirmed by
// the payment company.
func (s *Service) DecideTestFunding(ctx context.Context, id string, confirm bool, by string) error {
	f, err := s.getFunding(ctx, id)
	if err != nil {
		return err
	}
	if f.Provider != "test" {
		return fmt.Errorf("%w: real payments are confirmed only by the payment company", ErrForbidden)
	}
	if s.pay.Live() && confirm {
		return fmt.Errorf("%w: live payments are on; test funding can only be rejected", ErrForbidden)
	}
	if !confirm {
		return s.settleFunding(ctx, id, payments.Status{State: payments.Failed, Reason: "test funding rejected by " + by})
	}
	return s.settleFunding(ctx, id, payments.Status{State: payments.Paid, Reason: "test funding confirmed by " + by, Receipt: id})
}

func (s *Service) settleFunding(ctx context.Context, id string, st payments.Status) error {
	switch st.State {
	case payments.Paid:
		_, err := s.db.Exec(ctx, `UPDATE fundings SET status='completed', amount=CASE WHEN $2>0 THEN $2 ELSE amount END,
			net_amount=CASE WHEN $3>0 THEN $3 ELSE (CASE WHEN $2>0 THEN $2 ELSE amount END) - $4 END, fee=$4, receipt=$5, result_desc=$6, checked_at=now()
			WHERE id=$1 AND status='pending'`, id, st.Amount, st.Net, st.Fee, truncate(st.Receipt, 64), truncate(st.Reason, 300))
		return err
	case payments.Failed:
		_, err := s.db.Exec(ctx, `UPDATE fundings SET status='failed', result_desc=$2, checked_at=now() WHERE id=$1 AND status='pending'`,
			id, truncate(firstNonEmpty(st.Reason, "payment not completed"), 300))
		return err
	}
	_, err := s.db.Exec(ctx, `UPDATE fundings SET checked_at=now() WHERE id=$1`, id)
	return err
}

// VerifyFunding asks the payment company about a pending funding. Notifications only trigger this.
func (s *Service) VerifyFunding(ctx context.Context, id string) error {
	f, err := s.getFunding(ctx, id)
	if err != nil || f.Status != "pending" || f.Provider != s.pay.Name() || f.Provider == "test" {
		return err
	}
	st, err := s.pay.CollectStatus(ctx, id)
	if err != nil {
		_, _ = s.db.Exec(ctx, `UPDATE fundings SET checked_at=now() WHERE id=$1`, id)
		return err
	}
	return s.settleFunding(ctx, id, st)
}

func (s *Service) pollFundings(ctx context.Context) {
	if s.pay.Name() == "test" {
		return
	}
	rows, err := s.db.Query(ctx, `SELECT id FROM fundings WHERE status='pending' AND provider=$1 AND created_at > now() - interval '25 hours'
		AND (checked_at IS NULL OR checked_at < now() - interval '20 seconds') ORDER BY created_at LIMIT 50`, s.pay.Name())
	if err != nil {
		return
	}
	var ids []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	for _, id := range ids {
		if err := s.VerifyFunding(ctx, id); err != nil {
			s.log.Warn("funding check failed", "funding", id, "err", err)
		}
	}
}

// CoopFunds is the money a cooperative has available for paying farmers.
func (s *Service) CoopFunds(ctx context.Context, coopID string) (int64, error) {
	var v int64
	err := s.db.QueryRow(ctx, `SELECT
		COALESCE((SELECT SUM(COALESCE(net_amount, amount)) FROM fundings WHERE coop_id=$1 AND status='completed'),0)
		- COALESCE((SELECT SUM(amount + fee) FROM payouts WHERE coop_id=$1 AND status<>'returned'),0)`, coopID).Scan(&v)
	return v, err
}

// ---------- pay runs ----------

type Payrun struct {
	ID         string     `json:"id"`
	CoopID     string     `json:"coop_id"`
	CoopName   string     `json:"coop_name"`
	CreatedBy  string     `json:"created_by"`
	ApprovedBy string     `json:"approved_by"`
	Status     string     `json:"status"`
	Total      int64      `json:"total"`
	Farmers    int        `json:"farmers"`
	Deliveries int        `json:"deliveries"`
	Paid       int        `json:"paid"`
	Problems   int        `json:"problems"`
	CreatedAt  time.Time  `json:"created_at"`
	DecidedAt  *time.Time `json:"decided_at,omitempty"`
}

const payrunCols = `r.id, r.coop_id, c.name, t.name, r.approved_by, r.status, r.total, r.farmers, r.deliveries,
	(SELECT COUNT(*) FROM payouts p WHERE p.payrun_id=r.id AND p.status='paid'),
	(SELECT COUNT(*) FROM payouts p WHERE p.payrun_id=r.id AND p.status IN ('unknown','manual','failed')),
	r.created_at, r.decided_at`
const payrunFrom = ` FROM payruns r JOIN cooperatives c ON c.id=r.coop_id JOIN staff t ON t.id=r.created_by`

func scanPayrun(row pgx.Row) (Payrun, error) {
	var r Payrun
	err := row.Scan(&r.ID, &r.CoopID, &r.CoopName, &r.CreatedBy, &r.ApprovedBy, &r.Status, &r.Total, &r.Farmers, &r.Deliveries,
		&r.Paid, &r.Problems, &r.CreatedAt, &r.DecidedAt)
	return r, err
}

func (s *Service) GetPayrun(ctx context.Context, id string) (Payrun, error) {
	r, err := scanPayrun(s.db.QueryRow(ctx, `SELECT `+payrunCols+payrunFrom+` WHERE r.id=$1`, id))
	return r, notFoundIf(err, "pay run")
}

func (s *Service) Payruns(ctx context.Context, coopID string, limit int) ([]Payrun, error) {
	rows, err := s.db.Query(ctx, `SELECT `+payrunCols+payrunFrom+` WHERE ($1='' OR r.coop_id::text=$1)
		ORDER BY (r.status='draft') DESC, r.created_at DESC LIMIT $2`, coopID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Payrun{}
	for rows.Next() {
		r, err := scanPayrun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// CreatePayrun gathers every cleared, unpaid delivery into a draft. Held deliveries wait.
func (s *Service) CreatePayrun(ctx context.Context, manager Staff) (Payrun, error) {
	if manager.Role != "manager" {
		return Payrun{}, fmt.Errorf("%w: only a manager can prepare payments", ErrForbidden)
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return Payrun{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err := lockCoop(ctx, tx, manager.CoopID); err != nil {
		return Payrun{}, err
	}
	var open int
	if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM payruns WHERE coop_id=$1 AND status='draft'`, manager.CoopID).Scan(&open); err != nil {
		return Payrun{}, err
	}
	if open > 0 {
		return Payrun{}, fmt.Errorf("%w: a pay run is already waiting for approval; approve or cancel it first", ErrConflict)
	}
	var id string
	if err := tx.QueryRow(ctx, `INSERT INTO payruns(coop_id, created_by) VALUES ($1,$2) RETURNING id`, manager.CoopID, manager.ID).Scan(&id); err != nil {
		return Payrun{}, err
	}
	tag, err := tx.Exec(ctx, `UPDATE deliveries d SET payrun_id=$1 FROM farmers f WHERE f.id=d.farmer_id AND f.status='active'
		AND d.coop_id=$2 AND d.status='recorded' AND d.payrun_id IS NULL AND d.amount > 0`, id, manager.CoopID)
	if err != nil {
		return Payrun{}, err
	}
	if tag.RowsAffected() == 0 {
		return Payrun{}, fmt.Errorf("%w: there are no cleared, unpaid deliveries to pay", ErrConflict)
	}
	if _, err := tx.Exec(ctx, `UPDATE payruns r SET total=x.total, farmers=x.farmers, deliveries=x.n FROM
		(SELECT COALESCE(SUM(amount),0) total, COUNT(DISTINCT farmer_id) farmers, COUNT(*) n FROM deliveries WHERE payrun_id=$1) x WHERE r.id=$1`, id); err != nil {
		return Payrun{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Payrun{}, err
	}
	return s.GetPayrun(ctx, id)
}

type PayrunLine struct {
	FarmerID   string  `json:"farmer_id"`
	Name       string  `json:"name"`
	MemberNo   int     `json:"member_no"`
	Phone      string  `json:"phone"`
	Deliveries int     `json:"deliveries"`
	Kg         float64 `json:"kg"`
	Amount     int64   `json:"amount"`
	Status     string  `json:"payout_status"`
}

func (s *Service) PayrunLines(ctx context.Context, payrunID string) ([]PayrunLine, error) {
	rows, err := s.db.Query(ctx, `SELECT f.id, f.name, f.member_no, f.phone, COUNT(*), SUM(d.weight_kg)::float8, SUM(d.amount),
		COALESCE((SELECT p.status FROM payouts p WHERE p.payrun_id=$1 AND p.farmer_id=f.id),'')
		FROM deliveries d JOIN farmers f ON f.id=d.farmer_id WHERE d.payrun_id=$1 GROUP BY f.id ORDER BY f.member_no`, payrunID)
	if err != nil {
		return nil, notFoundIf(err, "pay run")
	}
	defer rows.Close()
	out := []PayrunLine{}
	for rows.Next() {
		var l PayrunLine
		if err := rows.Scan(&l.FarmerID, &l.Name, &l.MemberNo, &l.Phone, &l.Deliveries, &l.Kg, &l.Amount, &l.Status); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// ApprovePayrun releases the money. The approver must be a different person from the manager who
// prepared it (another manager, or an operator in the console), and the cooperative must have the funds.
func (s *Service) ApprovePayrun(ctx context.Context, payrunID string, approverStaffID, approvedBy string) (Payrun, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return Payrun{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var coopID, createdBy, status string
	var total int64
	if err := tx.QueryRow(ctx, `SELECT coop_id, created_by, status, total FROM payruns WHERE id=$1`, payrunID).Scan(&coopID, &createdBy, &status, &total); err != nil {
		return Payrun{}, notFoundIf(err, "pay run")
	}
	if err := lockCoop(ctx, tx, coopID); err != nil {
		return Payrun{}, err
	}
	if err := tx.QueryRow(ctx, `SELECT status FROM payruns WHERE id=$1 FOR UPDATE`, payrunID).Scan(&status); err != nil {
		return Payrun{}, err
	}
	if status != "draft" {
		return Payrun{}, fmt.Errorf("%w: this pay run is already %s", ErrConflict, status)
	}
	if approverStaffID != "" && approverStaffID == createdBy {
		return Payrun{}, fmt.Errorf("%w: a different person must approve the payments you prepared", ErrForbidden)
	}
	funds, err := s.CoopFunds(ctx, coopID)
	if err != nil {
		return Payrun{}, err
	}
	var farmers int64
	if err := tx.QueryRow(ctx, `SELECT COUNT(DISTINCT farmer_id) FROM deliveries WHERE payrun_id=$1`, payrunID).Scan(&farmers); err != nil {
		return Payrun{}, err
	}
	fees := farmers * s.payoutFeeEstimate()
	if funds < total+fees {
		return Payrun{}, fmt.Errorf("%w: the cooperative has KES %s available but this pay run needs KES %s (KES %s for farmers plus up to KES %s in payment fees); add funds first",
			ErrConflict, digits(funds), digits(total+fees), digits(total), digits(fees))
	}
	rows, err := tx.Query(ctx, `SELECT f.id, f.phone, f.name, SUM(d.amount) FROM deliveries d JOIN farmers f ON f.id=d.farmer_id
		WHERE d.payrun_id=$1 GROUP BY f.id`, payrunID)
	if err != nil {
		return Payrun{}, err
	}
	type line struct {
		id, phone, name string
		amount          int64
	}
	var lines []line
	for rows.Next() {
		var l line
		if err := rows.Scan(&l.id, &l.phone, &l.name, &l.amount); err != nil {
			rows.Close()
			return Payrun{}, err
		}
		lines = append(lines, l)
	}
	rows.Close()
	for _, l := range lines {
		if s.payoutMax > 0 && l.amount > s.payoutMax {
			return Payrun{}, fmt.Errorf("%w: %s is due KES %s, above the KES %s limit for one payment; split their deliveries or raise PAYOUT_MAX",
				ErrConflict, l.name, digits(l.amount), digits(s.payoutMax))
		}
		if _, err := tx.Exec(ctx, `INSERT INTO payouts(payrun_id, coop_id, farmer_id, phone, name, amount, provider, fee) VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
			payrunID, coopID, l.id, l.phone, l.name, l.amount, s.pay.Name(), s.payoutFeeEstimate()); err != nil {
			return Payrun{}, err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE payruns SET status='approved', approved_by=$2, decided_at=now() WHERE id=$1`, payrunID, approvedBy); err != nil {
		return Payrun{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Payrun{}, err
	}
	s.wake()
	return s.GetPayrun(ctx, payrunID)
}

// CancelPayrun drops a draft; its deliveries go back to the unpaid list.
func (s *Service) CancelPayrun(ctx context.Context, coopID, payrunID, by string) (Payrun, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return Payrun{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	tag, err := tx.Exec(ctx, `UPDATE payruns SET status='cancelled', approved_by=$3, decided_at=now() WHERE id=$1 AND coop_id=$2 AND status='draft'`, payrunID, coopID, by)
	if err != nil {
		return Payrun{}, notFoundIf(err, "pay run")
	}
	if tag.RowsAffected() == 0 {
		return Payrun{}, fmt.Errorf("%w: only a pay run waiting for approval can be cancelled", ErrConflict)
	}
	if _, err := tx.Exec(ctx, `UPDATE deliveries SET payrun_id=NULL WHERE payrun_id=$1`, payrunID); err != nil {
		return Payrun{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Payrun{}, err
	}
	return s.GetPayrun(ctx, payrunID)
}

// ---------- payout worker ----------

const maxPayoutAttempts = 5

var payoutBackoff = []time.Duration{time.Minute, 5 * time.Minute, 30 * time.Minute, 2 * time.Hour, 6 * time.Hour}

func (s *Service) wake() {
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

func (s *Service) WakePayments() { s.wake() }

// RunPaymentWorker sends payouts, confirms fundings and runs the daily money check. Only one worker
// runs per database (a database lock makes sure of it).
func (s *Service) RunPaymentWorker(ctx context.Context) {
	for {
		conn, err := s.db.Acquire(ctx)
		if err == nil {
			var got bool
			if conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(8128101)`).Scan(&got) == nil && got {
				defer conn.Release()
				break
			}
			conn.Release()
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Minute):
		}
	}
	if tag, err := s.db.Exec(ctx, `UPDATE payouts SET status='unknown', updated_at=now(),
		last_error='interrupted while sending; check the payment company before retrying' WHERE status='sending'`); err == nil && tag.RowsAffected() > 0 {
		s.log.Error("payouts interrupted while sending need checking", "count", tag.RowsAffected())
	}
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		s.processPayouts(ctx)
		s.pollFundings(ctx)
		s.dailyReconcile(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-s.kick:
		}
	}
}

type payoutJob struct {
	ID, PayrunID, FarmerID, Phone, Name, Provider, Ref string
	Amount                                             int64
	Attempts                                           int
	CreatedAt                                          time.Time
}

func (s *Service) processPayouts(ctx context.Context) {
	_, _ = s.db.Exec(ctx, `UPDATE payouts SET status='unknown', updated_at=now(),
		last_error='sent through ' || provider || ', which is no longer in use: check that dashboard, then mark paid or return'
		WHERE status='sent' AND provider<>$1`, s.pay.Name())
	if !s.payoutsPaused(ctx) {
		for i := 0; i < 50; i++ {
			var j payoutJob
			err := s.db.QueryRow(ctx, `UPDATE payouts SET status='sending', attempts=attempts+1, updated_at=now()
				WHERE id = (SELECT id FROM payouts WHERE status='queued' AND next_attempt_at <= now() ORDER BY created_at LIMIT 1 FOR UPDATE SKIP LOCKED)
				RETURNING id, payrun_id, farmer_id, phone, name, provider, amount, attempts`).
				Scan(&j.ID, &j.PayrunID, &j.FarmerID, &j.Phone, &j.Name, &j.Provider, &j.Amount, &j.Attempts)
			if errors.Is(err, pgx.ErrNoRows) {
				break
			}
			if err != nil {
				s.log.Error("payout queue", "err", err)
				break
			}
			s.sendPayout(ctx, j)
		}
	}
	s.checkSentPayouts(ctx)
}

func (s *Service) sendPayout(ctx context.Context, j payoutJob) {
	set := func(status, ref, msg string, next time.Duration) {
		_, err := s.db.Exec(ctx, `UPDATE payouts SET status=$2, provider_ref=CASE WHEN $3='' THEN provider_ref ELSE $3 END,
			last_error=$4, next_attempt_at=now()+$5::interval, updated_at=now() WHERE id=$1 AND status='sending'`,
			j.ID, status, ref, truncate(msg, 300), fmt.Sprintf("%d seconds", int(next.Seconds())))
		if err != nil {
			s.log.Error("payout update failed", "payout", j.ID, "err", err)
		}
	}
	if j.Provider != s.pay.Name() {
		set("manual", "", fmt.Sprintf("queued for %s but %s is now in use: pay by hand, then mark paid", j.Provider, s.pay.Name()), 0)
		return
	}
	ref, err := s.pay.Payout(ctx, j.Phone, j.Name, j.Amount, "PAY-"+j.ID[:8], "Produce payment")
	switch {
	case err == nil:
		set("sent", ref, "", 0)
	case errors.Is(err, payments.ErrUnsupported):
		set("manual", "", "this payment company cannot pay out automatically: send the money by hand, then mark it paid", 0)
	case errors.Is(err, payments.ErrRejected):
		if j.Attempts >= maxPayoutAttempts {
			set("failed", "", err.Error(), 0)
		} else {
			set("queued", "", err.Error(), payoutBackoff[j.Attempts-1])
		}
	default:
		set("unknown", "", "no clear answer from the payment company ("+err.Error()+"): check its dashboard before retrying", 0)
	}
}

func (s *Service) checkSentPayouts(ctx context.Context) {
	rows, err := s.db.Query(ctx, `SELECT id, payrun_id, farmer_id, phone, amount, provider_ref, attempts, created_at FROM payouts
		WHERE status='sent' AND provider=$1 AND updated_at < now() - interval '5 seconds' ORDER BY updated_at LIMIT 100`, s.pay.Name())
	if err != nil {
		return
	}
	var jobs []payoutJob
	for rows.Next() {
		var j payoutJob
		if rows.Scan(&j.ID, &j.PayrunID, &j.FarmerID, &j.Phone, &j.Amount, &j.Ref, &j.Attempts, &j.CreatedAt) == nil {
			jobs = append(jobs, j)
		}
	}
	rows.Close()
	for _, j := range jobs {
		st, err := s.pay.PayoutStatus(ctx, j.Ref)
		if err != nil {
			_, _ = s.db.Exec(ctx, `UPDATE payouts SET updated_at=now(), last_error=$2 WHERE id=$1 AND status='sent'`, j.ID, truncate(err.Error(), 300))
			continue
		}
		switch st.State {
		case payments.Paid:
			s.markPaid(ctx, j.ID, st.Receipt, st.Fee, "", "sent")
		case payments.Failed:
			if j.Attempts >= maxPayoutAttempts {
				_, _ = s.db.Exec(ctx, `UPDATE payouts SET status='failed', last_error=$2, updated_at=now() WHERE id=$1 AND status='sent'`, j.ID, truncate(st.Reason, 300))
			} else {
				_, _ = s.db.Exec(ctx, `UPDATE payouts SET status='queued', last_error=$2, next_attempt_at=now()+interval '5 minutes', updated_at=now()
					WHERE id=$1 AND status='sent'`, j.ID, truncate("payment company reported failure: "+st.Reason, 300))
			}
		default:
			if time.Since(j.CreatedAt) > 24*time.Hour {
				_, _ = s.db.Exec(ctx, `UPDATE payouts SET status='unknown', last_error='no final answer after 24 hours', updated_at=now() WHERE id=$1 AND status='sent'`, j.ID)
			} else {
				_, _ = s.db.Exec(ctx, `UPDATE payouts SET updated_at=now() WHERE id=$1 AND status='sent'`, j.ID)
			}
		}
	}
}

// markPaid records a payout as paid, writes the payment into the sealed ledger and texts the farmer.
func (s *Service) markPaid(ctx context.Context, payoutID, receipt string, fee int64, resolvedBy, fromStatus string) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var coopID, farmerID, phone, coopName string
	var amount int64
	if err := tx.QueryRow(ctx, `SELECT p.coop_id, p.farmer_id, p.phone, p.amount, c.name FROM payouts p JOIN cooperatives c ON c.id=p.coop_id WHERE p.id=$1`,
		payoutID).Scan(&coopID, &farmerID, &phone, &amount, &coopName); err != nil {
		return notFoundIf(err, "payout")
	}
	if err := lockCoop(ctx, tx, coopID); err != nil {
		return err
	}
	q := `UPDATE payouts SET status='paid', receipt=$2, fee=CASE WHEN $3::bigint > 0 THEN $3::bigint ELSE fee END, resolved_by=$4, last_error='', updated_at=now() WHERE id=$1 AND status=$5`
	if fromStatus == "" {
		q = `UPDATE payouts SET status='paid', receipt=$2, fee=CASE WHEN $3::bigint > 0 THEN $3::bigint ELSE fee END, resolved_by=$4, last_error='', updated_at=now() WHERE id=$1
			AND status IN ('unknown','manual','failed') AND $5=''`
	}
	tag, err := tx.Exec(ctx, q, payoutID, truncate(receipt, 64), fee, resolvedBy, fromStatus)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: only payouts that need checking can be marked paid", ErrConflict)
	}
	if _, err := appendEntry(ctx, tx, coopID, "payment", &farmerID, -amount, "PAY-"+strings.ToUpper(payoutID[:8])+"|"+truncate(receipt, 40)); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	s.anchorAsync(coopID)
	msg := fmt.Sprintf("AgriProof: %s paid you KES %s for your produce. Receipt %s.", coopName, digits(amount), receipt)
	if !s.pay.Live() {
		msg = fmt.Sprintf("AgriProof (test mode, no real money): %s marked KES %s as paid to you.", coopName, digits(amount))
	}
	s.notify([]string{phone}, msg)
	return nil
}

type Payout struct {
	ID         string    `json:"id"`
	PayrunID   string    `json:"payrun_id"`
	CoopName   string    `json:"coop_name"`
	FarmerID   string    `json:"farmer_id"`
	Name       string    `json:"name"`
	Phone      string    `json:"phone"`
	Amount     int64     `json:"amount"`
	Provider   string    `json:"provider"`
	Status     string    `json:"status"`
	Receipt    string    `json:"receipt"`
	Fee        int64     `json:"fee"`
	Attempts   int       `json:"attempts"`
	LastError  string    `json:"last_error"`
	ResolvedBy string    `json:"resolved_by"`
	CreatedAt  time.Time `json:"created_at"`
}

func (s *Service) ListPayouts(ctx context.Context, farmerID string, limit int) ([]Payout, error) {
	rows, err := s.db.Query(ctx, `SELECT p.id, p.payrun_id, c.name, p.farmer_id, p.name, p.phone, p.amount, p.provider, p.status, p.receipt, p.fee,
		p.attempts, p.last_error, p.resolved_by, p.created_at FROM payouts p JOIN cooperatives c ON c.id=p.coop_id
		WHERE ($1='' OR p.farmer_id::text=$1) ORDER BY (p.status IN ('unknown','manual','failed')) DESC, p.created_at DESC LIMIT $2`, farmerID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Payout{}
	for rows.Next() {
		var p Payout
		if err := rows.Scan(&p.ID, &p.PayrunID, &p.CoopName, &p.FarmerID, &p.Name, &p.Phone, &p.Amount, &p.Provider, &p.Status, &p.Receipt, &p.Fee,
			&p.Attempts, &p.LastError, &p.ResolvedBy, &p.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Service) MarkPayoutPaid(ctx context.Context, id, receipt, by string) error {
	receipt = strings.TrimSpace(receipt)
	if len(receipt) < 4 {
		return invalid("enter the M-Pesa or payment company receipt number as proof")
	}
	return s.markPaid(ctx, id, receipt, 0, by, "")
}

// confirmUnpaid makes sure a payout with an unclear result really did not reach the farmer before it is
// sent again or returned, so nobody is paid twice. It asks the payment company when it can; otherwise
// a person must write down what they checked.
func (s *Service) confirmUnpaid(ctx context.Context, id, reason string) (string, error) {
	var status, ref, provider string
	if err := s.db.QueryRow(ctx, `SELECT status, provider_ref, provider FROM payouts WHERE id=$1`, id).Scan(&status, &ref, &provider); err != nil {
		return "", notFoundIf(err, "payout")
	}
	reason = cleanText(reason, 300)
	if status == "unknown" && ref != "" && provider == s.pay.Name() {
		st, err := s.pay.PayoutStatus(ctx, ref)
		if err != nil {
			return "", fmt.Errorf("%w: could not confirm with the payment company whether this was paid (%s); check your payment dashboard and try again",
				ErrUnavailable, truncate(err.Error(), 120))
		}
		switch st.State {
		case payments.Paid:
			if err := s.markPaid(ctx, id, st.Receipt, st.Fee, "payment company confirmed", ""); err != nil {
				return "", err
			}
			return "", fmt.Errorf("%w: the payment company says this farmer WAS paid (receipt %s), so it has been marked paid instead", ErrConflict, st.Receipt)
		case payments.Failed:
			return "payment company confirmed it failed", nil
		default:
			return "", fmt.Errorf("%w: the payment company still shows this payment as in progress; wait and check again", ErrConflict)
		}
	}
	if status == "unknown" || status == "manual" {
		if len(reason) < 10 {
			return "", invalid("write what you checked to be sure the farmer was not paid (for example: checked the payment dashboard, no payment to this number)")
		}
		return reason, nil
	}
	return "payment company refused it", nil
}

func (s *Service) RetryPayout(ctx context.Context, id, reason, by string) error {
	why, err := s.confirmUnpaid(ctx, id, reason)
	if err != nil {
		return err
	}
	tag, err := s.db.Exec(ctx, `UPDATE payouts SET status='queued', attempts=0, next_attempt_at=now(), resolved_by=$2, updated_at=now()
		WHERE id=$1 AND status IN ('unknown','failed')`, id, truncate("retry by "+by+": "+why, 200))
	if err != nil {
		return notFoundIf(err, "payout")
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: only failed or unclear payouts can be retried", ErrConflict)
	}
	s.wake()
	return nil
}

// ReturnPayout gives up on a payout that never reached the farmer; their deliveries go back to the
// unpaid list for the next pay run.
func (s *Service) ReturnPayout(ctx context.Context, id, reason, by string) error {
	why, err := s.confirmUnpaid(ctx, id, reason)
	if err != nil {
		return err
	}
	by = truncate(by+": "+why, 200)
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var payrunID, farmerID, phone string
	if err := tx.QueryRow(ctx, `SELECT payrun_id, farmer_id, phone FROM payouts WHERE id=$1`, id).Scan(&payrunID, &farmerID, &phone); err != nil {
		return notFoundIf(err, "payout")
	}
	tag, err := tx.Exec(ctx, `UPDATE payouts SET status='returned', resolved_by=$2, updated_at=now() WHERE id=$1 AND status IN ('unknown','manual','failed','queued')`, id, by)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: this payout cannot be returned now", ErrConflict)
	}
	if _, err := tx.Exec(ctx, `UPDATE deliveries SET payrun_id=NULL WHERE payrun_id=$1 AND farmer_id=$2`, payrunID, farmerID); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	s.notify([]string{phone}, "AgriProof: your produce payment could not be sent this time. It is still owed to you and will be in the next payment run.")
	return nil
}

func (s *Service) payoutsPaused(ctx context.Context) bool {
	if !s.pay.Live() {
		return false
	}
	var status string
	// An unreadable balance proves nothing either way, so only an ok run lifts a pause.
	err := s.db.QueryRow(ctx, `SELECT status FROM reconciliations WHERE status IN ('ok','mismatch') ORDER BY id DESC LIMIT 1`).Scan(&status)
	return err == nil && status == "mismatch"
}

// ---------- daily money check ----------

type Reconciliation struct {
	ID              int64     `json:"id"`
	Provider        string    `json:"provider"`
	Live            bool      `json:"live"`
	AccountBalance  *int64    `json:"account_balance"`
	AccountSource   string    `json:"account_source"`
	ExpectedAccount int64     `json:"expected_account"`
	Difference      *int64    `json:"difference"`
	Status          string    `json:"status"`
	Detail          string    `json:"detail"`
	CreatedAt       time.Time `json:"created_at"`
}

func (s *Service) Reconcile(ctx context.Context) (Reconciliation, error) {
	r := Reconciliation{Provider: s.pay.Name(), Live: s.pay.Live()}
	if err := s.db.QueryRow(ctx, `SELECT
		COALESCE((SELECT SUM(COALESCE(net_amount, amount)) FROM fundings WHERE status='completed' AND provider=$1),0)
		- COALESCE((SELECT SUM(amount + fee) FROM payouts WHERE status='paid' AND provider=$1),0)`, r.Provider).Scan(&r.ExpectedAccount); err != nil {
		return r, err
	}
	if r.Provider == "test" {
		v := r.ExpectedAccount
		r.AccountBalance, r.AccountSource = &v, "simulated"
	} else {
		bal, ok, err := s.pay.Balance(ctx)
		if err != nil || !ok {
			r.AccountSource, r.Status, r.Detail = "unavailable", "unavailable", "could not read the account balance"
			if err != nil {
				r.Detail += ": " + truncate(err.Error(), 200)
			}
		} else {
			r.AccountBalance, r.AccountSource = &bal, "provider"
		}
	}
	if r.AccountBalance != nil {
		d := *r.AccountBalance - r.ExpectedAccount
		r.Difference = &d
		r.Status = "ok"
		if d > s.reconTolerance || d < -s.reconTolerance {
			r.Status = "mismatch"
			r.Detail = fmt.Sprintf("the account holds %s but the records say %s", digits(*r.AccountBalance), digits(r.ExpectedAccount))
			if r.Live {
				r.Detail += ": payouts are paused until this is explained"
			}
		}
	}
	err := s.db.QueryRow(ctx, `INSERT INTO reconciliations(provider, live, account_balance, account_source, expected_account, difference, status, detail)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8) RETURNING id, created_at`, r.Provider, r.Live, r.AccountBalance, r.AccountSource, r.ExpectedAccount,
		r.Difference, r.Status, r.Detail).Scan(&r.ID, &r.CreatedAt)
	return r, err
}

func (s *Service) LatestReconciliation(ctx context.Context) (*Reconciliation, error) {
	var r Reconciliation
	err := s.db.QueryRow(ctx, `SELECT id, provider, live, account_balance, account_source, expected_account, difference, status, detail, created_at
		FROM reconciliations ORDER BY id DESC LIMIT 1`).Scan(&r.ID, &r.Provider, &r.Live, &r.AccountBalance, &r.AccountSource, &r.ExpectedAccount,
		&r.Difference, &r.Status, &r.Detail, &r.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &r, err
}

func (s *Service) dailyReconcile(ctx context.Context) {
	var due bool
	if err := s.db.QueryRow(ctx, `SELECT NOT EXISTS(SELECT 1 FROM reconciliations WHERE created_at > now() - interval '24 hours')`).Scan(&due); err != nil || !due {
		return
	}
	if _, err := s.Reconcile(ctx); err != nil {
		s.log.Error("reconciliation failed", "err", err)
	}
}

// CheckMoneyMode stops test money from ever becoming real money: live payments only start on a database
// with no test fundings or payouts, and a live database refuses to run in test or sandbox mode.
func (s *Service) CheckMoneyMode(ctx context.Context) error {
	var liveSince string
	err := s.db.QueryRow(ctx, `SELECT value FROM app_state WHERE key='live_since'`).Scan(&liveSince)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if !s.pay.Live() {
		if liveSince != "" {
			return fmt.Errorf("this database has handled real money since %s, so it cannot run in test or sandbox mode; put the live payment keys back in .env", liveSince)
		}
		return nil
	}
	if liveSince != "" {
		return nil
	}
	var n int64
	if err := s.db.QueryRow(ctx, `SELECT (SELECT COUNT(*) FROM fundings WHERE status='completed') + (SELECT COUNT(*) FROM payouts)`).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return fmt.Errorf("this database holds %d test-money records (fundings or payouts); real money must start on a clean database. "+
			"Back up, then start fresh (make reset, then make setup) or use a new server, and switch to live keys there", n)
	}
	_, err = s.db.Exec(ctx, `INSERT INTO app_state(key, value) VALUES ('live_since', $1)`, time.Now().UTC().Format(time.RFC3339)+" via "+s.pay.Name())
	return err
}

// expireStale closes old pending fundings and is called hourly.
func (s *Service) expireStale(ctx context.Context) {
	_, _ = s.db.Exec(ctx, `UPDATE fundings SET status='failed', result_desc='expired: not confirmed within 24 hours'
		WHERE status='pending' AND created_at < now() - interval '24 hours' AND (provider='test' OR provider<>$1 OR checked_at > created_at + interval '24 hours')`,
		s.pay.Name())
}
