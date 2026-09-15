#!/usr/bin/env bash
# beta-db-daily-backup.sh — Daily backup, overwrites the same file.
# No accumulation — only 1 backup stored, because the host runs near-full and a
# 27GB database compresses to ~2.8GB per copy.
#
# Driven by urnetwork-beta-db-backup.timer (installed 2026-09-15). Before that
# nothing invoked this script at all and the newest dump was 5 days old.
#
# BACKUP_DIR is deliberately OUTSIDE the repo. It used to point at
# ./beta-backups, which is inside both the git worktree and the Dockerfile.beta
# build context (the context is the urnetwork parent directory) and is not
# gitignored -- one run would have staged a multi-GB dump into git and shipped
# it to the docker daemon on every build.

set -euo pipefail

BACKUP_DIR="/root/beta-backups"
BACKUP_FILE="${BACKUP_DIR}/beta-db-latest.sql.gz"

mkdir -p "$BACKUP_DIR"

echo "Backing up beta database to ${BACKUP_FILE} ..."

docker exec server-postgres-1 pg_dump -U postgres --clean --if-exists 2>/dev/null \
  | gzip > "${BACKUP_FILE}.tmp"

mv "${BACKUP_FILE}.tmp" "$BACKUP_FILE"

SIZE=$(du -h "$BACKUP_FILE" | cut -f1)
echo "Done. Backup saved: ${BACKUP_FILE} (${SIZE})"
