-- 0004_outbox_retry: delivery attempts, last error and a dead-letter mark on
-- the outbox row itself (day-2 review M4). A row is marked sent only after
-- its consumers succeed, so failed deliveries are retried from here with
-- backoff, and a row that keeps failing stays in the table, dead-lettered,
-- instead of being dropped from an in-memory queue.

ALTER TABLE outbox ADD COLUMN attempts   integer     NOT NULL DEFAULT 0;
ALTER TABLE outbox ADD COLUMN last_error text;
ALTER TABLE outbox ADD COLUMN dead_at    timestamptz;

CREATE INDEX outbox_dead ON outbox (seq) WHERE dead_at IS NOT NULL;
