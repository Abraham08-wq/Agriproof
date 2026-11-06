#!/usr/bin/env bash
# Deploys ProofLedger to the local blockchain if it is not already there, and saves the address to .env.
set -euo pipefail
cd "$(dirname "$0")/.."
export PATH="$HOME/.foundry/bin:$PATH"

RPC="http://127.0.0.1:48545"

# On a public network (after make deploy-testnet) the contract is already deployed there.
if [[ "$(grep -E '^CHAIN_ID=' .env | cut -d= -f2-)" != "31337" ]]; then
  info "Using the public blockchain configured in .env (chain $(grep -E '^CHAIN_ID=' .env | cut -d= -f2-))"
  exit 0
fi
info() { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
fail() { printf '\033[1;31mERROR:\033[0m %s\n' "$*" >&2; exit 1; }

for _ in $(seq 1 30); do
  cast block-number --rpc-url "$RPC" >/dev/null 2>&1 && break
  sleep 1
done
cast block-number --rpc-url "$RPC" >/dev/null 2>&1 || fail "local blockchain is not reachable on $RPC (run: docker compose up -d anvil)"

CURRENT=$(grep -E '^CHAIN_CONTRACT_ADDRESS=' .env | cut -d= -f2- || true)
if [[ -n "$CURRENT" ]] && [[ "$(cast code "$CURRENT" --rpc-url "$RPC" 2>/dev/null)" != "0x" ]]; then
  info "ProofLedger already deployed at $CURRENT"
  exit 0
fi

info "Deploying ProofLedger to the local blockchain"
PK=$(grep -E '^CHAIN_PRIVATE_KEY=' .env | cut -d= -f2-)
OUT=$(cd contracts && forge script script/Deploy.s.sol --rpc-url "$RPC" --broadcast --private-key "$PK" 2>&1)
ADDR=$(grep -oE 'ProofLedger deployed at: 0x[0-9a-fA-F]{40}' <<<"$OUT" | awk '{print $4}')
[[ -n "$ADDR" ]] || { echo "$OUT"; fail "contract deployment failed"; }
sed -i "s|^CHAIN_CONTRACT_ADDRESS=.*|CHAIN_CONTRACT_ADDRESS=$ADDR|" .env
info "Contract address: $ADDR (saved to .env)"
