#!/bin/sh
# PostgreSQL load: a rate-limited pgbench that never stops, in the postgres
# database Kula monitors, so the transaction, tuple, connection and size charts
# move. Every few minutes one session sits idle in a transaction for a while.
set -eu

until pg_isready -q; do sleep 1; done
if ! psql -qtAc 'SELECT 1 FROM pgbench_branches LIMIT 1' >/dev/null 2>&1; then
    pgbench --initialize --scale=5 --quiet
fi

while true; do
    printf '%s\n' 'BEGIN;' 'SELECT count(*) FROM pgbench_branches;' '\! sleep 45' 'COMMIT;' \
        | psql -q >/dev/null &
    pgbench --no-vacuum --client=4 --jobs=2 --rate=40 --time=240 >/dev/null
    wait
done
