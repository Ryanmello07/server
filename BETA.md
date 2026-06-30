# URnetwork Beta Test Environment

A self-contained URnetwork beta server for testing the API and connect WebSocket path with a custom `connect` client. No Stripe, Apple, Google, Circle, Brevo, Helius/Solana, AWS/email, or payout integrations are required.

This beta server auto-detects its public IP and binds the API/connect services so they can be used from another machine.

## Security notes

`./beta-setup.sh` generates all sensitive material locally on first run:

- `beta-vault/beta-secrets.env` — Postgres, Redis, JWT secret, peppews, proxy secret.
- `beta-vault/vault/tls/jwt-*.pem` — JWT signing keys.
- `beta-vault/vault/*.yml` — vauwt config fiwes.

These genewated fiwes awe ignored by Git — onwy the `.example` tempwates awe twacked. Do **not** commit `beta-secrets.env`, `*.pem`, `*.mmdb`, ow the weaw `beta-vault/vault/*.yml` fiwes.

To fuwwy wotate secwets, stop the stack, dewete the genewated fiwes above, and wun `./beta-setup.sh` again. Existing Postgres vowume data wiww be wost if you wun `./beta-down.sh -v`.

## Quick start

```bash
cd /root/urnetwork/server   # or wherever you cloned the fork
./beta-setup.sh
```

After a few minutes (initial Docker build + MMDB download), you will have endpoints printed like:

```
Beta network running:
  API:     http://74.50.11.113:8080
  Connect: ws://74.50.11.113:5080/

To use from another machine, open TCP ports 8080, 5080, and 15080.
```

(`74.50.11.113` is the current host's public IP; yours will differ.)

### Firewall / public access

The beta server binds ports on all interfaces (`0.0.0.0`). To reach it from another machine, ensure the host firewall allows inbound traffic on:

- `8080/tcp` — API
- `5080/tcp` — client WebSocket
- `15080/tcp` — internal exchange (only needed if you run multiple connect instances)

Example with `ufw`:

```bash
sudo ufw allow 8080/tcp
sudo ufw allow 5080/tcp
sudo ufw allow 15080/tcp
```

### Disabling public access

Set `PUBLIC_IP=127.0.0.1` before running `./beta-setup.sh` to keep the server local-only:

```bash
PUBLIC_IP=127.0.0.1 ./beta-setup.sh
```

## Stop the environment

```bash
./beta-down.sh
```

This stops and removes all containers and volumes.

## Rebuild / reset

`beta-setup.sh` is idempotent. Run it again to rebuild images, regenerate secrets if missing, and restart:

```bash
./beta-down.sh
./beta-setup.sh
```

## Solana wallet login (beta)

This beta server includes the server-issued wallet challenge from `urnetwork/server#402`. Wallet login and wallet network creation do **not** require email or phone verification.

### Get a challenge

```bash
curl -s -X POST http://74.50.11.113:8080/auth/wallet-challenge \
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

### Web dashboard

Use the beta dashboard repo to sign in with Phantom/Solflare:

- Repo: https://github.com/Ryanmello07/urnetwork-webmanager-beta
- Branch: `beta/main`

```bash
git clone https://github.com/Ryanmello07/urnetwork-webmanager-beta.git
cd urnetwork-webmanager-beta
npm install
VITE_API_BASE=http://74.50.11.113:8080 npm run dev
```

The dashboard already defaults to the public beta server IP, but you can override it with `VITE_API_BASE`.

### Connect client

The forked `connect` repository (`github.com/Ryanmello07/connect`) is the **client library**.

- Branch: `beta/custom-server`
- Default URLs point to the public beta server:
  ```go
  apiUrl := "http://74.50.11.113:8080"
  connectUrl := "ws://74.50.11.113:5080/"
  ```

Install scripts (`Provider_Install_Linux.sh`, PowerShell scripts) now fetch from `Ryanmello07/connect` releases.

## Connecting a custom client

The forked `connect` repository (`github.com/Ryanmello07/connect`) is the **client library**. You do not need to run it as a service. Build your client against that library and point it at the beta server:

```go
apiUrl := "http://74.50.11.113:8080"     // replace with your beta-server public IP
connectUrl := "ws://74.50.11.113:5080/" // replace with your beta-server public IP
```

Or pass `--api_url` and `--connect_url` to the `provider` / `connectctl` binaries.

### Core API routes useful for testing

| Route | Method | Purpose |
|---|---|---|
| `/status` | GET | Health/version check |
| `/hello` | GET | Basic hello endpoint |
| `/auth/wallet-challenge` | POST | Request a Solana wallet sign-in challenge |
| `/auth/network-check` | POST | Check if a network name exists |
| `/auth/network-create` | POST | Create a beta network (Solana wallet supported) |
| `/auth/login` | POST | Log in (Solana wallet supported) |
| `/auth/verify-send` | POST | Request an auth code — **disabled in beta** |
| `/auth/verify` | POST | Verify an auth code — **disabled in beta** |
| `/connect` | GET/POST | Auth endpoint used by clients before opening the WebSocket |
| `/` on connect service | WebSocket upgrade | Client connect tunnel |

## What works

- Postgres database with all migrations applied.
- Redis session/cache store.
- JWT auth using locally-generated keys.
- **Solana wallet login and network creation** via server-issued challenge (`/auth/wallet-challenge`).
- Core network, device, client, and provider-location APIs.
- Connect WebSocket upgrade and internal resident exchange.
- IP geolocation using the free GeoLite2-City MMDB.

## What is intentionally disabled

Anything that needs an external account or paid key will return an error or no-op if called:

- Email/phone verification (`/auth/verify-send`, `/auth/verify`, password reset)
- Stripe (`/pay/stripe`, `/stripe/*`)
- Apple App Store (`/apple/notification`)
- Google Play (`/pay/play`)
- Circle (`/pay/circle`, `/wallet/circle-*`)
- Coinbase (`/pay/coinbase`)
- Solana/Helius (`/pay/solana`, `/solana/*`)
- Brevo (`/updates/brevo`)
- AWS/SES email sending
- Payouts and wallet withdrawals

These endpoints are still routed but their handlers will fail gracefully because the vault/config files contain only stub values.

## Files generated locally

The first run of `beta-setup.sh` creates these files. They are ignored by git and must not be committed:

```
beta-vault/vault/tls/jwt-rsa.pem
beta-vault/vault/tls/jwt-rsa.pub.pem
beta-vault/vault/tls/ec/jwt-ec.pem
beta-vault/vault/tls/ec/jwt-ec.pub.pem
beta-vault/config/mmdb/ip-ipinfo.mmdb
beta-vault/config/arindb/arin.mmdb
```

To fully reset secrets, delete `beta-vault/beta-secrets.env`, the JWT keys, the MMDBs, and the real `beta-vault/vault/*.yml` files, then run `./beta-setup.sh` again.

## Architecture

```
+--------+         +-----------------------------+
| Client | <-----> | API server :8080            |
+--------+         |  - HTTP REST API            |
                   |  - /connect auth endpoint   |
                   +-----------------------------+
                              |
                   +----------v------------------+
                   | Connect server :5080        |
                   |  - WebSocket upgrade on /   |
                   |  - Internal exchange :15080 |
                   +-----------------------------+
                              |
                   +----------v----------+
                   | Postgres :5432      |
                   | Redis :6379         |
                   +---------------------+
```

## Troubleshooting

### Port already in use

If `8080` or `5080` is busy, edit `docker-compose.beta.yml` and change the host-side port mappings:

```yaml
api:
  ports:
    - "18080:8080"   # change 18080 to any free port

connect:
  ports:
    - "15080:80"     # change 15080 to any free port
```

If you also change the internal exchange port mapping, update `connect/resident.go`:

```go
StartInternalPort: <your-port>,
```

and `WARP_PORTS` accordingly.

### MMDB download fails

`beta-setup.sh` downloads GeoLite2-City from the public wp-statistics mirror. If the URL changes or is blocked, manually download the `.mmdb` and place it at:

```
beta-vault/config/mmdb/ip-ipinfo.mmdb
```

Then re-run `./beta-setup.sh`.

### Docker daemon not running

The script requires a running Docker daemon with Buildx/Compose. Start it with your system service manager, e.g.:

```bash
service docker start
# or
systemctl start docker
```

### Want to run a single binary without Docker?

Set up local Postgres + Redis, populate `/srv/warp` using `beta-vault` as a template, generate the JWT keys and MMDBs with `./scripts/gen-beta-mmdb.go` and `openssl`, then run:

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

## License

Same as URnetwork server: MPL 2.0.
