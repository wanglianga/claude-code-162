# 区域再生资源回收：预约估价·称重积分·入站凭证服务

面向社区再生资源回收的一体化服务，覆盖一次上门回收的六个可追踪环节：

**居民预约 → 回收员接单 → 现场定价 → 站点复称 → 积分/现金结算 → 环保凭证**

技术栈：**Go 1.19（标准库 net/http + pgx）+ PostgreSQL 15 + Redis 7 + Kafka 3.7（KRaft）**。

## 一、解决的问题

| 角色 | 关注点 | 系统对应能力 |
|---|---|---|
| 居民 | 价格透明、积分到账 | 估价公式逐项可解释；现场称重需本人/代确认；积分台账+幂等键 |
| 回收员 | 路线顺畅、收益清晰 | Redis GEO 就近派单+排班过滤；接单路线、服务费与扣回流水 |
| 回收站 | 入库品类准确 | 入站按品类复称，混装拒收留痕，差异自动挂争议/补差 |
| 街道 | 居民参与、资源流向 | 看板：参与人数、品类重量/金额、减排量、按站点的资源流向 |

重点异常均闭环且**可追到是谁、何时确认、对后续结算的影响**：

- 居民上门时临时加品类（`added_later` 标记，计入估价但在现场定价前可加）
- 回收员迟到（到达时按承诺时间自动算 `late_minutes`，可发迟到争议）
- 称重争议（居民拒绝确认即挂起结算，可重新称重，旧单 `void`、新单 `seq+1`）
- 混入不可回收物（逐品类 `rejected_kg/rejected_reason` + 照片，站点再次分拣可补争议）
- 老人不会确认（`method=assist` 代确认，强制授权说明，确认人记录为回收员）
- 现金支付失败（结算单 `failed`+原因，备用金/积分均不动，可换方式重试）
- 积分不到账（积分台账+余额同事务，`idempotency_key` 唯一约束，重复请求不重复入账）
- 站点二次称重差异（±5% 阈值：更重→自动积分补差并补服务费；更轻→不追讨居民，开争议并扣回收员服务费）
- 投诉（居民发起、运营闭环，全部进入时间线）

## 二、本地启动

需要 Linux + JDK17（Kafka 用）。脚本会自动在**用户态**拉起：
PostgreSQL（5433，无需系统服务权限）、Kafka KRaft（9092），
Redis 使用本机 6379 的 **DB 15**（与其他用途隔离）。

```bash
# 1) 启动基础设施（首次会自动下载 Kafka 3.7.2 到 .local/）
bash scripts/setup-local.sh

# 2) 启动服务（自动建表迁移 + 写入种子账号数据 + 启动 outbox relay/消费者）
go run ./cmd/server
# 或：go build -o recycle-server ./cmd/server && ./recycle-server
```

健康检查：`curl http://127.0.0.1:8080/health`

常用环境变量（均有默认值）：

| 变量 | 默认值 |
|---|---|
| `HTTP_ADDR` | `:8080` |
| `DATABASE_URL` | `host=.local/run port=5433 user=recycle dbname=recycling sslmode=disable` |
| `REDIS_ADDR` | `127.0.0.1:6379`（固定使用 DB15） |
| `KAFKA_BROKERS` | `127.0.0.1:9092` |
| `RUN_WORKERS` | `true`（设 false 可只起 HTTP） |

重置全部演示数据（重建 PG 库、清 Redis、清空 Kafka 数据并重启）：

```bash
bash scripts/reset-local.sh
```

## 三、测试账号（种子数据，密码统一 `123456`）

| 角色 | 姓名 | 手机号 |
|---|---|---|
| 居民（老人，6 层无电梯） | 张奶奶 | `13800000021` |
| 居民（12 层有电梯） | 陈先生 | `13800000022` |
| 居民（一层平房） | 刘阿姨 | `13800000023` |
| 回收员（在岗） | 王强 | `13800000011` |
| 回收员（在岗） | 李敏 | `13800000012` |
| 回收站 | 阳光回收站 | `13800000001` |
| 平台运营员 | 平台运营员 | `13700000000` |
| 街道 | 和平街道办 | `13900000000` |

品类与市价（种子）：废纸 1.20 元/kg、塑料 0.80、金属 2.50、小家电 3.00、旧衣物 0.50；
积分系数 100 分/元（小家电、旧衣物有额外加成），并带每 kg 减排因子。

## 四、验证方式

### 1. 自动化测试

```bash
# 纯单元测试（估价公式、距离），无外部依赖
go test ./internal/pricing/

# 集成测试（需先 bash scripts/setup-local.sh；连 PG 5433/Redis，自动迁移种子）
TEST_DATABASE_URL="host=$PWD/.local/run port=5433 user=recycle dbname=recycling_test sslmode=disable" \
  go test ./internal/service/ -v
```

集成测试覆盖：完整六环节生命周期、积分幂等、凭证幂等、复称更重自动补差、
称重争议阻断结算/重称恢复、现金失败改积分重试、时间线六环节完整性。

### 2. 端到端演示脚本（推荐）

```bash
bash scripts/setup-local.sh        # 若尚未启动基础设施
go run ./cmd/server &              # 或已在运行
bash scripts/demo.sh
```

脚本用 curl+jq 串联五个场景并做 29 项断言，全部通过时退出码为 0：

1. 正常单：预约估价透明 → 接单 → 上门临时加品类 → 扫码核验 → 现场称重确认 →
   积分结算（重复结算验证幂等）→ 站点复称 → **Kafka worker 自动签发凭证**
2. 老人单：迟到自动记录 → 协助核验留痕 → 代确认 → 现金失败改积分 → 复称更重自动补差
3. 争议单：称重异议阻断结算 → 重称（混装拒收）→ 现金结算 → 复称偏轻 12.9%
   自动开争议+扣服务费 → 运营补偿积分 → 投诉闭环
4. 可追溯性：打印完整时间线（环节、事件、确认人角色/ID、时间、对结算的影响）
5. 街道看板：参与人数、品类流向、站点流向、在途环节；回收员收益与路线

## 五、主要 HTTP API

登录获取 Bearer Token：`POST /api/login {"phone","password"}`

| 环节 | 方法与路径 | 角色 |
|---|---|---|
| 预约 | `POST /api/appointments`、`GET /api/appointments` | 居民 |
| 预约 | `GET /api/appointments/{id}`（全链路聚合详情+时间线） | 各角色 |
| 预约 | `POST /api/appointments/{id}/items`（上门前临时加品类） | 居民 |
| 预约 | `POST /api/appointments/{id}/cancel`、`GET .../qr` | 居民 |
| 接单 | `GET /api/recycler/dispatch`、`POST .../{id}/accept|decline` | 回收员 |
| 接单 | `POST /api/appointments/{id}/reoffer`、`.../assign` | 运营 |
| 上门 | `POST .../{id}/enroute`、`.../arrive`（自动算迟到） | 回收员 |
| 现场 | `POST .../{id}/verify`（qr_scan/manual/**assist**） | 回收员 |
| 现场 | `POST .../{id}/weigh`（品类/重量/拒收/照片/现场价） | 回收员 |
| 现场 | `POST .../{id}/confirm-weigh` `{agree}`（false 即争议挂起） | 居民 |
| 现场 | `POST .../{id}/assist-confirm`（老人代确认，强制 note） | 回收员 |
| 结算 | `POST .../{id}/settle` `{method:cash|points, simulate_fail}` | 居民 |
| 结算 | `GET .../{id}/settlements`、`GET /api/points/balance|ledger` | 居民 |
| 复称 | `POST .../{id}/inbound`（站点按品类复称） | 回收站 |
| 凭证 | `POST .../{id}/certificate`（补开；正常由 worker 自动） | 站点/运营 |
| 争议 | `POST .../{id}/disputes`、`GET /api/disputes`、`POST /api/disputes/{id}/resolve` | 各角色/运营 |
| 投诉 | `POST /api/complaints`、`POST /api/complaints/{id}/handle` | 居民/运营 |
| 通知 | `GET /api/notifications`、`POST /api/notifications/{id}/read` | 居民 |
| 看板 | `GET /api/street/dashboard` | 街道 |
| 收益 | `GET /api/recycler/earnings`、`POST /api/recycler/duty` | 回收员 |
| 价格 | `GET /api/categories`、`POST /api/categories/{code}/price` | 全员/运营 |

## 六、架构与一致性设计

```
居民端 ──┐
回收员 ──┼─► Go HTTP API ──► PostgreSQL（业务事务）
回收站 ──┤      │                 │ 同事务写 outbox + stage_events
运营/街道┘      └─► Redis（GEO 派单/位置）
                                      │
                              outbox relay（500ms 轮询，幂等生产者）
                                      ▼
                              Kafka（7 个 recycle.* 主题，按预约 ID 分区有序）
                                      │ 消费组 recycle-service-workers
                    ┌─────────────────┼──────────────────┐
                    ▼                 ▼                  ▼
            certificate worker  notification worker  analytics worker
            （入站→自动发证）    （站内通知）          （街道日统计）
```

- **业务事务原子性**：预约/称重/结算等状态变更与 `stage_events`（时间线）、
  `outbox`（待发事件）在**同一个数据库事务**提交，避免"库改了事件没发"。
- **预约/上门/称重/积分/入站一致**：预约 `stage` 状态机
  `booked→assigned→enroute→arrived→priced→settled→inbound→certified`，
  各动作校验前置环节（未核验不能称重、争议未决不能结算、未结算不能入站）。
- **积分一致性**：`point_ledger`（流水）+ `point_accounts`（余额）同事务更新，
  以 `idempotency_key`（首结/补差/争议补偿各自独立）防重复；
  现金扣减回收员备用金时 `SELECT … FOR UPDATE`，失败用 SAVEPOINT 回滚支付动作但保留 failed 记录。
- **复称差异**：站点重量与现场逐品类比对，阈值 ±5%；
  更重→积分补差（adjustment 结算单，幂等）+补服务费；更轻→开重量争议+扣服务费，不向居民追讨。
- **Kafka 至少一次 + 消费幂等**：relay 至少一次投递；
  worker 每个副作用与 `worker_checkpoints(event_id,handler)` 同事务登记，
  凭证签发本身亦按预约唯一幂等。数据库重置后残留的历史消息会被识别并安全跳过。
- **可追溯**：`stage_events` 记录六环节每个动作的 actor、角色、时间与 payload，
  差异类事件额外写 `impact`（对后续结算的影响）；
  称重保留多版本（`phase` onsite/station × `seq`），确认方式与确认人永久留痕。

## 七、目录结构

```
cmd/server/            服务入口（迁移、种子、HTTP、relay、worker）
db/migrations/         SQL 迁移（内嵌进 internal/store 同内容副本）
internal/
  config/              环境配置
  store/               PG 连接池 + 嵌入式迁移执行
  models/              领域模型与六环节常量
  pricing/             估价公式（市价×重量保守系数+楼层+距离）、球面距离
  dispatch/            Redis GEO 附近回收员
  auth/                bcrypt + HMAC JWT 与角色中间件
  service/             领域服务（预约/派单/上门/称重/结算/入站/凭证/争议/看板）
  bus/                 Kafka 主题与事件类型常量
  events/              outbox relay、消费者组 worker
  api/                 HTTP 路由与处理器
scripts/
  setup-local.sh       用户态拉起 PG/Kafka/Redis
  reset-local.sh       重置全部演示数据
  demo.sh              端到端五场景 29 项断言
```
