-- 0003_tenant_scoped_keys: idempotency keys belong to a publisher (day-2
-- review M3). With the key alone as the primary key, a caller scoped to one
-- publisher could replay another publisher's stored result by naming its plan
-- and key, and two publishers choosing the same key collided.

ALTER TABLE apply_attempts ADD COLUMN publisher_id text;
UPDATE apply_attempts a SET publisher_id = p.publisher_id FROM plans p WHERE p.id = a.plan_id;
ALTER TABLE apply_attempts ALTER COLUMN publisher_id SET NOT NULL;
ALTER TABLE apply_attempts DROP CONSTRAINT apply_attempts_pkey;
ALTER TABLE apply_attempts ADD PRIMARY KEY (publisher_id, idempotency_key);
