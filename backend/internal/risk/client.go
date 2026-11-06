// Package risk asks the AgriProof AI service for checks. Every call has a safe fallback: if the
// AI is unreachable, nothing is waved through — risky items are held for a person instead.
package risk

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

type Client struct {
	baseURL string
	http    *http.Client
}

func NewClient(baseURL string) *Client {
	return &Client{baseURL: baseURL, http: &http.Client{Timeout: 4 * time.Second}}
}

// Result is the AI's answer: a score from 0 to 1, a level, and plain-language reasons.
type Result struct {
	Score   float64  `json:"score"`
	Level   string   `json:"level"` // low | medium | high
	Reasons []string `json:"reasons"`
	Source  string   `json:"source"` // ai | fallback
}

func (c *Client) post(ctx context.Context, path string, in any, out any) error {
	body, _ := json.Marshal(in)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ai service status %d", resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func valid(r Result) bool { return r.Level == "low" || r.Level == "medium" || r.Level == "high" }

func ensure(r Result) Result {
	if r.Reasons == nil {
		r.Reasons = []string{}
	}
	return r
}

// DeliveryFeatures describe one delivery at a collection centre.
type DeliveryFeatures struct {
	Crop                string  `json:"crop"`
	WeightKg            float64 `json:"weight_kg"`
	Grade               string  `json:"grade"`
	Moisture            float64 `json:"moisture"`
	HasMoisture         bool    `json:"has_moisture"`
	Acres               float64 `json:"acres"`
	MaxKgPerAcre        float64 `json:"max_kg_per_acre"`
	SeasonKg            float64 `json:"season_kg"` // this farmer, last 9 months, including this delivery
	FarmerDeliveriesDay int     `json:"farmer_deliveries_today"`
	MinutesSinceLast    float64 `json:"minutes_since_last"` // -1 if none
	SameWeightLastHour  bool    `json:"same_weight_last_hour"`
	ClerkDeliveries30d  int     `json:"clerk_deliveries_30d"`
	ClerkRoundShare     float64 `json:"clerk_round_share"`   // share of the clerk's weights that are whole multiples of 5
	ClerkAvgVsCentre    float64 `json:"clerk_avg_vs_centre"` // clerk's average weight / centre average (same crop, 30 days)
	HourOfDay           int     `json:"hour_of_day"`
}

func (c *Client) ScoreDelivery(ctx context.Context, f DeliveryFeatures) Result {
	var r Result
	if err := c.post(ctx, "/score-delivery", f, &r); err == nil && valid(r) {
		r.Source = "ai"
		return ensure(r)
	}
	return deliveryFallback(f)
}

// FallbackHoldKg: while the AI engine is down, deliveries above this weight are held for a manager.
var FallbackHoldKg = 300.0

func deliveryFallback(f DeliveryFeatures) Result {
	// Without the AI, only small, ordinary deliveries go through; anything bigger waits for a manager.
	r := Result{Level: "medium", Score: 0.5, Source: "fallback", Reasons: []string{"AI engine unavailable: basic checks only"}}
	if f.Acres > 0 && f.MaxKgPerAcre > 0 && f.SeasonKg > f.Acres*f.MaxKgPerAcre {
		r.Level, r.Score = "high", 0.8
		r.Reasons = append(r.Reasons, "this farm has delivered more this season than its size can grow")
	}
	if f.WeightKg > FallbackHoldKg {
		r.Level, r.Score = "high", 0.7
		r.Reasons = append(r.Reasons, "large delivery recorded while the AI check was down: a manager must confirm it")
	}
	if f.SameWeightLastHour || f.FarmerDeliveriesDay > 3 {
		r.Level, r.Score = "high", 0.7
		r.Reasons = append(r.Reasons, "repeated deliveries recorded while the AI check was down: a manager must confirm them")
	}
	return r
}

// FarmerFeatures describe a new farmer registration (to catch ghost farmers).
type FarmerFeatures struct {
	Name                 string  `json:"name"`
	Acres                float64 `json:"acres"`
	PhoneUsedByOthers    int     `json:"phone_used_by_others"`
	SimilarNames         int     `json:"similar_names"`
	MostSimilarName      string  `json:"most_similar_name"`
	ClerkRegistrations1h int     `json:"clerk_registrations_1h"`
	SameLocationFarmers  int     `json:"same_location_farmers"`
	CoopMedianAcres      float64 `json:"coop_median_acres"`
}

func (c *Client) ScoreFarmer(ctx context.Context, f FarmerFeatures) Result {
	var r Result
	if err := c.post(ctx, "/score-farmer", f, &r); err == nil && valid(r) {
		r.Source = "ai"
		return ensure(r)
	}
	return Result{Level: "high", Score: 1, Source: "fallback",
		Reasons: []string{"AI engine unavailable: new farmer held for a manager to confirm"}}
}

// CodeCheckFeatures describe a fertilizer bag code being checked.
type CodeCheckFeatures struct {
	Result           string  `json:"result"` // genuine | repeat | not_found | recalled
	DistinctPhones   int     `json:"distinct_phones"`
	TotalChecks      int     `json:"total_checks"`
	HoursSinceFirst  float64 `json:"hours_since_first"`
	PhoneNotFound24h int     `json:"phone_not_found_24h"`
	BatchRepeatShare float64 `json:"batch_repeat_share"` // share of this batch's bags checked by 2+ phones
	BatchBagsChecked int     `json:"batch_bags_checked"`
	BatchComplaints  int     `json:"batch_complaints"`
	BatchLabResult   string  `json:"batch_lab_result"`
}

func (c *Client) ScoreCodeCheck(ctx context.Context, f CodeCheckFeatures) Result {
	var r Result
	if err := c.post(ctx, "/score-code-check", f, &r); err == nil && valid(r) {
		r.Source = "ai"
		return ensure(r)
	}
	r = Result{Level: "low", Source: "fallback", Reasons: []string{}}
	if f.DistinctPhones >= 2 {
		r.Level, r.Score = "high", 0.7
		r.Reasons = append(r.Reasons, "this code has been checked from more than one phone: the bag may be a copy")
	}
	return r
}

// BatchPerformance compares harvests of farms that used a batch with similar farms that did not.
type BatchPerformance struct {
	BatchYields  []float64 `json:"batch_yields"` // kg per acre, farms that used the batch
	OtherYields  []float64 `json:"other_yields"` // kg per acre, same crop and season, other farms
	Complaints   int       `json:"complaints"`
	LabResult    string    `json:"lab_result"`
	RepeatShare  float64   `json:"repeat_share"`
	NotFoundNear int       `json:"not_found_checks"` // fake codes reported for this product recently
}

type BatchVerdict struct {
	Result
	BatchMedian float64 `json:"batch_median"`
	OtherMedian float64 `json:"other_median"`
	Difference  float64 `json:"difference_pct"` // negative = farms using the batch harvested less
	Enough      bool    `json:"enough_data"`
}

func (c *Client) ScoreBatch(ctx context.Context, f BatchPerformance) BatchVerdict {
	var v BatchVerdict
	if err := c.post(ctx, "/score-batch", f, &v); err == nil && valid(v.Result) {
		v.Source = "ai"
		v.Result = ensure(v.Result)
		return v
	}
	return BatchVerdict{Result: Result{Level: "low", Source: "fallback", Reasons: []string{"AI engine unavailable: harvest comparison not run"}}}
}

// Healthy reports whether the AI service answers its health check.
func (c *Client) Healthy(ctx context.Context) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/health", nil)
	if err != nil {
		return false
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}
