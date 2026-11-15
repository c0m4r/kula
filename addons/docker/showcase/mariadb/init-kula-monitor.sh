#!/bin/bash
# Runs once, when the container initializes an empty data directory: the
# monitoring user from docs/user/08-application-monitoring.md, with the
# replication grants too. Kula connects through the port published on
# 127.0.0.1, so it arrives from the Docker network's gateway: '%' host.
set -euo pipefail

mariadb --protocol=socket -uroot -p"$MARIADB_ROOT_PASSWORD" <<SQL
CREATE USER 'kula_monitor'@'%' IDENTIFIED BY '$KULA_MYSQL_PASSWORD';
GRANT PROCESS, REPLICATION CLIENT, SLAVE MONITOR, BINLOG MONITOR, REPLICATION MASTER ADMIN
  ON *.* TO 'kula_monitor'@'%';
SQL
