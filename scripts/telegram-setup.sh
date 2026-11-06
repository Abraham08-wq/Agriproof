#!/usr/bin/env bash
# Connect AgriProof alerts to your phone through a free Telegram bot.
set -euo pipefail
cd "$(dirname "$0")/.."
info() { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
fail() { printf '\033[1;31mERROR:\033[0m %s\n' "$*" >&2; exit 1; }
setenv() { sed -i "/^$1=/d" .env; echo "$1=$2" >> .env; chmod 600 .env; }
# Keep the bot token off the command line (other users could see it in the process list).
tg() { local method=$1; shift; printf 'url = "https://api.telegram.org/bot%s/%s"\n' "$TOKEN" "$method" | curl -fsS -K - "$@"; }
[[ -f .env ]] || fail "no .env file: run make setup first"
command -v jq >/dev/null || fail "jq is missing: sudo apt install -y jq"

cat <<'TXT'
Step 1. On your phone, open Telegram and search for  @BotFather  (blue tick).
        Send it:  /newbot
        Give the bot a name (e.g. AgriProof Alerts) and a username ending in "bot".
        BotFather replies with a token that looks like  123456789:AAH...
TXT
read -rsp "Paste the token here (it stays hidden): " TOKEN; echo
TOKEN=$(echo "$TOKEN" | tr -d '[:space:]')
me=$(tg getMe 2>/dev/null) || fail "Telegram did not accept that token. Copy it again from BotFather."
bot=$(jq -r '.result.username' <<<"$me")
info "Bot found: @$bot"

cat <<TXT

Step 2. In Telegram, open  https://t.me/$bot  and press START (or send it any message, like "hi").
TXT
read -rp "Press Enter after you have sent the message... " _
chat=""
for _ in 1 2 3 4 5 6; do
  chat=$(tg getUpdates | jq -r '[.result[] | (.message // .my_chat_member) | .chat.id] | last // empty')
  [[ -n "$chat" ]] && break
  sleep 3
done
[[ -n "$chat" ]] || fail "No message found. Send your bot a message in Telegram, then run make telegram-setup again."

setenv TELEGRAM_BOT_TOKEN "$TOKEN"
setenv TELEGRAM_CHAT_ID "$chat"
tg sendMessage --data-urlencode "chat_id=$chat" \
  --data-urlencode "text=✅ AgriProof alerts are connected. You will hear from me here if anything goes wrong." >/dev/null \
  || fail "Could not send the test message"
info "Saved. A test message was sent to your Telegram."
if docker compose ps --status running 2>/dev/null | grep -q api; then
  docker compose up -d monitor >/dev/null && info "The watchman restarted with your alert settings."
fi
