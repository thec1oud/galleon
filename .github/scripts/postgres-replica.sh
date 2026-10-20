#!/usr/bin/env bash
# Starts a Postgres 16 primary and a streaming standby of it in Docker, on
# the host's network: the primary on port $PRIMARY_PORT (5432), the standby
# on $STANDBY_PORT (5433), both with user postgres, password postgres. It
# returns once the standby is in recovery and streaming from the primary.
#
# The tests that need a replica pause and resume its replay, which needs a
# superuser: postgres is one.
set -euo pipefail

PRIMARY_PORT=${PRIMARY_PORT:-5432}
STANDBY_PORT=${STANDBY_PORT:-5433}
IMAGE=${IMAGE:-postgres:16}

# wait_for runs "$@" until it succeeds, for up to two minutes.
wait_for() {
  local what=$1
  shift
  for _ in $(seq 1 240); do
    if "$@" >/dev/null 2>&1; then
      return 0
    fi
    sleep 0.5
  done
  echo "timed out waiting for $what" >&2
  docker logs galleon-pg-primary >&2 || true
  docker logs galleon-pg-standby >&2 || true
  return 1
}

docker rm -f galleon-pg-primary galleon-pg-standby >/dev/null 2>&1 || true

# The host network lets the standby reach the primary on 127.0.0.1, which
# the image's pg_hba.conf admits for replication.
docker run -d --name galleon-pg-primary --network host \
  -e POSTGRES_PASSWORD=postgres \
  "$IMAGE" -c port="$PRIMARY_PORT" -c listen_addresses=127.0.0.1 \
  -c wal_level=replica -c max_wal_senders=10 -c max_connections=200 >/dev/null

# The entrypoint starts a temporary server to initialize the database, so
# wait for the final one: it answers a query over TCP.
wait_for "the primary" docker exec galleon-pg-primary \
  psql "postgres://postgres:postgres@127.0.0.1:$PRIMARY_PORT/postgres" -Atc "SELECT 1"

# The standby is a base backup of the primary that streams from it; -R
# writes the connection, with its password, for it.
docker run -d --name galleon-pg-standby --network host --user postgres \
  -e PGPASSWORD=postgres --entrypoint bash "$IMAGE" -c "
    set -e
    pg_basebackup -h 127.0.0.1 -p $PRIMARY_PORT -U postgres -D /tmp/standby -R -X stream
    chmod 700 /tmp/standby
    exec postgres -D /tmp/standby -c port=$STANDBY_PORT -c listen_addresses=127.0.0.1 -c hot_standby=on -c max_connections=200
  " >/dev/null

wait_for "the standby to stream" docker exec galleon-pg-primary \
  psql "postgres://postgres:postgres@127.0.0.1:$PRIMARY_PORT/postgres" -v ON_ERROR_STOP=1 -c \
  "DO \$\$ BEGIN IF NOT EXISTS (SELECT 1 FROM pg_stat_replication WHERE state = 'streaming') THEN RAISE 'not streaming yet'; END IF; END \$\$"
recovery=$(docker exec galleon-pg-standby psql "postgres://postgres:postgres@127.0.0.1:$STANDBY_PORT/postgres" -Atc "SELECT pg_is_in_recovery()")
if [ "$recovery" != t ]; then
  echo "the standby is not in recovery" >&2
  exit 1
fi
echo "primary on $PRIMARY_PORT, streaming standby on $STANDBY_PORT"
