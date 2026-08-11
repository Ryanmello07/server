#!/usr/bin/env bash
# Re-mint the egress-prober's client jwt and restart the prober.
#
# Why this exists: jwt.expiryDuration is 24h, so the prober's credential dies
# every day. When it does, every probe fails with no_consensus -- the tunnel
# cannot authenticate, so no geolocation source is reachable, and the failure
# looks like "all providers are bad" rather than "our token expired".
#
# The seedphrase in prober-account.txt is the root credential and the only way
# back into the network. Losing it is unrecoverable (the database stores a
# salted hash), which is exactly what happened to the previous prober account.
#
# Usage: refresh-prober-jwt.sh [--dry-run]
#   --dry-run  mint and verify a new jwt, but do not write prober.env or restart

set -euo pipefail

# readlink -f, not dirname alone: this is invoked through a symlink in
# /root/bin, where BASH_SOURCE points at the link and would resolve REPO to the
# wrong tree.
REPO="$(cd "$(dirname "$(readlink -f "${BASH_SOURCE[0]}")")/.." && pwd)"
ACCT="$REPO/beta-vault/prober-account.txt"
ENVF="$REPO/beta-vault/prober.env"
NET=server_prober
API=http://api:8080

DRY=0
[ "${1:-}" = "--dry-run" ] && DRY=1

need () { grep -E "^$1=" "$ACCT" | head -1 | cut -d= -f2-; }
SEED=$(need seedphrase)
CLIENT_ID=$(need client_id)
[ -n "$SEED" ] || { echo "no seedphrase in $ACCT" >&2; exit 1; }

curl_api () { docker run --rm --network "$NET" curlimages/curl:latest -s "$@"; }

# 1. seedphrase -> network jwt
LOGIN=$(curl_api -X POST -H 'Content-Type: application/json' \
  -d "$(python3 -c 'import json,sys; print(json.dumps({"seedphrase": sys.argv[1]}))' "$SEED")" \
  "$API/auth/login")
BYJWT=$(python3 -c '
import json,sys
d=json.loads(sys.stdin.read())
if d.get("error"): sys.exit("login failed: %s" % d["error"])
n=d.get("network") or {}
if not n.get("by_jwt"): sys.exit("login returned no by_jwt: %s" % sorted(d))
print(n["by_jwt"])' <<<"$LOGIN")

# 2. network jwt -> client jwt, reusing the SAME client_id so the prober keeps
#    one identity instead of accumulating a new client row every day
AC=$(curl_api -X POST -H "Authorization: Bearer $BYJWT" -H 'Content-Type: application/json' \
  -d "{\"client_id\":\"$CLIENT_ID\",\"description\":\"egress-prober\",\"device_spec\":\"egress-prober\"}" \
  "$API/network/auth-client")
CJWT=$(python3 -c '
import json,sys
d=json.loads(sys.stdin.read())
if d.get("error"): sys.exit("auth-client failed: %s" % d["error"])
if not d.get("by_client_jwt"): sys.exit("no by_client_jwt: %s" % sorted(d))
print(d["by_client_jwt"])' <<<"$AC")

# 3. prove the new token is actually accepted before we install it -- a token
#    that parses but is rejected would silently reproduce the outage
CODE=$(docker run --rm --network "$NET" curlimages/curl:latest -s -o /dev/null -w '%{http_code}' \
  -H "Authorization: Bearer $CJWT" "$API/network/clients")
[ "$CODE" = "200" ] || { echo "new jwt rejected with HTTP $CODE; not installing" >&2; exit 1; }

EXP=$(python3 -c '
import sys,json,base64,datetime
p=sys.argv[1].split(".")[1]; p+="="*(-len(p)%4)
d=json.loads(base64.urlsafe_b64decode(p))
print(datetime.datetime.fromtimestamp(d["exp"],datetime.timezone.utc).strftime("%Y-%m-%d %H:%M UTC"))' "$CJWT")

if [ "$DRY" = "1" ]; then
  echo "dry-run: minted a valid jwt (HTTP 200), expires $EXP; nothing written"
  exit 0
fi

python3 - "$ENVF" "$CJWT" <<'PY'
import os, sys
envf, cjwt = sys.argv[1], sys.argv[2]
lines = open(envf).read().splitlines()
out = [("UR_PROBER_BY_JWT=" + cjwt) if l.startswith("UR_PROBER_BY_JWT=") else l for l in lines]
open(envf, "w").write("\n".join(out) + "\n")
os.chmod(envf, 0o600)
PY

cd "$REPO"
docker compose -f docker-compose.beta.yml up -d --force-recreate egress-prober >/dev/null
echo "prober jwt refreshed; expires $EXP"
