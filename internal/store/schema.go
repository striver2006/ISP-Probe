package store

// schema 采用两层结构：samples_raw 保留近期逐次采样（默认 7 天），
// samples_1m / samples_1h 保存长期聚合，避免数据库无限膨胀。
const schema = `
PRAGMA journal_mode = WAL;
PRAGMA synchronous  = NORMAL;
PRAGMA busy_timeout = 5000;

-- 逐次连通性采样
CREATE TABLE IF NOT EXISTS samples_raw (
    ts            INTEGER NOT NULL,   -- unix 毫秒
    link_id       TEXT    NOT NULL,
    ok            INTEGER NOT NULL,   -- 本次探测是否成功
    dns_rtt_ms    REAL,               -- 经光猫的 DNS 往返
    anchor_rtt_ms REAL,               -- TCP connect 到锚点的往返
    forced_ok     INTEGER,            -- 强制递归查询是否成功（上游真实可达）
    modem_alive   INTEGER,            -- 光猫本身是否在线（用于隔离故障域）
    distinct_ok   INTEGER,            -- 解析结果是否与其它线路不同（分线通道有效的证据）
    resolved      TEXT,               -- 解析到的 IP，逗号分隔
    detail        TEXT                -- 失败原因
);
CREATE INDEX IF NOT EXISTS idx_raw_link_ts ON samples_raw(link_id, ts);

-- 分钟/小时聚合
CREATE TABLE IF NOT EXISTS samples_1m (
    bucket   INTEGER NOT NULL,        -- 桶起始时间，unix 毫秒
    link_id  TEXT    NOT NULL,
    n        INTEGER NOT NULL,
    n_ok     INTEGER NOT NULL,
    rtt_min  REAL, rtt_p50 REAL, rtt_p95 REAL, rtt_max REAL,
    PRIMARY KEY (bucket, link_id)
);
CREATE TABLE IF NOT EXISTS samples_1h (
    bucket   INTEGER NOT NULL,
    link_id  TEXT    NOT NULL,
    n        INTEGER NOT NULL,
    n_ok     INTEGER NOT NULL,
    rtt_min  REAL, rtt_p50 REAL, rtt_p95 REAL, rtt_max REAL,
    PRIMARY KEY (bucket, link_id)
);

-- 测速结果（手动触发，一次一条线）
CREATE TABLE IF NOT EXISTS speed_tests (
    ts          INTEGER NOT NULL,
    link_id     TEXT    NOT NULL,
    down_mbps   REAL,
    up_mbps     REAL,
    egress_ip   TEXT,
    egress_isp  TEXT,
    streams     INTEGER,
    duration_ms INTEGER,
    wireless    INTEGER,             -- 是否经无线链路（吞吐上限受限，影响结果解读）
    note        TEXT
);
CREATE INDEX IF NOT EXISTS idx_speed_link_ts ON speed_tests(link_id, ts);

-- 状态变更事件（告警与恢复）
CREATE TABLE IF NOT EXISTS events (
    ts      INTEGER NOT NULL,
    link_id TEXT    NOT NULL,
    kind    TEXT    NOT NULL,        -- down / up / isp_mismatch / degraded
    message TEXT
);
CREATE INDEX IF NOT EXISTS idx_events_ts ON events(ts);
`
