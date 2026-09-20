#!/usr/bin/env bash
# Backs up ForgeSync's database, which is the only thing that can't be
# rebuilt: what each repository's primary is, what ForgeSync last wrote
# to every replica, who owns what, the conflicts people are working
# through, the archived copies of deleted repositories, and ForgeSync's
# own accounts.
#
#   ./backup.sh /var/backups/forgesync            # take one
#   ./backup.sh /var/backups/forgesync --verify   # and check it restores
#
# The database URL comes from FORGESYNC_DATABASE_URL, or from the
# url_file in the config given by -config (default
# /etc/forgesync/forgesync.yaml).
#
# It does not back up: the git cache under replication.work_dir (a
# cache), the Forgejo nodes (whoever runs them backs those up), or the
# secrets (they belong wherever your secrets live). Restoring is in
# README.md; the short version is: stop the controllers, restore, start
# one.
set -Eeuo pipefail

dest=${1:?usage: $0 <directory> [--verify] [-config <file>]}
shift || true
verify=""
config=/etc/forgesync/forgesync.yaml
while [ $# -gt 0 ]; do
  case "$1" in
    --verify) verify=1 ;;
    -config) shift; config=$1 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
  shift
done

url=${FORGESYNC_DATABASE_URL:-}
if [ -z "$url" ]; then
  # url_file from the config, resolved against the config's directory.
  file=$(sed -n 's/^[[:space:]]*url_file:[[:space:]]*//p' "$config" | head -1 | tr -d '"')
  [ -n "$file" ] || { echo "no database url: set FORGESYNC_DATABASE_URL or database.url_file" >&2; exit 1; }
  case "$file" in
    /*) ;;
    *) file="$(dirname "$config")/$file" ;;
  esac
  url=$(tr -d '[:space:]' < "$file")
fi

mkdir -p "$dest"
stamp=$(date -u +%Y%m%dT%H%M%SZ)
out="$dest/forgesync-$stamp.dump"

# -Fc is the custom format: compressed, and pg_restore can read parts of
# it. --no-owner so it restores into whatever user does the restoring.
pg_dump --dbname="$url" --format=custom --no-owner --file="$out"
chmod 600 "$out"
echo "wrote $out ($(du -h "$out" | cut -f1))"

# A dump nobody has restored is a hope, not a backup. --verify restores
# it into a scratch database beside the real one and counts what came
# back, then throws the scratch away.
if [ -n "$verify" ]; then
  scratch="forgesync_verify_$stamp"
  admin=${url%/*}/postgres
  if ! psql --dbname="$admin" -qc "CREATE DATABASE \"$scratch\"" 2>/dev/null; then
    echo "can't verify: this role may not create a database." >&2
    echo "Either grant it (ALTER ROLE forgesync CREATEDB) or verify with a role that can." >&2
    exit 1
  fi
  trap 'psql --dbname="$admin" -qc "DROP DATABASE IF EXISTS \"$scratch\"" >/dev/null || true' EXIT
  pg_restore --dbname="${url%/*}/$scratch" --no-owner "$out" >/dev/null
  counts=$(psql --dbname="${url%/*}/$scratch" -At -c "
    SELECT 'repositories ' || count(*) FROM repositories
    UNION ALL SELECT 'replicas ' || count(*) FROM replica_sync
    UNION ALL SELECT 'refs ' || count(*) FROM replicated_refs
    UNION ALL SELECT 'accounts ' || count(*) FROM accounts
    UNION ALL SELECT 'conflicts ' || count(*) FROM conflicts")
  echo "restored into $scratch and read back:"
  echo "$counts" | sed 's/^/  /'
fi
