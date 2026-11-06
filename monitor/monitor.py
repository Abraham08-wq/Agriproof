"""AgriProof watchman: checks the system every minute and sends alerts to your phone by Telegram.

Runs as its own container, so it can report when the API itself is down. Without Telegram settings
it still runs and writes alerts to its log (docker compose logs monitor).
"""
from __future__ import annotations

import json
import os
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
from datetime import datetime, timedelta, timezone

API = os.environ.get("API_URL", "http://api:8080").rstrip("/")
KEY = os.environ.get("API_KEY", "")
TG_TOKEN = os.environ.get("TELEGRAM_BOT_TOKEN", "").strip()
TG_CHAT = os.environ.get("TELEGRAM_CHAT_ID", "").strip()
STATE = os.environ.get("MONITOR_STATE", "/data/monitor-state.json")
EVERY = int(os.environ.get("MONITOR_INTERVAL", "60"))
REPEAT_H = float(os.environ.get("MONITOR_REPEAT_HOURS", "6"))
DEPOSIT_WAIT_MIN = int(os.environ.get("MONITOR_DEPOSIT_MINUTES", "30"))
SUMMARY_HOUR = int(os.environ.get("MONITOR_SUMMARY_HOUR", "8"))  # East Africa time; -1 turns it off
EAT = timezone(timedelta(hours=3))
NAME = os.environ.get("MONITOR_NAME", "AgriProof")
TG_API = os.environ.get("TELEGRAM_API", "https://api.telegram.org")  # changed only for tests


def log(msg: str) -> None:
    print(f"{datetime.now(EAT):%Y-%m-%d %H:%M:%S} {msg}", flush=True)


def get(path: str, timeout: float = 10):
    req = urllib.request.Request(API + path, headers={"X-API-Key": KEY, "Accept": "application/json"})
    with urllib.request.urlopen(req, timeout=timeout) as r:
        return json.loads(r.read() or b"null")


def telegram(text: str) -> bool:
    if not (TG_TOKEN and TG_CHAT):
        log("ALERT (Telegram not set up): " + text.replace("\n", " | "))
        return True
    data = urllib.parse.urlencode({"chat_id": TG_CHAT, "text": text, "disable_web_page_preview": "true"}).encode()
    try:
        with urllib.request.urlopen(f"{TG_API}/bot{TG_TOKEN}/sendMessage", data=data, timeout=15) as r:
            return json.loads(r.read()).get("ok", False)
    except Exception as e:  # never let a Telegram problem stop the checks
        log(f"Telegram send failed: {e}")
        return False


def first_name(p: dict) -> str:
    """Only a first name goes to Telegram; full names and phone numbers stay in the console."""
    n = (p.get("name") or "").split()
    return n[0] if n else "a member"


def parse_time(s: str) -> datetime:
    return datetime.fromisoformat(s.replace("Z", "+00:00"))


def age_minutes(s: str) -> float:
    return (datetime.now(timezone.utc) - parse_time(s)).total_seconds() / 60


# ---------- checks: each returns {alert_id: message} for problems that exist right now ----------

def check_all(state: dict) -> dict[str, str]:
    problems: dict[str, str] = {}
    try:
        get("/health", timeout=8)
        state["health_fails"] = 0
    except Exception as e:
        state["health_fails"] = state.get("health_fails", 0) + 1
        if state["health_fails"] >= 2:
            problems["down"] = f"🔴 {NAME} is DOWN: the app is not answering ({type(e).__name__}). Members cannot use it."
        else:
            problems["_unreachable"] = ""  # one miss: wait for the next check before alerting
        return problems  # nothing else can be checked

    try:
        sysinfo = get("/system")
        names = {"database": "Database", "blockchain": "Blockchain", "ai": "AI engine"}
        for k, label in names.items():
            c = sysinfo.get(k) or {}
            if not c.get("ok", False):
                problems[f"component:{k}"] = f"🟠 {label} problem: {c.get('detail', 'not responding')}"
    except Exception as e:
        problems["system"] = f"🟠 Could not read system status: {e}"

    groups = []
    try:
        groups = get("/coops") or []
    except Exception:
        pass
    # Ledger integrity: every group, every 10 minutes (spread out to stay light).
    now = time.time()
    if now - state.get("last_ledger_check", 0) >= 600:
        state["last_ledger_check"] = now
        bad = {}
        for g in groups:
            try:
                v = get(f"/coops/{g['id']}/verify", timeout=20)
            except Exception:
                continue
            if not v.get("valid", True):
                where = f" at record {v['broken_at_seq']}" if v.get("broken_at_seq") else ""
                bad[g["id"]] = f"🚨 RECORDS TAMPERED at {g['name']}{where}: {v.get('reason', 'ledger check failed')}. Investigate before any money moves."
        state["ledger_bad"] = bad
    for gid, msg in state.get("ledger_bad", {}).items():
        problems[f"ledger:{gid}"] = msg

    try:
        deps = [d for d in (get("/fundings") or []) if d["status"] == "pending"]
        old = [d for d in deps if age_minutes(d["created_at"]) >= DEPOSIT_WAIT_MIN]
        if old:
            total = sum(d["amount"] for d in old)
            problems["deposits"] = (f"🟡 {len(old)} cooperative funding payment(s) waiting over {DEPOSIT_WAIT_MIN} min (total KES {total:,}). "
                                    "In test mode they need you to confirm them in the console.")
    except Exception:
        pass

    try:
        pays = get("/payouts") or []
        for p in pays:
            if p["status"] in ("unknown", "manual", "failed"):
                what = {"unknown": "outcome unclear: check the payment company before retrying",
                        "manual": "must be paid by hand, then marked paid",
                        "failed": "failed after several tries"}[p["status"]]
                problems[f"payout:{p['id']}"] = (f"🟠 Farmer payment KES {p['amount']:,} to {first_name(p)} "
                                                 f"({p['coop_name']}): {what}.")
            elif p["status"] == "queued" and p.get("attempts", 0) >= 3:
                problems[f"payout:{p['id']}"] = (f"🟡 Farmer payment KES {p['amount']:,} to {first_name(p)} "
                                                 f"refused {p['attempts']} times: {p.get('last_error', '')[:120]}")
    except Exception:
        pass

    try:
        rc = get("/reconciliation") or {}
        r = rc.get("latest")
        if r and r["status"] == "mismatch":
            problems["recon"] = f"🚨 MONEY CHECK MISMATCH: {r.get('detail', '')}. Find out why before paying anyone."
        elif r and r["status"] == "unavailable" and r.get("live"):
            problems["recon"] = f"🟠 Money check could not read the account balance: {r.get('detail', '')}"
        elif r and age_minutes(r["created_at"]) > 26 * 60:
            problems["recon-old"] = "🟡 The daily money check has not run for over a day."
    except Exception:
        pass

    try:
        held_f = [f for f in (get("/farmers/held") or []) if age_minutes(f["created_at"]) >= 60]
        if held_f:
            problems["held-farmers"] = f"🟡 {len(held_f)} new farmer registration(s) held by the AI have waited over an hour for a manager."
        held_d = [d for d in (get("/deliveries/held") or []) if age_minutes(d["created_at"]) >= 120]
        if held_d:
            kgs = sum(d["weight_kg"] for d in held_d)
            problems["held-deliveries"] = (f"🟠 {len(held_d)} deliveries ({kgs:,.0f} kg) flagged by the AI are still waiting for review; "
                                           "those farmers are not paid until a manager decides.")
        sus = [c for c in (get("/checks/suspicious?limit=100") or []) if c["result"] in ("not_found", "repeat") and age_minutes(c["created_at"]) <= 60]
        if len(sus) >= 5:
            problems["fakes"] = f"🚨 {len(sus)} fake or copied fertilizer codes were checked in the last hour: fake bags may be on sale."
    except Exception:
        pass

    try:
        audit = get("/audit") or []
        fails = [a for a in audit if a["action"] == "admin.login_failed" and age_minutes(a["created_at"]) <= 15]
        if len(fails) >= 5:
            ips = sorted({a.get("ip", "") for a in fails})
            problems["logins"] = f"🚨 {len(fails)} failed console sign-ins in 15 minutes (from {', '.join(ips)[:80]}). Possible break-in attempt."
    except Exception:
        pass

    state["groups"] = len(groups)
    return problems


def daily_summary(state: dict) -> None:
    if SUMMARY_HOUR < 0:
        return
    now = datetime.now(EAT)
    today = now.strftime("%Y-%m-%d")
    if now.hour < SUMMARY_HOUR or state.get("summary_day") == today:
        return
    state["summary_day"] = today
    try:
        s = get("/stats")
        sysinfo = get("/system")
        mode = sysinfo.get("payments_mode", "test")
        open_issues = len(state.get("open", {}))
        status = "all checks passing" if open_issues == 0 else f"{open_issues} open issue(s)"
        telegram(f"☀️ {NAME} daily report: {status}.\n"
                 f"Cooperatives: {s.get('cooperatives', 0)}, farmers: {s.get('farmers', 0)}, produce this season: {s.get('kg_season', 0):,.0f} kg, "
                 f"paid to farmers: KES {s.get('paid_to_farmers', 0):,}, fake codes caught: {s.get('fakes_caught', 0)}.\n"
                 f"Payments: {mode}. Watchman is running.")
    except Exception as e:
        log(f"daily summary failed: {e}")


def load_state() -> dict:
    try:
        with open(STATE) as f:
            return json.load(f)
    except Exception:
        return {}


def save_state(state: dict) -> None:
    try:
        tmp = STATE + ".tmp"
        with open(tmp, "w") as f:
            json.dump(state, f)
        os.replace(tmp, STATE)
    except Exception as e:
        log(f"could not save state: {e}")


def run_once(state: dict) -> None:
    problems = check_all(state)
    opened = state.setdefault("open", {})
    now = time.time()
    for aid, msg in problems.items():
        if aid.startswith("_"):
            continue
        prev = opened.get(aid)
        if prev is None or now - prev["sent"] >= REPEAT_H * 3600:
            prefix = "" if prev is None else "(still) "
            if telegram(prefix + msg):
                opened[aid] = {"sent": now, "msg": msg}
        else:
            prev["msg"] = msg
    # While the app is down nothing else can be checked, so other alerts stay open (not "resolved").
    still_unknown = "down" in problems or "_unreachable" in problems
    for aid in [a for a in opened if a not in problems and (not still_unknown or a == "down")]:
        telegram("✅ Resolved: " + opened[aid]["msg"].lstrip("🔴🟠🟡🚨 ")[:300])
        del opened[aid]
    daily_summary(state)


def main() -> None:
    if "--test" in sys.argv:
        ok = telegram(f"✅ {NAME} watchman test: alerts reach this chat.")
        print("sent" if ok else "FAILED: check TELEGRAM_BOT_TOKEN and TELEGRAM_CHAT_ID")
        sys.exit(0 if ok else 1)
    if not KEY:
        log("API_KEY is not set; cannot check the system")
        sys.exit(1)
    log(f"watchman started: checking {API} every {EVERY}s; Telegram {'on' if TG_TOKEN and TG_CHAT else 'OFF (alerts only in this log)'}")
    state = load_state()
    if "--once" in sys.argv:
        run_once(state)
        save_state(state)
        return
    while True:
        try:
            run_once(state)
        except Exception as e:  # keep watching whatever happens
            log(f"check failed: {e}")
        save_state(state)
        time.sleep(EVERY)


if __name__ == "__main__":
    main()
