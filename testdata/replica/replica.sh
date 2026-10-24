#!/usr/bin/env bash
# Starts a streaming replica of the keelpg test container for keel's replica tests.
#
#   testdata/replica/replica.sh up      create keelpg-replica (idempotent) and print its DSN
#   testdata/replica/replica.sh down    remove keelpg-replica, its replication slot and its network
#   testdata/replica/replica.sh status  show the replica's recovery state and lag
#
# The replica is a pg_basebackup of the primary streaming from a physical replication slot, in a
# container on a network shared with the primary. The slot keeps WAL on the primary while the
# replica is stopped; "down" drops it.
#
# Environment: PRIMARY (default keelpg), REPLICA (default keelpg-replica), REPLICA_PORT (default
# 55433), SLOT (default keel_replica), NETWORK (default keelnet), PGUSER and PGPASSWORD (default
# keel). Run the replica tests with the printed DSN (make test-replica does both):
#
#   KEEL_TEST_REPLICA_DSN=postgres://keel:keel@localhost:55433/keel go test -run Replica .
set -euo pipefail

PRIMARY=${PRIMARY:-keelpg}
REPLICA=${REPLICA:-keelpg-replica}
REPLICA_PORT=${REPLICA_PORT:-55433}
PGUSER=${PGUSER:-keel}
PGPASSWORD=${PGPASSWORD:-keel}
NETWORK=${NETWORK:-keelnet}
SLOT=${SLOT:-keel_replica}

dsn() {
	echo "postgres://${PGUSER}:${PGPASSWORD}@localhost:${REPLICA_PORT}/keel?sslmode=disable"
}

psql_primary() {
	docker exec "$PRIMARY" psql -U "$PGUSER" -d postgres -Atc "$1"
}

up() {
	if [ "$(docker inspect -f '{{.State.Running}}' "$REPLICA" 2>/dev/null || true)" = "true" ]; then
		dsn
		return
	fi
	docker rm -f "$REPLICA" >/dev/null 2>&1 || true
	docker network inspect "$NETWORK" >/dev/null 2>&1 || docker network create "$NETWORK" >/dev/null
	if ! docker inspect -f '{{json .NetworkSettings.Networks}}' "$PRIMARY" | grep -q "\"$NETWORK\""; then
		docker network connect "$NETWORK" "$PRIMARY"
	fi
	local pgdata hba
	pgdata=$(docker exec "$PRIMARY" printenv PGDATA)
	hba="host replication all all scram-sha-256"
	if ! docker exec "$PRIMARY" grep -qxF "$hba" "$pgdata/pg_hba.conf"; then
		docker exec -u postgres "$PRIMARY" sed -i "\$a $hba" "$pgdata/pg_hba.conf"
		psql_primary "SELECT pg_reload_conf()" >/dev/null
	fi
	drop_slot
	local image
	image=$(docker inspect -f '{{.Config.Image}}' "$PRIMARY")
	docker run -d --name "$REPLICA" --network "$NETWORK" -p "${REPLICA_PORT}:5432" \
		-e POSTGRES_PASSWORD="$PGPASSWORD" \
		-e SLOT="$SLOT" \
		-e PRIMARY_CONNINFO="host=${PRIMARY} port=5432 user=${PGUSER} password=${PGPASSWORD} application_name=${REPLICA}" \
		--entrypoint bash "$image" -c '
			set -e
			if [ ! -s "$PGDATA/PG_VERSION" ]; then
				mkdir -p "$PGDATA"
				chown postgres:postgres "$PGDATA"
				chmod 700 "$PGDATA"
				gosu postgres pg_basebackup -d "$PRIMARY_CONNINFO" -D "$PGDATA" -X stream -C -S "$SLOT" -R -c fast
			fi
			exec docker-entrypoint.sh postgres -c hot_standby_feedback=on
		' >/dev/null
	for _ in $(seq 1 60); do
		if docker exec "$REPLICA" psql -U "$PGUSER" -d postgres -Atc "SELECT pg_is_in_recovery()" 2>/dev/null | grep -qx t; then
			dsn
			return
		fi
		sleep 1
	done
	docker logs "$REPLICA" >&2
	echo "replica did not start" >&2
	exit 1
}

drop_slot() {
	psql_primary "SELECT pg_drop_replication_slot(slot_name) FROM pg_replication_slots WHERE slot_name = '${SLOT}' AND NOT active" >/dev/null
}

down() {
	docker rm -f "$REPLICA" >/dev/null 2>&1 || true
	for _ in $(seq 1 10); do
		drop_slot && break
		sleep 1
	done
	docker network disconnect "$NETWORK" "$PRIMARY" >/dev/null 2>&1 || true
	docker network rm "$NETWORK" >/dev/null 2>&1 || true
}

status() {
	docker exec "$REPLICA" psql -U "$PGUSER" -d postgres -Atc \
		"SELECT pg_is_in_recovery(), pg_last_wal_replay_lsn(), now() - pg_last_xact_replay_timestamp()"
	psql_primary "SELECT application_name, state, replay_lsn, replay_lag FROM pg_stat_replication"
}

case "${1:-up}" in
up) up ;;
down) down ;;
status) status ;;
*)
	echo "usage: $0 up|down|status" >&2
	exit 2
	;;
esac
