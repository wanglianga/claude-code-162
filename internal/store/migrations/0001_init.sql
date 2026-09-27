-- 区域再生资源回收：预约 / 估价 / 派单 / 上门 / 称重 / 结算 / 入站 / 凭证
-- 0001_init.sql

CREATE TABLE IF NOT EXISTS accounts (
    id            BIGSERIAL PRIMARY KEY,
    role          TEXT NOT NULL CHECK (role IN ('resident','recycler','station','street','operator')),
    name          TEXT NOT NULL,
    phone         TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 居民档案（住址、经纬度、默认楼层/电梯）
CREATE TABLE IF NOT EXISTS residents (
    account_id       BIGINT PRIMARY KEY REFERENCES accounts(id),
    address          TEXT NOT NULL,
    lng              DOUBLE PRECISION NOT NULL,
    lat              DOUBLE PRECISION NOT NULL,
    default_floor    INT NOT NULL DEFAULT 1,
    default_elevator BOOLEAN NOT NULL DEFAULT false,
    elderly          BOOLEAN NOT NULL DEFAULT false
);

-- 回收站
CREATE TABLE IF NOT EXISTS stations (
    id         BIGSERIAL PRIMARY KEY,
    account_id BIGINT NOT NULL UNIQUE REFERENCES accounts(id),
    name       TEXT NOT NULL,
    address    TEXT NOT NULL,
    lng        DOUBLE PRECISION NOT NULL,
    lat        DOUBLE PRECISION NOT NULL
);

-- 回收员
CREATE TABLE IF NOT EXISTS recyclers (
    account_id      BIGINT PRIMARY KEY REFERENCES accounts(id),
    station_id      BIGINT NOT NULL REFERENCES stations(id),
    -- 出发驻点（派单测距用，模拟实时位置）
    lng             DOUBLE PRECISION NOT NULL,
    lat             DOUBLE PRECISION NOT NULL,
    service_radius_m INT NOT NULL DEFAULT 5000,
    duty_status     TEXT NOT NULL DEFAULT 'off' CHECK (duty_status IN ('on','off')),
    cash_float      NUMERIC(12,2) NOT NULL DEFAULT 500.00  -- 备用金
);

-- 回收员排班（按日期 + 起止分钟）
CREATE TABLE IF NOT EXISTS recycler_schedules (
    id          BIGSERIAL PRIMARY KEY,
    recycler_id BIGINT NOT NULL REFERENCES recyclers(account_id),
    work_date   DATE NOT NULL,
    start_min   INT NOT NULL,  -- 从 00:00 起的分钟数
    end_min     INT NOT NULL,
    active      BOOLEAN NOT NULL DEFAULT true,
    UNIQUE (recycler_id, work_date)
);

-- 品类与市价（最新一条为当前价；保留历史）
CREATE TABLE IF NOT EXISTS categories (
    code            TEXT PRIMARY KEY,           -- paper/plastic/metal/appliance/textile
    name            TEXT NOT NULL,
    unit_price      NUMERIC(10,2) NOT NULL,     -- 每公斤收购价（元）
    points_per_yuan NUMERIC(10,2) NOT NULL DEFAULT 100, -- 1 元 = 多少积分
    co2_per_kg      NUMERIC(10,3) NOT NULL DEFAULT 0,   -- 每公斤减排 kg CO2
    active          BOOLEAN NOT NULL DEFAULT true,
    sort_no         INT NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS category_price_history (
    id           BIGSERIAL PRIMARY KEY,
    code         TEXT NOT NULL REFERENCES categories(code),
    unit_price   NUMERIC(10,2) NOT NULL,
    effective_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    note         TEXT
);

-- 预约主单
CREATE TABLE IF NOT EXISTS appointments (
    id                BIGSERIAL PRIMARY KEY,
    code              TEXT NOT NULL UNIQUE,          -- 业务单号 R20260926xxxx
    qr_token          TEXT NOT NULL UNIQUE,          -- 上门扫码校验令牌
    resident_id       BIGINT NOT NULL REFERENCES accounts(id),
    -- 地址快照
    address           TEXT NOT NULL,
    lng               DOUBLE PRECISION NOT NULL,
    lat               DOUBLE PRECISION NOT NULL,
    floor             INT NOT NULL,
    has_elevator      BOOLEAN NOT NULL,
    -- 可上门时间窗
    window_start      TIMESTAMPTZ NOT NULL,
    window_end        TIMESTAMPTZ NOT NULL,
    promised_arrive_at TIMESTAMPTZ,                  -- 接单时承诺到达时间
    -- 估价（公式明细存 price_detail）
    est_amount        NUMERIC(12,2) NOT NULL DEFAULT 0,
    est_weight_kg     NUMERIC(12,3) NOT NULL DEFAULT 0,
    price_detail      JSONB NOT NULL DEFAULT '{}',
    remark            TEXT,
    -- booked 已预约 / assigned 已接单 / enroute 已出发 / arrived 已上门 /
    -- priced 现场定价完成 / settled 已结算 / inbound 已入站 / certified 已发证 /
    -- closed 完结 / canceled 已取消
    stage             TEXT NOT NULL DEFAULT 'booked'
        CHECK (stage IN ('booked','assigned','enroute','arrived','priced',
                         'settled','inbound','certified','closed','canceled')),
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_appt_resident ON appointments(resident_id);
CREATE INDEX IF NOT EXISTS idx_appt_stage ON appointments(stage);

-- 预约品类明细（居民填写；added_later=TRUE 表示上门后临时加的品类）
CREATE TABLE IF NOT EXISTS appointment_items (
    id             BIGSERIAL PRIMARY KEY,
    appointment_id BIGINT NOT NULL REFERENCES appointments(id) ON DELETE CASCADE,
    category       TEXT NOT NULL REFERENCES categories(code),
    est_weight_kg  NUMERIC(12,3) NOT NULL,
    est_unit_price NUMERIC(10,2) NOT NULL,
    est_amount     NUMERIC(12,2) NOT NULL,
    photo_urls     TEXT[] NOT NULL DEFAULT '{}',
    added_later    BOOLEAN NOT NULL DEFAULT false,
    added_by       BIGINT REFERENCES accounts(id),
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_item_appt ON appointment_items(appointment_id);

-- 派单（每次派发一行，保留抢单/改派轨迹）
CREATE TABLE IF NOT EXISTS dispatches (
    id             BIGSERIAL PRIMARY KEY,
    appointment_id BIGINT NOT NULL REFERENCES appointments(id),
    recycler_id    BIGINT NOT NULL REFERENCES accounts(id),
    station_id     BIGINT NOT NULL REFERENCES stations(id),
    distance_km    NUMERIC(6,2) NOT NULL,
    -- offered 已推送 / accepted 已接单 / declined 已拒单 / expired 超时 / canceled 改派
    status         TEXT NOT NULL DEFAULT 'offered'
        CHECK (status IN ('offered','accepted','declined','expired','canceled')),
    assigned_by    TEXT NOT NULL DEFAULT 'auto',
    offered_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    responded_at   TIMESTAMPTZ,
    UNIQUE (appointment_id, recycler_id)
);
CREATE INDEX IF NOT EXISTS idx_dispatch_recycler ON dispatches(recycler_id, status);

-- 上门记录（扫码核验、迟到、老人代确认）
CREATE TABLE IF NOT EXISTS visits (
    id              BIGSERIAL PRIMARY KEY,
    appointment_id  BIGINT NOT NULL UNIQUE REFERENCES appointments(id),
    recycler_id     BIGINT NOT NULL REFERENCES accounts(id),
    enroute_at      TIMESTAMPTZ,
    arrived_at      TIMESTAMPTZ,
    late_minutes    INT NOT NULL DEFAULT 0,
    -- 居民身份核验
    verified_at     TIMESTAMPTZ,
    verify_method   TEXT CHECK (verify_method IN ('qr_scan','manual','assist')),
    verified_by     BIGINT REFERENCES accounts(id), -- 谁确认的（老人场景为回收员/坐席代确认）
    assist_note     TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 称重单（一上门一单，现场单 phase=onsite，站点复称单 phase=station）
CREATE TABLE IF NOT EXISTS weigh_records (
    id              BIGSERIAL PRIMARY KEY,
    appointment_id  BIGINT NOT NULL REFERENCES appointments(id),
    phase           TEXT NOT NULL CHECK (phase IN ('onsite','station')),
    seq             INT NOT NULL DEFAULT 1,           -- 同一阶段争议后可多次称重
    station_id      BIGINT REFERENCES stations(id),
    weighed_by      BIGINT NOT NULL REFERENCES accounts(id),
    scale_no        TEXT,
    total_weight_kg NUMERIC(12,3) NOT NULL DEFAULT 0,
    total_amount    NUMERIC(12,2) NOT NULL DEFAULT 0,
    -- pending 待确认 / confirmed 已确认 / disputed 有争议 / adjusted 已调整 / void 作废
    status          TEXT NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending','confirmed','disputed','adjusted','void')),
    confirmed_by    BIGINT REFERENCES accounts(id),
    confirm_method  TEXT CHECK (confirm_method IN ('self','assist','review')),
    confirmed_at    TIMESTAMPTZ,
    note            TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- 每阶段仅保留一张有效单（confirmed/adjusted 视为有效）
CREATE UNIQUE INDEX IF NOT EXISTS uq_weigh_active
    ON weigh_records(appointment_id, phase)
    WHERE status IN ('confirmed','adjusted');
CREATE INDEX IF NOT EXISTS idx_weigh_appt ON weigh_records(appointment_id);

-- 称重品类行（含混装拒收、现场定价）
CREATE TABLE IF NOT EXISTS weigh_items (
    id               BIGSERIAL PRIMARY KEY,
    weigh_record_id  BIGINT NOT NULL REFERENCES weigh_records(id) ON DELETE CASCADE,
    category         TEXT NOT NULL REFERENCES categories(code),
    weight_kg        NUMERIC(12,3) NOT NULL,
    unit_price       NUMERIC(10,2) NOT NULL,
    amount           NUMERIC(12,2) NOT NULL,
    -- 混装/不可回收：该部分重量拒收
    rejected_kg      NUMERIC(12,3) NOT NULL DEFAULT 0,
    rejected_reason  TEXT,
    photo_urls       TEXT[] NOT NULL DEFAULT '{}'
);
CREATE INDEX IF NOT EXISTS idx_wi_record ON weigh_items(weigh_record_id);

-- 结算单（initial 首结 / adjustment 复称差额调整）
CREATE TABLE IF NOT EXISTS settlements (
    id              BIGSERIAL PRIMARY KEY,
    appointment_id  BIGINT NOT NULL REFERENCES appointments(id),
    kind            TEXT NOT NULL DEFAULT 'initial' CHECK (kind IN ('initial','adjustment')),
    weigh_record_id BIGINT REFERENCES weigh_records(id),
    method          TEXT NOT NULL CHECK (method IN ('cash','points')),
    amount          NUMERIC(12,2) NOT NULL,
    points          INT NOT NULL DEFAULT 0,
    -- pending 处理中 / succeeded 成功 / failed 失败 / canceled 已取消
    status          TEXT NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending','succeeded','failed','canceled')),
    idempotency_key TEXT NOT NULL UNIQUE,
    attempts        INT NOT NULL DEFAULT 0,
    fail_reason     TEXT,
    settled_by      BIGINT REFERENCES accounts(id),
    settled_at      TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_settle_appt ON settlements(appointment_id);

-- 积分账户与流水
CREATE TABLE IF NOT EXISTS point_accounts (
    account_id BIGINT PRIMARY KEY REFERENCES accounts(id),
    balance    BIGINT NOT NULL DEFAULT 0,
    version    BIGINT NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS point_ledger (
    id              BIGSERIAL PRIMARY KEY,
    account_id      BIGINT NOT NULL REFERENCES accounts(id),
    appointment_id  BIGINT REFERENCES appointments(id),
    change_points   BIGINT NOT NULL,
    balance_after   BIGINT NOT NULL,
    type            TEXT NOT NULL CHECK (type IN ('earn','adjust_add','adjust_deduct','compensation')),
    idempotency_key TEXT NOT NULL UNIQUE,
    remark          TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_ledger_account ON point_ledger(account_id);

-- 回收员收益流水
CREATE TABLE IF NOT EXISTS earnings_ledger (
    id              BIGSERIAL PRIMARY KEY,
    recycler_id     BIGINT NOT NULL REFERENCES accounts(id),
    appointment_id  BIGINT NOT NULL REFERENCES appointments(id),
    amount          NUMERIC(12,2) NOT NULL,
    type            TEXT NOT NULL DEFAULT 'service_fee',
    idempotency_key TEXT NOT NULL UNIQUE,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 入站记录（站点复称结果、差异与差额结算）
CREATE TABLE IF NOT EXISTS inbound_records (
    id                  BIGSERIAL PRIMARY KEY,
    appointment_id      BIGINT NOT NULL UNIQUE REFERENCES appointments(id),
    station_id          BIGINT NOT NULL REFERENCES stations(id),
    onsite_weigh_id     BIGINT NOT NULL REFERENCES weigh_records(id),
    station_weigh_id    BIGINT NOT NULL REFERENCES weigh_records(id),
    onsite_weight_kg    NUMERIC(12,3) NOT NULL,
    station_weight_kg   NUMERIC(12,3) NOT NULL,
    diff_kg             NUMERIC(12,3) NOT NULL,
    diff_pct            NUMERIC(8,2) NOT NULL,
    rejected_kg         NUMERIC(12,3) NOT NULL DEFAULT 0,
    adjustment_amount   NUMERIC(12,2) NOT NULL DEFAULT 0,
    adjustment_points   BIGINT NOT NULL DEFAULT 0,
    -- normal 正常 / adjusted 有差异已调整 / disputed 差异争议
    status              TEXT NOT NULL DEFAULT 'normal'
        CHECK (status IN ('normal','adjusted','disputed')),
    handled_by          BIGINT NOT NULL REFERENCES accounts(id),
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 环保凭证
CREATE TABLE IF NOT EXISTS certificates (
    id              BIGSERIAL PRIMARY KEY,
    cert_no         TEXT NOT NULL UNIQUE,
    appointment_id  BIGINT NOT NULL UNIQUE REFERENCES appointments(id),
    resident_id     BIGINT NOT NULL REFERENCES accounts(id),
    station_id      BIGINT NOT NULL REFERENCES stations(id),
    items           JSONB NOT NULL,           -- [{category,name,weight_kg,amount}]
    total_weight_kg NUMERIC(12,3) NOT NULL,
    co2_saved_kg    NUMERIC(12,3) NOT NULL,
    issued_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked         BOOLEAN NOT NULL DEFAULT false
);

-- 争议（称重/品类/结算/迟到/混装）
CREATE TABLE IF NOT EXISTS disputes (
    id              BIGSERIAL PRIMARY KEY,
    appointment_id  BIGINT NOT NULL REFERENCES appointments(id),
    type            TEXT NOT NULL CHECK (type IN ('weight','category','settlement','late','contamination','other')),
    raised_by       BIGINT NOT NULL REFERENCES accounts(id),
    title           TEXT NOT NULL,
    description     TEXT,
    -- open 待处理 / reviewing 处理中 / resolved 已解决 / closed 已关闭
    status          TEXT NOT NULL DEFAULT 'open'
        CHECK (status IN ('open','reviewing','resolved','closed')),
    resolution      TEXT,
    adjust_amount   NUMERIC(12,2) NOT NULL DEFAULT 0,
    adjust_points   BIGINT NOT NULL DEFAULT 0,
    resolver_id     BIGINT REFERENCES accounts(id),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    resolved_at     TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_dispute_appt ON disputes(appointment_id);
CREATE INDEX IF NOT EXISTS idx_dispute_status ON disputes(status);

-- 投诉
CREATE TABLE IF NOT EXISTS complaints (
    id             BIGSERIAL PRIMARY KEY,
    appointment_id BIGINT REFERENCES appointments(id),
    complainant_id BIGINT NOT NULL REFERENCES accounts(id),
    category       TEXT NOT NULL,
    content        TEXT NOT NULL,
    status         TEXT NOT NULL DEFAULT 'open'
        CHECK (status IN ('open','handling','closed')),
    response       TEXT,
    handler_id     BIGINT REFERENCES accounts(id),
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    handled_at     TIMESTAMPTZ
);

-- 六环节事件时间线（谁、何时、做了什么、对后续结算的影响）
CREATE TABLE IF NOT EXISTS stage_events (
    id             BIGSERIAL PRIMARY KEY,
    appointment_id BIGINT NOT NULL REFERENCES appointments(id),
    -- appointment 居民预约 / dispatch 回收员接单 / onsite 现场定价 /
    -- reweigh 站点复称 / settlement 积分现金结算 / certificate 环保凭证
    stage          TEXT NOT NULL CHECK (stage IN
        ('appointment','dispatch','onsite','reweigh','settlement','certificate',
         'dispute','complaint','system')),
    event_type     TEXT NOT NULL,
    actor_id       BIGINT REFERENCES accounts(id),
    actor_role     TEXT,
    payload        JSONB NOT NULL DEFAULT '{}',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_stage_appt ON stage_events(appointment_id, id);

-- 事务发件箱（业务事务内写入，relay 轮询投递 Kafka）
CREATE TABLE IF NOT EXISTS outbox (
    id           BIGSERIAL PRIMARY KEY,
    aggregate_id TEXT NOT NULL,
    topic        TEXT NOT NULL,
    event_type   TEXT NOT NULL,
    payload      JSONB NOT NULL,
    published    BOOLEAN NOT NULL DEFAULT false,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    published_at TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_outbox_unpub ON outbox(id) WHERE published = false;

-- 街道每日统计（analytics worker 维护）
CREATE TABLE IF NOT EXISTS street_daily_stats (
    stat_date        DATE NOT NULL,
    category         TEXT NOT NULL,
    appointment_cnt  INT NOT NULL DEFAULT 0,
    weight_kg        NUMERIC(14,3) NOT NULL DEFAULT 0,
    amount           NUMERIC(14,2) NOT NULL DEFAULT 0,
    points           BIGINT NOT NULL DEFAULT 0,
    participants     INT NOT NULL DEFAULT 0,
    PRIMARY KEY (stat_date, category)
);
