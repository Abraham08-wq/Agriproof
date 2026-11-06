package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/agriproof/backend/internal/mpesa"
	"github.com/agriproof/backend/internal/risk"
)

// ---------- cooperatives, centres, staff, prices (set up from the console) ----------

type Coop struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	County    string    `json:"county"`
	Currency  string    `json:"currency"`
	CreatedAt time.Time `json:"created_at"`
	Farmers   int64     `json:"farmers"`
	Staff     int64     `json:"staff"`
	KgSeason  float64   `json:"kg_season"`
	Owed      int64     `json:"owed"`
	Flagged   int64     `json:"flagged"`
}

func (s *Service) CreateCoop(ctx context.Context, name, county string) (Coop, error) {
	name, county = cleanText(name, 120), cleanText(county, 60)
	if len(name) < 2 {
		return Coop{}, invalid("enter the cooperative's name")
	}
	var c Coop
	err := s.db.QueryRow(ctx, `INSERT INTO cooperatives(name, county) VALUES ($1,$2) RETURNING id, name, county, currency, created_at`,
		name, county).Scan(&c.ID, &c.Name, &c.County, &c.Currency, &c.CreatedAt)
	return c, err
}

func (s *Service) ListCoops(ctx context.Context) ([]Coop, error) {
	rows, err := s.db.Query(ctx, `SELECT c.id, c.name, c.county, c.currency, c.created_at,
		(SELECT COUNT(*) FROM farmers f WHERE f.coop_id=c.id AND f.status='active'),
		(SELECT COUNT(*) FROM staff t WHERE t.coop_id=c.id AND t.active),
		(SELECT COALESCE(SUM(weight_kg),0)::float8 FROM deliveries d WHERE d.coop_id=c.id AND d.status<>'voided' AND d.created_at > now() - interval '9 months'),
		(SELECT COALESCE(SUM(amount),0) FROM deliveries d WHERE d.coop_id=c.id AND d.status='recorded' AND d.payrun_id IS NULL),
		(SELECT COUNT(*) FROM deliveries d WHERE d.coop_id=c.id AND d.status='held')
		FROM cooperatives c ORDER BY c.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Coop{}
	for rows.Next() {
		var c Coop
		if err := rows.Scan(&c.ID, &c.Name, &c.County, &c.Currency, &c.CreatedAt, &c.Farmers, &c.Staff, &c.KgSeason, &c.Owed, &c.Flagged); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Service) GetCoop(ctx context.Context, id string) (Coop, error) {
	list, err := s.ListCoops(ctx)
	if err != nil {
		return Coop{}, err
	}
	for _, c := range list {
		if c.ID == id {
			return c, nil
		}
	}
	return Coop{}, fmt.Errorf("%w: cooperative", ErrNotFound)
}

type Centre struct {
	ID       string `json:"id"`
	CoopID   string `json:"coop_id"`
	Name     string `json:"name"`
	Location string `json:"location"`
}

func (s *Service) AddCentre(ctx context.Context, coopID, name, location string) (Centre, error) {
	name, location = cleanText(name, 120), cleanText(location, 120)
	if len(name) < 2 {
		return Centre{}, invalid("enter the collection centre's name")
	}
	c := Centre{CoopID: coopID, Name: name, Location: location}
	err := s.db.QueryRow(ctx, `INSERT INTO centres(coop_id, name, location) VALUES ($1,$2,$3) RETURNING id`, coopID, name, location).Scan(&c.ID)
	if isUniqueViolation(err) {
		return c, fmt.Errorf("%w: a centre with that name already exists", ErrConflict)
	}
	return c, notFoundIf(err, "cooperative")
}

func (s *Service) Centres(ctx context.Context, coopID string) ([]Centre, error) {
	rows, err := s.db.Query(ctx, `SELECT id, coop_id, name, location FROM centres WHERE coop_id=$1 ORDER BY name`, coopID)
	if err != nil {
		return nil, notFoundIf(err, "cooperative")
	}
	defer rows.Close()
	out := []Centre{}
	for rows.Next() {
		var c Centre
		if err := rows.Scan(&c.ID, &c.CoopID, &c.Name, &c.Location); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

type Staff struct {
	ID         string `json:"id"`
	CoopID     string `json:"coop_id"`
	CoopName   string `json:"coop_name"`
	CentreID   string `json:"centre_id"`
	CentreName string `json:"centre_name"`
	Name       string `json:"name"`
	Phone      string `json:"phone"`
	Role       string `json:"role"`
	Active     bool   `json:"active"`
}

func (s *Service) AddStaff(ctx context.Context, coopID, centreID, name, phone, role string) (Staff, error) {
	name = cleanText(name, 120)
	if len(name) < 2 {
		return Staff{}, invalid("enter the person's name")
	}
	p, err := mpesa.NormalizePhone(phone)
	if err != nil {
		return Staff{}, invalid("enter a valid phone number")
	}
	if role != "manager" && role != "clerk" {
		return Staff{}, invalid("role must be manager or clerk")
	}
	var centre *string
	if centreID != "" {
		var ok bool
		if err := s.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM centres WHERE id=$1 AND coop_id=$2)`, centreID, coopID).Scan(&ok); err != nil || !ok {
			return Staff{}, invalid("that centre does not belong to this cooperative")
		}
		centre = &centreID
	}
	st := Staff{CoopID: coopID, Name: name, Phone: p, Role: role, Active: true}
	err = s.db.QueryRow(ctx, `INSERT INTO staff(coop_id, centre_id, name, phone, role) VALUES ($1,$2,$3,$4,$5) RETURNING id`,
		coopID, centre, name, p, role).Scan(&st.ID)
	if isUniqueViolation(err) {
		return st, fmt.Errorf("%w: this phone number is already staff here", ErrConflict)
	}
	return st, notFoundIf(err, "cooperative")
}

func (s *Service) SetStaffActive(ctx context.Context, staffID string, active bool) error {
	tag, err := s.db.Exec(ctx, `UPDATE staff SET active=$2 WHERE id=$1`, staffID, active)
	if err != nil {
		return notFoundIf(err, "staff")
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: staff", ErrNotFound)
	}
	return nil
}

const staffCols = `t.id, t.coop_id, c.name, COALESCE(t.centre_id::text,''), COALESCE(ce.name,''), t.name, t.phone, t.role, t.active`
const staffFrom = ` FROM staff t JOIN cooperatives c ON c.id=t.coop_id LEFT JOIN centres ce ON ce.id=t.centre_id`

func scanStaff(row pgx.Row) (Staff, error) {
	var st Staff
	err := row.Scan(&st.ID, &st.CoopID, &st.CoopName, &st.CentreID, &st.CentreName, &st.Name, &st.Phone, &st.Role, &st.Active)
	return st, err
}

func (s *Service) StaffList(ctx context.Context, coopID string) ([]Staff, error) {
	rows, err := s.db.Query(ctx, `SELECT `+staffCols+staffFrom+` WHERE t.coop_id=$1 ORDER BY t.active DESC, t.role, t.name`, coopID)
	if err != nil {
		return nil, notFoundIf(err, "cooperative")
	}
	defer rows.Close()
	out := []Staff{}
	for rows.Next() {
		st, err := scanStaff(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// StaffIn returns the signed-in phone's active staff record in a cooperative, or ErrForbidden.
func (s *Service) StaffIn(ctx context.Context, phone, coopID string) (Staff, error) {
	st, err := scanStaff(s.db.QueryRow(ctx, `SELECT `+staffCols+staffFrom+` WHERE t.phone=$1 AND t.coop_id=$2 AND t.active`, phone, coopID))
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return st, fmt.Errorf("%w: you are not staff of this cooperative", ErrForbidden)
	}
	return st, err
}

type Price struct {
	Crop       string    `json:"crop"`
	Grade      string    `json:"grade"`
	PricePerKg float64   `json:"price_per_kg"`
	SetBy      string    `json:"set_by"`
	From       time.Time `json:"effective_from"`
}

func (s *Service) SetPrice(ctx context.Context, coopID, crop, grade string, price float64, by string) error {
	if grade != "1" && grade != "2" {
		return invalid("grade must be 1 or 2")
	}
	if price <= 0 || price > 100000 {
		return invalid("enter a price per kg greater than zero")
	}
	_, err := s.db.Exec(ctx, `INSERT INTO prices(coop_id, crop, grade, price_per_kg, set_by) VALUES ($1,$2,$3,$4,$5)`, coopID, crop, grade, price, by)
	if err != nil && strings.Contains(err.Error(), "crops") {
		return invalid("unknown crop")
	}
	return notFoundIf(err, "cooperative")
}

// Prices lists the current price for each crop and grade.
func (s *Service) Prices(ctx context.Context, coopID string) ([]Price, error) {
	rows, err := s.db.Query(ctx, `SELECT DISTINCT ON (crop, grade) crop, grade, price_per_kg::float8, set_by, effective_from
		FROM prices WHERE coop_id=$1 ORDER BY crop, grade, effective_from DESC`, coopID)
	if err != nil {
		return nil, notFoundIf(err, "cooperative")
	}
	defer rows.Close()
	out := []Price{}
	for rows.Next() {
		var p Price
		if err := rows.Scan(&p.Crop, &p.Grade, &p.PricePerKg, &p.SetBy, &p.From); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

type Crop struct {
	Code         string `json:"code"`
	Name         string `json:"name"`
	MaxKgPerAcre int    `json:"max_kg_per_acre"`
}

func (s *Service) Crops(ctx context.Context) ([]Crop, error) {
	rows, err := s.db.Query(ctx, `SELECT code, name, max_kg_per_acre FROM crops ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Crop{}
	for rows.Next() {
		var c Crop
		if err := rows.Scan(&c.Code, &c.Name, &c.MaxKgPerAcre); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ---------- farmers ----------

type Farmer struct {
	ID        string          `json:"id"`
	CoopID    string          `json:"coop_id"`
	CoopName  string          `json:"coop_name"`
	MemberNo  int             `json:"member_no"`
	Name      string          `json:"name"`
	Phone     string          `json:"phone"`
	Village   string          `json:"village"`
	Acres     float64         `json:"acres"`
	Lat       *float64        `json:"lat,omitempty"`
	Lng       *float64        `json:"lng,omitempty"`
	Status    string          `json:"status"`
	AILevel   string          `json:"ai_level"`
	AIReasons json.RawMessage `json:"ai_reasons"`
	DecidedBy string          `json:"decided_by"`
	CreatedAt time.Time       `json:"created_at"`
	SeasonKg  float64         `json:"season_kg"`
	Owed      int64           `json:"owed"`
}

const farmerCols = `f.id, f.coop_id, c.name, f.member_no, f.name, f.phone, f.village, f.acres::float8, f.lat, f.lng, f.status,
	f.ai_level, f.ai_reasons, f.decided_by, f.created_at,
	(SELECT COALESCE(SUM(weight_kg),0)::float8 FROM deliveries d WHERE d.farmer_id=f.id AND d.status<>'voided' AND d.created_at > now() - interval '9 months'),
	(SELECT COALESCE(SUM(amount),0) FROM deliveries d WHERE d.farmer_id=f.id AND d.status='recorded' AND d.payrun_id IS NULL)`
const farmerFrom = ` FROM farmers f JOIN cooperatives c ON c.id=f.coop_id`

func scanFarmer(row pgx.Row) (Farmer, error) {
	var f Farmer
	err := row.Scan(&f.ID, &f.CoopID, &f.CoopName, &f.MemberNo, &f.Name, &f.Phone, &f.Village, &f.Acres, &f.Lat, &f.Lng, &f.Status,
		&f.AILevel, &f.AIReasons, &f.DecidedBy, &f.CreatedAt, &f.SeasonKg, &f.Owed)
	return f, err
}

type FarmerInput struct {
	Name    string   `json:"name"`
	Phone   string   `json:"phone"`
	Village string   `json:"village"`
	Acres   float64  `json:"acres"`
	Lat     *float64 `json:"lat"`
	Lng     *float64 `json:"lng"`
}

// RegisterFarmer adds a farmer. The AI checks for ghost farmers (shared phones, near-duplicate names,
// a clerk registering in bulk); doubtful registrations are held until a manager confirms.
func (s *Service) RegisterFarmer(ctx context.Context, by Staff, in FarmerInput) (Farmer, risk.Result, error) {
	name := cleanText(in.Name, 120)
	if len(strings.Fields(name)) < 2 {
		return Farmer{}, risk.Result{}, invalid("enter the farmer's first and last name")
	}
	p, err := mpesa.NormalizePhone(in.Phone)
	if err != nil {
		return Farmer{}, risk.Result{}, invalid("enter the farmer's M-Pesa phone number")
	}
	if in.Acres <= 0 || in.Acres > 1000 {
		return Farmer{}, risk.Result{}, invalid("enter the farm size in acres (for example 1.5)")
	}
	if (in.Lat == nil) != (in.Lng == nil) || (in.Lat != nil && (*in.Lat < -90 || *in.Lat > 90 || *in.Lng < -180 || *in.Lng > 180)) {
		return Farmer{}, risk.Result{}, invalid("farm location is not valid")
	}
	f := risk.FarmerFeatures{Name: name, Acres: in.Acres}
	_ = s.db.QueryRow(ctx, `SELECT (SELECT COUNT(*) FROM farmers WHERE phone=$1) + (SELECT COUNT(*) FROM staff WHERE phone=$1 AND coop_id=$2)`,
		p, by.CoopID).Scan(&f.PhoneUsedByOthers)
	_ = s.db.QueryRow(ctx, `SELECT COUNT(*) FROM farmers WHERE registered_by=$1 AND created_at > now() - interval '1 hour'`, by.ID).Scan(&f.ClerkRegistrations1h)
	_ = s.db.QueryRow(ctx, `SELECT COALESCE(percentile_cont(0.5) WITHIN GROUP (ORDER BY acres),0)::float8 FROM farmers WHERE coop_id=$1`, by.CoopID).Scan(&f.CoopMedianAcres)
	if in.Lat != nil {
		_ = s.db.QueryRow(ctx, `SELECT COUNT(*) FROM farmers WHERE coop_id=$1 AND abs(lat-$2) < 0.0003 AND abs(lng-$3) < 0.0003`,
			by.CoopID, *in.Lat, *in.Lng).Scan(&f.SameLocationFarmers)
	}
	f.SimilarNames, f.MostSimilarName = s.similarNames(ctx, by.CoopID, name)
	verdict := s.ai.ScoreFarmer(ctx, f)
	status := "active"
	if verdict.Level == "high" {
		status = "held"
	}
	reasons, _ := json.Marshal(verdict.Reasons)

	tx, err := s.db.Begin(ctx)
	if err != nil {
		return Farmer{}, verdict, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err := lockCoop(ctx, tx, by.CoopID); err != nil {
		return Farmer{}, verdict, err
	}
	var id string
	err = tx.QueryRow(ctx, `INSERT INTO farmers(coop_id, member_no, name, phone, village, acres, lat, lng, status, ai_level, ai_reasons, registered_by)
		VALUES ($1, (SELECT COALESCE(MAX(member_no),0)+1 FROM farmers WHERE coop_id=$1), $2,$3,$4,$5,$6,$7,$8,$9,$10,$11) RETURNING id`,
		by.CoopID, name, p, cleanText(in.Village, 80), in.Acres, in.Lat, in.Lng, status, verdict.Level, reasons, by.ID).Scan(&id)
	if isUniqueViolation(err) {
		return Farmer{}, verdict, fmt.Errorf("%w: a farmer with this phone number is already registered here", ErrConflict)
	}
	if err != nil {
		return Farmer{}, verdict, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Farmer{}, verdict, err
	}
	out, err := s.GetFarmer(ctx, id)
	if err == nil && status == "active" {
		s.notify([]string{p}, fmt.Sprintf("Welcome to %s on AgriProof, member no. %d. Every delivery you make will be texted to you and sealed so nobody can change it.", out.CoopName, out.MemberNo))
	}
	return out, verdict, err
}

func (s *Service) GetFarmer(ctx context.Context, id string) (Farmer, error) {
	f, err := scanFarmer(s.db.QueryRow(ctx, `SELECT `+farmerCols+farmerFrom+` WHERE f.id=$1`, id))
	return f, notFoundIf(err, "farmer")
}

// FindFarmers searches a cooperative's farmers by member number, phone or name.
func (s *Service) FindFarmers(ctx context.Context, coopID, q, status string, limit int) ([]Farmer, error) {
	q = strings.TrimSpace(q)
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	phone := ""
	if p, err := mpesa.NormalizePhone(q); err == nil {
		phone = p
	}
	rows, err := s.db.Query(ctx, `SELECT `+farmerCols+farmerFrom+` WHERE f.coop_id=$1 AND ($2='' OR f.status=$2)
		AND ($3='' OR f.member_no::text=$3 OR f.phone=$4 OR f.name ILIKE '%' || $3 || '%')
		ORDER BY (f.status='held') DESC, f.member_no DESC LIMIT $5`, coopID, status, q, phone, limit)
	if err != nil {
		return nil, notFoundIf(err, "cooperative")
	}
	defer rows.Close()
	out := []Farmer{}
	for rows.Next() {
		f, err := scanFarmer(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// DecideFarmer confirms or rejects a held registration. A clerk cannot confirm a farmer they registered.
func (s *Service) DecideFarmer(ctx context.Context, farmerID string, approve bool, decidedBy, deciderStaffID string) (Farmer, error) {
	f, err := s.GetFarmer(ctx, farmerID)
	if err != nil {
		return f, err
	}
	if f.Status != "held" {
		return f, fmt.Errorf("%w: this farmer is already %s", ErrConflict, f.Status)
	}
	if deciderStaffID != "" {
		var registeredBy *string
		_ = s.db.QueryRow(ctx, `SELECT registered_by FROM farmers WHERE id=$1`, farmerID).Scan(&registeredBy)
		if registeredBy != nil && *registeredBy == deciderStaffID {
			return f, fmt.Errorf("%w: someone other than the person who registered this farmer must confirm", ErrForbidden)
		}
	}
	status := "rejected"
	if approve {
		status = "active"
	}
	if _, err := s.db.Exec(ctx, `UPDATE farmers SET status=$2, decided_by=$3 WHERE id=$1 AND status='held'`, farmerID, status, decidedBy); err != nil {
		return f, err
	}
	if approve {
		s.notify([]string{f.Phone}, fmt.Sprintf("Welcome to %s on AgriProof, member no. %d. Every delivery you make will be texted to you.", f.CoopName, f.MemberNo))
	}
	return s.GetFarmer(ctx, farmerID)
}

// similarNames counts farmers with a near-identical name in the cooperative (word order ignored).
func (s *Service) similarNames(ctx context.Context, coopID, name string) (int, string) {
	rows, err := s.db.Query(ctx, `SELECT name FROM farmers WHERE coop_id=$1 AND status<>'rejected'`, coopID)
	if err != nil {
		return 0, ""
	}
	defer rows.Close()
	target := nameKey(name)
	n, best, bestScore := 0, "", 0.0
	for rows.Next() {
		var other string
		if rows.Scan(&other) != nil {
			continue
		}
		score := similarity(target, nameKey(other))
		if score >= 0.88 {
			n++
		}
		if score > bestScore {
			best, bestScore = other, score
		}
	}
	if bestScore < 0.88 {
		best = ""
	}
	return n, best
}

func nameKey(n string) string {
	w := strings.Fields(strings.ToLower(n))
	sort.Strings(w)
	return strings.Join(w, " ")
}

// similarity is 1 - (edit distance / longer length).
func similarity(a, b string) float64 {
	ra, rb := []rune(a), []rune(b)
	if len(ra) == 0 && len(rb) == 0 {
		return 1
	}
	prev := make([]int, len(rb)+1)
	cur := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	longer := max(len(ra), len(rb))
	return 1 - float64(prev[len(rb)])/float64(longer)
}

// ---------- who is signed in ----------

type FarmerRef struct {
	FarmerID string `json:"farmer_id"`
	CoopID   string `json:"coop_id"`
	CoopName string `json:"coop_name"`
	Name     string `json:"name"`
	MemberNo int    `json:"member_no"`
	Status   string `json:"status"`
}

type Me struct {
	Phone        string      `json:"phone"`
	Name         string      `json:"name"`
	Staff        []Staff     `json:"staff"`
	Farmer       []FarmerRef `json:"farmer"`
	PaymentsMode string      `json:"payments_mode"`
}

func (s *Service) WhoAmI(ctx context.Context, phone string) (Me, error) {
	me := Me{Phone: phone, Staff: []Staff{}, Farmer: []FarmerRef{}, PaymentsMode: s.PaymentsMode()}
	rows, err := s.db.Query(ctx, `SELECT `+staffCols+staffFrom+` WHERE t.phone=$1 AND t.active ORDER BY c.name`, phone)
	if err != nil {
		return me, err
	}
	for rows.Next() {
		st, err := scanStaff(rows)
		if err != nil {
			rows.Close()
			return me, err
		}
		me.Staff = append(me.Staff, st)
		if me.Name == "" {
			me.Name = st.Name
		}
	}
	rows.Close()
	rows, err = s.db.Query(ctx, `SELECT f.id, f.coop_id, c.name, f.name, f.member_no, f.status FROM farmers f JOIN cooperatives c ON c.id=f.coop_id
		WHERE f.phone=$1 AND f.status IN ('active','held') ORDER BY c.name`, phone)
	if err != nil {
		return me, err
	}
	defer rows.Close()
	for rows.Next() {
		var r FarmerRef
		if err := rows.Scan(&r.FarmerID, &r.CoopID, &r.CoopName, &r.Name, &r.MemberNo, &r.Status); err != nil {
			return me, err
		}
		me.Farmer = append(me.Farmer, r)
		if me.Name == "" {
			me.Name = r.Name
		}
	}
	return me, rows.Err()
}

// FarmerFor returns the signed-in phone's farmer record in a cooperative.
func (s *Service) FarmerFor(ctx context.Context, phone, coopID string) (Farmer, error) {
	f, err := scanFarmer(s.db.QueryRow(ctx, `SELECT `+farmerCols+farmerFrom+` WHERE f.phone=$1 AND f.coop_id=$2 AND f.status IN ('active','held')`, phone, coopID))
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return f, fmt.Errorf("%w: you are not a farmer of this cooperative", ErrForbidden)
	}
	return f, err
}

// HeldFarmers lists registrations the AI held, across cooperatives (console).
func (s *Service) HeldFarmers(ctx context.Context) ([]Farmer, error) {
	rows, err := s.db.Query(ctx, `SELECT `+farmerCols+farmerFrom+` WHERE f.status='held' ORDER BY f.created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Farmer{}
	for rows.Next() {
		f, err := scanFarmer(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}
