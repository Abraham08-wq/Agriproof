#!/usr/bin/env bash
# Encrypted backup of everything needed to rebuild AgriProof: the database, the settings file
# (.env) and the local blockchain state. Keeps the newest BACKUP_KEEP backups (default 14).
#   ./scripts/backup.sh            (PROD=1 on a production server)
set -euo pipefail
cd "$(dirname "$0")/.."

info() { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
fail() { printf '\033[1;31mERROR:\033[0m %s\n' "$*" >&2; exit 1; }
envval() { grep -E "^$1=" .env 2>/dev/null | tail -1 | cut -d= -f2- || true; }

[[ -f .env ]] || fail "no .env file: run make setup first"
COMPOSE="docker compose"; [[ "${PROD:-}" == 1 ]] && COMPOSE="docker compose -f docker-compose.prod.yml"
DUMP_CMD=${DUMP_CMD:-"$COMPOSE exec -T postgres pg_dump -U agri -d agriproof --format=custom --no-owner"}
CHAIN_CMD=${CHAIN_CMD:-"$COMPOSE exec -T anvil cat /data/anvil-state.json"}

export BACKUP_PASSPHRASE=${BACKUP_PASSPHRASE:-$(envval BACKUP_PASSPHRASE)}
(( ${#BACKUP_PASSPHRASE} >= 24 )) || fail "BACKUP_PASSPHRASE in .env is missing or too short: run make setup"
DIR=${BACKUP_DIR:-$(envval BACKUP_DIR)}; DIR=${DIR:-$HOME/agriproof-backups}; DIR=${DIR/#\~/$HOME}
KEEP=${BACKUP_KEEP:-$(envval BACKUP_KEEP)}; KEEP=${KEEP:-14}
COPY=${BACKUP_COPY_DIR:-$(envval BACKUP_COPY_DIR)}; COPY=${COPY/#\~/$HOME}
mkdir -p "$DIR"; chmod 700 "$DIR"
umask 077

ts=$(date +%Y%m%d-%H%M%S)
work=$(mktemp -d); trap 'rm -rf "$work"' EXIT
enc() { openssl enc -aes-256-cbc -pbkdf2 -iter 200000 -salt -pass env:BACKUP_PASSPHRASE "$@"; }

info "Saving the database"
$DUMP_CMD > "$work/db.dump" || fail "database dump failed (is AgriProof running? try: make up)"
[[ -s "$work/db.dump" ]] || fail "database dump is empty"
cp .env "$work/env"
[[ -f .testnet-wallet ]] && cp .testnet-wallet "$work/testnet-wallet"
if [[ "$(envval CHAIN_ID)" == "31337" ]]; then
  $CHAIN_CMD > "$work/anvil-state.json" 2>/dev/null || rm -f "$work/anvil-state.json"
  [[ -s "$work/anvil-state.json" ]] || rm -f "$work/anvil-state.json"
fi

name="agriproof-$ts.backup"
tar -C "$work" -czf - . | enc -out "$DIR/$name.part"
# Prove the backup can be opened before keeping it.
listing=$(enc -d -in "$DIR/$name.part" | tar -tzf -) || { rm -f "$DIR/$name.part"; fail "backup check failed: could not read it back"; }
grep -q 'db.dump' <<<"$listing" || { rm -f "$DIR/$name.part"; fail "backup check failed: database missing"; }
mv "$DIR/$name.part" "$DIR/$name"
(cd "$DIR" && sha256sum "$name" > "$name.sha256")
info "Backup saved: $DIR/$name ($(du -h "$DIR/$name" | cut -f1))"

if [[ -n "$COPY" ]]; then
  if mkdir -p "$COPY" 2>/dev/null && cp "$DIR/$name" "$DIR/$name.sha256" "$COPY/"; then
    info "Extra copy saved in $COPY"
  else
    printf '\033[1;33mWARNING:\033[0m could not copy to %s\n' "$COPY" >&2
  fi
fi

# Keep only the newest backups.
for d in "$DIR" ${COPY:+"$COPY"}; do
  ls -1t "$d"/agriproof-*.backup 2>/dev/null | tail -n +$((KEEP + 1)) | while read -r old; do rm -f "$old" "$old.sha256"; done
done
echo "$(date -Is) ok $name" >> "$DIR/backup.log"
