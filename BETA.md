# URnetwork Beta Test Environment

A self-contained, local-only URnetwork server for testing the API and connect WebSocket path with a custom `connect` client. No Stripe, Apple, Google, Circle, Brevo, Helius/Solana, AWS/email, or payout integrations are required.

## Quick start

```bash
cd /root/urnetwork/server   # or wherever you cloned the fork
./beta-setup.sh
```

After a few minutes (initial Docker build + MMDB download), you will have:

- **API server:** http://127.0.0.1:8080
- **Connect WebSocket:** ws://127.0.0.1:5080/

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

## Connecting a custom client

The forked `connect` repository (`github.com/Ryanmello07/connect`) is the **client library**. You do not need to run it as a service. Build your client against that library and point it at the beta server:

```go
apiUrl := "http://127.0.0.1:8080"
connectUrl := "ws://127.0.0.1:5080/"
```

### Core API routes useful for testing

| Route | Method | Purpose |
|---|---|---|
| `/status` | GET | Health/version check |
| `/hello` | GET | Basic hello endpoint |
| `/auth/network-check` | POST | Check if a network name exists |
| `/auth/network-create` | POST | Create a beta network |
| `/auth/verify-send` | POST | Request an auth code (no email is actually sent in beta) |
| `/auth/verify` | POST | Verify an auth code |
| `/connect` | GET/POST | Auth endpoint used by clients before opening the WebSocket |
| `/` on connect service | WebSocket upgrade | Client connect tunnel |

## What works

- Postgres database with all migrations applied.
- Redis session/cache store.
- JWT auth using locally-generated keys.
- Core network, device, client, and provider-location APIs.
- Connect WebSocket upgrade and internal resident exchange.
- IP geolocation using the free GeoLite2-City MMDB.

## What is intentionally disabled

Anything that needs an external account or paid key will return an error or no-op if called:

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

To fully reset secrets, delete the files above and run `./beta-setup.sh` again.

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
