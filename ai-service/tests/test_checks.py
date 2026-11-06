from app.checks import score_batch, score_code_check, score_delivery, score_farmer

NORMAL = dict(crop="maize", weight_kg=180, grade="1", acres=2, max_kg_per_acre=3200, season_kg=900,
              minutes_since_last=4000, clerk_deliveries_30d=50, clerk_round_share=0.2, clerk_avg_vs_centre=1.0, hour_of_day=10)


def test_normal_delivery_is_low():
    r = score_delivery(NORMAL)
    assert r["level"] == "low" and r["reasons"] == []


def test_more_than_the_farm_can_grow_is_high():
    r = score_delivery({**NORMAL, "season_kg": 9000})
    assert r["level"] == "high"
    assert "more than it can grow" in r["reasons"][0]


def test_double_recording_and_short_weighing():
    r = score_delivery({**NORMAL, "same_weight_last_hour": True, "minutes_since_last": 3, "clerk_avg_vs_centre": 0.8})
    assert r["level"] == "high"
    assert any("recorded twice" in x for x in r["reasons"])
    assert any("lighter than other clerks" in x for x in r["reasons"])


def test_wet_maize_graded_one():
    r = score_delivery({**NORMAL, "has_moisture": True, "moisture": 18})
    assert r["level"] == "medium"


def test_ghost_farmer_shared_phone_and_name():
    r = score_farmer(dict(name="Jane Wanjiku", acres=2, phone_used_by_others=2, similar_names=1, most_similar_name="Wanjiku Jane"))
    assert r["level"] == "high"
    ok = score_farmer(dict(name="John Kamau", acres=2, coop_median_acres=2))
    assert ok["level"] == "low"


def test_code_checks():
    assert score_code_check({"result": "genuine"})["level"] == "low"
    assert score_code_check({"result": "not_found"})["level"] == "high"
    assert score_code_check({"result": "repeat", "distinct_phones": 3})["level"] == "high"


def test_batch_that_underperforms():
    good = [1400, 1500, 1600, 1550, 1450, 1500, 1520, 1480]
    bad = [800, 850, 900, 780, 820, 870]
    r = score_batch({"batch_yields": bad, "other_yields": good})
    assert r["enough_data"] and r["difference_pct"] < -40 and r["level"] == "high"
    fine = score_batch({"batch_yields": good[:6], "other_yields": good})
    assert fine["level"] == "low"
    few = score_batch({"batch_yields": bad[:2], "other_yields": good})
    assert not few["enough_data"] and few["level"] == "low"
