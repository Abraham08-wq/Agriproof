"""AgriProof AI service: field checks for deliveries, farmer registrations, fertilizer codes and batches."""

from fastapi import FastAPI
from pydantic import BaseModel

from .checks import score_batch, score_code_check, score_delivery, score_farmer

app = FastAPI(title="AgriProof AI", version="1.0.0")


class Delivery(BaseModel):
    crop: str = "maize"
    weight_kg: float
    grade: str = "1"
    moisture: float = 0
    has_moisture: bool = False
    acres: float = 0
    max_kg_per_acre: float = 0
    season_kg: float = 0
    farmer_deliveries_today: int = 0
    minutes_since_last: float = -1
    same_weight_last_hour: bool = False
    clerk_deliveries_30d: int = 0
    clerk_round_share: float = 0
    clerk_avg_vs_centre: float = 1
    hour_of_day: int = 12


class FarmerReg(BaseModel):
    name: str = ""
    acres: float = 0
    phone_used_by_others: int = 0
    similar_names: int = 0
    most_similar_name: str = ""
    clerk_registrations_1h: int = 0
    same_location_farmers: int = 0
    coop_median_acres: float = 0


class CodeCheck(BaseModel):
    result: str
    distinct_phones: int = 0
    total_checks: int = 0
    hours_since_first: float = 0
    phone_not_found_24h: int = 0
    batch_repeat_share: float = 0
    batch_bags_checked: int = 0
    batch_complaints: int = 0
    batch_lab_result: str = ""


class Batch(BaseModel):
    batch_yields: list[float] = []
    other_yields: list[float] = []
    complaints: int = 0
    lab_result: str = ""
    repeat_share: float = 0
    not_found_checks: int = 0


@app.get("/health")
def health() -> dict:
    return {"status": "ok"}


@app.post("/score-delivery")
def delivery(req: Delivery) -> dict:
    return score_delivery(req.model_dump())


@app.post("/score-farmer")
def farmer(req: FarmerReg) -> dict:
    return score_farmer(req.model_dump())


@app.post("/score-code-check")
def code_check(req: CodeCheck) -> dict:
    return score_code_check(req.model_dump())


@app.post("/score-batch")
def batch(req: Batch) -> dict:
    return score_batch(req.model_dump())
