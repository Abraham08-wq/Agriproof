#!/usr/bin/env bash
# Creates a personal admin account for the AgriProof Field Office console.
# The password is hashed with Argon2id; sign-in also needs a code from an authenticator app.
set -euo pipefail
cd "$(dirname "$0")/.."

info() { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
fail() { printf '\033[1;31mERROR:\033[0m %s\n' "$*" >&2; exit 1; }

# PROD=1 targets the production stack (docker-compose.prod.yml) on a server
COMPOSE=(docker compose)
[[ "${PROD:-}" == "1" ]] && COMPOSE=(docker compose -f docker-compose.prod.yml)
"${COMPOSE[@]}" ps --status running api 2>/dev/null | grep -q api || fail "The API is not running. Start it with: make up"

read -rp "Choose a username (lowercase, e.g. ibrahim): " USERNAME
[[ "$USERNAME" =~ ^[a-z0-9._-]{3,40}$ ]] || fail "Use 3-40 lowercase letters, numbers, dot, dash or underscore."
read -rsp "Choose a password (at least 12 characters): " PW1; echo
read -rsp "Type the password again: " PW2; echo
[[ "$PW1" == "$PW2" ]] || fail "The passwords do not match."
(( ${#PW1} >= 12 )) || fail "The password must be at least 12 characters."

OUT=$(printf '%s\n' "$PW1" | "${COMPOSE[@]}" exec -T api /api create-admin "$USERNAME" 2>&1) || fail "$(sed -E 's/^Error: (validation failed|conflict): //' <<<"$OUT")"
unset PW1 PW2
SECRET=$(grep '^SECRET=' <<<"$OUT" | cut -d= -f2-)
URI=$(grep '^URI=' <<<"$OUT" | cut -d= -f2-)
[[ -n "$SECRET" ]] || fail "Unexpected output: $OUT"

info "Admin account '$USERNAME' created."
echo
echo "Now add AgriProof Field Office to an authenticator app on your phone"
echo "(Google Authenticator, Microsoft Authenticator, Authy or similar):"
echo
if command -v qrencode >/dev/null; then
  echo "Scan this QR code:"
  qrencode -t ansiutf8 "$URI"
else
  echo "  Choose 'Enter a setup key' and type:"
  echo "    Account:  AgriProof Field Office ($USERNAME)"
  echo "    Key:      $SECRET"
  echo "    Type:     Time based"
  echo
  echo "  Tip: install qrencode (sudo apt install -y qrencode) and run 'make admin' for a scannable QR code next time."
fi
echo
echo "This key is shown only once. Anyone with it can generate your codes, so do not save it in plain text."
echo "Then open the console and sign in with your username, password and the 6-digit code from the app."
