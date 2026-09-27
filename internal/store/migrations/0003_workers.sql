-- 0003_workers.sql
-- worker 幂等处理记录（一条事件可被多个 handler 处理）
CREATE TABLE IF NOT EXISTS worker_checkpoints (
    event_id     BIGINT NOT NULL,
    handler      TEXT NOT NULL,
    topic        TEXT NOT NULL,
    processed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (event_id, handler)
);

-- 站内通知（notification worker 生成，居民端可查价格/积分/差异通知）
CREATE TABLE IF NOT EXISTS notifications (
    id          BIGSERIAL PRIMARY KEY,
    account_id  BIGINT NOT NULL REFERENCES accounts(id),
    appointment_id BIGINT REFERENCES appointments(id),
    title       TEXT NOT NULL,
    content     TEXT NOT NULL,
    read        BOOLEAN NOT NULL DEFAULT false,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_notif_account ON notifications(account_id, read, id DESC);
