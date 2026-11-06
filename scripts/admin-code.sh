#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
USERNAME=$(docker compose exec -T postgres psql -U agri -d agriproof -tAc "SELECT username FROM admins ORDER BY created_at LIMIT 1" | tr -d '[:space:]')
[[ -n "$USERNAME" ]] || { echo "No admin account yet. Create one with: make admin"; exit 1; }
SECRET=$(docker compose exec -T postgres psql -U agri -d agriproof -tAc "SELECT totp_secret FROM admins WHERE username='$USERNAME'" | tr -d '[:space:]')
python3 - "$SECRET" "$USERNAME" <<'PY'
import base64, hmac, struct, sys, time
s, user = sys.argv[1], sys.argv[2]
k = base64.b32decode(s + "=" * (-len(s) % 8), casefold=True)
now = time.time(); h = hmac.new(k, struct.pack(">Q", int(now) // 30), "sha1").digest()
o = h[-1] & 15
print(f"\n  Username: {user}\n  Code:     {(struct.unpack('>I', h[o:o+4])[0] & 0x7FFFFFFF) % 1000000:06d}   (works for {30 - int(now) % 30} more seconds)\n")
PY
