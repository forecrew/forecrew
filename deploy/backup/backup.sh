#!/bin/sh
# Dumps the Forecrew database once and deletes dumps older than the retention.
# Run it daily (the Compose "backup" service and deploy/systemd/forecrew-backup.timer do).
#
# Connection: the standard libpq variables (PGHOST, PGUSER, PGDATABASE and
# PGPASSWORD or PGPASSFILE). The password is never put on the pg_dump command
# line, where every user of the host could read it.
# BACKUP_DIR           where dumps go (default /var/backups/forecrew)
# BACKUP_RETAIN_DAYS   how many days to keep them (default 14)
set -eu

dir="${BACKUP_DIR:-/var/backups/forecrew}"
retain="${BACKUP_RETAIN_DAYS:-14}"

case "$retain" in
'' | *[!0-9]*)
	echo "backup: BACKUP_RETAIN_DAYS must be a number, got '$retain'" >&2
	exit 2
	;;
esac

umask 077
mkdir -p "$dir"
file="$dir/forecrew-$(date -u +%Y%m%dT%H%M%SZ).dump"

# Write to a temporary name so a failed dump never looks like a backup.
pg_dump --format=custom --file="$file.partial" || { rm -f "$file.partial"; exit 1; }
mv "$file.partial" "$file"
echo "backup: wrote $file"

# Anything older than N*24 hours goes.
find "$dir" -maxdepth 1 -name 'forecrew-*.dump' -type f -mmin "+$((retain * 1440))" -print -delete |
	sed 's/^/backup: removed /'
