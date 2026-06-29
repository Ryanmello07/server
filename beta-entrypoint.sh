#!/usr/bin/env bash
set -e
# All built binaries live in /app; add to PATH so compose commands can be bare names.
export PATH="/app:${PATH}"
exec "$@"
