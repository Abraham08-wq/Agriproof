#!/usr/bin/env bash
# Check that the payment company keys in .env work, without moving any money.
set -euo pipefail
cd "$(dirname "$0")/.."
COMPOSE="docker compose"; [[ "${PROD:-}" == 1 ]] && COMPOSE="docker compose -f docker-compose.prod.yml"
$COMPOSE up -d api >/dev/null
$COMPOSE exec -T api /api payments-check
