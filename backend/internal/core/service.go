// Package core holds AgriProof's rules: farmers, deliveries, fertilizer checks, payments and the
// sealed ledger. Every function enforces its own checks, whatever screen calls it.
package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/agriproof/backend/internal/chain"
	"github.com/agriproof/backend/internal/ledger"
	"github.com/agriproof/backend/internal/payments"
	"github.com/agriproof/backend/internal/risk"
	"github.com/agriproof/backend/internal/sms"
)

var (
	ErrNotFound    = errors.New("not found")
	ErrValidation  = errors.New("validation failed")
	ErrConflict    = errors.New("conflict")
	ErrForbidden   = errors.New("forbidden")
	ErrUnavailable = errors.New("unavailable")
)

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrValidation, fmt.Sprintf(format, a...))
}

type Service struct {
	db       *pgxpool.Pool
	ai       *risk.Client
	pay      payments.Provider
	anchorer *chain.Anchorer // nil when anchoring is disabled
	sms      *sms.Client
	secret   []byte // keys sign-in code and bag code hashes
	log      *slog.Logger
	anchorMu sync.Mutex
	wg       sync.WaitGroup

	payoutMax      int64 // largest single payout to one farmer
	reconTolerance int64
	feeReserve     int64 // held back per payout until the real fee is known
	kick           chan struct{}

	proofMu    sync.Mutex
	proofCache map[string]cachedVerify // public lot pages: one full check per cooperative every few minutes
}

type cachedVerify struct {
	head string
	at   time.Time
	rep  VerifyReport
}

// verifyForPublic serves the public proof pages without re-checking the whole record book on every
// visit. A new record changes the head and forces a fresh check; otherwise the result lasts 2 minutes.
func (s *Service) verifyForPublic(ctx context.Context, coopID string) (VerifyReport, error) {
	var head string
	_ = s.db.QueryRow(ctx, `SELECT hash FROM ledger_entries WHERE coop_id=$1 ORDER BY seq DESC LIMIT 1`, coopID).Scan(&head)
	s.proofMu.Lock()
	c, ok := s.proofCache[coopID]
	s.proofMu.Unlock()
	if ok && c.head == head && time.Since(c.at) < 2*time.Minute {
		return c.rep, nil
	}
	rep, err := s.VerifyLedger(ctx, coopID)
	if err != nil {
		return rep, err
	}
	s.proofMu.Lock()
	if s.proofCache == nil || len(s.proofCache) > 5000 {
		s.proofCache = map[string]cachedVerify{}
	}
	s.proofCache[coopID] = cachedVerify{head: head, at: time.Now(), rep: rep}
	s.proofMu.Unlock()
	return rep, nil
}

func NewService(db *pgxpool.Pool, ai *risk.Client, p payments.Provider, a *chain.Anchorer, s *sms.Client, secret []byte, log *slog.Logger) *Service {
	if p == nil {
		p = payments.Simulator{}
	}
	return &Service{db: db, ai: ai, pay: p, anchorer: a, sms: s, secret: secret, log: log,
		payoutMax: 300000, kick: make(chan struct{}, 1)}
}

func (s *Service) SetPaymentLimits(payoutMax, tolerance, feeReserve int64) {
	s.payoutMax, s.reconTolerance, s.feeReserve = payoutMax, tolerance, feeReserve
}

// payoutFeeEstimate is the fee held back for each payout: the real fee is only known after payment.
func (s *Service) payoutFeeEstimate() int64 {
	if s.pay.Name() == "test" || s.feeReserve < 0 {
		return 0
	}
	return s.feeReserve
}

// Wait blocks until background jobs (anchoring, SMS) finish.
func (s *Service) Wait() { s.wg.Wait() }

func (s *Service) SMSTestMode() bool { return s.sms == nil || s.sms.TestMode() }

// ---------- ledger ----------

type LedgerEntry struct {
	Seq       int64     `json:"seq"`
	Type      string    `json:"type"`
	FarmerID  *string   `json:"farmer_id,omitempty"`
	Amount    int64     `json:"amount"`
	Reference string    `json:"reference"`
	PrevHash  string    `json:"prev_hash"`
	Hash      string    `json:"hash"`
	CreatedAt time.Time `json:"created_at"`
}

func lockCoop(ctx context.Context, tx pgx.Tx, coopID string) error {
	var id string
	err := tx.QueryRow(ctx, `SELECT id FROM cooperatives WHERE id=$1 FOR UPDATE`, coopID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return fmt.Errorf("%w: cooperative", ErrNotFound)
	}
	return err
}

// appendEntry adds a hash-chained entry. The caller must hold lockCoop in the same transaction.
func appendEntry(ctx context.Context, tx pgx.Tx, coopID, entryType string, farmerID *string, amount int64, ref string) (LedgerEntry, error) {
	var lastSeq int64
	var lastHash string
	err := tx.QueryRow(ctx, `SELECT seq, hash FROM ledger_entries WHERE coop_id=$1 ORDER BY seq DESC LIMIT 1`, coopID).Scan(&lastSeq, &lastHash)
	if errors.Is(err, pgx.ErrNoRows) {
		lastSeq, lastHash = 0, ledger.GenesisHash
	} else if err != nil {
		return LedgerEntry{}, err
	}
	e := LedgerEntry{Seq: lastSeq + 1, Type: entryType, FarmerID: farmerID, Amount: amount, Reference: ref,
		PrevHash: lastHash, CreatedAt: ledger.Normalize(time.Now())}
	e.Hash = ledger.Hash(toLedgerEntry(coopID, e))
	_, err = tx.Exec(ctx, `INSERT INTO ledger_entries(coop_id, seq, entry_type, farmer_id, amount, reference, prev_hash, hash, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`, coopID, e.Seq, e.Type, e.FarmerID, e.Amount, e.Reference, e.PrevHash, e.Hash, e.CreatedAt)
	return e, err
}

func toLedgerEntry(coopID string, e LedgerEntry) ledger.Entry {
	fid := ""
	if e.FarmerID != nil {
		fid = *e.FarmerID
	}
	return ledger.Entry{GroupID: coopID, Seq: e.Seq, Type: e.Type, MemberID: fid, Amount: e.Amount,
		Reference: e.Reference, PrevHash: e.PrevHash, CreatedAt: e.CreatedAt}
}

func (s *Service) Ledger(ctx context.Context, coopID string, limit int) ([]LedgerEntry, error) {
	q := `SELECT seq, entry_type, farmer_id, amount, reference, prev_hash, hash, created_at FROM ledger_entries WHERE coop_id=$1 ORDER BY seq`
	args := []any{coopID}
	if limit > 0 {
		q = `SELECT * FROM (SELECT seq, entry_type, farmer_id, amount, reference, prev_hash, hash, created_at FROM ledger_entries
			WHERE coop_id=$1 ORDER BY seq DESC LIMIT $2) t ORDER BY seq`
		args = append(args, limit)
	}
	rows, err := s.db.Query(ctx, q, args...)
	if isInvalidUUID(err) {
		return nil, fmt.Errorf("%w: cooperative", ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []LedgerEntry{}
	for rows.Next() {
		var e LedgerEntry
		if err := rows.Scan(&e.Seq, &e.Type, &e.FarmerID, &e.Amount, &e.Reference, &e.PrevHash, &e.Hash, &e.CreatedAt); err != nil {
			return nil, err
		}
		e.CreatedAt = ledger.Normalize(e.CreatedAt)
		out = append(out, e)
	}
	return out, rows.Err()
}

type VerifyReport struct {
	ledger.VerifyResult
	AnchoredEntries int64  `json:"anchored_entries"`
	AnchoredHead    string `json:"anchored_head,omitempty"`
	AnchorTx        string `json:"anchor_tx,omitempty"`
	AnchorMatches   *bool  `json:"anchor_matches,omitempty"`
	Source          string `json:"anchor_source"` // blockchain | database
	Warning         string `json:"warning,omitempty"`
}

// VerifyLedger recomputes every hash and compares the chain with the last on-chain anchor.
func (s *Service) VerifyLedger(ctx context.Context, coopID string) (VerifyReport, error) {
	entries, err := s.Ledger(ctx, coopID, 0)
	if err != nil {
		return VerifyReport{}, err
	}
	le := make([]ledger.Entry, len(entries))
	hashes := make([]string, len(entries))
	for i, e := range entries {
		le[i] = toLedgerEntry(coopID, e)
		hashes[i] = e.Hash
	}
	rep := VerifyReport{VerifyResult: ledger.Verify(le, hashes), Source: "database"}
	_ = s.db.QueryRow(ctx, `SELECT entries, head, tx_hash FROM anchors WHERE coop_id=$1 ORDER BY entries DESC LIMIT 1`, coopID).
		Scan(&rep.AnchoredEntries, &rep.AnchoredHead, &rep.AnchorTx)
	if s.anchorer == nil {
		rep.Warning = "blockchain anchoring is turned off; only the hash chain was checked"
	} else {
		head, n, err := s.anchorer.Latest(ctx, coopID)
		switch {
		case err != nil:
			rep.Warning = "could not read the blockchain: " + err.Error()
		case n > 0:
			rep.AnchoredEntries, rep.AnchoredHead, rep.Source = int64(n), head, "blockchain"
		case len(entries) > 0:
			rep.Warning = "the blockchain has no record for this cooperative yet"
		}
	}
	if rep.AnchoredEntries > 0 {
		match := rep.AnchoredEntries <= int64(len(entries)) && entries[rep.AnchoredEntries-1].Hash == rep.AnchoredHead
		rep.AnchorMatches = &match
		if !match {
			rep.Valid = false
			rep.BrokenWhy = "records no longer match the blockchain seal: entries were altered or deleted"
		}
	}
	return rep, nil
}

func (s *Service) anchorAsync(coopID string) {
	if s.anchorer == nil {
		return
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		if err := s.anchorLatest(ctx, coopID); err != nil {
			s.log.Error("anchoring failed (will retry on next entry)", "coop", coopID, "err", err)
		}
	}()
}

func (s *Service) anchorLatest(ctx context.Context, coopID string) error {
	s.anchorMu.Lock()
	defer s.anchorMu.Unlock()
	var seq int64
	var head string
	if err := s.db.QueryRow(ctx, `SELECT seq, hash FROM ledger_entries WHERE coop_id=$1 ORDER BY seq DESC LIMIT 1`, coopID).Scan(&seq, &head); err != nil {
		return err
	}
	var anchored int64
	if err := s.db.QueryRow(ctx, `SELECT COALESCE(MAX(entries),0) FROM anchors WHERE coop_id=$1`, coopID).Scan(&anchored); err != nil {
		return err
	}
	if seq <= anchored {
		return nil
	}
	txHash, err := s.anchorer.Anchor(ctx, coopID, head, uint64(seq))
	if err != nil {
		return err
	}
	_, err = s.db.Exec(ctx, `INSERT INTO anchors(coop_id, entries, head, tx_hash) VALUES ($1,$2,$3,$4)`, coopID, seq, head, txHash)
	return err
}

// ---------- system status and totals ----------

type ComponentStatus struct {
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
	Ms     int64  `json:"latency_ms"`
}

type SystemStatus struct {
	Database     ComponentStatus `json:"database"`
	Blockchain   ComponentStatus `json:"blockchain"`
	AI           ComponentStatus `json:"ai"`
	Payments     ComponentStatus `json:"payments"`
	SMS          ComponentStatus `json:"sms"`
	Block        uint64          `json:"block_number"`
	ChainID      int64           `json:"chain_id"`
	Contract     string          `json:"contract,omitempty"`
	PaymentsMode string          `json:"payments_mode"`
	CheckedAt    time.Time       `json:"checked_at"`
}

func (s *Service) SystemStatus(ctx context.Context) SystemStatus {
	st := SystemStatus{CheckedAt: time.Now().UTC()}
	t := time.Now()
	if err := s.db.Ping(ctx); err != nil {
		st.Database = ComponentStatus{OK: false, Detail: "database unreachable"}
	} else {
		var n int64
		_ = s.db.QueryRow(ctx, `SELECT COUNT(*) FROM cooperatives`).Scan(&n)
		st.Database = ComponentStatus{OK: true, Detail: fmt.Sprintf("records online, %d cooperatives", n)}
	}
	st.Database.Ms = time.Since(t).Milliseconds()

	t = time.Now()
	if s.anchorer == nil {
		st.Blockchain = ComponentStatus{OK: false, Detail: "anchoring not configured"}
	} else {
		st.ChainID, st.Contract = s.anchorer.ChainID(), s.anchorer.Contract()
		n, err := s.anchorer.BlockNumber(ctx)
		switch {
		case err != nil:
			st.Blockchain = ComponentStatus{OK: false, Detail: "chain node unreachable"}
		case s.anchorer.CheckContract(ctx) != nil:
			st.Block = n
			st.Blockchain = ComponentStatus{OK: false, Detail: "proof contract missing on chain"}
		default:
			st.Block = n
			st.Blockchain = ComponentStatus{OK: true, Detail: fmt.Sprintf("chain %d, block %d", st.ChainID, n)}
		}
	}
	st.Blockchain.Ms = time.Since(t).Milliseconds()

	t = time.Now()
	if s.ai.Healthy(ctx) {
		st.AI = ComponentStatus{OK: true, Detail: "field checks running"}
	} else {
		st.AI = ComponentStatus{OK: false, Detail: "AI engine offline: risky items are held for people"}
	}
	st.AI.Ms = time.Since(t).Milliseconds()

	st.PaymentsMode = s.PaymentsMode()
	switch st.PaymentsMode {
	case "test":
		st.Payments = ComponentStatus{OK: true, Detail: "test mode: no real money moves"}
	case "sandbox":
		st.Payments = ComponentStatus{OK: true, Detail: s.pay.Name() + " sandbox: test money only"}
	default:
		st.Payments = ComponentStatus{OK: true, Detail: s.pay.Name() + " live: real money"}
	}
	if s.SMSTestMode() {
		st.SMS = ComponentStatus{OK: true, Detail: "test mode: messages written to the log"}
	} else {
		st.SMS = ComponentStatus{OK: true, Detail: "sending real SMS"}
	}
	return st
}

type Stats struct {
	Cooperatives      int64   `json:"cooperatives"`
	Farmers           int64   `json:"farmers"`
	KgSeason          float64 `json:"kg_season"`
	OwedToFarmers     int64   `json:"owed_to_farmers"`
	PaidToFarmers     int64   `json:"paid_to_farmers"`
	FlaggedDeliveries int64   `json:"flagged_deliveries"`
	HeldFarmers       int64   `json:"held_farmers"`
	CodesChecked      int64   `json:"codes_checked"`
	FakesCaught       int64   `json:"fakes_caught"`
	BatchesRecalled   int64   `json:"batches_recalled"`
	Anchors           int64   `json:"anchors"`
}

func (s *Service) Stats(ctx context.Context) (Stats, error) {
	var st Stats
	err := s.db.QueryRow(ctx, `SELECT
		(SELECT COUNT(*) FROM cooperatives),
		(SELECT COUNT(*) FROM farmers WHERE status='active'),
		(SELECT COALESCE(SUM(weight_kg),0)::float8 FROM deliveries WHERE status<>'voided' AND created_at > now() - interval '9 months'),
		(SELECT COALESCE(SUM(amount),0) FROM deliveries WHERE status='recorded' AND payrun_id IS NULL),
		(SELECT COALESCE(SUM(amount),0) FROM payouts WHERE status='paid'),
		(SELECT COUNT(*) FROM deliveries WHERE status='held'),
		(SELECT COUNT(*) FROM farmers WHERE status='held'),
		(SELECT COUNT(*) FROM code_checks),
		(SELECT COUNT(*) FROM code_checks WHERE result IN ('repeat','not_found','recalled')),
		(SELECT COUNT(*) FROM input_batches WHERE status='recalled'),
		(SELECT COUNT(*) FROM anchors)`).
		Scan(&st.Cooperatives, &st.Farmers, &st.KgSeason, &st.OwedToFarmers, &st.PaidToFarmers, &st.FlaggedDeliveries,
			&st.HeldFarmers, &st.CodesChecked, &st.FakesCaught, &st.BatchesRecalled, &st.Anchors)
	return st, err
}

// ---------- SMS ----------

func (s *Service) notify(phones []string, msg string) {
	if s.sms == nil || len(phones) == 0 {
		return
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		for _, p := range phones {
			if err := s.sms.Send(ctx, p, msg); err != nil {
				s.log.Warn("SMS not sent", "err", err)
			}
		}
	}()
}

// ---------- helpers ----------

type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func isInvalidUUID(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "22P02"
}

func notFoundIf(err error, what string) error {
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return fmt.Errorf("%w: %s", ErrNotFound, what)
	}
	return err
}

func cleanText(v string, max int) string {
	v = strings.Join(strings.Fields(v), " ")
	if len(v) > max {
		v = v[:max]
	}
	return v
}

func truncate(v string, n int) string {
	if len(v) <= n {
		return v
	}
	return v[:n]
}

func firstNonEmpty(a ...string) string {
	for _, v := range a {
		if v != "" {
			return v
		}
	}
	return ""
}

// digits formats 1234567 as 1,234,567.
func digits(n int64) string {
	neg := n < 0
	if neg {
		n = -n
	}
	str := fmt.Sprintf("%d", n)
	var b strings.Builder
	for i, c := range str {
		if i > 0 && (len(str)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

// SendSMS sends one message in the background (used for replies to incoming SMS).
func (s *Service) SendSMS(phone, msg string) { s.notify([]string{phone}, msg) }
