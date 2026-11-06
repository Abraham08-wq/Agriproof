"""AgriProof field checks.

Each check returns a score from 0 to 1, a level (low / medium / high) and plain-language reasons
that a cooperative manager can act on. Scores add up from independent signals, so every point of
risk can be explained. "high" means a person must look before money moves.
"""

from __future__ import annotations

import statistics
from dataclasses import dataclass, field


@dataclass
class Verdict:
    score: float = 0.0
    reasons: list[str] = field(default_factory=list)

    def add(self, points: float, why: str) -> None:
        self.score += points
        self.reasons.append(why)

    def level(self, high: float = 0.6, medium: float = 0.3) -> str:
        s = min(self.score, 1.0)
        return "high" if s >= high else "medium" if s >= medium else "low"

    def out(self) -> dict:
        return {"score": round(min(self.score, 1.0), 3), "level": self.level(), "reasons": self.reasons}


def kg(v: float) -> str:
    return f"{v:,.0f} kg" if v >= 10 else f"{v:.1f} kg"


# ---------- 1. deliveries at the collection centre ----------

def score_delivery(f: dict) -> dict:
    v = Verdict()
    acres = max(float(f.get("acres") or 0), 0.0)
    cap = float(f.get("max_kg_per_acre") or 0)
    season = float(f.get("season_kg") or 0)
    weight = float(f.get("weight_kg") or 0)
    crop = f.get("crop") or "produce"

    # Produce brought in from elsewhere (side-selling, or a broker using a member's name)
    if acres > 0 and cap > 0:
        possible = acres * cap
        if season > possible:
            v.add(0.7, f"this {acres:g}-acre farm has delivered {kg(season)} of {crop} this season, more than it can grow "
                       f"(about {kg(possible)} at most): the produce may come from somewhere else")
        elif season > 0.8 * possible:
            v.add(0.3, f"this farm has already delivered {kg(season)} this season, close to the most its {acres:g} acres can grow")
        if weight > 0.6 * possible:
            v.add(0.3, f"one delivery of {kg(weight)} is a very large share of what a {acres:g}-acre farm can grow in a season")

    # Double recording
    if f.get("same_weight_last_hour"):
        v.add(0.45, f"the same farmer already delivered exactly {kg(weight)} within the last hour: possibly recorded twice")
    mins = float(f.get("minutes_since_last") if f.get("minutes_since_last") is not None else -1)
    if 0 <= mins < 10:
        v.add(0.2, f"another delivery from this farmer was recorded {mins:.0f} minutes ago")
    if int(f.get("farmer_deliveries_today") or 0) >= 4:
        v.add(0.2, f"{f.get('farmer_deliveries_today')} deliveries from this farmer today")

    # Weighing fraud by the clerk
    n = int(f.get("clerk_deliveries_30d") or 0)
    if n >= 20:
        rs = float(f.get("clerk_round_share") or 0)
        if rs >= 0.6:
            v.add(0.3, f"{rs:.0%} of this clerk's weights are round numbers (multiples of 5 kg): weights may be guessed, not read from the scale")
        ratio = float(f.get("clerk_avg_vs_centre") or 1)
        if ratio < 0.85:
            v.add(0.3, f"this clerk's deliveries average {1 - ratio:.0%} lighter than other clerks' at the same centre: check the scale and the clerk")
        elif ratio > 1.25:
            v.add(0.2, f"this clerk's deliveries average {ratio - 1:.0%} heavier than other clerks' at the same centre")

    # Grade and moisture (maize should be dry, about 13.5% or less, to store safely)
    if f.get("has_moisture"):
        m = float(f.get("moisture") or 0)
        if crop == "maize" and f.get("grade") == "1" and m > 14.0:
            v.add(0.3, f"graded 1 at {m:.1f}% moisture; grade 1 maize should be dry (about 13.5% or less)")
        if m > 25:
            v.add(0.15, f"moisture of {m:.1f}% is unusually high")

    hour = int(f.get("hour_of_day") or 12)
    if hour < 5 or hour >= 21:
        v.add(0.1, "recorded outside normal collection hours")
    return v.out()


# ---------- 2. ghost farmers at registration ----------

def score_farmer(f: dict) -> dict:
    v = Verdict()
    shared = int(f.get("phone_used_by_others") or 0)
    if shared >= 2:
        v.add(0.6, f"this phone number is already used by {shared} other registered people: payments could all go to one person")
    elif shared == 1:
        v.add(0.35, "this phone number is already registered to someone else")
    similar = int(f.get("similar_names") or 0)
    if similar >= 1:
        who = f.get("most_similar_name") or "an existing member"
        v.add(0.5 if similar == 1 else 0.6, f"the name is almost the same as {who}: possibly a duplicate or ghost member")
    burst = int(f.get("clerk_registrations_1h") or 0)
    if burst >= 25:
        v.add(0.45, f"the same clerk registered {burst} farmers in the last hour")
    elif burst >= 12:
        v.add(0.2, f"the same clerk registered {burst} farmers in the last hour")
    same_loc = int(f.get("same_location_farmers") or 0)
    if same_loc >= 3:
        v.add(0.35, f"{same_loc} farmers are registered at exactly the same spot on the map")
    acres = float(f.get("acres") or 0)
    median = float(f.get("coop_median_acres") or 0)
    if median > 0 and acres > 10 * median and acres > 20:
        v.add(0.3, f"{acres:g} acres is far larger than this cooperative's typical farm ({median:g} acres): payments would be inflated")
    return v.out()


# ---------- 3. fertilizer bag code checks ----------

def score_code_check(f: dict) -> dict:
    v = Verdict()
    result = f.get("result")
    if result == "not_found":
        v.add(0.6, "this code is not on any registered bag")
        if int(f.get("phone_not_found_24h") or 0) >= 3:
            v.add(0.2, "several unknown codes were checked from this phone today: fake bags may be on sale nearby")
    if result == "recalled":
        v.add(0.8, "this batch has been recalled")
    phones = int(f.get("distinct_phones") or 0)
    if phones >= 3:
        v.add(0.6, f"this bag's code has been checked by {phones} different phones: the label has probably been copied onto other bags")
    elif phones == 2:
        v.add(0.4, "this bag's code was checked by two different phones: the label may have been copied")
    if int(f.get("batch_bags_checked") or 0) >= 10 and float(f.get("batch_repeat_share") or 0) >= 0.2:
        v.add(0.25, f"{float(f['batch_repeat_share']):.0%} of this batch's checked bags show copied codes: copies of this batch may be in the market")
    if int(f.get("batch_complaints") or 0) >= 3:
        v.add(0.2, f"{f['batch_complaints']} farmers have reported problems with this batch")
    if f.get("batch_lab_result") == "fail":
        v.add(0.3, "a lab test of this batch failed")
    return v.out()


# ---------- 4. does the fertilizer work? ----------

MIN_FARMS = 6


def score_batch(f: dict) -> dict:
    v = Verdict()
    batch = [float(x) for x in (f.get("batch_yields") or []) if x and x > 0]
    other = [float(x) for x in (f.get("other_yields") or []) if x and x > 0]
    out = {"batch_median": 0.0, "other_median": 0.0, "difference_pct": 0.0, "enough_data": False}
    if len(batch) >= MIN_FARMS and len(other) >= MIN_FARMS:
        bm, om = statistics.median(batch), statistics.median(other)
        out.update(batch_median=round(bm, 1), other_median=round(om, 1), enough_data=True)
        if om > 0:
            diff = (bm - om) / om
            out["difference_pct"] = round(diff * 100, 1)
            # How unusual is the batch's median among other farms? (robust spread: median absolute deviation)
            mad = statistics.median([abs(x - om) for x in other]) or (0.1 * om)
            z = (bm - om) / (1.4826 * mad)
            if diff <= -0.30 and z <= -1.0:
                v.add(0.6, f"farms that used this batch harvested {abs(diff):.0%} less per acre than similar farms "
                           f"({bm:,.0f} vs {om:,.0f} kg per acre, {len(batch)} farms compared with {len(other)})")
            elif diff <= -0.15:
                v.add(0.3, f"farms that used this batch harvested {abs(diff):.0%} less per acre than similar farms")
    else:
        v.reasons.append(f"not enough harvest data yet to compare (needs {MIN_FARMS} farms on each side; have {len(batch)} and {len(other)})")
    c = int(f.get("complaints") or 0)
    if c >= 5:
        v.add(0.3, f"{c} farmers reported problems with this batch")
    elif c >= 2:
        v.add(0.15, f"{c} farmers reported problems with this batch")
    if f.get("lab_result") == "fail":
        v.add(0.5, "a lab test of this batch failed")
    rs = float(f.get("repeat_share") or 0)
    if rs >= 0.2:
        v.add(0.25, f"{rs:.0%} of checked bags show copied codes: fake copies of this batch are likely in the market")
    res = v.out()
    res.update(out)
    return res
