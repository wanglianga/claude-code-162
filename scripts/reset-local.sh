#!/usr/bin/env bash
# 完全重置本地演示环境：停服务 → 重建 PG 库 → 清 Redis DB15 → 清空 Kafka 数据并重启。
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
LOCAL="$ROOT/.local"
PGBIN=/usr/lib/postgresql/15/bin
KAFKA="$LOCAL/kafka"

pgrep -x recycle-server | xargs -r kill || true
sleep 1

# PostgreSQL 重建业务库
"$PGBIN/psql" -h "$LOCAL/run" -p 5433 -U recycle -d postgres -c "DROP DATABASE IF EXISTS recycling;" >/dev/null
"$PGBIN/psql" -h "$LOCAL/run" -p 5433 -U recycle -d postgres -c "CREATE DATABASE recycling;" >/dev/null
# 测试库保持可用（表结构由 go test 启动时自动迁移）
"$PGBIN/psql" -h "$LOCAL/run" -p 5433 -U recycle -d postgres -tAc \
  "SELECT 1 FROM pg_database WHERE datname='recycling_test'" | grep -q 1 || \
  "$PGBIN/psql" -h "$LOCAL/run" -p 5433 -U recycle -d postgres -c "CREATE DATABASE recycling_test;" >/dev/null

# Redis 演示库
redis-cli -n 15 flushdb >/dev/null

# Kafka：清空数据目录并以新集群 ID 重启（消除历史消息）
if [ -d "$KAFKA" ]; then
  "$KAFKA/bin/kafka-server-stop.sh" >/dev/null 2>&1 || true
  sleep 3
  rm -rf "$LOCAL/kafka-data"
  mkdir -p "$LOCAL/kafka-data"
  CID="$($KAFKA/bin/kafka-storage.sh random-uuid)"
  "$KAFKA/bin/kafka-storage.sh" format -t "$CID" \
    -c "$KAFKA/config/kraft/server.properties" >/dev/null
  nohup "$KAFKA/bin/kafka-server-start.sh" "$KAFKA/config/kraft/server.properties" \
    > "$LOCAL/kafka.log" 2>&1 &
  for _ in $(seq 1 30); do
    if "$KAFKA/bin/kafka-broker-api-versions.sh" --bootstrap-server 127.0.0.1:9092 >/dev/null 2>&1; then
      break
    fi
    sleep 1
  done
fi

echo "reset done"
