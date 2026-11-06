package core

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/agriproof/backend/internal/mpesa"
	"github.com/agriproof/backend/internal/risk"
)

// ---------- suppliers ----------

type Supplier struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Kind       string    `json:"kind"`
	RegNo      string    `json:"reg_no"`
	KebsPermit string    `json:"kebs_permit"`
	Phone      string    `json:"phone"`
	County     string    `json:"county"`
	Status     string    `json:"status"`
	Notes      string    `json:"notes"`
	DecidedBy  string    `json:"decided_by"`
	CreatedAt  time.Time `json:"created_at"`
	Batches    int       `json:"batches"`
	Recalled   int       `json:"recalled"`
}

type SupplierInput struct {
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	RegNo      string `json:"reg_no"`
	KebsPermit string `json:"kebs_permit"`
	Phone      string `json:"phone"`
	County     string `json:"county"`
	Notes      string `json:"notes"`
}

// AddSupplier registers a manufacturer, importer or distributor. It starts as pending: no batch can be
// added until someone has checked its registration and KEBS permit and marked it verified.
func (s *Service) AddSupplier(ctx context.Context, in SupplierInput) (Supplier, error) {
	name := cleanText(in.Name, 160)
	if len(name) < 2 {
		return Supplier{}, invalid("enter the supplier's name")
	}
	if in.Kind != "manufacturer" && in.Kind != "importer" && in.Kind != "distributor" {
		return Supplier{}, invalid("type must be manufacturer, importer or distributor")
	}
	phone := ""
	if strings.TrimSpace(in.Phone) != "" {
		p, err := mpesa.NormalizePhone(in.Phone)
		if err != nil {
			return Supplier{}, invalid("phone number is not valid")
		}
		phone = p
	}
	var id string
	err := s.db.QueryRow(ctx, `INSERT INTO suppliers(name, kind, reg_no, kebs_permit, phone, county, notes) VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING id`,
		name, in.Kind, cleanText(in.RegNo, 60), cleanText(in.KebsPermit, 60), phone, cleanText(in.County, 60), cleanText(in.Notes, 500)).Scan(&id)
	if isUniqueViolation(err) {
		return Supplier{}, fmt.Errorf("%w: a supplier with that name already exists", ErrConflict)
	}
	if err != nil {
		return Supplier{}, err
	}
	return s.getSupplier(ctx, id)
}

const supplierCols = `s.id, s.name, s.kind, s.reg_no, s.kebs_permit, s.phone, s.county, s.status, s.notes, s.decided_by, s.created_at,
	(SELECT COUNT(*) FROM input_batches b WHERE b.supplier_id=s.id), (SELECT COUNT(*) FROM input_batches b WHERE b.supplier_id=s.id AND b.status='recalled')`

func scanSupplier(row pgx.Row) (Supplier, error) {
	var x Supplier
	err := row.Scan(&x.ID, &x.Name, &x.Kind, &x.RegNo, &x.KebsPermit, &x.Phone, &x.County, &x.Status, &x.Notes, &x.DecidedBy, &x.CreatedAt, &x.Batches, &x.Recalled)
	return x, err
}

func (s *Service) getSupplier(ctx context.Context, id string) (Supplier, error) {
	x, err := scanSupplier(s.db.QueryRow(ctx, `SELECT `+supplierCols+` FROM suppliers s WHERE s.id=$1`, id))
	return x, notFoundIf(err, "supplier")
}

func (s *Service) Suppliers(ctx context.Context) ([]Supplier, error) {
	rows, err := s.db.Query(ctx, `SELECT `+supplierCols+` FROM suppliers s ORDER BY (s.status='pending') DESC, s.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Supplier{}
	for rows.Next() {
		x, err := scanSupplier(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// SetSupplierStatus verifies or suspends a supplier. Suspending stops new batches; existing bag codes
// still check, but the answer warns that the supplier is suspended.
func (s *Service) SetSupplierStatus(ctx context.Context, id, status, by string) (Supplier, error) {
	if status != "verified" && status != "suspended" {
		return Supplier{}, invalid("status must be verified or suspended")
	}
	if status == "verified" {
		var reg, permit string
		if err := s.db.QueryRow(ctx, `SELECT reg_no, kebs_permit FROM suppliers WHERE id=$1`, id).Scan(&reg, &permit); err != nil {
			return Supplier{}, notFoundIf(err, "supplier")
		}
		if reg == "" || permit == "" {
			return Supplier{}, invalid("record the business registration number and KEBS permit before verifying")
		}
	}
	tag, err := s.db.Exec(ctx, `UPDATE suppliers SET status=$2, decided_by=$3 WHERE id=$1`, id, status, by)
	if err != nil {
		return Supplier{}, notFoundIf(err, "supplier")
	}
	if tag.RowsAffected() == 0 {
		return Supplier{}, fmt.Errorf("%w: supplier", ErrNotFound)
	}
	return s.getSupplier(ctx, id)
}

// ---------- batches and bag codes ----------

type Batch struct {
	ID            string     `json:"id"`
	SupplierID    string     `json:"supplier_id"`
	SupplierName  string     `json:"supplier_name"`
	SupplierState string     `json:"supplier_status"`
	Product       string     `json:"product"`
	InputType     string     `json:"input_type"`
	StatedContent string     `json:"stated_content"`
	BagKg         float64    `json:"bag_kg"`
	BatchNo       string     `json:"batch_no"`
	MadeOn        *time.Time `json:"made_on,omitempty"`
	ExpiresOn     *time.Time `json:"expires_on,omitempty"`
	Bags          int        `json:"bags"`
	LabResult     string     `json:"lab_result"`
	LabContent    string     `json:"lab_content"`
	LabRef        string     `json:"lab_ref"`
	Status        string     `json:"status"`
	RecallReason  string     `json:"recall_reason"`
	CodesIssued   bool       `json:"codes_issued"`
	CreatedAt     time.Time  `json:"created_at"`
	BagsChecked   int        `json:"bags_checked"`
	BagsRepeat    int        `json:"bags_checked_by_several_phones"`
	BagsIssued    int        `json:"bags_issued_to_farmers"`
	Complaints    int        `json:"complaints"`
	Shipped       int        `json:"bags_shipped"`
}

type BatchInput struct {
	SupplierID    string  `json:"supplier_id"`
	Product       string  `json:"product"`
	InputType     string  `json:"input_type"`
	StatedContent string  `json:"stated_content"`
	BagKg         float64 `json:"bag_kg"`
	BatchNo       string  `json:"batch_no"`
	MadeOn        string  `json:"made_on"`
	ExpiresOn     string  `json:"expires_on"`
	Bags          int     `json:"bags"`
}

const batchCols = `b.id, b.supplier_id, su.name, su.status, b.product, b.input_type, b.stated_content, b.bag_kg::float8, b.batch_no, b.made_on, b.expires_on,
	b.bags, b.lab_result, b.lab_content, b.lab_ref, b.status, b.recall_reason, b.codes_issued, b.created_at,
	(SELECT COUNT(*) FROM bag_codes c WHERE c.batch_id=b.id AND c.checks > 0),
	(SELECT COUNT(*) FROM bag_codes c WHERE c.batch_id=b.id AND (SELECT COUNT(DISTINCT COALESCE(NULLIF(k.phone,''), 'ip:'||k.ip)) FROM code_checks k WHERE k.bag_id=c.id) > 1),
	(SELECT COALESCE(SUM(bags),0)::int FROM input_issues i WHERE i.batch_id=b.id),
	(SELECT COUNT(*) FROM input_complaints m WHERE m.batch_id=b.id),
	(SELECT COALESCE(SUM(bags),0)::int FROM shipments sh WHERE sh.batch_id=b.id)`
const batchFrom = ` FROM input_batches b JOIN suppliers su ON su.id=b.supplier_id`

func scanBatch(row pgx.Row) (Batch, error) {
	var b Batch
	err := row.Scan(&b.ID, &b.SupplierID, &b.SupplierName, &b.SupplierState, &b.Product, &b.InputType, &b.StatedContent, &b.BagKg, &b.BatchNo,
		&b.MadeOn, &b.ExpiresOn, &b.Bags, &b.LabResult, &b.LabContent, &b.LabRef, &b.Status, &b.RecallReason, &b.CodesIssued, &b.CreatedAt,
		&b.BagsChecked, &b.BagsRepeat, &b.BagsIssued, &b.Complaints, &b.Shipped)
	return b, err
}

func (s *Service) GetBatch(ctx context.Context, id string) (Batch, error) {
	b, err := scanBatch(s.db.QueryRow(ctx, `SELECT `+batchCols+batchFrom+` WHERE b.id=$1`, id))
	return b, notFoundIf(err, "batch")
}

func (s *Service) Batches(ctx context.Context) ([]Batch, error) {
	rows, err := s.db.Query(ctx, `SELECT `+batchCols+batchFrom+` ORDER BY b.created_at DESC LIMIT 300`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Batch{}
	for rows.Next() {
		b, err := scanBatch(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func parseDate(v string) (*time.Time, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil, nil
	}
	t, err := time.Parse("2006-01-02", v)
	if err != nil {
		return nil, invalid("dates must look like 2026-11-03")
	}
	return &t, nil
}

// AddBatch registers a production batch from a verified supplier.
func (s *Service) AddBatch(ctx context.Context, in BatchInput, by string) (Batch, error) {
	sup, err := s.getSupplier(ctx, in.SupplierID)
	if err != nil {
		return Batch{}, err
	}
	if sup.Status != "verified" {
		return Batch{}, fmt.Errorf("%w: only verified suppliers can add batches (this one is %s)", ErrForbidden, sup.Status)
	}
	if in.InputType == "" {
		in.InputType = "fertilizer"
	}
	if in.InputType != "fertilizer" && in.InputType != "seed" && in.InputType != "agrochemical" {
		return Batch{}, invalid("type must be fertilizer, seed or agrochemical")
	}
	product, batchNo := cleanText(in.Product, 120), cleanText(in.BatchNo, 60)
	if len(product) < 2 || batchNo == "" {
		return Batch{}, invalid("enter the product name and batch number printed on the bag")
	}
	if in.BagKg <= 0 || in.BagKg > 1000 || in.Bags <= 0 || in.Bags > 200000 {
		return Batch{}, invalid("enter the bag size in kg and the number of bags (up to 200,000)")
	}
	made, err := parseDate(in.MadeOn)
	if err != nil {
		return Batch{}, err
	}
	expires, err := parseDate(in.ExpiresOn)
	if err != nil {
		return Batch{}, err
	}
	var id string
	err = s.db.QueryRow(ctx, `INSERT INTO input_batches(supplier_id, product, input_type, stated_content, bag_kg, batch_no, made_on, expires_on, bags, created_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) RETURNING id`,
		sup.ID, product, in.InputType, cleanText(in.StatedContent, 120), in.BagKg, batchNo, made, expires, in.Bags, by).Scan(&id)
	if isUniqueViolation(err) {
		return Batch{}, fmt.Errorf("%w: this supplier already has a batch with that number", ErrConflict)
	}
	if err != nil {
		return Batch{}, err
	}
	return s.GetBatch(ctx, id)
}

const bagAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"

// codeHash keys bag codes with the server secret, so a copy of the database alone cannot be used
// to print valid codes.
func (s *Service) codeHash(code string) string {
	m := hmac.New(sha256.New, s.secret)
	m.Write([]byte("bag|" + code))
	return hex.EncodeToString(m.Sum(nil))
}

// NormalizeBagCode accepts "abcd-efgh-jkmn", spaces, or the full check link.
func NormalizeBagCode(v string) string {
	if i := strings.LastIndex(v, "/"); i >= 0 {
		v = v[i+1:]
	}
	v = strings.ToUpper(v)
	var b strings.Builder
	for _, r := range v {
		if strings.ContainsRune(bagAlphabet, r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func FormatBagCode(c string) string {
	if len(c) != 12 {
		return c
	}
	return c[:4] + "-" + c[4:8] + "-" + c[8:]
}

type BagCode struct {
	Serial int    `json:"serial"`
	Code   string `json:"code"`
}

// IssueCodes creates one secret code per bag. The codes are returned only this once, for printing on
// tamper-evident labels; afterwards only their keyed hashes exist.
func (s *Service) IssueCodes(ctx context.Context, batchID string) ([]BagCode, Batch, error) {
	b, err := s.GetBatch(ctx, batchID)
	if err != nil {
		return nil, b, err
	}
	if b.Status != "active" || b.SupplierState != "verified" {
		return nil, b, fmt.Errorf("%w: codes can only be issued for active batches of verified suppliers", ErrForbidden)
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return nil, b, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	tag, err := tx.Exec(ctx, `UPDATE input_batches SET codes_issued=TRUE WHERE id=$1 AND NOT codes_issued`, batchID)
	if err != nil {
		return nil, b, err
	}
	if tag.RowsAffected() == 0 {
		return nil, b, fmt.Errorf("%w: codes for this batch were already issued; they are shown only once", ErrConflict)
	}
	out := make([]BagCode, 0, b.Bags)
	rows := make([][]any, 0, b.Bags)
	for i := 1; i <= b.Bags; i++ {
		c, err := randomBagCode()
		if err != nil {
			return nil, b, err
		}
		out = append(out, BagCode{Serial: i, Code: FormatBagCode(c)})
		rows = append(rows, []any{batchID, i, s.codeHash(c)})
	}
	if _, err := tx.CopyFrom(ctx, pgx.Identifier{"bag_codes"}, []string{"batch_id", "serial", "code_hash"}, pgx.CopyFromRows(rows)); err != nil {
		return nil, b, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, b, err
	}
	b, _ = s.GetBatch(ctx, batchID)
	return out, b, nil
}

func randomBagCode() (string, error) { return randomCode(12) }

// RecordLab stores a lab test of the batch (KEBS or an accredited lab).
func (s *Service) RecordLab(ctx context.Context, batchID, result, content, ref string) (Batch, error) {
	if result != "pass" && result != "fail" {
		return Batch{}, invalid("result must be pass or fail")
	}
	ref = cleanText(ref, 80)
	if ref == "" {
		return Batch{}, invalid("enter the lab report number")
	}
	tag, err := s.db.Exec(ctx, `UPDATE input_batches SET lab_result=$2, lab_content=$3, lab_ref=$4 WHERE id=$1`, batchID, result, cleanText(content, 120), ref)
	if err != nil {
		return Batch{}, notFoundIf(err, "batch")
	}
	if tag.RowsAffected() == 0 {
		return Batch{}, fmt.Errorf("%w: batch", ErrNotFound)
	}
	return s.GetBatch(ctx, batchID)
}

func (s *Service) AddShipment(ctx context.Context, batchID, toCoopID, toName string, bags int, by string) (Batch, error) {
	b, err := s.GetBatch(ctx, batchID)
	if err != nil {
		return b, err
	}
	toName = cleanText(toName, 120)
	if bags <= 0 || toName == "" {
		return b, invalid("enter where the bags went and how many")
	}
	if b.Shipped+bags > b.Bags {
		return b, invalid("only %d bags of this batch are left to ship", b.Bags-b.Shipped)
	}
	var coop *string
	if toCoopID != "" {
		coop = &toCoopID
	}
	if _, err := s.db.Exec(ctx, `INSERT INTO shipments(batch_id, to_coop_id, to_name, bags, recorded_by) VALUES ($1,$2,$3,$4,$5)`,
		batchID, coop, toName, bags, by); err != nil {
		return b, notFoundIf(err, "cooperative")
	}
	return s.GetBatch(ctx, batchID)
}

// RecallBatch stops a batch and texts every farmer who received or checked a bag from it.
func (s *Service) RecallBatch(ctx context.Context, batchID, reason, by string) (Batch, int, error) {
	reason = cleanText(reason, 200)
	if len(reason) < 5 {
		return Batch{}, 0, invalid("say why the batch is recalled; farmers will see this")
	}
	tag, err := s.db.Exec(ctx, `UPDATE input_batches SET status='recalled', recall_reason=$2 WHERE id=$1 AND status='active'`, batchID, reason)
	if err != nil {
		return Batch{}, 0, notFoundIf(err, "batch")
	}
	if tag.RowsAffected() == 0 {
		return Batch{}, 0, fmt.Errorf("%w: batch not found or already recalled", ErrConflict)
	}
	b, err := s.GetBatch(ctx, batchID)
	if err != nil {
		return b, 0, err
	}
	rows, err := s.db.Query(ctx, `SELECT DISTINCT phone FROM (
		SELECT f.phone FROM input_issues i JOIN farmers f ON f.id=i.farmer_id WHERE i.batch_id=$1
		UNION SELECT k.phone FROM code_checks k WHERE k.batch_id=$1 AND k.phone<>'') p`, batchID)
	var phones []string
	if err == nil {
		for rows.Next() {
			var p string
			if rows.Scan(&p) == nil {
				phones = append(phones, p)
			}
		}
		rows.Close()
	}
	s.notify(phones, fmt.Sprintf("AgriProof RECALL: %s batch %s from %s should not be used: %s. Keep the bag and receipt and contact your cooperative.",
		b.Product, b.BatchNo, b.SupplierName, reason))
	s.log.Warn("batch recalled", "batch", batchID, "by", by, "farmers_told", len(phones))
	return b, len(phones), nil
}

// ---------- checking a bag code ----------

type CheckResult struct {
	Result     string     `json:"result"` // genuine | repeat | not_found | recalled
	Headline   string     `json:"headline"`
	Message    string     `json:"message"`
	Product    string     `json:"product,omitempty"`
	Supplier   string     `json:"supplier,omitempty"`
	BatchNo    string     `json:"batch_no,omitempty"`
	BagKg      float64    `json:"bag_kg,omitempty"`
	Stated     string     `json:"stated_content,omitempty"`
	MadeOn     *time.Time `json:"made_on,omitempty"`
	ExpiresOn  *time.Time `json:"expires_on,omitempty"`
	LabResult  string     `json:"lab_result,omitempty"`
	FirstCheck *time.Time `json:"first_checked_at,omitempty"`
	Warnings   []string   `json:"warnings"`
	AILevel    string     `json:"ai_level"`
}

// CheckCode tells a farmer whether a bag is genuine. Each code is meant for one bag: the first phone
// to check it "owns" it; a check from a different phone means the code was copied.
func (s *Service) CheckCode(ctx context.Context, raw, phone, channel, ip string) (CheckResult, error) {
	code := NormalizeBagCode(raw)
	if len(code) != 12 {
		return CheckResult{}, invalid("the code on the bag has 12 letters and numbers, like ABCD-EFGH-JKMN")
	}
	if phone != "" {
		if p, err := mpesa.NormalizePhone(phone); err == nil {
			phone = p
		} else {
			phone = ""
		}
	}
	res := CheckResult{Warnings: []string{}, AILevel: "low"}
	var bagID int64
	var batchID, firstPhone, supplierStatus string
	var firstAt *time.Time
	var checks int
	err := s.db.QueryRow(ctx, `SELECT c.id, c.batch_id, c.first_checked_phone, c.first_checked_at, c.checks, b.product, su.name, su.status, b.batch_no,
		b.bag_kg::float8, b.stated_content, b.made_on, b.expires_on, b.lab_result
		FROM bag_codes c JOIN input_batches b ON b.id=c.batch_id JOIN suppliers su ON su.id=b.supplier_id WHERE c.code_hash=$1`, s.codeHash(code)).
		Scan(&bagID, &batchID, &firstPhone, &firstAt, &checks, &res.Product, &res.Supplier, &supplierStatus, &res.BatchNo,
			&res.BagKg, &res.Stated, &res.MadeOn, &res.ExpiresOn, &res.LabResult)
	if errors.Is(err, pgx.ErrNoRows) {
		res.Result = "not_found"
		res.Headline = "NOT FOUND: this code is not on any registered bag"
		res.Message = "Do not buy or use this bag. Check you typed the code correctly; if it is right, the bag is probably fake. Report it to your cooperative."
		s.logCheck(ctx, nil, nil, phone, channel, ip, res.Result)
		res.AILevel, res.Warnings = s.codeAI(ctx, res.Result, 0, "", phone, res.Warnings)
		return res, nil
	}
	if err != nil {
		return res, err
	}
	var status, recallReason string
	_ = s.db.QueryRow(ctx, `SELECT status, recall_reason FROM input_batches WHERE id=$1`, batchID).Scan(&status, &recallReason)

	// Record this check; the first phone (or, on the website, connection) to check a bag owns it.
	owner := phone
	if owner == "" {
		owner = "ip:" + ip
	}
	samePhone := owner == firstPhone
	if _, err := s.db.Exec(ctx, `UPDATE bag_codes SET checks=checks+1,
		first_checked_at=COALESCE(first_checked_at, now()),
		first_checked_phone=CASE WHEN first_checked_phone='' THEN $2 ELSE first_checked_phone END WHERE id=$1`, bagID, owner); err != nil {
		return res, err
	}
	switch {
	case status == "recalled":
		res.Result = "recalled"
		res.Headline = "RECALLED: do not use this bag"
		res.Message = "This batch was recalled: " + recallReason + ". Keep the bag and your receipt and contact your cooperative or the seller."
	case checks == 0 || samePhone:
		res.Result = "genuine"
		res.Headline = "GENUINE: this bag is registered"
		res.Message = fmt.Sprintf("%s, %s kg, batch %s from %s.", res.Product, trimKg(res.BagKg), res.BatchNo, res.Supplier)
		if checks > 0 {
			res.Message += " You checked this bag before."
		}
	default:
		res.Result = "repeat"
		res.Headline = "WARNING: this code was already checked by another phone"
		res.Message = "Each bag has its own code. This one was first checked"
		if firstAt != nil {
			res.Message += " on " + firstAt.In(eat).Format("2 Jan 2006")
		}
		res.Message += ". The bag may be a copy with a reused label. Do not buy it unless the seller can explain."
	}
	res.FirstCheck = firstAt
	if res.LabResult == "fail" && res.Result != "recalled" {
		res.Warnings = append(res.Warnings, "a lab test of this batch failed")
	}
	if supplierStatus == "suspended" {
		res.Warnings = append(res.Warnings, "this supplier is currently suspended")
	}
	if res.ExpiresOn != nil && res.ExpiresOn.Before(time.Now()) {
		res.Warnings = append(res.Warnings, "this bag is past its expiry date")
	}
	bid := bagID
	s.logCheck(ctx, &bid, &batchID, phone, channel, ip, res.Result)
	res.AILevel, res.Warnings = s.codeAI(ctx, res.Result, bagID, batchID, phone, res.Warnings)
	return res, nil
}

func (s *Service) logCheck(ctx context.Context, bagID *int64, batchID *string, phone, channel, ip, result string) {
	_, err := s.db.Exec(ctx, `INSERT INTO code_checks(bag_id, batch_id, phone, channel, ip, result) VALUES ($1,$2,$3,$4,$5,$6)`,
		bagID, batchID, phone, channel, ip, result)
	if err != nil {
		s.log.Error("could not log code check", "err", err)
	}
}

func (s *Service) codeAI(ctx context.Context, result string, bagID int64, batchID, phone string, warnings []string) (string, []string) {
	f := risk.CodeCheckFeatures{Result: result}
	if bagID > 0 {
		var first *time.Time
		_ = s.db.QueryRow(ctx, `SELECT COUNT(DISTINCT COALESCE(NULLIF(phone,''), 'ip:'||ip)), COUNT(*), MIN(created_at) FROM code_checks WHERE bag_id=$1`, bagID).
			Scan(&f.DistinctPhones, &f.TotalChecks, &first)
		if first != nil {
			f.HoursSinceFirst = time.Since(*first).Hours()
		}
	}
	if phone != "" {
		_ = s.db.QueryRow(ctx, `SELECT COUNT(*) FROM code_checks WHERE phone=$1 AND result='not_found' AND created_at > now() - interval '24 hours'`, phone).Scan(&f.PhoneNotFound24h)
	}
	if batchID != "" {
		_ = s.db.QueryRow(ctx, `SELECT COUNT(*) FILTER (WHERE checks>0),
			COALESCE(AVG(CASE WHEN (SELECT COUNT(DISTINCT COALESCE(NULLIF(k.phone,''), 'ip:'||k.ip)) FROM code_checks k WHERE k.bag_id=c.id) > 1 THEN 1.0 ELSE 0 END)
				FILTER (WHERE checks>0), 0)::float8 FROM bag_codes c WHERE batch_id=$1`, batchID).Scan(&f.BatchBagsChecked, &f.BatchRepeatShare)
		_ = s.db.QueryRow(ctx, `SELECT COUNT(*) FROM input_complaints WHERE batch_id=$1`, batchID).Scan(&f.BatchComplaints)
		_ = s.db.QueryRow(ctx, `SELECT lab_result FROM input_batches WHERE id=$1`, batchID).Scan(&f.BatchLabResult)
	}
	v := s.ai.ScoreCodeCheck(ctx, f)
	if result == "not_found" || result == "recalled" {
		return v.Level, warnings // the headline already says it all
	}
	for _, r := range v.Reasons {
		dup := false
		for _, w := range warnings {
			if w == r {
				dup = true
			}
		}
		if !dup && v.Source == "ai" {
			warnings = append(warnings, r)
		}
	}
	return v.Level, warnings
}

// ---------- inputs given to farmers, complaints ----------

type Issue struct {
	ID        int64     `json:"id"`
	FarmerID  string    `json:"farmer_id"`
	Farmer    string    `json:"farmer_name"`
	BatchID   string    `json:"batch_id"`
	Product   string    `json:"product"`
	BatchNo   string    `json:"batch_no"`
	Supplier  string    `json:"supplier"`
	Bags      float64   `json:"bags"`
	Status    string    `json:"batch_status"`
	IssuedBy  string    `json:"issued_by"`
	CreatedAt time.Time `json:"created_at"`
}

// IssueInput records fertilizer or seed handed to a farmer. With a bag code, the bag must be genuine,
// not recalled, and not already given to someone else.
func (s *Service) IssueInput(ctx context.Context, by Staff, farmerID, code, batchID string, bags float64) (Issue, error) {
	far, err := s.GetFarmer(ctx, farmerID)
	if err != nil {
		return Issue{}, err
	}
	if far.CoopID != by.CoopID || far.Status != "active" {
		return Issue{}, fmt.Errorf("%w: this farmer is not an active member of your cooperative", ErrForbidden)
	}
	var bagID *int64
	if code != "" {
		c := NormalizeBagCode(code)
		var id int64
		var issuedTo *string
		err := s.db.QueryRow(ctx, `SELECT id, batch_id, issued_to FROM bag_codes WHERE code_hash=$1`, s.codeHash(c)).Scan(&id, &batchID, &issuedTo)
		if errors.Is(err, pgx.ErrNoRows) {
			return Issue{}, fmt.Errorf("%w: that bag code is not registered: do not hand out this bag", ErrNotFound)
		}
		if err != nil {
			return Issue{}, err
		}
		if issuedTo != nil {
			return Issue{}, fmt.Errorf("%w: this bag was already given to another farmer", ErrConflict)
		}
		bagID, bags = &id, 1
	}
	b, err := s.GetBatch(ctx, batchID)
	if err != nil {
		return Issue{}, err
	}
	if b.Status == "recalled" {
		return Issue{}, fmt.Errorf("%w: this batch is recalled: do not hand it out", ErrForbidden)
	}
	// Only stock that was shipped to this cooperative can be handed out here, so nobody can mark
	// another region's bags as given and block their real owners.
	var shipped bool
	if err := s.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM shipments WHERE batch_id=$1 AND to_coop_id=$2)`, b.ID, by.CoopID).Scan(&shipped); err != nil {
		return Issue{}, err
	}
	if !shipped {
		return Issue{}, fmt.Errorf("%w: this batch was not shipped to your cooperative; ask the AgriProof office to record the delivery of this stock first", ErrForbidden)
	}
	if bags <= 0 || bags > 1000 {
		return Issue{}, invalid("enter the number of bags")
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return Issue{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if bagID != nil {
		tag, err := tx.Exec(ctx, `UPDATE bag_codes SET issued_to=$2 WHERE id=$1 AND issued_to IS NULL`, *bagID, farmerID)
		if err != nil {
			return Issue{}, err
		}
		if tag.RowsAffected() == 0 {
			return Issue{}, fmt.Errorf("%w: this bag was already given to another farmer", ErrConflict)
		}
	}
	var id int64
	if err := tx.QueryRow(ctx, `INSERT INTO input_issues(coop_id, farmer_id, batch_id, bag_id, bags, issued_by) VALUES ($1,$2,$3,$4,$5,$6) RETURNING id`,
		by.CoopID, farmerID, b.ID, bagID, bags, by.ID).Scan(&id); err != nil {
		return Issue{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Issue{}, err
	}
	s.notify([]string{far.Phone}, fmt.Sprintf("AgriProof: %s gave you %s bag(s) of %s, batch %s from %s. If it does not work as it should, report it in the app or reply to your cooperative.",
		far.CoopName, trimKg(bags), b.Product, b.BatchNo, b.SupplierName))
	list, err := s.Issues(ctx, by.CoopID, farmerID, 1)
	if err != nil || len(list) == 0 {
		return Issue{ID: id}, err
	}
	return list[0], nil
}

func (s *Service) Issues(ctx context.Context, coopID, farmerID string, limit int) ([]Issue, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.Query(ctx, `SELECT i.id, i.farmer_id, f.name, i.batch_id, b.product, b.batch_no, su.name, i.bags::float8, b.status, t.name, i.created_at
		FROM input_issues i JOIN farmers f ON f.id=i.farmer_id JOIN input_batches b ON b.id=i.batch_id JOIN suppliers su ON su.id=b.supplier_id
		JOIN staff t ON t.id=i.issued_by WHERE i.coop_id=$1 AND ($2='' OR i.farmer_id::text=$2) ORDER BY i.created_at DESC LIMIT $3`, coopID, farmerID, limit)
	if err != nil {
		return nil, notFoundIf(err, "cooperative")
	}
	defer rows.Close()
	out := []Issue{}
	for rows.Next() {
		var x Issue
		if err := rows.Scan(&x.ID, &x.FarmerID, &x.Farmer, &x.BatchID, &x.Product, &x.BatchNo, &x.Supplier, &x.Bags, &x.Status, &x.IssuedBy, &x.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// Complain records a farmer's report about a bag (by code, or a batch they received).
func (s *Service) Complain(ctx context.Context, phone, code, batchID, problem string) error {
	problem = cleanText(problem, 500)
	if len(problem) < 3 {
		return invalid("describe the problem in a few words")
	}
	if code != "" {
		c := NormalizeBagCode(code)
		if err := s.db.QueryRow(ctx, `SELECT batch_id FROM bag_codes WHERE code_hash=$1`, s.codeHash(c)).Scan(&batchID); err != nil {
			return fmt.Errorf("%w: that code is not registered; please also report the seller to your cooperative", ErrNotFound)
		}
	}
	if batchID == "" {
		return invalid("enter the bag code, or choose the fertilizer you received")
	}
	var farmerID *string
	var fid string
	if err := s.db.QueryRow(ctx, `SELECT i.farmer_id FROM input_issues i JOIN farmers f ON f.id=i.farmer_id WHERE i.batch_id=$1 AND f.phone=$2 LIMIT 1`,
		batchID, phone).Scan(&fid); err == nil {
		farmerID = &fid
	}
	// One report per phone per batch per day, so reports cannot be stuffed to damage a supplier.
	var recent bool
	_ = s.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM input_complaints WHERE batch_id=$1 AND phone=$2 AND created_at > now() - interval '1 day')`,
		batchID, phone).Scan(&recent)
	if recent {
		return fmt.Errorf("%w: you already reported this batch today; thank you", ErrConflict)
	}
	_, err := s.db.Exec(ctx, `INSERT INTO input_complaints(batch_id, farmer_id, phone, problem) VALUES ($1,$2,$3,$4)`, batchID, farmerID, phone, problem)
	return notFoundIf(err, "batch")
}

type Complaint struct {
	ID        int64     `json:"id"`
	BatchID   string    `json:"batch_id"`
	Product   string    `json:"product"`
	BatchNo   string    `json:"batch_no"`
	Farmer    string    `json:"farmer_name"`
	Phone     string    `json:"phone"`
	Problem   string    `json:"problem"`
	CreatedAt time.Time `json:"created_at"`
}

func (s *Service) Complaints(ctx context.Context, limit int) ([]Complaint, error) {
	rows, err := s.db.Query(ctx, `SELECT m.id, m.batch_id, b.product, b.batch_no, COALESCE(f.name,''), m.phone, m.problem, m.created_at
		FROM input_complaints m JOIN input_batches b ON b.id=m.batch_id LEFT JOIN farmers f ON f.id=m.farmer_id ORDER BY m.created_at DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Complaint{}
	for rows.Next() {
		var c Complaint
		if err := rows.Scan(&c.ID, &c.BatchID, &c.Product, &c.BatchNo, &c.Farmer, &c.Phone, &c.Problem, &c.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

type SuspiciousCheck struct {
	Result    string    `json:"result"`
	Product   string    `json:"product"`
	BatchNo   string    `json:"batch_no"`
	Phone     string    `json:"phone"`
	Channel   string    `json:"channel"`
	CreatedAt time.Time `json:"created_at"`
}

// SuspiciousChecks lists recent checks that found fakes, copies or recalled bags.
func (s *Service) SuspiciousChecks(ctx context.Context, limit int) ([]SuspiciousCheck, error) {
	rows, err := s.db.Query(ctx, `SELECT k.result, COALESCE(b.product,''), COALESCE(b.batch_no,''), k.phone, k.channel, k.created_at
		FROM code_checks k LEFT JOIN input_batches b ON b.id=k.batch_id WHERE k.result<>'genuine' ORDER BY k.created_at DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SuspiciousCheck{}
	for rows.Next() {
		var c SuspiciousCheck
		if err := rows.Scan(&c.Result, &c.Product, &c.BatchNo, &c.Phone, &c.Channel, &c.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ---------- does the fertilizer work? harvest comparison ----------

// BatchHarvest compares the harvest per acre of farms that used a batch with similar farms that did not
// (same cooperatives, same crop, deliveries in the 9 months after the fertilizer was given).
func (s *Service) BatchHarvest(ctx context.Context, batchID string) (risk.BatchVerdict, error) {
	b, err := s.GetBatch(ctx, batchID)
	if err != nil {
		return risk.BatchVerdict{}, err
	}
	var crop string
	_ = s.db.QueryRow(ctx, `SELECT d.crop FROM input_issues i JOIN deliveries d ON d.farmer_id=i.farmer_id AND d.created_at > i.created_at
		WHERE i.batch_id=$1 GROUP BY d.crop ORDER BY SUM(d.weight_kg) DESC LIMIT 1`, batchID).Scan(&crop)
	in := risk.BatchPerformance{Complaints: b.Complaints, LabResult: b.LabResult, BatchYields: []float64{}, OtherYields: []float64{}}
	if b.BagsChecked > 0 {
		in.RepeatShare = float64(b.BagsRepeat) / float64(b.BagsChecked)
	}
	if crop != "" {
		rows, err := s.db.Query(ctx, `
			WITH used AS (SELECT farmer_id, MIN(created_at) AS given FROM input_issues WHERE batch_id=$1 GROUP BY farmer_id),
			coops AS (SELECT DISTINCT coop_id FROM input_issues WHERE batch_id=$1)
			SELECT (u.farmer_id IS NOT NULL) AS used,
				SUM(d.weight_kg)::float8 / f.acres::float8 AS kg_per_acre
			FROM farmers f
			JOIN deliveries d ON d.farmer_id=f.id AND d.crop=$2 AND d.status<>'voided' AND d.created_at > now() - interval '12 months'
			LEFT JOIN used u ON u.farmer_id=f.id
			WHERE f.coop_id IN (SELECT coop_id FROM coops) AND f.status='active'
				AND (u.farmer_id IS NULL OR d.created_at > u.given)
			GROUP BY f.id, f.acres, u.farmer_id`, batchID, crop)
		if err != nil {
			return risk.BatchVerdict{}, err
		}
		for rows.Next() {
			var used bool
			var y float64
			if rows.Scan(&used, &y) == nil && !math.IsNaN(y) && !math.IsInf(y, 0) {
				if used {
					in.BatchYields = append(in.BatchYields, y)
				} else {
					in.OtherYields = append(in.OtherYields, y)
				}
			}
		}
		rows.Close()
	}
	return s.ai.ScoreBatch(ctx, in), nil
}

// CoopBatches lists active batches shipped to a cooperative (what its store can hand out).
func (s *Service) CoopBatches(ctx context.Context, coopID string) ([]Batch, error) {
	rows, err := s.db.Query(ctx, `SELECT `+batchCols+batchFrom+` WHERE b.status='active'
		AND EXISTS(SELECT 1 FROM shipments sh WHERE sh.batch_id=b.id AND sh.to_coop_id=$1) ORDER BY b.product, b.batch_no`, coopID)
	if err != nil {
		return nil, notFoundIf(err, "cooperative")
	}
	defer rows.Close()
	out := []Batch{}
	for rows.Next() {
		b, err := scanBatch(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}
