-- 0002_settlement_extras.sql
ALTER TABLE settlements ADD COLUMN IF NOT EXISTS simulate_fail BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE appointments ADD COLUMN IF NOT EXISTS settle_method TEXT
    CHECK (settle_method IN ('cash','points'));
