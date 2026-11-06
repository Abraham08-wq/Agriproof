#!/usr/bin/env bash
# Restore AgriProof from an encrypted backup. Takes a safety backup of the current state first.
#   ./scripts/restore.sh                     newest backup
#   ./scripts/restore.sh path/to/file.backup a specific one
set -euo pipefail
cd "$(dirname "$0")/.."
umask 077

info() { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
fail() { printf '\033[1;31mERROR:\033[0m %s\n' "$*" >&2; exit 1; }
envval() { grep -E "^$1=" .env 2>/dev/null | tail -1 | cut -d= -f2- || true; }

COMPOSE="docker compose"; [[ "${PROD:-}" == 1 ]] && COMPOSE="docker compose -f docker-compose.prod.yml"
RESTORE_CMD=${RESTORE_CMD:-"$COMPOSE exec -T postgres pg_restore -U agri -d agriproof --clean --if-exists --no-owner --single-transaction"}
SQL_CMD=${SQL_CMD:-"$COMPOSE exec -T postgres psql -U agri -d agriproof -v ON_ERROR_STOP=1 -q"}
DIR=${BACKUP_DIR:-$(envval BACKUP_DIR)}; DIR=${DIR:-$HOME/agriproof-backups}; DIR=${DIR/#\~/$HOME}

file=${1:-$(ls -1t "$DIR"/agriproof-*.backup 2>/dev/null | head -1 || true)}
[[ -n "$file" && -f "$file" ]] || fail "no backup found in $DIR"
if [[ -f "$file.sha256" ]]; then
  (cd "$(dirname "$file")" && sha256sum -c --quiet "$(basename "$file").sha256") || fail "backup file is damaged (checksum does not match)"
fi
export BACKUP_PASSPHRASE=${BACKUP_PASSPHRASE:-$(envval BACKUP_PASSPHRASE)}
if (( ${#BACKUP_PASSPHRASE} < 24 )); then
  read -rsp "Backup passphrase: " BACKUP_PASSPHRASE; echo; export BACKUP_PASSPHRASE
fi
work=$(mktemp -d); trap 'rm -rf "$work"' EXIT
openssl enc -d -aes-256-cbc -pbkdf2 -iter 200000 -pass env:BACKUP_PASSPHRASE -in "$file" | tar -C "$work" -xzf - \
  || fail "could not open the backup (wrong passphrase?)"
[[ -s "$work/db.dump" ]] || fail "backup has no database"

echo "This replaces ALL current AgriProof data with the backup:"
echo "  $file"
if [[ "${YES:-}" != 1 ]]; then
  read -rp "Type RESTORE to continue: " answer
  [[ "$answer" == RESTORE ]] || fail "cancelled"
fi

if [[ "${SKIP_SAFETY_BACKUP:-}" != 1 ]]; then
  info "Safety backup of the current data first"
  ./scripts/backup.sh || fail "safety backup failed; nothing was changed"
fi

[[ "${SKIP_DOCKER:-}" == 1 ]] || { info "Stopping the API"; $COMPOSE stop api >/dev/null; }
info "Restoring the database"
$RESTORE_CMD < "$work/db.dump" || fail "database restore failed; the safety backup has the previous data"
# A backup can be older than payouts that already went out. Never resend from an old copy:
# unfinished payouts wait for a person to check the payment company, and a fresh money check runs.
$SQL_CMD <<'SQL' || fail "could not secure payouts after the restore"
UPDATE payouts SET status='unknown', updated_at=now(),
  last_error='restored from a backup: check the payment company before retrying, marking paid or returning'
  WHERE status IN ('queued','sending','sent');
DELETE FROM reconciliations WHERE created_at > now() - interval '24 hours';
SQL
if [[ -s "$work/anvil-state.json" && "$(envval CHAIN_ID)" == "31337" && "${SKIP_DOCKER:-}" != 1 ]]; then
  info "Restoring the local blockchain"
  $COMPOSE stop anvil >/dev/null
  $COMPOSE cp "$work/anvil-state.json" anvil:/data/anvil-state.json
  $COMPOSE start anvil >/dev/null
fi
if ! cmp -s "$work/env" .env; then
  cp "$work/env" .env.restored
  info "The backup's settings were saved as .env.restored (your current .env was kept). Compare them if anything looks wrong."
fi
[[ "${SKIP_DOCKER:-}" == 1 ]] || { info "Starting the API"; $COMPOSE up -d api >/dev/null; }
info "Restore finished. Check it with: make check"
