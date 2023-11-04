#!/bin/bash
set -xu -o pipefail

CURRENT_DIR=$(cd $(dirname $0);pwd)
export MYSQL_HOST1=${MYSQL_HOST1:-${MYSQL_HOST:-127.0.0.1}}
export MYSQL_PORT1=${MYSQL_PORT1:-${MYSQL_PORT:-3306}}
export MYSQL_USER1=${MYSQL_USER1:-${MYSQL_USER:-isucon}}
export MYSQL_DBNAME1=${MYSQL_DBNAME1:-${MYSQL_DBNAME:-isucondition}}
export MYSQL_PWD=${MYSQL_PASS1:-${MYSQL_PASS:-isucon}}
export LANG="C.UTF-8"
cd $CURRENT_DIR

cat 0_Schema.sql 1_InitData.sql 2_NewColumn.sql 3_dropColumn.sql | mysql --defaults-file=/dev/null -h $MYSQL_HOST1 -P $MYSQL_PORT1 -u $MYSQL_USER1 $MYSQL_DBNAME1
