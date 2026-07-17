#!/usr/bin/env bash
# beta-db-backup.sh — Dump the beta Postgres database to a timestamped file.
# Usage: ./beta-db-backup.sh [output-dir]
# Default output dir: ./beta-backups/

set -euo pipefail

OUTPUT_DIR="${1:-./beta-backups}"
mkdir -p "$OUTPUT_DIR"

TIMESTAMP=$(date -u +%Y%m%dT%H%M%SZ)
BACKUP_FILE="${OUTPUT_DIR}/beta-db-${TIMESTAMP}.sql"

echo "Backing up beta database to ${BACKUP_FILE} ..."

docker exec server-postgres-1 pg_dump -U postgres --clean --if-exists \
  | gzip > "${BACKUP_FILE}.gz"

echo "Done. $(wc -c < "${BACKUP_FILE}.gz" | tr -d ' ') bytes written."
echo ""
echo "To restore:"
echo "  gunzip -c ${BACKUP_FILE}.gz | docker exec -i server-postgres-1 psql -U postgres"
