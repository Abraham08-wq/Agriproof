#!/usr/bin/env bash
# Starts AgriProof on a cloud server with HTTPS. Run on the server, inside the agriproof folder.
set -euo pipefail
cd "$(dirname "$0")/.."

info() { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
fail() { printf '\033[1;31mERROR:\033[0m %s\n' "$*" >&2; exit 1; }
val() { grep -E "^$1=" .env | cut -d= -f2-; }

[[ -f .env ]] || fail ".env is missing. Copy it from your laptop or run: cp .env.example .env"
[[ -n "$(val DOMAIN)" ]] || fail "Add your domain to .env, for example: DOMAIN=app.yourdomain.com"
[[ "$(val CHAIN_ID)" != "31337" ]] || fail "Production must use a public blockchain. Run 'make deploy-testnet' first (on this server or your laptop, then copy the CHAIN_ values here)."
[[ "$(val POSTGRES_PASSWORD)" != "agri_dev_password" ]] || fail "Set a strong POSTGRES_PASSWORD in .env (for example: openssl rand -hex 24)."
SS=$(val SESSION_SECRET); [[ ${#SS} -ge 32 ]] || fail "SESSION_SECRET in .env must be at least 32 characters (openssl rand -hex 32)."

DOMAIN=$(val DOMAIN)
# Bag labels must point at your real address, never at whatever address a visitor's browser sends.
if [[ -z "$(val PUBLIC_URL)" ]]; then
  if grep -qE '^PUBLIC_URL=' .env; then sed -i "s#^PUBLIC_URL=.*#PUBLIC_URL=https://$DOMAIN#" .env; else echo "PUBLIC_URL=https://$DOMAIN" >> .env; fi
  info "Set PUBLIC_URL=https://$DOMAIN (used in the check links printed on bag labels)"
fi
SERVER_IP=$(curl -fsS https://api.ipify.org 2>/dev/null || echo "")
DNS_IP=$(getent hosts "$DOMAIN" | awk '{print $1}' | head -1 || true)
if [[ -n "$SERVER_IP" && "$DNS_IP" != "$SERVER_IP" ]]; then
  echo "Warning: $DOMAIN points to '${DNS_IP:-nothing}', but this server is $SERVER_IP."
  echo "Create an A record for $DOMAIN pointing to $SERVER_IP at your domain provider, wait a few minutes, then run this again."
  read -rp "Continue anyway? [y/N] " ok; [[ "$ok" == "y" ]] || exit 1
fi

info "Building and starting AgriProof"
docker compose -f docker-compose.prod.yml --env-file .env up -d --build

for _ in $(seq 1 90); do
  curl -fsS "https://$DOMAIN/health" >/dev/null 2>&1 && break
  sleep 2
done
curl -fsS "https://$DOMAIN/health" >/dev/null || fail "AgriProof did not answer on https://$DOMAIN. See: docker compose -f docker-compose.prod.yml logs caddy api"

info "AgriProof is live at https://$DOMAIN"
echo "  Create your admin sign-in on this server:  make admin PROD=1"
echo "  Field app for staff and farmers:          https://$DOMAIN/app/"
echo "  Bag check page:                           https://$DOMAIN/check"
