# URnetwork Beta One-Command Launch — Implementation Plan

> For Hermes: use `superpowers-subagent-driven-development` to implement this plan task-by-task.

**Goal:** Provide a single `./beta-setup.sh` script that builds and launches a self-contained URnetwork beta server (API + connect) so a custom `connect` client can immediately talk to it.

**Architecture:** Docker Compose with a Go 1.26.3 builder image, Postgres 16, Redis 7. The setup script generates local JWT keys, free GeoLite2-City MMDB, and a tiny ARIN MMDB on first run, then starts the stack. The connect service separates its external WebSocket port (host 5080) from its internal exchange port (15080).

**Tech Stack:** Docker Compose, bash, OpenSSL, `github.com/maxmind/mmdbwriter` (via Go), free GeoLite2-City MMDB.

---

## Global Constraints

- No live third-party accounts or purchased keys.
- Only modify the fork (`Ryanmello07/server`).
- Generated secrets and large MMDBs must not be committed.
- The script must be idempotent: safe to run again after `./beta-down.sh` / cleanup.
- The connect client path must be simple: `ws://127.0.0.1:5080/`, API at `http://127.0.0.1:8080`.

---

## Current State

- Branch `beta/self-contained-env` exists and is pushed.
- `Dockerfile.beta`, `docker-compose.beta.yml`, `beta-entrypoint.sh`, `.env.example`, and `beta-vault/` stubs exist.
- `ip.go` already recognizes `GeoLite2-City` schema.
- The connect service crashes because the external HTTP listener and the internal exchange listener both try to bind container port 5080.

---

## Task 1: Fix Connect Port Conflict

**Objective:** Make connect bind its client HTTP/WebSocket port and internal exchange port on different container ports.

**Files:**
- Modify: `docker-compose.beta.yml`
- Modify: `connect/resident.go` (or find a config override)

**Step 1: Inspect how exchange internal port is set**
Read `connect/resident.go` around `StartInternalPort` and `DefaultExchangeSettings`.

**Step 2: Choose internal exchange port**
Use `15080` for the internal exchange, leaving `80` for the external client HTTP/WebSocket.

**Step 3: Update compose for connect service**
```yaml
connect:
  environment:
    WARP_PORTS: "80:5080,15080:15080"
  command: ["connect", "--port=80"]
  ports:
    - "5080:80"
```
Wait — this still maps client endpoint to host 5080, but inside the container the connect HTTP server listens on port 80. The internal exchange listens on 15080, and `15080:15080` is also mapped so cross-host exchange could work if ever needed (not required for single-box beta).

However, `WARP_PORTS` format is `service_port:host_port`. For connect to expose its client web socket on host 5080, we need `80:5080`. For the internal exchange to listen on container port 15080 with host port 15080, we need `15080:15080`.

But we must also change `DefaultExchangeSettings.StartInternalPort` from 5080 to 15080.

**Step 4: Patch `connect/resident.go`**
Change:
```go
StartInternalPort: 5080,
```
to:
```go
StartInternalPort: 15080,
```
Add comment: `// beta: use a non-conflicting internal exchange port`.

**Step 5: Verify build**
```bash
cd /root/urnetwork/server
go build ./...
```
Expected: PASS.

---

## Task 2: Create `beta-setup.sh`

**Objective:** One script to prepare secrets/MMDB, build, migrate, and start the beta network.

**Files:**
- Create: `/root/urnetwork/server/beta-setup.sh`
- Create: `/root/urnetwork/server/scripts/gen-beta-mmdb.go`

**Step 1: Write `scripts/gen-beta-mmdb.go`**
A tiny Go program that generates:
- `beta-vault/config/mmdb/ip-ipinfo.mmdb` — downloads free GeoLite2-City from GitHub wp-statistics mirror.
- `beta-vault/config/arindb/arin.mmdb` — minimal MMDB with a default record.

Use `github.com/maxmind/mmdbwriter`. Embed download URL:
`https://github.com/wp-statistics/GeoLite2-City/raw/master/GeoLite2-City.mmdb.gz`

**Step 2: Write `beta-setup.sh`**
```bash
#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")"

VAULT_DIR="./beta-vault"
TLS_DIR="$VAULT_DIR/vault/tls"
MMDB_DIR="$VAULT_DIR/config/mmdb"
ARIN_DIR="$VAULT_DIR/config/arindb"

mkdir -p "$TLS_DIR/ec" "$MMDB_DIR" "$ARIN_DIR"

# Generate JWT keys if missing
if [ ! -f "$TLS_DIR/jwt-rsa.pem" ]; then
    openssl genrsa -out "$TLS_DIR/jwt-rsa.pem" 2048
    openssl rsa -in "$TLS_DIR/jwt-rsa.pem" -pubout -out "$TLS_DIR/jwt-rsa.pub.pem"
fi
if [ ! -f "$TLS_DIR/ec/jwt-ec.pem" ]; then
    openssl ecparam -genkey -name prime256v1 -out "$TLS_DIR/ec/jwt-ec.pem"
    openssl ec -in "$TLS_DIR/ec/jwt-ec.pem" -pubout -out "$TLS_DIR/ec/jwt-ec.pub.pem"
fi

# Download or generate MMDBs if missing
if [ ! -f "$MMDB_DIR/ip-ipinfo.mmdb" ] || [ ! -f "$ARIN_DIR/arin.mmdb" ]; then
    go run ./scripts/gen-beta-mmdb.go
fi

# Build and start
docker compose -f docker-compose.beta.yml down -v 2>/dev/null || true
docker compose -f docker-compose.beta.yml build --no-cache
docker compose -f docker-compose.beta.yml up -d postgres redis
sleep 5
# wait for health
until docker compose -f docker-compose.beta.yml ps | grep -q healthy; do sleep 2; done
docker compose -f docker-compose.beta.yml run --rm migrate
docker compose -f docker-compose.beta.yml up -d api connect

echo "Beta network running:"
echo "  API:     http://127.0.0.1:8080"
echo "  Connect: ws://127.0.0.1:5080/"
```

Use proper health-check waiting with `docker compose ps` or `docker compose wait` if available. Replace `sleep` with `docker compose wait postgres redis` if Compose v5 supports it.

**Step 3: Make executable**
```bash
chmod +x beta-setup.sh
```

---

## Task 3: Create `beta-down.sh`

**Objective:** Simple teardown script.

**Files:**
- Create: `/root/urnetwork/server/beta-down.sh`

Contents:
```bash
#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")"
docker compose -f docker-compose.beta.yml down -v "$@"
```

Make executable.

---

## Task 4: Update `beta-vault/vault/jwt.yml`

**Objective:** Point at the generated key files.

**Files:**
- Modify: `beta-vault/vault/jwt.yml`

Contents:
```yaml
secret: "beta-test-jwt-secret-min-32-bytes-long!!"
issuer: "urnetwork-beta"
tls_key_paths:
  - "tls/jwt-rsa.pem"
  - "tls/ec/jwt-ec.pem"
```

**Note:** The subagent earlier created it pointing at `tls/`; verify. If it points at `tls/ec/` only, adjust.

---

## Task 5: Write `BETA.md` Runbook

**Objective:** Concise documentation for the beta test environment.

**Files:**
- Create: `/root/urnetwork/server/BETA.md`

Sections:
1. Quick start — `./beta-setup.sh`
2. Endpoints — API `http://127.0.0.1:8080`, connect `ws://127.0.0.1:5080/`
3. Stopping — `./beta-down.sh`
4. Rebuilding — `./beta-setup.sh` is idempotent
5. Connecting a custom `/connect` client — point it at the API URL and connect WebSocket URL.
6. What works / what doesn’t — auth, network, device, provider search works; payments, Apple/Google/Circle/Stripe/Brevo/Helius, email, payouts are disabled.
7. Files generated locally (not in git) — JWT keys, MMDBs.

---

## Task 6: Test End-to-End

**Objective:** Verify one-command launch and core endpoints.

**Steps:**
1. Run `./beta-down.sh` to ensure clean state.
2. Run `./beta-setup.sh`.
3. Wait for it to finish.
4. Run:
   ```bash
   curl http://127.0.0.1:8080/status
   curl -i -N -H "Upgrade: websocket" -H "Connection: Upgrade" http://127.0.0.1:5080/
   ```
5. Expected:
   - API returns JSON status.
   - WebSocket handshake returns HTTP 101 or at least not 500; connection will close if no protocol upgrade is accepted, which is fine.
6. `./beta-down.sh`

---

## Task 7: Commit and Push

**Objective:** Save the work to the fork.

**Steps:**
1. `git add -A`
2. `git commit -m "feat: one-command beta setup (beta-setup.sh), fix connect port conflict, BETA.md"`
3. `git push origin beta/self-contained-env`

---

## Risks / Trade-offs

- **MMDB download URL may break.** Fallback: document manual download step in `BETA.md`.
- **Internal exchange port `15080` might be in use on some hosts.** We map it to host `15080`; if conflict, the user can change `WARP_PORTS` and compose port mapping.
- **JWT key regeneration on fresh clone.** The script generates new keys every fresh checkout. That’s acceptable for a local beta.
- **Generated files not tracked.** Collaborators must run `./beta-setup.sh`; they cannot reuse keys/MMDBs from git.

---

## Open Questions

1. Should `beta-setup.sh` use random host ports for API/connect to avoid collisions? (Recommended: fixed 8080/5080 for simplicity.)
2. Should the script prune images on each run? (Recommended: `docker compose build` without `--no-cache` for speed; add `--no-cache` option.)
3. Do we need the `connect` internal exchange port exposed to the host at all? (Recommended: keep 15080:15080 mapped for clarity, but not strictly required for single-box testing.)
