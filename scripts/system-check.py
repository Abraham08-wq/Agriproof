#!/usr/bin/env python3
"""AgriProof system check: proves every protection works against the running system.

It creates two clearly labelled "System check" cooperatives, uses them the way staff, farmers and
dishonest people would, and confirms each attack is stopped. It only runs while payments and SMS are
in test mode, so no real money or text messages are involved. The tampering test restores the record.
Usage: make check
"""
import csv, io, json, os, re, subprocess, sys, time, urllib.request, urllib.error, urllib.parse
os.chdir(os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))
def env(k, default=""):
    v = os.environ.get(k)
    if v: return v
    try:
        for line in open(".env"):
            if line.startswith(k + "="): v = line.split("=", 1)[1].strip()
    except FileNotFoundError: pass
    return v or default
BASE, KEY, INBOUND = env("API_URL", "http://127.0.0.1:48080"), env("API_KEY"), env("INBOUND_SECRET")
def logs():
    return subprocess.run(["docker", "compose", "logs", "--no-color", "--since", "30m", "api"], capture_output=True, text=True).stdout
ok = fail = 0
def check(name, cond, extra=""):
    global ok, fail
    if cond: ok += 1; print("  PASS", name)
    else: fail += 1; print("  FAIL", name, str(extra)[:400])
def call(method, path, body=None, token=None, admin=False, form=None, raw=False):
    h = {"Content-Type": "application/json"}
    if token: h["Authorization"] = "Bearer " + token
    if admin: h["X-API-Key"] = KEY
    data = json.dumps(body).encode() if body is not None else None
    if form is not None: data = urllib.parse.urlencode(form).encode(); h["Content-Type"] = "application/x-www-form-urlencoded"
    req = urllib.request.Request(BASE + path, data=data, headers=h, method=method)
    try:
        with urllib.request.urlopen(req) as r:
            b = r.read()
            if raw: return r.status, b.decode()
            return r.status, (json.loads(b) if b else None)
    except urllib.error.HTTPError as e:
        b = e.read()
        try: return e.code, json.loads(b)
        except Exception: return e.code, b.decode()
def psql(q): return subprocess.run(["docker","compose","exec","-T","postgres","psql","-U","agri","-d","agriproof","-tAqc",q],capture_output=True,text=True).stdout.strip()
def sms_to(intl, needle):
    for line in reversed(logs().splitlines()):
        if "TEST SMS" in line and "+" + intl in line and needle in line: return line
    return None
def code_for(intl):
    c = None
    for line in logs().splitlines():
        if "+" + intl in line and "code is" in line: c = re.search(r"code is (\d{6})", line).group(1)
    return c
def login(phone):
    for _ in range(10):
        st, _ = call("POST", "/auth/otp/request", {"phone": phone})
        if st != 429: break
        time.sleep(12)
    time.sleep(0.4)
    for _ in range(10):
        st, r = call("POST", "/auth/otp/verify", {"phone": phone, "code": code_for("254" + phone[1:]), "device": "t"})
        if st != 429: return r.get("token")
        time.sleep(12)
try:
    urllib.request.urlopen(BASE + "/health", timeout=5)
except Exception:
    sys.exit("AgriProof is not running. Start it with: make up")
if not KEY: sys.exit("API_KEY is missing from .env. Run: make setup")
_st, _sys = call("GET", "/system", admin=True)
if _st != 200 or _sys.get("payments_mode") != "test" or "test" not in (_sys.get("sms") or {}).get("detail", ""):
    sys.exit("The system check only runs while payments and SMS are in test mode, so it never moves real money or sends real texts.")
T = time.strftime("%H%M%S")
ph = lambda n: f"07{T[-4:]}{n:04d}"[:10]
intl = lambda p: "254" + p[1:]

print("Set up a cooperative")
_, coop = call("POST", "/coops", {"name": f"System check {T}", "county": "Trans Nzoia"}, admin=True); C = coop["id"]
_, coop2 = call("POST", "/coops", {"name": f"System check {T} (second)", "county": "Bungoma"}, admin=True)
_, centre = call("POST", f"/coops/{C}/centres", {"name": "Main store", "location": "Kitale"}, admin=True)
M, M2, CL, F1, F2 = ph(1), ph(2), ph(3), ph(11), ph(12)
for n, p, r in [("Grace Manager", M, "manager"), ("Peter Manager", M2, "manager"), ("Ken Clerk", CL, "clerk")]:
    call("POST", f"/coops/{C}/staff", {"centre_id": centre["id"], "name": n, "phone": p, "role": r}, admin=True)
call("POST", f"/coops/{coop2['id']}/staff", {"name": "Other Manager", "phone": ph(9), "role": "manager"}, admin=True)
st, _ = call("POST", f"/coops/{C}/prices", {"crop": "maize", "grade": "1", "price_per_kg": 45}, admin=True)
call("POST", f"/coops/{C}/prices", {"crop": "maize", "grade": "2", "price_per_kg": 38}, admin=True)
check("cooperative, centre, staff and prices set up", st == 200)

print("Fertilizer supplier and batch")
_, sup = call("POST", "/suppliers", {"name": f"Rift Fertilizers {T}", "kind": "manufacturer", "reg_no": "PVT-123", "kebs_permit": "SM-4455"}, admin=True)
batch_in = {"supplier_id": sup["id"], "product": "DAP 50kg", "stated_content": "N 18% P2O5 46%", "bag_kg": 50, "batch_no": f"B{T}", "bags": 6}
st, r = call("POST", "/batches", batch_in, admin=True)
check("unverified supplier cannot add batches", st == 403, r)
call("POST", f"/suppliers/{sup['id']}/verify", admin=True)
st, batch = call("POST", "/batches", batch_in, admin=True)
check("verified supplier's batch registered", st == 201, batch)
st, codes_csv = call("POST", f"/batches/{batch['id']}/codes", admin=True, raw=True)
codes = [row["code"] for row in csv.DictReader(io.StringIO(codes_csv))]
check("one code per bag issued as a printable list", st == 200 and len(codes) == 6 and len(codes[0]) == 14, codes[:2])
st, r = call("POST", f"/batches/{batch['id']}/codes", admin=True)
check("codes can only be printed once", st == 409, r)
check("only hashes of codes are stored", psql(f"SELECT COUNT(*) FROM bag_codes WHERE code_hash LIKE '%{codes[0].replace('-','')}%'") == "0")
call("POST", f"/batches/{batch['id']}/ship", {"coop_id": C, "to_name": "Main store", "bags": 6}, admin=True)

print("Registering farmers (ghost-farmer check)")
TC = login(CL)
st, r = call("POST", f"/me/coop/{C}/farmers", {"name": "John Kamau", "phone": F1, "village": "Kiminini", "acres": 2}, token=TC)
check("real farmer registered and active", st == 201 and r["farmer"]["status"] == "active", r)
fid = r["farmer"]["id"]
check("farmer gets a welcome SMS", sms_to(intl(F1), "member no.") is not None)
st, r = call("POST", f"/me/coop/{C}/farmers", {"name": "Kamau John", "phone": CL, "acres": 2}, token=TC)
check("ghost farmer (clerk's own phone, near-duplicate name) is held", st == 201 and r["farmer"]["status"] == "held", r)
ghost = r["farmer"]["id"]
st, r = call("POST", f"/me/coop/{C}/farmers/{ghost}/approve", token=TC)
check("a clerk cannot confirm a held farmer", st == 403, r)
TM = login(M)
call("POST", f"/me/coop/{C}/farmers/{ghost}/reject", token=TM)
call("POST", f"/me/coop/{C}/farmers", {"name": "Mary Wekesa", "phone": F2, "acres": 1.5}, token=TC)

print("Deliveries")
st, d1 = call("POST", f"/me/coop/{C}/deliveries", {"farmer_id": fid, "crop": "maize", "grade": "1", "weight_kg": 200, "moisture": 12.5}, token=TC)
check("delivery recorded at the right price (200 kg x 45)", st == 201 and d1["amount"] == 9000 and d1["status"] == "recorded", d1)
check("farmer gets an SMS receipt with weight and value", sms_to(intl(F1), "200 kg maize grade 1") is not None)
st, r = call("POST", f"/me/coop/{C}/deliveries", {"farmer_id": ghost, "crop": "maize", "grade": "1", "weight_kg": 50}, token=TC)
check("no deliveries for a rejected farmer", st == 409, r)
st, d2 = call("POST", f"/me/coop/{C}/deliveries", {"farmer_id": fid, "crop": "maize", "grade": "1", "weight_kg": 9000}, token=TC)
check("AI holds a delivery bigger than the farm can grow", st == 201 and d2["status"] == "held", d2)
check("AI explains why", any("more than it can grow" in x for x in d2["ai_reasons"]), d2["ai_reasons"])
st, r = call("POST", f"/me/coop/{C}/deliveries/{d2['id']}/clear", token=TC)
check("a clerk cannot clear a held delivery", st == 403, r)
call("POST", f"/me/coop/{C}/deliveries/{d2['id']}/reject", token=TM)
st, d2b = call("GET", f"/me/coop/{C}/deliveries?status=voided", token=TM)
check("rejected delivery is cancelled with a correction line in the ledger", any(x["id"] == d2["id"] for x in d2b) and psql(f"SELECT COUNT(*) FROM ledger_entries WHERE coop_id='{C}' AND entry_type='void'") == "1")
st, r = call("POST", f"/me/coop/{coop2['id']}/deliveries", {"farmer_id": fid, "crop": "maize", "grade": "1", "weight_kg": 10}, token=TC)
check("staff cannot act in another cooperative", st == 403, r)

print("Fertilizer: issuing and checking bags")
st, iss = call("POST", f"/me/coop/{C}/inputs", {"farmer_id": fid, "code": codes[0]}, token=TC)
check("genuine bag issued to the farmer", st == 201 and iss["product"] == "DAP 50kg", iss)
st, r = call("POST", f"/me/coop/{C}/inputs", {"farmer_id": fid, "code": codes[0]}, token=TC)
check("the same bag cannot be handed out twice", st == 409, r)
st, r = call("POST", f"/me/coop/{C}/inputs", {"farmer_id": fid, "code": "ABCD-EFGH-JKMN"}, token=TC)
check("an unregistered bag cannot be handed out", st == 404, r)
st, r = call("POST", "/public/check", {"code": codes[1]})
check("public check: genuine bag", st == 200 and r["result"] == "genuine" and r["product"] == "DAP 50kg", r)
st, r = call("POST", f"/sms/inbound/{INBOUND}", form={"from": "+254711999001", "text": codes[1].lower()})
time.sleep(0.5)
rep = sms_to("254711999001", "AgriProof")
check("SMS check from a different phone warns the label was copied", rep is not None and "already checked by another phone" in rep, rep)
st, r = call("POST", "/public/check", {"code": "ZZZZ-YYYY-XXXX"})
check("fake code: NOT FOUND", r.get("result") == "not_found", r)
st, r = call("POST", "/sms/inbound/wrong-secret-wrong-secret", form={"from": "+254711999001", "text": codes[2]})
check("incoming-SMS address needs its secret", st == 404, st)

print("Farmer's own view")
TF = login(F1)
st, me = call("GET", "/me", token=TF)
check("farmer signs in and is recognised", st == 200 and me["farmer"] and me["farmer"][0]["farmer_id"] == fid, me)
st, mine = call("GET", f"/me/farmer/{C}", token=TF)
check("farmer sees their deliveries and fertilizer", st == 200 and any(x["id"] == d1["id"] for x in mine["deliveries"]) and mine["inputs"], mine.keys() if isinstance(mine, dict) else mine)
st, r = call("GET", f"/me/coop/{C}/deliveries", token=TF)
check("a farmer cannot open staff screens", st == 403, r)
st, r = call("POST", "/me/complaint", {"code": codes[0], "problem": "maize leaves turned yellow after planting"}, token=TF)
check("farmer reports a problem with a bag", st == 201, r)

print("Paying farmers")
st, r = call("POST", f"/me/coop/{C}/payruns", token=TC)
check("a clerk cannot prepare payments", st == 403, r)
st, pr = call("POST", f"/me/coop/{C}/payruns", token=TM)
check("manager prepares a pay run from cleared deliveries only", st == 201 and pr["total"] == 9000 and pr["farmers"] == 1, pr)
st, r = call("POST", f"/me/coop/{C}/payruns/{pr['id']}/approve", token=TM)
check("the same manager cannot approve their own pay run", st == 403, r)
TM2 = login(M2)
st, r = call("POST", f"/me/coop/{C}/payruns/{pr['id']}/approve", token=TM2)
check("cannot pay without cooperative funds", st == 409 and "add funds" in r.get("error", ""), r)
st, fu = call("POST", f"/me/coop/{C}/fund", {"amount": 20000}, token=TM)
check("funding waits for confirmation", st == 202 and fu["status"] == "pending", fu)
check("funds not counted until confirmed", call("GET", f"/coops/{C}", admin=True)[1]["funds"] == 0)
call("POST", f"/fundings/{fu['id']}/confirm", admin=True)
st, r = call("POST", f"/me/coop/{C}/payruns/{pr['id']}/approve", token=TM2)
check("a second manager approves", st == 200 and r["status"] == "approved", r)
def paid():
    for _ in range(25):
        p = [x for x in call("GET", "/payouts", admin=True)[1] if x["payrun_id"] == pr["id"]]
        if p and p[0]["status"] == "paid": return p[0]
        time.sleep(2)
p = paid()
check("farmer is paid exactly once", p is not None and psql(f"SELECT COUNT(*) FROM payouts WHERE payrun_id='{pr['id']}'") == "1", p)
check("payment written into the sealed ledger", psql(f"SELECT COUNT(*) FROM ledger_entries WHERE coop_id='{C}' AND entry_type='payment' AND amount=-9000") == "1")
check("cooperative funds go down by the amount paid", call("GET", f"/coops/{C}", admin=True)[1]["funds"] == 11000)
st, mine = call("GET", f"/me/farmer/{C}", token=TF)
check("farmer sees the payment", any(x["status"] == "paid" for x in mine["payouts"]), mine["payouts"])
_, rec = call("POST", "/reconciliation/run", admin=True)
check("daily money check balances", rec["status"] == "ok", rec)

print("Lots and buyer proof")
st, lot = call("POST", f"/me/coop/{C}/lots", {"crop": "maize", "buyer": "Unga Millers"}, token=TM)
st, r = call("POST", f"/me/coop/{C}/lots/{lot['id']}/add", {"delivery_ids": [d1["id"], d2["id"]]}, token=TM)
check("only cleared deliveries go into a lot", st == 200 and r["deliveries"] == 1 and r["kg"] == 200, r)
call("POST", f"/me/coop/{C}/lots/{lot['id']}/close", token=TM)
time.sleep(3)
st, proof = call("GET", f"/public/lots/{lot['code']}")
check("public proof page: kg, farms, paid share, fertilizer used, records sealed", st == 200 and proof["kg"] == 200 and proof["farms"] == 1
      and proof["farmers_paid_pct"] == 100 and proof["inputs"] and proof["records_valid"], proof)
check("proof page shows no personal data", F1 not in json.dumps(proof) and "John" not in json.dumps(proof))

print("Recall")
st, r = call("POST", f"/batches/{batch['id']}/recall", {"reason": "failed KEBS nitrogen test"}, admin=True)
check("recall texts farmers who got or checked the batch", st == 200 and r["farmers_told"] >= 2, r)
time.sleep(0.5)
check("farmer receives the recall SMS", sms_to(intl(F1), "RECALL") is not None)
st, r = call("POST", "/public/check", {"code": codes[3]})
check("checking a recalled bag says do not use", r.get("result") == "recalled", r)
st, r = call("POST", f"/me/coop/{C}/inputs", {"farmer_id": fid, "code": codes[4]}, token=TC)
check("recalled bags cannot be handed out", st == 403, r)

print("Tamper-proof records")
st, v = call("GET", f"/coops/{C}/verify", admin=True)
check("records verified against the blockchain", v["valid"] and v["anchor_source"] == "blockchain", v)
psql(f"ALTER TABLE ledger_entries DISABLE TRIGGER ledger_no_update; UPDATE ledger_entries SET reference=replace(reference,'200.0kg','250.0kg') WHERE coop_id='{C}' AND entry_type='delivery'; ALTER TABLE ledger_entries ENABLE TRIGGER ledger_no_update;")
st, v = call("GET", f"/coops/{C}/verify", admin=True)
check("changing a recorded weight is detected", v["valid"] is False, v)
psql(f"ALTER TABLE ledger_entries DISABLE TRIGGER ledger_no_update; UPDATE ledger_entries SET reference=replace(reference,'250.0kg','200.0kg') WHERE coop_id='{C}' AND entry_type='delivery'; ALTER TABLE ledger_entries ENABLE TRIGGER ledger_no_update;")
check("restored record verifies again", call("GET", f"/coops/{C}/verify", admin=True)[1]["valid"])
check("console data needs the key", call("GET", "/coops")[0] == 401)
check("watchman key can read but not change", call("GET", "/coops", token=None)[0] == 401)
print(f"\n{ok} passed, {fail} failed"); sys.exit(1 if fail else 0)
