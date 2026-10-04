-- 0005_attempt_abandoned: day-2 review L3. An in_progress attempt whose lease expired is superseded
-- by the next key on the plan and marked abandoned, instead of blocking the
-- plan forever. 'abandoned' is terminal, like succeeded and failed, so the
-- one-in-flight partial index (status = 'in_progress') no longer covers it.
ALTER TABLE apply_attempts DROP CONSTRAINT apply_attempts_status_check;
ALTER TABLE apply_attempts ADD CONSTRAINT apply_attempts_status_check
    CHECK (status IN ('in_progress', 'succeeded', 'failed', 'abandoned'));
