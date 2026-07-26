# URnetwork Beta Test Environment

A self-contained URnetwork server for beta testing the API, WebSocket connect path, SOCKS5 proxy, and WireGuard client flow using a custom `connect` client. This environment intentionally omits paid or third-party integrations: Stripe, Apple App Store, Google Play, Circle, Coinbase, Brevo, Helius, AWS/SES, and payouts are disabled.

The server auto-detects its public IP and binds the API and connect services so they can be reached from another machine. All sensitive material is generated locally on first run and is not committed to the repository.

## Table of Contents

- [What is included](#what-is-included)
- [Prerequisites](#prerequisites)
- [Security notes](#security-notes)
- [Quick start](#quick-start)
- [Common operations](#common-operations)
- [Public access and firewall](#public-access-and-firewall)
- [Running locally only](#running-locally-only)
- [Solana wallet login](#solana-wallet-login)
- [Web dashboard](#web-dashboard)
- [Connect client / provider](#connect-client--provider)
- [SOCKS5 proxy](#socks5-proxy)
- [Provider egress prober](#provider-egress-prober)
- [Core API routes](#core-api-routes)
- [What works](#what-works)
- [What is disabled](#what-is-disabled)
- [Generated files](#generated-files)
- [Architecture](#architecture)
- [Frequently asked questions](#frequently-asked-questions)
- [Troubleshooting](#troubleshooting)
- [Advanced: running without Docker](#advanced-running-without-docker)
- [License](#license)

## What is included

- **Server fork**: `https://github.com/Ryanmello07/server`, branch `beta/self-contained-env`
- **Connect client fork**: `https://github.com/Ryanmello07/connect`, branch `beta/custom-server`
- **Web manager fork**: `https://github.com/Ryanmello07/urnetwork-webmanager-beta`, branch `beta/main`
- **Proxy fork**: `https://github.com/Ryanmello07/proxy`, branch `beta/custom-server`

The server contains:

- Postgres 16 database with all migrations applied by `bringyourctl db migrate`.
- Redis 7 session and cache store.
- Locally generated JWT signing keys (RSA + EC).
- Free GeoLite2-City and synthetic ARIN MMDBs for IP geolocation.
- The Solana wallet challenge flow from `urnetwork/server#402`.

## Prerequisites

- Linux host with Docker Engine 24+ and Docker Compose v2.
- `curl`, `openssl`, and Go 1.26+ installed on the host (Go is only used by setup scripts that generate the MMDB).
- Public IP address if you want to use the server from another machine.
- TCP ports 80 and 443 available on the host (Caddy terminates tls and is the only ingress).

## Security notes

`./beta-setup.sh` generates all sensitive material locally on first run:

- `beta-vault/beta-secrets.env` — Postgres password, Redis password, JWT symmetric secret, password pepper, client IP hash pepper, proxy signing secret, and provider-egress ingest secret.
- `beta-vault/vault/tls/*.pem` — JWT RSA and EC signing keys.
- `beta-vault/vault/*.yml` — runtime vault files consumed by the Go services.

These generated files are ignored by Git. Only `.example` templates and config stubs are tracked. Do **not** commit `beta-secrets.env`, `*.pem`, `*.mmdb`, or the real `beta-vault/vault/*.yml` files.

To fully rotate secrets:

```bash
./beta-down.sh
rm -f beta-vault/beta-secrets.env
rm -f beta-vault/vault/pg.yml beta-vault/vault/redis.yml beta-vault/vault/jwt.yml \
      beta-vault/vault/password.yml beta-vault/vault/client.yml beta-vault/vault/proxy.yml \
      beta-vault/vault/provider_egress.yml
rm -rf beta-vault/vault/tls beta-vault/config/mmdb beta-vault/config/arindb
./beta-setup.sh
```

> **Warning**: Running `./beta-down.sh` without `-v` keeps the named Docker volume, so Postgres data survives. Running `./beta-down.sh -v` deletes the volume and resets the database. Rotating secrets does not require deleting the volume, but if you also delete the data volume you will lose all user accounts and network data.

> **Note**: the provider-egress-location ingest endpoint (used by an operator-run prober that learns a provider's real egress location and submits it so the server can prefer it over the built-in mmdb lookup) is disabled until `beta-vault/vault/provider_egress.yml` exists with a non-empty `ingest_secret` — `./beta-setup.sh` generates this automatically. The secret is memoized in the API process on first request, so the `api` service must be restarted after creating or rotating it: `docker compose -f docker-compose.beta.yml restart api`.

## Quick start

```bash
git clone -b beta/self-contained-env https://github.com/Ryanmello07/server.git
cd server
./beta-setup.sh
```

The first run takes several minutes because it:

1. Generates random secrets.
2. Generates JWT RSA/EC keys.
3. Downloads the free GeoLite2-City MMDB and builds a synthetic ARIN MMDB.
4. Builds the Docker images from the multi-module Go workspace.
5. Applies all database migrations.
6. Starts Postgres, Redis, the API server, and the connect server.

When finished you will see output similar to:

```
Beta network running:
  API:     https://api.beta-test.net
  Connect: wss://connect.beta-test.net/

All generated secrets live in ./beta-vault/beta-secrets.env and beta-vault/vault/*.yml
These files are NOT tracked by git.
Reachable from anywhere over https; no other ports need opening.
```

Replace `74.50.11.113` with your host's public IP.

### Verify the stack

```bash
# Health check
curl -s https://api.beta-test.net/status | jq .

# WebSocket upgrade handshake
curl -fsS -i -N \
  -H 'Upgrade: websocket' \
  -H 'Connection: Upgrade' \
  -H 'Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==' \
  -H 'Sec-WebSocket-Version: 13' \
  --http1.1 \
  https://connect.beta-test.net/
```

You should receive `HTTP/1.1 101 Switching Protocols`. Use `--http1.1`: over
HTTP/2 a `Connection: Upgrade` header is illegal, so curl reports 400 and it
looks like an outage when the service is fine.

## Common operations

### Stop the environment

```bash
./beta-down.sh
```

This stops and removes containers and networks but keeps the Docker volume named `server_postgres_data` so the database is preserved across restarts.

### Stop and reset everything including database

```bash
./beta-down.sh -v
```

This also removes the Postgres volume. Use this when you want a completely fresh install.

### Rebuild after code changes

`beta-setup.sh` is idempotent and preserves data. Re-running it rebuilds images, regenerates any missing secrets, and restarts services without touching the Postgres volume:

```bash
./beta-setup.sh
```

To wipe the database and start fresh, run:

```bash
./beta-down.sh -v
./beta-setup.sh
```

If you only changed code and want to keep data and secrets without running the full setup script:

```bash
./beta-down.sh              # keeps volume
docker compose -f docker-compose.beta.yml build --no-cache
docker compose -f docker-compose.beta.yml up -d api connect taskworker
```

### View logs

```bash
# All services
docker compose -f docker-compose.beta.yml logs -f

# One service
docker compose -f docker-compose.beta.yml logs -f api
```

## Public access and firewall

The beta server binds ports on all interfaces (`0.0.0.0`). To reach it from another machine, the host firewall must allow inbound traffic on:

- `80/tcp` — Caddy; redirects to https and answers the ACME http-01 challenge
- `443/tcp` — Caddy; terminates tls for `api.beta-test.net` and `connect.beta-test.net`

The api and connect service ports are deliberately **not** published. Caddy is
the only ingress, and that is load-bearing beyond transport security: connect
trusts a forwarded client address only when it receives both `X-Forwarded-For`
and `X-Forwarded-Source-Port` (`connect/transport.go:220`). A client arriving
on a published port presents the docker gateway address instead — a private
address with no mmdb entry — so no location row is written and the provider
never appears in the provider list.

Example with `ufw`:

```bash
sudo ufw allow 80/tcp
sudo ufw allow 443/tcp
```

Example with `iptables`:

```bash
sudo iptables -A INPUT -p tcp --dport 80 -j ACCEPT
sudo iptables -A INPUT -p tcp --dport 443 -j ACCEPT
```

Remember that the beta server uses plain HTTP and WebSocket, not TLS. Do not send real production secrets through it.

## Running locally only

Set `PUBLIC_IP=127.0.0.1` before running `./beta-setup.sh` to keep the server local-only:

```bash
PUBLIC_IP=127.0.0.1 ./beta-setup.sh
```

In this mode the server is reachable only from the same host. `74.50.11.113` in this document should be replaced with `127.0.0.1`.

## Solana wallet login

This beta server uses the server-issued wallet challenge from `urnetwork/server#402`. Signing up with a Solana wallet does not require email or phone verification.

### Get a challenge

```bash
curl -s -X POST https://api.beta-test.net/auth/wallet-challenge \
  -H "Content-Type: application/json" \
  -d '{}'
```

Expected response:

```json
{
  "challenge": "...",
  "timestamp": 1234567890,
  "expires_in": 300,
  "message_template": "Sign in to URnetwork\nChallenge: ...\nTimestamp: ..."
}
```

### Create a network with a wallet signature

Sign `message_template` with the wallet's private key, base64-encode the signature, and post to `/auth/network-create`:

```json
{
  "user_name": "alice",
  "network_name": "alicenet",
  "terms": true,
  "wallet_auth": {
    "wallet_address": "<base58 public key>",
    "wallet_signature": "<base64 signature>",
    "wallet_message": "<exact message_template string>",
    "blockchain": "SOL",
    "challenge": "<challenge value>",
    "timestamp": 1234567890
  }
}
```

The response contains a `by_jwt` that authenticates subsequent requests.

## Web dashboard

The beta web manager lets users sign in with Phantom or Solflare and manage the network.

Repository: `https://github.com/Ryanmello07/urnetwork-webmanager-beta`, branch `beta/main`.

```bash
git clone -b beta/main https://github.com/Ryanmello07/urnetwork-webmanager-beta.git
cd urnetwork-webmanager-beta
npm install
VITE_API_BASE=https://api.beta-test.net npm run dev
```

If you deploy the dashboard to a static host such as Bolt or Netlify, use its `_redirects` file to proxy `/api/*` to the beta server. This avoids mixed-content and CORS issues when the dashboard is served over HTTPS.

```
/api/* https://api.beta-test.net/:splat 200
/* /index.html 200
```

## Connect client / provider

Repository: `https://github.com/Ryanmello07/connect`, branch `beta/custom-server`.

The fork defaults to the beta server:

```go
apiUrl := "https://api.beta-test.net"
connectUrl := "wss://connect.beta-test.net/"
```

### Run a provider with a dashboard JWT

The patched provider CLI accepts `--by-jwt` so you do not need to create a `~/.urnetwork/jwt` file first:

```bash
git clone -b beta/custom-server https://github.com/Ryanmello07/connect.git
cd connect
go run ./provider auth-provide --by-jwt=<DASHBOARD_JWT> --port=15081
```

The provider registers itself with the server and appears in `/network/provider-locations` after the background taskworker aggregates counts. To make provider locations populate immediately, you can trigger the taskworker from the server repo:

```bash
cd /path/to/server
docker compose -f docker-compose.beta.yml exec api bringyourctl taskworker eval-tasks
```

> Note: Only run providers on the server host for technical testing. For a real multi-node test, run providers on separate machines to avoid port and resource conflicts.

## SOCKS5 proxy

Repository: `https://github.com/Ryanmello07/proxy`, branch `beta/custom-server`.

The proxy CLI also defaults to the beta server and supports `--by-jwt`.

```bash
git clone -b beta/custom-server https://github.com/Ryanmello07/proxy.git
cd proxy
go run ./socks --by-jwt=<DASHBOARD_JWT> --country US
```

This creates an authenticated proxy client via `/network/auth-client`, selects a provider in the requested country, and starts a local SOCKS5 proxy (default `127.0.0.1:9999`). At least one provider must be connected for the proxy to find a route.

To generate a WireGuard config instead of SOCKS5, open an issue on the proxy fork or use the dashboard's wireguard flow once it is wired to the beta API.

## Provider egress prober

The `egress-prober` service measures where each provider's traffic actually
comes out. For every provider it opens a tunnel **through that provider** and
asks public geolocation APIs what address they see, then submits the answer to
the server's provider-egress ingest endpoint, so the server can prefer the
measured location over the built-in mmdb lookup.

It lives in a separate repository:
`https://github.com/Ryanmello07/urnetwork-operator-proxy`.

### The confinement is the network attachment

The prober must never reach a geolocation API except through a provider tunnel.
A direct lookup would record **this host's** location for the provider and hand
the operator's own address to third-party APIs.

That is enforced by the `prober` network in `docker-compose.beta.yml`, which is
`internal: true`: containers on it have no gateway and no NAT, so nothing on it
can reach the internet. The prober is on that network and no other, which lets
it reach `api` and `connect` and nothing else. Do not attach it to `default`,
do not publish ports from it, and never add `--skip-confinement-check` — a
check disabled in a config file is not a check.

The prober verifies its own confinement at startup and exits non-zero if it is
wrong, so a misconfigured network stops the prober instead of leaking silently.

Because an `internal: true` network cannot resolve external names at all
(docker's embedded resolver answers SERVFAIL, having no route to forward the
query), the self-check would otherwise be unable to resolve the geolocation
hosts, would obtain no evidence either way, and would refuse to start. The
service therefore passes `--confinement-address` for each geolocation endpoint,
which keeps the check real without needing DNS. Those addresses are a second
copy of the endpoint list — keep them in step with the hostnames the prober
logs on every start.

For the non-Docker deployment see
[docs/operator/prober-systemd.md](docs/operator/prober-systemd.md), which
confines the process with `IPAddressDeny=any` instead and takes the other route
on DNS: it allows the loopback stub resolver, so the check resolves the hosts
and then finds them unreachable.

### Secrets

Copy the template and fill in both values:

```bash
cp beta-vault/prober.env.example beta-vault/prober.env
chmod 600 beta-vault/prober.env
```

| Variable | Where it comes from |
|---|---|
| `UR_PROBER_BY_JWT` | A network **client** jwt: create a network, then `POST /network/auth-client` with the dashboard jwt and use the `by_client_jwt` it returns. A plain user jwt has no `client_id` and the prober exits with `parse by-jwt client id`. |
| `UR_OPERATOR_SECRET` | Must equal `ingest_secret` in `beta-vault/vault/provider_egress.yml`, generated by `./beta-setup.sh`: `grep ingest_secret beta-vault/vault/provider_egress.yml`. |

The real `beta-vault/prober.env` is gitignored; only the `.example` template is
tracked. Both secrets are passed as environment variables, never on the command
line, so they do not appear in `docker inspect` args or in `ps`.

### Starting it

The service sits behind the `prober` compose profile, so `./beta-setup.sh`
neither builds nor starts it. Clone the prober repository next to this one (the
same parent directory `Dockerfile.beta` builds from) and start the service by
name:

```bash
git clone https://github.com/Ryanmello07/urnetwork-operator-proxy.git ../urnetwork-operator-proxy
docker compose -f docker-compose.beta.yml build egress-prober
docker compose -f docker-compose.beta.yml up -d --no-deps egress-prober
```

The prober's `go.mod` replaces `github.com/urnetwork/connect` and `.../glog`
with `../connect` and `../glog`, so those checkouts take part in the build too.
`PROBER_CONTEXT`, `CONNECT_CONTEXT` and `GLOG_CONTEXT` override the three build
contexts if your checkouts live elsewhere.

> **One-time step**: `api` and `connect` now also attach to the `prober`
> network, since the prober has no other way to reach them. An already-running
> stack keeps its old attachments until those two containers are recreated:
> `docker compose -f docker-compose.beta.yml up -d api connect`. Until then the
> prober starts and stays confined, but every pass logs
> `lookup api on 127.0.0.11:53: server misbehaving`.

### Confirming the self-check passed

```bash
docker compose -f docker-compose.beta.yml logs --tail=20 egress-prober
```

Confined and healthy — the container keeps running:

```
egress-prober: confinement self-check: dialing the 4 address(es) given with -confinement-address, skipping resolution: 134.119.216.174:443 ... (geolocation hosts: ip.pn free.freeipapi.com ipinfo.io -- keep these addresses current with that list)
egress-prober: confinement self-check passed: 4 address(es) tested, none directly reachable
```

Not confined — the container exits 1 and, under `restart: unless-stopped`,
restarts into the same failure rather than probing:

```
egress-prober: confinement self-check failed: confinement: a direct connection to a geolocation address succeeded; this process is not confined: 134.119.216.174:443
egress-prober: this process must not be able to reach a geolocation api except through a provider tunnel. Confine it ... and start it again.
```

Check the exit code and the attached networks directly with:

```bash
docker inspect -f 'exit={{.State.ExitCode}} restarts={{.RestartCount}} nets={{range $k,$v := .NetworkSettings.Networks}}{{$k}} {{end}}' server-egress-prober-1
```

`nets` must be `server_prober` and nothing else.

## Core API routes

| Route | Method | Purpose |
|---|---|---|
| `/status` | GET | Health and version check |
| `/hello` | GET | Basic hello endpoint |
| `/auth/wallet-challenge` | POST | Request a Solana wallet sign-in challenge |
| `/auth/network-check` | POST | Check if a network name is available |
| `/auth/network-create` | POST | Create a network using Solana wallet auth |
| `/auth/login` | POST | Log in using Solana wallet auth |
| `/auth/verify-send` | POST | Request an email/SMS auth code — **disabled** |
| `/auth/verify` | POST | Verify an email/SMS auth code — **disabled** |
| `/network/user` | GET | Authenticated user/network info |
| `/network/clients` | GET | List clients in the network |
| `/network/provider-locations` | GET | Available provider locations |
| `/network/auth-client` | POST | Create an authenticated client for proxy/provider use |
| `/connect` | GET/POST | Connect auth endpoint used before the WebSocket |
| `/` on connect service | WebSocket upgrade | Client connect tunnel |

## What works

- Postgres database with all migrations applied.
- Redis session and cache store.
- JWT auth using locally generated RSA and EC keys.
- Solana wallet login and network creation through server-issued challenge.
- Core network, user, device, client, and provider-location APIs.
- Connect WebSocket upgrade and internal resident exchange.
- IP geolocation using the free GeoLite2-City MMDB.
- SOCKS5 proxy authentication against `/network/auth-client`.

## What is disabled

Endpoints that require an external account or paid API key will return an error or no-op:

- Email and phone verification (`/auth/verify-send`, `/auth/verify`, password reset)
- Stripe (`/pay/stripe`, `/stripe/*`)
- Apple App Store (`/apple/notification`)
- Google Play (`/pay/play`)
- Circle (`/pay/circle`, `/wallet/circle-*`)
- Coinbase (`/pay/coinbase`)
- Solana/Helius (`/pay/solana`, `/solana/*`)
- Brevo (`/updates/brevo`)
- AWS/SES email sending
- Payouts and wallet withdrawals

These routes are still registered but their handlers fail gracefully because the corresponding vault/config files contain only stub values.

## Generated files

The first run of `./beta-setup.sh` creates these files. They are ignored by Git and must never be committed:

```
beta-vault/beta-secrets.env
beta-vault/vault/pg.yml
beta-vault/vault/redis.yml
beta-vault/vault/jwt.yml
beta-vault/vault/password.yml
beta-vault/vault/client.yml
beta-vault/vault/proxy.yml
beta-vault/vault/provider_egress.yml
beta-vault/vault/tls/jwt-rsa.pem
beta-vault/vault/tls/jwt-rsa.pub.pem
beta-vault/vault/tls/ec/jwt-ec.pem
beta-vault/vault/tls/ec/jwt-ec.pub.pem
beta-vault/config/mmdb/ip-ipinfo.mmdb
beta-vault/config/arindb/arin.mmdb
```

`beta-vault/prober.env` is also gitignored. It is not generated — you create it
from `beta-vault/prober.env.example` if you run the
[provider egress prober](#provider-egress-prober).

To share the beta environment with a teammate, share the repository and have them run `./beta-setup.sh` on their own host. Never share `beta-secrets.env` or the PEM keys between independent deployments.

## Architecture

```text
+--------+  https  +-----------------------------+
| Client | <-----> | Caddy :80 :443              |
+--------+         |  - the ONLY ingress         |
                   |  - lets encrypt certs       |
                   |  - sets the forwarded       |
                   |    address headers connect  |
                   |    needs to see real client |
                   |    ips                      |
                   +-----------------------------+
                         |                 |
          api.beta-test  |                 | connect.beta-test
                   +-----v-------+  +------v----------------------+
                   | API :8080   |  | Connect :80                 |
                   | (unpublished)|  |  - WebSocket upgrade on /   |
                   +-------------+  |  - Internal exchange :15080 |
                                    |  (both unpublished)         |
                                    +-----------------------------+
                              |
                   +----------v----------+
                   | Postgres :5432      |
                   | Redis :6379         |
                   +---------------------+
```

## Frequently asked questions

**Q: Why does the beta server use plain HTTP and WS instead of HTTPS/WSS?**

A: The goal is a self-contained test environment with no domain or certificate management. Use a reverse proxy such as Nginx, Caddy, or a static-host rewrite if you need TLS.

**Q: Can I reuse the same JWT across the dashboard, provider, and proxy?**

A: Yes. The `by_jwt` obtained from the wallet login is a network-level user JWT. The provider CLI consumes it to obtain a client JWT, and the proxy CLI does the same.

**Q: Why is `/network/provider-locations` empty?**

A: It returns locations only when at least one provider is connected and the server's background taskworker has aggregated provider counts. Start a provider with `go run ./provider auth-provide --by-jwt=<JWT>`.

**Q: Can I run multiple providers?**

A: Yes. Run the provider CLI on separate hosts or in separate containers, each with its own `--port`, to avoid conflicts.

**Q: Will my data survive a `./beta-setup.sh` rerun?**

A: Yes, as long as you do not run `./beta-down.sh -v` and do not delete the generated secrets. `beta-setup.sh` reuses existing `beta-secrets.env` and vault `.yml` files if they exist.

**Q: How do I completely reset?**

A: Run `./beta-down.sh -v`, delete the generated files listed in [Generated files](#generated-files), and run `./beta-setup.sh` again.

**Q: Is this safe to expose to the public internet?**

A: It is reasonably secure for beta testing because all secrets are generated locally and no production payment or messaging integrations are active. It is still plain HTTP/WS, so do not send real user data or credentials through it.

## Troubleshooting

### Port already in use

Only Caddy publishes ports (`80` and `443`). If either is busy, stop whatever
holds it — they cannot be remapped, because Let's Encrypt's http-01 challenge
and public https both require the standard ports.

The api and connect service ports are intentionally unpublished; do not add
`ports:` entries for them. See *Firewall* above for why that is load-bearing.

### The provider list is empty

Check whether connect can see real client addresses:

```bash
docker compose -f docker-compose.beta.yml logs --since 5m connect | grep -c 'could not find client location'
```

Anything above zero means connect is recording a private proxy address for
clients, so no location row is written. Confirm the Caddyfile still sends
`X-Forwarded-Source-Port`, and note that the Caddyfile is a **single-file bind
mount**: editing it on the host replaces the inode, the container keeps reading
the old one, and `caddy reload` reports success while reloading stale content.
Apply changes with:

```bash
docker compose -f docker-compose.beta.yml up -d --force-recreate caddy
```

If you change the internal exchange port mapping, update `connect/resident.go`:

```go
StartInternalPort: <your-port>,
```

and update `WARP_PORTS` in `docker-compose.beta.yml` accordingly.

### `failed to fetch` from the browser dashboard

If the dashboard is served over HTTPS and tries to call `https://api.beta-test.net`, browsers block mixed content. Use the dashboard's `/api/*` rewrite or run the dashboard locally over HTTP:

```bash
VITE_API_BASE=https://api.beta-test.net npm run dev
```

For a static HTTPS host, route `/api/*` to the beta server with a 200 rewrite, not a 302 redirect.

### 401 on authenticated endpoints right after login

The most common causes are:

1. The API container was rebuilt with a new JWT signing key after the token was issued. Restart the dashboard and log in again to get a new token.
2. The browser or host rewrote the request and dropped the `Authorization: Bearer <token>` header. Inspect the request in browser DevTools to confirm the header is present.

### MMDB download fails

`beta-setup.sh` downloads the free GeoLite2-City MMDB from a public mirror. If the URL changes or is blocked, you can manually download the `.mmdb` and place it at:

```
beta-vault/config/mmdb/ip-ipinfo.mmdb
```

Then re-run `./beta-setup.sh`.

### Docker daemon not running

The script requires a running Docker daemon with Buildx and Compose. Start it with:

```bash
sudo systemctl start docker
# or
sudo service docker start
```

### Migrations fail or Postgres does not start

Check that the generated `beta-vault/beta-secrets.env` matches the real `beta-vault/vault/pg.yml`:

```bash
cat beta-vault/beta-secrets.env | grep POSTGRES_PASSWORD
cat beta-vault/vault/pg.yml
```

If they differ, stop the stack and delete both files so `./beta-setup.sh` regenerates them consistently.

### Redis connection errors

The Redis container now requires authentication. Confirm the `redis.yml` password matches the `REDIS_PASSWORD` value in `beta-secrets.env`. The Compose healthcheck uses the same password.

### Provider fails to authenticate

The provider CLI needs a valid `by_jwt`. Obtain one from:

- The dashboard's local storage after login, or
- The `/auth/wallet-challenge` + `/auth/network-create` flow.

Then start it with `--by-jwt=<TOKEN>` from the patched `Ryanmello07/connect` fork.

### Proxy reports no providers

Before using the SOCKS5 proxy, connect at least one provider. Use the provider CLI or ask someone to run one against the beta server. Then verify:

```bash
curl -s https://api.beta-test.net/network/provider-locations | jq '.locations | length'
```

If the count is 0, no provider is registered yet.

## Advanced: running without Docker

Set up local Postgres and Redis, populate `/srv/warp` using the `beta-vault` templates, generate the JWT keys and MMDBs with `openssl` and `./scripts/gen-beta-mmdb.go`, then run:

```bash
cd /root/urnetwork/server
export WARP_ENV=local
export WARP_BLOCK=local
export WARP_SERVICE=api
export WARP_VERSION=0.0.0-beta
export WARP_HOST=127.0.0.1
export BRINGYOUR_POSTGRES_HOSTNAME=localhost
export BRINGYOUR_REDIS_HOSTNAME=localhost
export WARP_HOME=/srv/warp
go run ./cli/api --port=8080
```

The `./beta-setup.sh` and `./beta-down.sh` scripts are the supported path; running without Docker is undocumented and error-prone.

## License

Same as URnetwork server: MPL 2.0.
