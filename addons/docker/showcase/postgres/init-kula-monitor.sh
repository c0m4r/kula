#!/bin/sh
# Runs once, when the container initializes an empty data directory: the
# read-only monitoring role from docs/user/08-application-monitoring.md.
set -eu

psql -v ON_ERROR_STOP=1 -v pw="$KULA_POSTGRES_PASSWORD" \
    --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" <<'SQL'
CREATE ROLE kula_monitor WITH LOGIN PASSWORD :'pw';
GRANT pg_monitor TO kula_monitor;
SQL
