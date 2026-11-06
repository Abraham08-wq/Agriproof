package core

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/agriproof/backend/internal/risk"
)

type Delivery struct {
	ID         string          `json:"id"`
	Ref        string          `json:"ref"`
	CoopID     string          `json:"coop_id"`
	CentreName string          `json:"centre_name"`
	FarmerID   string          `json:"farmer_id"`
	FarmerName string          `json:"farmer_name"`
	MemberNo   int             `json:"member_no"`
	ClerkName  string          `json:"clerk_name"`
	Crop       string          `json:"crop"`
	Grade      string          `json:"grade"`
	WeightKg   float64         `json:"weight_kg"`
	Moisture   *float64        `json:"moisture,omitempty"`
	PricePerKg float64         `json:"price_per_kg"`
	Amount     int64           `json:"amount"`
	Status     string          `json:"status"`
	AILevel    string          `json:"ai_level"`
	AIReasons  json.RawMessage `json:"ai_reasons"`
	ReviewedBy string          `json:"reviewed_by"`
	LotCode    string          `json:"lot_code"`
	Paid       bool            `json:"paid"`
	InPayrun   bool            `json:"in_payrun"`
	LedgerSeq  *int64          `json:"ledger_seq,omitempty"`
	CreatedAt  time.Time       `json:"created_at"`
}

const deliveryCols = `d.id, d.coop_id, COALESCE(ce.name,''), d.farmer_id, f.name, f.member_no, t.name, d.crop, d.grade, d.weight_kg::float8,
	d.moisture::float8, d.price_per_kg::float8, d.amount, d.status, d.ai_level, d.ai_reasons, d.reviewed_by, COALESCE(l.code,''),
	EXISTS(SELECT 1 FROM payouts p WHERE p.payrun_id=d.payrun_id AND p.farmer_id=d.farmer_id AND p.status='paid'),
	d.payrun_id IS NOT NULL, d.ledger_seq, d.created_at`
const deliveryFrom = ` FROM deliveries d JOIN farmers f ON f.id=d.farmer_id JOIN staff t ON t.id=d.clerk_id
	LEFT JOIN centres ce ON ce.id=d.centre_id LEFT JOIN lots l ON l.id=d.lot_id`

func scanDelivery(row pgx.Row) (Delivery, error) {
	var d Delivery
	err := row.Scan(&d.ID, &d.CoopID, &d.CentreName, &d.FarmerID, &d.FarmerName, &d.MemberNo, &d.ClerkName, &d.Crop, &d.Grade,
		&d.WeightKg, &d.Moisture, &d.PricePerKg, &d.Amount, &d.Status, &d.AILevel, &d.AIReasons, &d.ReviewedBy, &d.LotCode,
		&d.Paid, &d.InPayrun, &d.LedgerSeq, &d.CreatedAt)
	d.Ref = "DLV-" + strings.ToUpper(d.ID[:8])
	return d, err
}

func (s *Service) GetDelivery(ctx context.Context, id string) (Delivery, error) {
	d, err := scanDelivery(s.db.QueryRow(ctx, `SELECT `+deliveryCols+deliveryFrom+` WHERE d.id=$1`, id))
	return d, notFoundIf(err, "delivery")
}

type DeliveryInput struct {
	FarmerID string   `json:"farmer_id"`
	Crop     string   `json:"crop"`
	Grade    string   `json:"grade"`
	WeightKg float64  `json:"weight_kg"`
	Moisture *float64 `json:"moisture"`
}

// RecordDelivery weighs in produce. The record is sealed in the cooperative's ledger, the farmer gets an
// SMS receipt at once, and the AI checks it. Doubtful deliveries are held: recorded, but not paid until
// a manager reviews them.
func (s *Service) RecordDelivery(ctx context.Context, clerk Staff, in DeliveryInput) (Delivery, error) {
	in.WeightKg = math.Round(in.WeightKg*10) / 10
	if in.WeightKg <= 0 || in.WeightKg > 10000 {
		return Delivery{}, invalid("enter the weight in kg (more than 0)")
	}
	if in.Grade != "1" && in.Grade != "2" && in.Grade != "reject" {
		return Delivery{}, invalid("grade must be 1, 2 or reject")
	}
	if in.Moisture != nil && (*in.Moisture < 0 || *in.Moisture > 60) {
		return Delivery{}, invalid("moisture must be between 0 and 60%%")
	}
	far, err := s.GetFarmer(ctx, in.FarmerID)
	if err != nil {
		return Delivery{}, err
	}
	if far.CoopID != clerk.CoopID {
		return Delivery{}, fmt.Errorf("%w: this farmer belongs to another cooperative", ErrForbidden)
	}
	if far.Status != "active" {
		return Delivery{}, fmt.Errorf("%w: this farmer's registration is %s; a manager must confirm it first", ErrConflict, far.Status)
	}
	var maxPerAcre float64
	if err := s.db.QueryRow(ctx, `SELECT max_kg_per_acre FROM crops WHERE code=$1`, in.Crop).Scan(&maxPerAcre); err != nil {
		return Delivery{}, invalid("unknown crop")
	}
	price := 0.0
	if in.Grade != "reject" {
		err := s.db.QueryRow(ctx, `SELECT price_per_kg::float8 FROM prices WHERE coop_id=$1 AND crop=$2 AND grade=$3 ORDER BY effective_from DESC LIMIT 1`,
			clerk.CoopID, in.Crop, in.Grade).Scan(&price)
		if errors.Is(err, pgx.ErrNoRows) {
			return Delivery{}, fmt.Errorf("%w: no price is set for %s grade %s yet; ask the manager to set it", ErrConflict, in.Crop, in.Grade)
		}
		if err != nil {
			return Delivery{}, err
		}
	}
	amount := int64(math.Round(in.WeightKg * price))

	feat := s.deliveryFeatures(ctx, clerk, far, in, maxPerAcre)
	verdict := s.ai.ScoreDelivery(ctx, feat)
	status := "recorded"
	if verdict.Level == "high" {
		status = "held"
	}
	reasons, _ := json.Marshal(verdict.Reasons)

	tx, err := s.db.Begin(ctx)
	if err != nil {
		return Delivery{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err := lockCoop(ctx, tx, clerk.CoopID); err != nil {
		return Delivery{}, err
	}
	var centre *string
	if clerk.CentreID != "" {
		centre = &clerk.CentreID
	}
	var id string
	if err := tx.QueryRow(ctx, `INSERT INTO deliveries(coop_id, centre_id, farmer_id, clerk_id, crop, grade, weight_kg, moisture, price_per_kg, amount,
		status, ai_score, ai_level, ai_reasons) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14) RETURNING id`,
		clerk.CoopID, centre, far.ID, clerk.ID, in.Crop, in.Grade, in.WeightKg, in.Moisture, price, amount,
		status, verdict.Score, verdict.Level, reasons).Scan(&id); err != nil {
		return Delivery{}, err
	}
	ref := deliveryRef(id, in.Crop, in.WeightKg, in.Grade, in.Moisture)
	fid := far.ID
	e, err := appendEntry(ctx, tx, clerk.CoopID, "delivery", &fid, amount, ref)
	if err != nil {
		return Delivery{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE deliveries SET ledger_seq=$2 WHERE id=$1`, id, e.Seq); err != nil {
		return Delivery{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Delivery{}, err
	}
	s.anchorAsync(clerk.CoopID)
	d, err := s.GetDelivery(ctx, id)
	if err != nil {
		return d, err
	}
	msg := fmt.Sprintf("AgriProof: %s received %s kg %s grade %s from you on %s. Value KES %s. Receipt %s, sealed record #%d.",
		far.CoopName, trimKg(in.WeightKg), in.Crop, in.Grade, time.Now().In(eat).Format("2 Jan 15:04"), digits(amount), d.Ref, e.Seq)
	if in.Grade == "reject" {
		msg = fmt.Sprintf("AgriProof: %s recorded %s kg %s from you as REJECTED on %s. Receipt %s. If you disagree, talk to the manager.",
			far.CoopName, trimKg(in.WeightKg), in.Crop, time.Now().In(eat).Format("2 Jan 15:04"), d.Ref)
	}
	if status == "held" {
		msg += " It is being checked before payment."
	}
	s.notify([]string{far.Phone}, msg)
	return d, nil
}

var eat = time.FixedZone("EAT", 3*3600)

func trimKg(v float64) string {
	if v == math.Trunc(v) {
		return fmt.Sprintf("%.0f", v)
	}
	return fmt.Sprintf("%.1f", v)
}

// deliveryRef puts weight, grade and moisture into the hashed ledger line, so none of them can be
// changed later without breaking the seal.
func deliveryRef(id, crop string, kg float64, grade string, moisture *float64) string {
	m := "-"
	if moisture != nil {
		m = fmt.Sprintf("%.1f", *moisture)
	}
	return fmt.Sprintf("DLV-%s|%s|%.1fkg|g%s|m%s", strings.ToUpper(id[:8]), crop, kg, grade, m)
}

func (s *Service) deliveryFeatures(ctx context.Context, clerk Staff, far Farmer, in DeliveryInput, maxPerAcre float64) risk.DeliveryFeatures {
	f := risk.DeliveryFeatures{Crop: in.Crop, WeightKg: in.WeightKg, Grade: in.Grade, Acres: far.Acres, MaxKgPerAcre: maxPerAcre,
		MinutesSinceLast: -1, HourOfDay: time.Now().In(eat).Hour(), ClerkAvgVsCentre: 1}
	if in.Moisture != nil {
		f.Moisture, f.HasMoisture = *in.Moisture, true
	}
	_ = s.db.QueryRow(ctx, `SELECT COALESCE(SUM(weight_kg),0)::float8 + $3 FROM deliveries WHERE farmer_id=$1 AND crop=$2 AND status<>'voided'
		AND created_at > now() - interval '9 months'`, far.ID, in.Crop, in.WeightKg).Scan(&f.SeasonKg)
	_ = s.db.QueryRow(ctx, `SELECT COUNT(*) FROM deliveries WHERE farmer_id=$1 AND created_at > date_trunc('day', now() AT TIME ZONE 'Africa/Nairobi') AT TIME ZONE 'Africa/Nairobi'`,
		far.ID).Scan(&f.FarmerDeliveriesDay)
	var last *time.Time
	_ = s.db.QueryRow(ctx, `SELECT MAX(created_at) FROM deliveries WHERE farmer_id=$1 AND status<>'voided'`, far.ID).Scan(&last)
	if last != nil {
		f.MinutesSinceLast = time.Since(*last).Minutes()
	}
	_ = s.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM deliveries WHERE farmer_id=$1 AND weight_kg=$2 AND status<>'voided'
		AND created_at > now() - interval '1 hour')`, far.ID, in.WeightKg).Scan(&f.SameWeightLastHour)
	// How this clerk weighs compared with others at the same centre, same crop, last 30 days.
	var clerkAvg, centreAvg float64
	_ = s.db.QueryRow(ctx, `SELECT COUNT(*), COALESCE(AVG(CASE WHEN weight_kg = round(weight_kg/5)*5 THEN 1 ELSE 0 END),0)::float8,
		COALESCE(AVG(weight_kg),0)::float8 FROM deliveries WHERE clerk_id=$1 AND crop=$2 AND status<>'voided' AND created_at > now() - interval '30 days'`,
		clerk.ID, in.Crop).Scan(&f.ClerkDeliveries30d, &f.ClerkRoundShare, &clerkAvg)
	if clerk.CentreID != "" {
		_ = s.db.QueryRow(ctx, `SELECT COALESCE(AVG(weight_kg),0)::float8 FROM deliveries WHERE centre_id=$1 AND crop=$2 AND clerk_id<>$3
			AND status<>'voided' AND created_at > now() - interval '30 days'`, clerk.CentreID, in.Crop, clerk.ID).Scan(&centreAvg)
	}
	if clerkAvg > 0 && centreAvg > 0 {
		f.ClerkAvgVsCentre = clerkAvg / centreAvg
	}
	return f
}

// ReviewDelivery lets a manager (or the console) clear or void a held delivery.
func (s *Service) ReviewDelivery(ctx context.Context, coopID, deliveryID string, approve bool, by string) (Delivery, error) {
	d, err := s.GetDelivery(ctx, deliveryID)
	if err != nil {
		return d, err
	}
	if d.CoopID != coopID {
		return d, fmt.Errorf("%w: delivery", ErrNotFound)
	}
	if d.Status != "held" {
		return d, fmt.Errorf("%w: this delivery is not waiting for review", ErrConflict)
	}
	if approve {
		if _, err := s.db.Exec(ctx, `UPDATE deliveries SET status='recorded', reviewed_by=$2 WHERE id=$1 AND status='held'`, deliveryID, by); err != nil {
			return d, err
		}
		return s.GetDelivery(ctx, deliveryID)
	}
	return s.VoidDelivery(ctx, coopID, deliveryID, "rejected after review", by)
}

// VoidDelivery cancels a delivery recorded by mistake. The original line stays in the ledger and a
// cancelling line is added, so the correction itself is on the record. Paid deliveries cannot be voided.
func (s *Service) VoidDelivery(ctx context.Context, coopID, deliveryID, reason, by string) (Delivery, error) {
	reason = cleanText(reason, 200)
	if len(reason) < 3 {
		return Delivery{}, invalid("say why this delivery is being cancelled")
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return Delivery{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err := lockCoop(ctx, tx, coopID); err != nil {
		return Delivery{}, err
	}
	var farmerID, status string
	var amount int64
	var payrun *string
	err = tx.QueryRow(ctx, `SELECT farmer_id, status, amount, payrun_id FROM deliveries WHERE id=$1 AND coop_id=$2 FOR UPDATE`, deliveryID, coopID).
		Scan(&farmerID, &status, &amount, &payrun)
	if err != nil {
		return Delivery{}, notFoundIf(err, "delivery")
	}
	if status == "voided" {
		return Delivery{}, fmt.Errorf("%w: already cancelled", ErrConflict)
	}
	if payrun != nil {
		return Delivery{}, fmt.Errorf("%w: this delivery is already in a payment run; cancel the run first", ErrConflict)
	}
	if _, err := tx.Exec(ctx, `UPDATE deliveries SET status='voided', reviewed_by=$2 WHERE id=$1`, deliveryID, by); err != nil {
		return Delivery{}, err
	}
	if _, err := appendEntry(ctx, tx, coopID, "void", &farmerID, -amount, "VOID-DLV-"+strings.ToUpper(deliveryID[:8])+"|"+reason); err != nil {
		return Delivery{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Delivery{}, err
	}
	s.anchorAsync(coopID)
	d, err := s.GetDelivery(ctx, deliveryID)
	if err == nil {
		var phone string
		_ = s.db.QueryRow(ctx, `SELECT phone FROM farmers WHERE id=$1`, farmerID).Scan(&phone)
		s.notify([]string{phone}, fmt.Sprintf("AgriProof: delivery %s (%s kg) was cancelled: %s. If you disagree, talk to the cooperative.", d.Ref, trimKg(d.WeightKg), reason))
	}
	return d, err
}

// Deliveries lists a cooperative's deliveries; status "" for all, or recorded / held / voided.
func (s *Service) Deliveries(ctx context.Context, coopID, status, farmerID string, limit int) ([]Delivery, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.Query(ctx, `SELECT `+deliveryCols+deliveryFrom+` WHERE d.coop_id=$1 AND ($2='' OR d.status=$2)
		AND ($3='' OR d.farmer_id::text=$3) ORDER BY d.created_at DESC LIMIT $4`, coopID, status, farmerID, limit)
	if err != nil {
		return nil, notFoundIf(err, "cooperative")
	}
	defer rows.Close()
	out := []Delivery{}
	for rows.Next() {
		d, err := scanDelivery(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// HeldDeliveries lists every held delivery across cooperatives (console).
func (s *Service) HeldDeliveries(ctx context.Context) ([]Delivery, error) {
	rows, err := s.db.Query(ctx, `SELECT `+deliveryCols+deliveryFrom+` WHERE d.status='held' ORDER BY d.created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Delivery{}
	for rows.Next() {
		d, err := scanDelivery(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ---------- lots and public proof ----------

type Lot struct {
	ID         string     `json:"id"`
	CoopID     string     `json:"coop_id"`
	Code       string     `json:"code"`
	Crop       string     `json:"crop"`
	Buyer      string     `json:"buyer"`
	Status     string     `json:"status"`
	Deliveries int        `json:"deliveries"`
	Kg         float64    `json:"kg"`
	CreatedAt  time.Time  `json:"created_at"`
	ClosedAt   *time.Time `json:"closed_at,omitempty"`
}

const lotAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"

func randomCode(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	out := make([]byte, n)
	for i, v := range b {
		out[i] = lotAlphabet[int(v)%len(lotAlphabet)]
	}
	return string(out), nil
}

func (s *Service) CreateLot(ctx context.Context, coopID, crop, buyer, by string) (Lot, error) {
	code, err := randomCode(8)
	if err != nil {
		return Lot{}, err
	}
	var id string
	err = s.db.QueryRow(ctx, `INSERT INTO lots(coop_id, code, crop, buyer, created_by) VALUES ($1,$2,$3,$4,$5) RETURNING id`,
		coopID, code, crop, cleanText(buyer, 120), by).Scan(&id)
	if err != nil && strings.Contains(err.Error(), "crops") {
		return Lot{}, invalid("unknown crop")
	}
	if err != nil {
		return Lot{}, notFoundIf(err, "cooperative")
	}
	return s.getLot(ctx, id)
}

const lotCols = `l.id, l.coop_id, l.code, l.crop, l.buyer, l.status, l.created_at, l.closed_at,
	(SELECT COUNT(*) FROM deliveries d WHERE d.lot_id=l.id AND d.status<>'voided'),
	(SELECT COALESCE(SUM(weight_kg),0)::float8 FROM deliveries d WHERE d.lot_id=l.id AND d.status<>'voided')`

func scanLot(row pgx.Row) (Lot, error) {
	var l Lot
	err := row.Scan(&l.ID, &l.CoopID, &l.Code, &l.Crop, &l.Buyer, &l.Status, &l.CreatedAt, &l.ClosedAt, &l.Deliveries, &l.Kg)
	return l, err
}

func (s *Service) getLot(ctx context.Context, id string) (Lot, error) {
	l, err := scanLot(s.db.QueryRow(ctx, `SELECT `+lotCols+` FROM lots l WHERE l.id=$1`, id))
	return l, notFoundIf(err, "lot")
}

func (s *Service) Lots(ctx context.Context, coopID string) ([]Lot, error) {
	rows, err := s.db.Query(ctx, `SELECT `+lotCols+` FROM lots l WHERE l.coop_id=$1 ORDER BY l.created_at DESC LIMIT 100`, coopID)
	if err != nil {
		return nil, notFoundIf(err, "cooperative")
	}
	defer rows.Close()
	out := []Lot{}
	for rows.Next() {
		l, err := scanLot(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// AddToLot puts recorded deliveries of the lot's crop into an open lot.
func (s *Service) AddToLot(ctx context.Context, coopID, lotID string, deliveryIDs []string) (Lot, error) {
	l, err := s.getLot(ctx, lotID)
	if err != nil {
		return l, err
	}
	if l.CoopID != coopID {
		return l, fmt.Errorf("%w: lot", ErrNotFound)
	}
	if l.Status != "open" {
		return l, fmt.Errorf("%w: this lot is closed", ErrConflict)
	}
	if len(deliveryIDs) == 0 || len(deliveryIDs) > 2000 {
		return l, invalid("choose the deliveries to add")
	}
	tag, err := s.db.Exec(ctx, `UPDATE deliveries SET lot_id=$1 WHERE id = ANY($2::uuid[]) AND coop_id=$3 AND crop=$4
		AND status='recorded' AND lot_id IS NULL`, lotID, deliveryIDs, coopID, l.Crop)
	if err != nil {
		return l, notFoundIf(err, "delivery")
	}
	if tag.RowsAffected() == 0 {
		return l, fmt.Errorf("%w: none of those deliveries can go in this lot (wrong crop, already in a lot, or not cleared)", ErrConflict)
	}
	return s.getLot(ctx, lotID)
}

func (s *Service) CloseLot(ctx context.Context, coopID, lotID string) (Lot, error) {
	tag, err := s.db.Exec(ctx, `UPDATE lots SET status='closed', closed_at=now() WHERE id=$1 AND coop_id=$2 AND status='open'`, lotID, coopID)
	if err != nil {
		return Lot{}, notFoundIf(err, "lot")
	}
	if tag.RowsAffected() == 0 {
		return Lot{}, fmt.Errorf("%w: lot not found or already closed", ErrConflict)
	}
	return s.getLot(ctx, lotID)
}

type LotInput struct {
	Product  string `json:"product"`
	Supplier string `json:"supplier"`
	BatchNo  string `json:"batch_no"`
	Lab      string `json:"lab_result"`
	Status   string `json:"status"`
	Farms    int    `json:"farms"`
}

// LotProof is the public page a buyer sees after scanning the QR code. No personal data.
type LotProof struct {
	Code          string     `json:"code"`
	Cooperative   string     `json:"cooperative"`
	County        string     `json:"county"`
	Crop          string     `json:"crop"`
	CropName      string     `json:"crop_name"`
	Buyer         string     `json:"buyer"`
	Status        string     `json:"status"`
	Kg            float64    `json:"kg"`
	Deliveries    int        `json:"deliveries"`
	Farms         int        `json:"farms"`
	Grade1Kg      float64    `json:"grade1_kg"`
	First         *time.Time `json:"first_delivery,omitempty"`
	Last          *time.Time `json:"last_delivery,omitempty"`
	FarmersPaidPc float64    `json:"farmers_paid_pct"`
	Inputs        []LotInput `json:"inputs"`
	RecordsValid  bool       `json:"records_valid"`
	SealedOnChain bool       `json:"sealed_on_chain"`
	ChainNote     string     `json:"chain_note"`
	CreatedAt     time.Time  `json:"created_at"`
}

func (s *Service) LotProofByCode(ctx context.Context, code string) (LotProof, error) {
	code = strings.ToUpper(strings.TrimSpace(code))
	var p LotProof
	var lotID, coopID string
	err := s.db.QueryRow(ctx, `SELECT l.id, l.coop_id, l.code, c.name, c.county, l.crop, cr.name, l.buyer, l.status, l.created_at
		FROM lots l JOIN cooperatives c ON c.id=l.coop_id JOIN crops cr ON cr.code=l.crop WHERE l.code=$1`, code).
		Scan(&lotID, &coopID, &p.Code, &p.Cooperative, &p.County, &p.Crop, &p.CropName, &p.Buyer, &p.Status, &p.CreatedAt)
	if err != nil {
		return p, notFoundIf(err, "lot")
	}
	if err := s.db.QueryRow(ctx, `SELECT COUNT(*), COUNT(DISTINCT farmer_id), COALESCE(SUM(weight_kg),0)::float8,
		COALESCE(SUM(CASE WHEN grade='1' THEN weight_kg ELSE 0 END),0)::float8, MIN(created_at), MAX(created_at),
		COALESCE(100.0 * SUM(CASE WHEN EXISTS(SELECT 1 FROM payouts p WHERE p.payrun_id=d.payrun_id AND p.farmer_id=d.farmer_id AND p.status='paid')
			THEN amount ELSE 0 END) / NULLIF(SUM(amount),0), 0)::float8
		FROM deliveries d WHERE lot_id=$1 AND status<>'voided'`, lotID).
		Scan(&p.Deliveries, &p.Farms, &p.Kg, &p.Grade1Kg, &p.First, &p.Last, &p.FarmersPaidPc); err != nil {
		return p, err
	}
	p.Inputs = []LotInput{}
	rows, err := s.db.Query(ctx, `SELECT b.product, su.name, b.batch_no, b.lab_result, b.status, COUNT(DISTINCT i.farmer_id)
		FROM input_issues i JOIN input_batches b ON b.id=i.batch_id JOIN suppliers su ON su.id=b.supplier_id
		WHERE i.farmer_id IN (SELECT farmer_id FROM deliveries WHERE lot_id=$1 AND status<>'voided')
		GROUP BY b.product, su.name, b.batch_no, b.lab_result, b.status ORDER BY 6 DESC LIMIT 10`, lotID)
	if err == nil {
		for rows.Next() {
			var in LotInput
			if rows.Scan(&in.Product, &in.Supplier, &in.BatchNo, &in.Lab, &in.Status, &in.Farms) == nil {
				p.Inputs = append(p.Inputs, in)
			}
		}
		rows.Close()
	}
	rep, err := s.verifyForPublic(ctx, coopID)
	if err == nil {
		p.RecordsValid = rep.Valid
		p.SealedOnChain = rep.Source == "blockchain" && rep.AnchorMatches != nil && *rep.AnchorMatches
		p.ChainNote = rep.Warning
	}
	return p, nil
}
