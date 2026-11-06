#!/usr/bin/env bash
# Gives AgriProof a free public HTTPS link (Cloudflare quick tunnel), so staff and farmers can open the
# field app on their own phones from anywhere. No account, no card, no cost.
# The link works while this command runs and your computer stays on. Press Ctrl+C to stop sharing.
set -euo pipefail
cd "$(dirname "$0")/.."

info() { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
fail() { printf '\033[1;31mERROR:\033[0m %s\n' "$*" >&2; exit 1; }

curl -fsS http://127.0.0.1:48080/health >/dev/null 2>&1 || fail "AgriProof is not running. Start it with: make up"

# 1. Get cloudflared (one small program, downloaded once into ./bin)
CF=$(command -v cloudflared || true)
if [[ -z "$CF" ]]; then
  CF=./bin/cloudflared
  if [[ ! -x "$CF" ]]; then
    case "$(uname -m)" in
      x86_64) ARCH=amd64 ;;
      aarch64|arm64) ARCH=arm64 ;;
      armv7l) ARCH=arm ;;
      *) fail "Unsupported processor: $(uname -m)" ;;
    esac
    info "Downloading cloudflared (one time, about 40 MB)"
    mkdir -p bin
    curl -fL --progress-bar -o "$CF" "https://github.com/cloudflare/cloudflared/releases/latest/download/cloudflared-linux-$ARCH"
    chmod +x "$CF"
  fi
fi

# 2. While sharing, read each visitor's real IP from the tunnel, so rate limits apply per person.
set_env() { if grep -qE "^$1=" .env; then sed -i "s|^$1=.*|$1=$2|" .env; else echo "$1=$2" >> .env; fi; }
BEFORE=$(grep -E '^TRUST_PROXY=' .env | cut -d= -f2- || echo false)
LOG=$(mktemp)
cleanup() {
  [[ -n "${CF_PID:-}" ]] && kill "$CF_PID" 2>/dev/null || true
  set_env TRUST_PROXY "$BEFORE"
  docker compose up -d --force-recreate api >/dev/null 2>&1 || true
  rm -f "$LOG"
  echo
  info "Stopped sharing. The public link no longer works."
}
trap cleanup EXIT INT TERM
set_env TRUST_PROXY true
docker compose up -d --force-recreate api >/dev/null
for _ in $(seq 1 30); do curl -fsS http://127.0.0.1:48080/health >/dev/null 2>&1 && break; sleep 1; done

# 3. Open the tunnel
info "Opening a public link"
"$CF" tunnel --no-autoupdate --url http://127.0.0.1:48080 >"$LOG" 2>&1 &
CF_PID=$!
URL=""
for _ in $(seq 1 45); do
  URL=$(grep -oE 'https://[a-z0-9-]+\.trycloudflare\.com' "$LOG" | head -1 || true)
  [[ -n "$URL" ]] && break
  kill -0 "$CF_PID" 2>/dev/null || { cat "$LOG"; fail "cloudflared stopped unexpectedly"; }
  sleep 1
done
[[ -n "$URL" ]] || { tail -20 "$LOG"; fail "Could not get a public link. Check your internet connection and try again."; }

# Wait until the link actually answers
for _ in $(seq 1 30); do curl -fsS "$URL/health" >/dev/null 2>&1 && break; sleep 2; done

echo
printf '\033[1;32m  AgriProof is live.\033[0m\n\n'
printf '  Field app (staff and farmers):   \033[1m%s/app/\033[0m\n' "$URL"
printf '  Your console:                        %s\n\n' "$URL"
if command -v qrencode >/dev/null; then
  echo "  Scan this on a phone to open the field app:"
  qrencode -t ansiutf8 -m 2 "$URL/app/"
fi
cat <<EOF
  On the phone: open the link and sign in with a phone number registered as staff or farmer,
  then tap "Install app" (Android) or Share > "Add to Home Screen" (iPhone).

  SMS is in test mode until you add Africa's Talking keys, so get login codes with:  make code

  Farmers can check fertilizer bags without signing in at the same link + /check

  Keep this window open and your computer on. Press Ctrl+C to stop sharing.
  Each time you run 'make share' you get a new link; send the new one to testers.
EOF
wait "$CF_PID"
