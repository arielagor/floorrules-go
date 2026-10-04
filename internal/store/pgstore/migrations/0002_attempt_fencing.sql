-- 0002_attempt_fencing: a fencing token on apply attempts (day-2 review M1).
-- BeginApply writes a fresh token on insert and on every lease reclaim, and
-- FinishApply records a result only while the token still matches. A worker
-- that stalled past its lease and was taken over therefore cannot overwrite
-- its successor's result, audit row or event.

ALTER TABLE apply_attempts ADD COLUMN attempt_token uuid;
UPDATE apply_attempts SET attempt_token = gen_random_uuid() WHERE attempt_token IS NULL;
ALTER TABLE apply_attempts ALTER COLUMN attempt_token SET NOT NULL;
