#!/bin/bash
# Daily PostgreSQL backup for apps/cloud-control.
#
# The database credential lives in /etc/milevia/cloud-control.env (root:root 600)
# on the server; never commit that file.
set -euo pipefail
. /etc/milevia/cloud-control.env
umask 077
mkdir -p /var/backups/milevia
out="/var/backups/milevia/milevia-$(date +%F).sql.gz"
tmp="$(mktemp /var/backups/milevia/.milevia-XXXXXX.sql.gz)"
trap 'rm -f "$tmp"' EXIT
# pipefail is required: gzip alone would happily exit 0 after a failed pg_dump.
pg_dump "$MILEVIA_CLOUD_DATABASE_URL" | gzip > "$tmp"
mv "$tmp" "$out"
find /var/backups/milevia -type f -name 'milevia-*.sql.gz' -mtime +7 -delete
