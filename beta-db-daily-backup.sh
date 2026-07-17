#!/usr/bin/env bash
# beta-db-daily-backup.sh — Daily backup, overwrites the same file.
# Runs as a Hermes cron job. No accumulation — only 1 backup stored.

set -euo pipefail

BACKUP_DIR="/root/urnetwork/server/beta-backups"
BACKUP_FILE="${BACKUP_DIR}/beta-db-latest.sql.gz"

mkdir -p "$BACKUP_DIR"

echo "Backing up beta database to ${BACKUP_FILE} ..."

docker exec server-postgres-1 pg_dump -U postgres --clean --if-exists 2>/dev/null \
  | gzip > "${BACKUP_FILE}.tmp"

mv "${BACKUP_FILE}.tmp" "$BACKUP_FILE"

SIZE=$(du -h "$BACKUP_FILE" | cut -f1)
echo "Done. Backup saved: ${BACKUP_FILE} (${SIZE})"
