#!/usr/bin/env bash
# Moves AgriProof's blockchain anchoring from the local chain to the public Celo Sepolia test network,
# so anyone can verify each cooperative's sealed records on a public block explorer.
# Run it twice: the first run creates a wallet for you to fund from a free faucet; the second deploys.
set -euo pipefail
cd "$(dirname "$0")/.."
export PATH="$HOME/.foundry/bin:$PATH"

RPC="https://forno.celo-sepolia.celo-testnet.org"
CHAIN_ID=11142220
EXPLORER="https://celo-sepolia.blockscout.com"
KEYFILE=".testnet-wallet"

info() { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
fail() { printf '\033[1;31mERROR:\033[0m %s\n' "$*" >&2; exit 1; }
command -v cast >/dev/null || fail "Foundry is missing: run make setup first"

cast chain-id --rpc-url "$RPC" >/dev/null 2>&1 || fail "Cannot reach Celo Sepolia ($RPC). Check your internet connection."

if [[ ! -f "$KEYFILE" ]]; then
  info "Creating a new wallet for AgriProof on Celo Sepolia"
  OUT=$(cast wallet new)
  ADDR=$(grep -i 'Address' <<<"$OUT" | grep -oE '0x[0-9a-fA-F]{40}')
  PK=$(grep -i 'Private key' <<<"$OUT" | grep -oE '0x[0-9a-fA-F]{64}')
  [[ -n "$ADDR" && -n "$PK" ]] || fail "Could not create a wallet"
  umask 077
  printf 'ADDRESS=%s\nPRIVATE_KEY=%s\n' "$ADDR" "$PK" > "$KEYFILE"
  chmod 600 "$KEYFILE"
fi
ADDR=$(grep '^ADDRESS=' "$KEYFILE" | cut -d= -f2)
PK=$(grep '^PRIVATE_KEY=' "$KEYFILE" | cut -d= -f2)

BAL=$(cast balance "$ADDR" --rpc-url "$RPC")
if [[ "$BAL" == "0" ]]; then
  echo
  echo "Your AgriProof wallet needs free test CELO to pay for anchoring transactions."
  echo
  echo "  Wallet address:  $ADDR"
  echo
  echo "  1. Open one of these faucets and paste the address above:"
  echo "       https://faucet.celo.org/celo-sepolia"
  echo "       https://cloud.google.com/application/web3/faucet/celo/sepolia"
  echo "  2. Wait a minute, then run this command again: make deploy-testnet"
  echo
  echo "The wallet key is saved in $KEYFILE (readable only by you). Back it up somewhere safe and never share it."
  exit 0
fi
info "Wallet $ADDR has $(cast from-wei "$BAL") test CELO"

info "Deploying ProofLedger to Celo Sepolia"
OUT=$(cd contracts && forge script script/Deploy.s.sol --rpc-url "$RPC" --broadcast --private-key "$PK" 2>&1)
CONTRACT=$(grep -oE 'ProofLedger deployed at: 0x[0-9a-fA-F]{40}' <<<"$OUT" | awk '{print $4}')
[[ -n "$CONTRACT" ]] || { echo "$OUT"; fail "deployment failed"; }

cp .env .env.before-testnet
set_env() { if grep -qE "^$1=" .env; then sed -i "s|^$1=.*|$1=$2|" .env; else echo "$1=$2" >> .env; fi; }
set_env CHAIN_RPC_URL "$RPC"
set_env CHAIN_ID "$CHAIN_ID"
set_env CHAIN_CONTRACT_ADDRESS "$CONTRACT"
set_env CHAIN_PRIVATE_KEY "$PK"

info "Restarting the API on the public network"
docker compose up -d --force-recreate api >/dev/null

echo
info "AgriProof now anchors to the public Celo Sepolia network."
echo "  Contract:  $EXPLORER/address/$CONTRACT"
echo "  Wallet:    $EXPLORER/address/$ADDR"
echo "  Your previous settings are saved in .env.before-testnet (copy it back to .env to return to the local chain)."
