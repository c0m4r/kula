#!/bin/bash
# MariaDB load: inserts, selects, updates and deletes on a small orders table
# every second, and every half minute a query that runs past the server's
# one-second long_query_time, so the query, slow-query and InnoDB charts move.
set -euo pipefail

export MYSQL_PWD="$MARIADB_PASSWORD"
sql() { mariadb -h mariadb -u shop shop "$@"; }

until sql -e 'SELECT 1' >/dev/null 2>&1; do sleep 1; done
sql -e 'CREATE TABLE IF NOT EXISTS orders (
    id INT AUTO_INCREMENT PRIMARY KEY,
    item VARCHAR(32) NOT NULL,
    qty INT NOT NULL,
    created TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    KEY (created))'

n=0
while true; do
    {
        for _ in $(seq 1 20); do
            echo "INSERT INTO orders (item, qty) VALUES ('item$((RANDOM % 50))', $((RANDOM % 10 + 1)));"
            echo "SELECT item, SUM(qty) FROM orders GROUP BY item ORDER BY 2 DESC LIMIT 5;"
            echo "UPDATE orders SET qty = qty + 1 WHERE item = 'item$((RANDOM % 50))' LIMIT 3;"
        done
        echo "DELETE FROM orders WHERE created < NOW() - INTERVAL 10 MINUTE;"
        if (( n % 30 == 0 )); then
            echo "SELECT SLEEP(1.5);"
        fi
    } | sql >/dev/null
    n=$((n + 1))
    sleep 1
done
