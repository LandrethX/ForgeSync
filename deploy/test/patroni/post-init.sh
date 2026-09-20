#!/bin/sh
# Runs once, on the member that bootstraps the cluster. Patroni passes a
# connection string to the new cluster as $1. ForgeSync's database has to
# exist before the controller can migrate into it; everything inside it is
# the controller's own doing.
set -eu
psql "$1" -v ON_ERROR_STOP=1 \
  -c "CREATE DATABASE ${FORGESYNC_DB_NAME:-forgesync} OWNER ${PATRONI_SUPERUSER_USERNAME:-forgesync}"
