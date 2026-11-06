#!/usr/bin/env bash
# One-time setup for AgriProof on Linux. Safe to re-run.
set -euo pipefail
cd "$(dirname "$0")/.."

info() { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
fail() { printf '\033[1;31mERROR:\033[0m %s\n' "$*" >&2; exit 1; }

info "Checking prerequisites"
command -v git >/dev/null || fail "git is missing: sudo apt install -y git"
command -v curl >/dev/null || fail "curl is missing: sudo apt install -y curl"
command -v jq >/dev/null || fail "jq is missing: sudo apt install -y jq"
command -v docker >/dev/null || fail "Docker is missing: see https://docs.docker.com/engine/install/"
docker compose version >/dev/null 2>&1 || fail "Docker Compose plugin is missing: sudo apt install -y docker-compose-plugin"

# Foundry may already be installed but not yet on PATH in this terminal
export PATH="$HOME/.foundry/bin:$PATH"
if ! command -v forge >/dev/null; then
  info "Installing Foundry (smart contract tools)"
  curl -L https://foundry.paradigm.xyz | bash
  export PATH="$HOME/.foundry/bin:$PATH"
  foundryup
fi

rand_hex() { openssl rand -hex "$1" 2>/dev/null || head -c "$1" /dev/urandom | od -An -tx1 | tr -d ' \n'; }

if [[ ! -f .env ]]; then
  info "Creating .env with fresh secrets"
  cp .env.example .env
  sed -i "s|^API_KEY=.*|API_KEY=$(rand_hex 24)|" .env
fi
chmod 600 .env   # secrets: readable only by you
# Make sure the file ends with a line break, so added settings start on their own line
[[ -s .env && "$(tail -c1 .env | od -An -c | tr -d ' ')" != '\n' ]] && echo >> .env
# Add any settings introduced in newer versions, keeping existing values
while IFS= read -r line; do
  [[ "$line" =~ ^([A-Z_]+)= ]] || continue
  key=${BASH_REMATCH[1]}
  grep -qE "^${key}=" .env || echo "$line" >> .env
done < .env.example
SS=$(grep -E '^SESSION_SECRET=' .env | tail -1 | cut -d= -f2- || true)
if (( ${#SS} < 32 )); then
  sed -i '/^SESSION_SECRET=/d' .env
  echo "SESSION_SECRET=$(rand_hex 32)" >> .env
  info "Generated SESSION_SECRET"
fi
SS=$(grep -E '^SESSION_SECRET=' .env | tail -1 | cut -d= -f2- || true)
(( ${#SS} >= 32 )) || fail "Could not write SESSION_SECRET to .env"
# Other generated secrets: read-only key for the watchman, secret part of the incoming-SMS callback URL
for key in MONITOR_API_KEY INBOUND_SECRET; do
  v=$(grep -E "^${key}=" .env | tail -1 | cut -d= -f2- || true)
  if (( ${#v} < 24 )); then
    sed -i "/^${key}=/d" .env
    echo "${key}=$(rand_hex 24)" >> .env
  fi
done
BP=$(grep -E '^BACKUP_PASSPHRASE=' .env | tail -1 | cut -d= -f2- || true)
if (( ${#BP} < 24 )); then
  sed -i '/^BACKUP_PASSPHRASE=/d' .env
  echo "BACKUP_PASSPHRASE=$(rand_hex 24)" >> .env
  info "Generated BACKUP_PASSPHRASE (run 'make backup-schedule' to see it and turn on daily backups)"
fi

info "Saving farm photos (free under the Unsplash License) for offline use"
mkdir -p brand
for pair in \
  "hero.jpg|https://images.unsplash.com/photo-1758164280826-b7368e8109b5?fm=jpg&q=70&w=2000&auto=format&fit=crop" \
  "seedling.jpg|https://images.unsplash.com/photo-1542601906990-b4d3fb778b09?fm=jpg&q=70&w=1600&auto=format&fit=crop" \
  "rows.jpg|https://images.unsplash.com/photo-1788122290883-6587385ffb8c?fm=jpg&q=70&w=1600&auto=format&fit=crop" \
  "field.jpg|https://images.unsplash.com/photo-1782852588113-ff57cf895c98?fm=jpg&q=70&w=2000&auto=format&fit=crop"; do
  name=${pair%%|*}; url=${pair#*|}
  [[ -s "brand/$name" ]] || curl -fsSL --max-time 60 -o "brand/$name" "$url" || { rm -f "brand/$name"; echo "  (could not download $name; the page will load it online)"; }
done
chmod 644 brand/*.jpg 2>/dev/null || true

info "Installing contract library (forge-std)"
if [[ ! -d contracts/lib/forge-std ]]; then
  git clone --depth 1 https://github.com/foundry-rs/forge-std contracts/lib/forge-std
fi

info "Running smart contract tests"
(cd contracts && forge test)

info "Starting the stack (database, local blockchain, AI service, API)"
docker compose up -d --build postgres anvil ai

./scripts/deploy.sh

# Recreate the API so it picks up the contract address from .env
docker compose up -d --build --force-recreate api monitor
for _ in $(seq 1 60); do
  curl -fsS http://127.0.0.1:48080/health >/dev/null 2>&1 && break
  sleep 1
done
curl -fsS http://127.0.0.1:48080/health >/dev/null || fail "API did not start: run 'docker compose logs api'"

info "Done."
echo
echo "  Console:     http://127.0.0.1:48080"
echo "  Field app:   http://127.0.0.1:48080/app/"
echo "  Bag check:   http://127.0.0.1:48080/check"
if [[ "$(docker compose exec -T postgres psql -U agri -d agriproof -tAc 'SELECT COUNT(*) FROM admins' 2>/dev/null)" == "0" ]]; then
  echo "  Next step:   make admin    (create your personal sign-in)"
fi
echo "  System check: make check     Sample cooperative to explore: make sample"
