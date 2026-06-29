# URnetwork Public Beta Server + Custom Client Fork Implementation Plan

> For Hermes: Use `superpowers-subagent-driven-development` to implement this plan task-by-task.

**Goal:** Make the beta server publicly reachable on an IP address and modify the forked `connect` client repo so it installs from and points at our fork by default.

**Architecture:** On the server host (`74.50.11.113`), bind the beta API and connect services to the public IP instead of `127.0.0.1`. On the client side, fork `connect` and change the Linux install script plus the hard-coded `DefaultApiUrl`/`DefaultConnectUrl` constants to use the public beta endpoints. Keep changes minimal and clearly marked as beta overrides.

**Tech Stack:** Docker Compose, bash, Go.

---

## Task 1: Expose server API and connect on the public IP

**Objective:** Allow external clients to reach the beta server at `http://74.50.11.113:8080` and `ws://74.50.11.113:5080`.

**Files:**
- Modify: `/root/urnetwork/server/docker-compose.beta.yml`
- Modify: `/root/urnetwork/server/beta-setup.sh` (optional: add a `PUBLIC_IP` env var)
- Modify: `/root/urnetwork/server/BETA.md`

**Step 1: Determine public IP**
Use `curl -s ifconfig.me` or `ip route get 1.1.1.1`. Current host public IPv4: `74.50.11.113`.

**Step 2: Update compose environment**
For `api` and `connect` services, set `WARP_HOST: "74.50.11.113"` instead of `127.0.0.1`. Also add the host IP to port bindings if Docker needs to bind a specific interface (Docker `0.0.0.0` is fine; the service internally uses `WARP_HOST` for self-references and logging).

Example diff for `api`:
```yaml
    environment:
      WARP_HOST: "74.50.11.113"
    ports:
      - "74.50.11.113:8080:8080"
```

For `connect`:
```yaml
    environment:
      WARP_HOST: "74.50.11.113"
    ports:
      - "74.50.11.113:5080:80"
      - "74.50.11.113:15080:15080"
```

**Step 3: Update beta-setup.sh**
Add at the top:
```bash
PUBLIC_IP="${PUBLIC_IP:-74.50.11.113}"
```
Use `sed` or envsubst to inject the IP into `docker-compose.beta.yml` before `docker compose up`. Simpler: keep `WARP_HOST` as `127.0.0.1` in compose and override with `.env` or inline export. Recommended approach: keep compose using `WARP_HOST: "${PUBLIC_IP:-127.0.0.1}"` and have `beta-setup.sh` export `PUBLIC_IP`.

However, Docker Compose does not expand shell env vars in `ports:` host IP. So either:
- Use `0.0.0.0:8080:8080` (bind all interfaces) and set `WARP_HOST` to the public IP.
- Generate a compose override file from the script.

Recommended: bind all interfaces (`0.0.0.0`) for beta simplicity, and set `WARP_HOST` to the public IP.

**Step 4: Update BETA.md**
Document public endpoints:
```
API:     http://74.50.11.113:8080
Connect: ws://74.50.11.113:5080/
```

**Step 5: Verify firewall/security**
Ensure host firewall allows ports 8080/tcp, 5080/tcp, and 15080/tcp. On typical cloud hosts this may already be open; document the `ufw`/`iptables` command in BETA.md.

---

## Task 2: Modify forked connect install script to use our fork

**Objective:** Change `scripts/Provider_Install_Linux.sh` so it fetches releases from `Ryanmello07/connect` instead of `urnetwork/connect`.

**Files:**
- Modify: `/root/urnetwork/connect/scripts/Provider_Install_Linux.sh`
- Modify: `/root/urnetwork/connect/scripts/Provider_Install_Win32.ps1` (optional)
- Modify: `/root/urnetwork/connect/scripts/urnet-tools.ps1` (optional)

**Step 1: Update GitHub API base**
Change:
```sh
api_base="https://api.github.com/repos/urnetwork/connect"
```
to:
```sh
api_base="https://api.github.com/repos/Ryanmello07/connect"
```

**Step 2: Update comments/GitHub URL**
Change comments from `https://github.com/urnetwork/connect` to `https://github.com/Ryanmello07/connect`.

**Step 3: Update PowerShell scripts similarly**
For completeness, update `$GithubURLBase` and header comments in the `.ps1` scripts to point at `Ryanmello07/connect`.

**Step 4: Add a beta override note**
Add a comment near `api_base`:
```sh
# Beta fork: install from Ryanmello07/connect
```

---

## Task 3: Modify client default URLs to point at public beta server

**Objective:** Make `provider` and `connectctl` binaries default to the public beta server instead of `api.bringyour.com`/`connect.bringyour.com`.

**Files:**
- Modify: `/root/urnetwork/connect/provider/main.go`
- Modify: `/root/urnetwork/connect/connectctl/main.go`

**Step 1: Change constants**
In `provider/main.go`:
```go
const DefaultApiUrl = "http://74.50.11.113:8080"
const DefaultConnectUrl = "ws://74.50.11.113:5080"
```

In `connectctl/main.go`:
```go
const DefaultApiUrl = "http://74.50.11.113:8080"
const DefaultConnectUrl = "ws://74.50.11.113:5080"
```

**Step 2: Add build-time override option (optional but recommended)**
Make the constants overridable at link time:
```go
var DefaultApiUrl = "http://74.50.11.113:8080"
```
and in `Provider_Install_Linux.sh` pass `-ldflags "-X main.DefaultApiUrl=..."` if desired. For now, simple const changes are enough.

---

## Task 4: Build and test the provider binary against the public beta server

**Objective:** Compile `provider` and `connectctl` and confirm they can talk to the running public server.

**Files:**
- N/A (test only)

**Step 1: Build provider**
```bash
cd /root/urnetwork/connect
go build -o /tmp/provider ./provider
```

**Step 2: Create a beta network on the public server**
Use `connectctl`:
```bash
cd /root/urnetwork/connect
go run ./connectctl create-network \
  --api_url=http://74.50.11.113:8080 \
  --network_name=beta-test \
  --user_name=beta \
  --user_auth=beta@example.com \
  --password=betapass123
```

**Step 3: Verify login**
```bash
go run ./connectctl login-network \
  --api_url=http://74.50.11.113:8080 \
  --user_auth=beta@example.com \
  --password=betapass123
```

**Step 4: Run provider**
```bash
/tmp/provider provide \
  --api_url=http://74.50.11.113:8080 \
  --connect_url=ws://74.50.11.113:5080
```

Expected: provider starts, authenticates, opens platform transport, and begins providing.

---

## Task 5: Commit and push both forks

**Objective:** Save client changes to `Ryanmello07/connect` and server changes to `Ryanmello07/server`.

**Files:**
- N/A

**Step 1: Connect repo**
```bash
cd /root/urnetwork/connect
git checkout -b beta/custom-server
git add -A
git commit -m "feat: point install scripts and defaults to Ryanmello07 public beta server"
git push -u origin beta/custom-server
```

**Step 2: Server repo**
```bash
cd /root/urnetwork/server
git add -A
git commit -m "feat: bind beta server to public IP 74.50.11.113"
git push origin beta/self-contained-env
```

---

## Risks / Trade-offs

- **Public exposure:** Running an API server on a public IP with self-signed/local JWT keys is fine for beta but should never be used for production. Document that clearly in `BETA.md`.
- **IP changes:** If the host IP changes, `docker-compose.beta.yml` and `connect` constants must be updated. A better long-term fix is to make `PUBLIC_IP` dynamic in `beta-setup.sh`, but for now hard-code the discovered IP.
- **No TLS:** Using `ws://` and `http://` is acceptable for a beta lab network; real deployment needs TLS termination (e.g. reverse proxy with valid certs).
- **Firewall:** The user must open ports. We can provide commands but cannot configure their cloud provider.

---

## Open Questions

1. Should we add a cloudflare or reverse proxy in front, or keep direct public IP for now? (Recommended: direct public IP for simplicity.)
2. Should the client default to the public IP or to a configurable env var? (Recommended: const override for now; users can still pass `--api_url`.)
3. Do we need to expose port 15080 publicly? It is only used for internal exchange between connect instances; for a single connect service it can be left un-exposed. However, exposing it does not hurt for testing.
