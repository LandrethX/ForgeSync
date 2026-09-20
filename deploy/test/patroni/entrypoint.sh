#!/bin/sh
# PostgreSQL refuses to run as root and Patroni starts it, so both run as
# postgres. The data directory comes from a named volume owned by root.
set -eu
data=/var/lib/postgresql/data
mkdir -p "$data"
chown -R postgres:postgres "$data"
chmod 0700 "$data"
exec gosu postgres patroni /etc/patroni/patroni.yml
