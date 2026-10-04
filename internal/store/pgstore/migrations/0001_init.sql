-- 0001_init: rules, plans, idempotent apply attempts, audit log, outbox.

CREATE TABLE rules (
    id              uuid        PRIMARY KEY,
    seq             bigserial   NOT NULL,
    publisher_id    text        NOT NULL,
    device         text        NOT NULL,
    geo             text        NOT NULL,
    genre           text        NOT NULL,
    demand_partner  text        NOT NULL,
    floor_micros    bigint      NOT NULL CHECK (floor_micros > 0),
    currency        char(3)     NOT NULL DEFAULT 'USD',
    status          text        NOT NULL CHECK (status IN ('active', 'disabled')),
    version         integer     NOT NULL DEFAULT 1,
    created_by      text        NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);

-- At most one ACTIVE rule per publisher segment; disabled history is kept.
CREATE UNIQUE INDEX rules_one_active_per_segment
    ON rules (publisher_id, device, geo, genre, demand_partner)
    WHERE status = 'active';

CREATE INDEX rules_by_publisher ON rules (publisher_id, seq);

CREATE TABLE plans (
    id                uuid        PRIMARY KEY,
    publisher_id      text        NOT NULL,
    ops               jsonb       NOT NULL,
    base_fingerprint  text        NOT NULL,
    status            text        NOT NULL CHECK (status IN ('pending', 'applied', 'partially_applied', 'failed', 'stale')),
    created_by        text        NOT NULL,
    created_at        timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX plans_by_publisher ON plans (publisher_id, created_at);

CREATE TABLE apply_attempts (
    idempotency_key  text        PRIMARY KEY,
    plan_id          uuid        NOT NULL REFERENCES plans (id),
    request_hash     text        NOT NULL,
    status           text        NOT NULL CHECK (status IN ('in_progress', 'succeeded', 'failed')),
    actor            text        NOT NULL,
    result           jsonb,
    started_at       timestamptz NOT NULL DEFAULT now(),
    finished_at      timestamptz
);

-- Two different idempotency keys must not apply the same plan at once.
CREATE UNIQUE INDEX apply_attempts_one_inflight_per_plan
    ON apply_attempts (plan_id)
    WHERE status = 'in_progress';

CREATE TABLE audit_log (
    id            bigserial   PRIMARY KEY,
    at            timestamptz NOT NULL DEFAULT now(),
    actor         text        NOT NULL,
    publisher_id  text        NOT NULL,
    action        text        NOT NULL,
    entity_id     text        NOT NULL,
    detail        jsonb
);

CREATE INDEX audit_log_by_publisher ON audit_log (publisher_id, id DESC);

CREATE TABLE outbox (
    id             uuid        PRIMARY KEY,
    seq            bigserial   NOT NULL,
    topic          text        NOT NULL,
    payload        jsonb       NOT NULL,
    created_at     timestamptz NOT NULL DEFAULT now(),
    claimed_until  timestamptz,
    sent_at        timestamptz
);

CREATE INDEX outbox_unsent ON outbox (seq) WHERE sent_at IS NULL;

CREATE TABLE processed_events (
    consumer      text        NOT NULL,
    event_id      uuid        NOT NULL,
    processed_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (consumer, event_id)
);
