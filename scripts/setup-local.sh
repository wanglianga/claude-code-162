#!/usr/bin/env bash
# 启动本地用户态基础设施：PostgreSQL(5433) + Kafka KRaft(9092)；Redis 使用本机 6379 DB15。
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
LOCAL="$ROOT/.local"
PGBIN=/usr/lib/postgresql/15/bin
mkdir -p "$LOCAL/pgdata" "$LOCAL/run" "$LOCAL/download" "$LOCAL/kafka-data"

# ---------- PostgreSQL ----------
if [ ! -f "$LOCAL/pgdata/PG_VERSION" ]; then
  echo "[setup] initdb..."
  "$PGBIN/initdb" -D "$LOCAL/pgdata" -U recycle --auth=trust --encoding=UTF8 --locale=C.UTF-8 >/dev/null
  cat > "$LOCAL/pg.conf" <<EOF
port = 5433
unix_socket_directories = '$LOCAL/run'
listen_addresses = '127.0.0.1'
shared_buffers = 128MB
max_connections = 50
EOF
fi
if ! "$PGBIN/pg_ctl" -D "$LOCAL/pgdata" status >/dev/null 2>&1; then
  echo "[setup] starting postgres on 5433..."
  "$PGBIN/pg_ctl" -D "$LOCAL/pgdata" -l "$LOCAL/pg.log" \
    -o "--config-file=$LOCAL/pg.conf" start >/dev/null
  sleep 2
fi
"$PGBIN/psql" -h "$LOCAL/run" -p 5433 -U recycle -d postgres -tAc \
  "SELECT 1 FROM pg_database WHERE datname='recycling'" | grep -q 1 || \
  "$PGBIN/psql" -h "$LOCAL/run" -p 5433 -U recycle -d postgres -c "CREATE DATABASE recycling;"

# ---------- Redis ----------
if ! redis-cli -n 15 ping >/dev/null 2>&1; then
  echo "[setup] starting redis..."
  (redis-server --daemonize yes --save "" --appendonly no)
  sleep 1
fi
redis-cli -n 15 flushdb >/dev/null  # 清空演示用 DB15

# ---------- Kafka ----------
KAFKA="$LOCAL/kafka"
if [ ! -d "$KAFKA" ]; then
  echo "[setup] downloading kafka 3.7.2..."
  curl -sSL -o "$LOCAL/download/kafka.tgz" \
    https://archive.apache.org/dist/kafka/3.7.2/kafka_2.13-3.7.2.tgz
  tar xzf "$LOCAL/download/kafka.tgz" -C "$LOCAL"
  mv "$LOCAL/kafka_2.13-3.7.2" "$KAFKA"
  CID="$($KAFKA/bin/kafka-storage.sh random-uuid)"
  echo "$CID" > "$LOCAL/kafka-cluster-id"
  cat > "$KAFKA/config/kraft/server.properties" <<EOF
process.roles=broker,controller
node.id=1
controller.quorum.voters=1@127.0.0.1:9093
listeners=PLAINTEXT://127.0.0.1:9092,CONTROLLER://127.0.0.1:9093
advertised.listeners=PLAINTEXT://127.0.0.1:9092
listener.security.protocol.map=CONTROLLER:PLAINTEXT,PLAINTEXT:PLAINTEXT
inter.broker.listener.name=PLAINTEXT
controller.listener.names=CONTROLLER
log.dirs=$LOCAL/kafka-data
num.partitions=3
offsets.topic.replication.factor=1
transaction.state.log.replication.factor=1
transaction.state.log.min.isr=1
log.retention.hours=48
group.initial.rebalance.delay.ms=0
EOF
  "$KAFKA/bin/kafka-storage.sh" format -t "$CID" \
    -c "$KAFKA/config/kraft/server.properties" >/dev/null
fi
if ! "$KAFKA/bin/kafka-broker-api-versions.sh" --bootstrap-server 127.0.0.1:9092 >/dev/null 2>&1; then
  echo "[setup] starting kafka (KRaft) on 9092..."
  nohup "$KAFKA/bin/kafka-server-start.sh" "$KAFKA/config/kraft/server.properties" \
    > "$LOCAL/kafka.log" 2>&1 &
  for _ in $(seq 1 30); do
    if "$KAFKA/bin/kafka-broker-api-versions.sh" --bootstrap-server 127.0.0.1:9092 >/dev/null 2>&1; then
      break
    fi
    sleep 1
  done
fi

echo "[setup] all services ready:"
echo "  PostgreSQL: host=$LOCAL/run port=5433 user=recycle db=recycling"
echo "  Redis:      127.0.0.1:6379 db=15"
echo "  Kafka:      127.0.0.1:9092 (KRaft)"
