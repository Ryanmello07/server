#!/usr/bin/env bash
set -euo pipefail

# Change to the directory where this script lives (the repo root).
cd "$(dirname "$0")"

VAULT_DIR="./beta-vault"
TLS_DIR="$VAULT_DIR/vault/tls"
EC_DIR="$TLS_DIR/ec"
MMDB_DIR="$VAULT_DIR/config/mmdb"
ARIN_DIR="$VAULT_DIR/config/arindb"

mkdir -p "$TLS_DIR" "$EC_DIR" "$MMDB_DIR" "$ARIN_DIR"

# Generate JWT keys if missing.
if [ ! -f "$TLS_DIR/jwt-rsa.pem" ]; then
    echo "Generating RSA JWT key..."
    openssl genrsa -out "$TLS_DIR/jwt-rsa.pem" 2048
    openssl rsa -in "$TLS_DIR/jwt-rsa.pem" -pubout -out "$TLS_DIR/jwt-rsa.pub.pem"
fi

if [ ! -f "$EC_DIR/jwt-ec.pem" ]; then
    echo "Generating EC JWT key..."
    openssl ecparam -genkey -name prime256v1 -out "$EC_DIR/jwt-ec.pem"
    openssl ec -in "$EC_DIR/jwt-ec.pem" -pubout -out "$EC_DIR/jwt-ec.pub.pem"
fi

# Download or generate MMDBs if missing.
if [ ! -f "$MMDB_DIR/ip-ipinfo.mmdb" ] || [ ! -f "$ARIN_DIR/arin.mmdb" ]; then
    echo "Generating beta MMDBs..."
    go run ./scripts/gen-beta-mmdb.go
else
    echo "Beta MMDBs already present."
fi

# Build and start the Docker Compose stack.
echo "Bringing beta environment up..."
docker compose -f docker-compose.beta.yml down -v 2>/dev/null || true
docker compose -f docker-compose.beta.yml build

echo "Starting Postgres and Redis..."
docker compose -f docker-compose.beta.yml up -d postgres redis

# Wait for health checks.
until docker compose -f docker-compose.beta.yml ps --format table 2>/dev/null | grep -E 'postgres.*healthy|redis.*healthy' >/dev/null; do
    sleep 1
done

echo "Running migrations..."
docker compose -f docker-compose.beta.yml run --rm migrate

echo "Starting API and Connect services..."
docker compose -f docker-compose.beta.yml up -d api connect

echo
echo "Beta network running:"
echo "  API:     http://127.0.0.1:8080"
echo "  Connect: ws://127.0.0.1:5080/"
