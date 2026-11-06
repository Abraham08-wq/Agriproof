#!/usr/bin/env bash
# Run the backup automatically every day at 02:17 (cron). Run again to see the schedule; OFF=1 to remove.
set -euo pipefail
cd "$(dirname "$0")/.."
dir=$(pwd)
line="17 2 * * * cd $dir && ./scripts/backup.sh >> $dir/backup-cron.log 2>&1"
current=$(crontab -l 2>/dev/null | grep -vF "$dir && ./scripts/backup.sh" || true)
if [[ "${OFF:-}" == 1 ]]; then
  printf '%s\n' "$current" | sed '/^$/d' | crontab -
  echo "Daily backup turned off."; exit 0
fi
printf '%s\n%s\n' "$current" "$line" | sed '/^$/d' | crontab -
pass=$(grep -E '^BACKUP_PASSPHRASE=' .env | tail -1 | cut -d= -f2-)
cat <<MSG
Daily backup is on: every day at 02:17, while this computer is on.
Backups go to: $(grep -E '^BACKUP_DIR=' .env | cut -d= -f2- | sed "s|^$|$HOME/agriproof-backups|")

IMPORTANT: backups are encrypted with this passphrase. Without it they cannot be opened.
Write it on paper and keep it somewhere safe, away from this computer:

    $pass

MSG
