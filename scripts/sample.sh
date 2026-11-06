#!/usr/bin/env bash
# Sets up a clearly labelled sample cooperative so you can try AgriProof end to end:
# a collection centre, you as manager, maize prices, a verified fertilizer supplier, and a batch of
# 20 bag codes shipped to the cooperative. Farmers and deliveries you add yourself in the field app.
set -euo pipefail
cd "$(dirname "$0")/.."
API="${API_URL:-http://127.0.0.1:48080}"
KEY=${API_KEY:-$(grep -E '^API_KEY=' .env | tail -1 | cut -d= -f2-)}
info() { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
fail() { printf '\033[1;31mERROR:\033[0m %s\n' "$*" >&2; exit 1; }
call() { curl -fsS -X "$1" "$API$2" -H "X-API-Key: $KEY" -H 'Content-Type: application/json' ${3:+-d "$3"}; }
command -v jq >/dev/null || fail "jq is missing: sudo apt install -y jq"
curl -fsS "$API/health" >/dev/null || fail "AgriProof is not running: make up"

read -rp "Your phone number (you will be the sample cooperative's manager): " PHONE
read -rp "A second phone number for a clerk (Enter to skip): " CLERK || true

info "Creating 'Sample Maize Growers Cooperative'"
COOP=$(call POST /coops '{"name":"Sample Maize Growers Cooperative","county":"Trans Nzoia"}' | jq -r .id)
CENTRE=$(call POST "/coops/$COOP/centres" '{"name":"Kitale Main Store","location":"Kitale town"}' | jq -r .id)
call POST "/coops/$COOP/staff" "{\"centre_id\":\"$CENTRE\",\"name\":\"Sample Manager\",\"phone\":\"$PHONE\",\"role\":\"manager\"}" >/dev/null
[[ -n "${CLERK:-}" ]] && call POST "/coops/$COOP/staff" "{\"centre_id\":\"$CENTRE\",\"name\":\"Sample Clerk\",\"phone\":\"$CLERK\",\"role\":\"clerk\"}" >/dev/null
call POST "/coops/$COOP/prices" '{"crop":"maize","grade":"1","price_per_kg":45}' >/dev/null
call POST "/coops/$COOP/prices" '{"crop":"maize","grade":"2","price_per_kg":38}' >/dev/null

info "Adding a verified sample fertilizer supplier and batch"
SUP=$(call POST /suppliers '{"name":"Sample Fertilizer Ltd","kind":"manufacturer","reg_no":"SAMPLE-REG-001","kebs_permit":"SAMPLE-SM-001","county":"Nakuru"}' | jq -r .id)
call POST "/suppliers/$SUP/verify" >/dev/null
BATCH=$(call POST /batches '{"supplier_id":"'"$SUP"'","product":"DAP planting fertilizer (sample)","input_type":"fertilizer","stated_content":"N 18% P2O5 46%","bag_kg":50,"batch_no":"SAMPLE-'"$(date +%H%M%S)"'","bags":20}' | jq -r .id)
OUT="$HOME/agriproof-sample-bag-codes.csv"
call POST "/batches/$BATCH/codes" > "$OUT"
call POST "/batches/$BATCH/ship" '{"coop_id":"'"$COOP"'","to_name":"Kitale Main Store","bags":20}' >/dev/null

info "Done."
cat <<MSG

  Sign in to the field app with $PHONE (code from: make code), then:
   - register a farmer, record a maize delivery, issue a bag of fertilizer
  Sample bag codes (one per bag) are in: $OUT
  Try one at http://127.0.0.1:48080/check , then try the same code from another phone to see the copy warning.
MSG
