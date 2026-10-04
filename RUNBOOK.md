# floorsvc runbook

What to check and what to do when floorsvc misbehaves. Written for the GKE deployment in `deploy/k8s`; nothing here has been exercised on a real cluster yet (see VERIFY.md). Commands assume namespace `floorrules` and a `psql` session as a role that can read the service's tables. Never edit the floors on the ad platform by hand to "fix" an apply: the next plan will see your change as someone else's and refuse or overwrite it.

## Reading the metrics

floorsvc serves Prometheus text on `:9090/metrics` (not on the API port). Managed Service for Prometheus scrapes it through `podmonitoring.yaml`; the alerts are in `deploy/k8s/prometheus-rules.yaml`.

| Metric | Type | What it tells you |
|---|---|---|
| `apply_total{status}` | counter | One per recorded apply. `applied` is success. `stale` means the platform changed under the plan (expected now and then, not a fault). `failed` and `partially_applied` are failures. |
| `apply_record_failures_total` | counter | An apply ran but its result could not be written. Should stay at 0. |
| `apply_lease_lost_total` | counter | A worker stalled past `APPLY_LEASE`, another took over, and the stalled worker's result was discarded. Occasional is survivable; a steady rate means applies are outliving the lease. |
| `ssp_calls_total{op,outcome}` | counter | Ad platform calls after retries. A rise in `outcome="error"` points at the platform, not at us. |
| `ssp_retries_total{op}` | counter | Retries spent. Climbs before failures do when the platform throttles (429) or degrades (5xx). |
| `http_requests_total{route,code}` | counter | API traffic by route template. 401s are bad tokens: the reason is in the logs (`"msg":"token rejected"`), never in the response. |
| `outbox_pending` | gauge | Events written but not yet delivered, excluding dead letters. Database-wide; every replica reports the same number. |
| `outbox_oldest_pending_seconds` | gauge | Age of the oldest undelivered event. The best single number for "is the relay keeping up". |
| `outbox_dead_letters` | gauge | Events that failed 10 deliveries and stopped retrying. Each one needs a person. |
| `outbox_published_total`, `outbox_delivery_failures_total{topic}`, `outbox_dead_lettered_total{topic}` | counter | Relay throughput and failures. |
| `auto_plans_total`, `auto_plans_skipped_total{reason}`, `events_deduplicated_total{consumer}` | counter | The `rule.changed` consumer. Duplicates are normal (delivery is at least once). |

Logs are JSON on stdout. Every request line carries `request_id`, which is also returned in the `X-Request-ID` response header; start there when a caller reports a problem.

## Apply failures

Alerts: `FloorsvcApplyFailureRate`, `FloorsvcApplyNotRecorded`.

1. Look at the result itself. `GET /v1/publishers/{pub}/plans/{id}` and the apply response carry `status`, `reason`, and per-op `outcome`, `attempts`, `error` and `platform_status`.
   - HTTP 422 with `platform_status` 4xx: the platform refused the change (a floor it does not accept, a segment it does not know). Retrying the same plan will fail the same way. Fix the rule, compute a new plan.
   - HTTP 502: the platform kept failing after retries (5xx, 429, timeouts). Check `ssp_calls_total` and `ssp_retries_total` across publishers. If every publisher is affected, the platform is down; wait, then compute new plans.
   - `partially_applied`: some ops landed. Do not retry this plan. Compute a new plan; it diffs from what is now on the platform and finishes the rest.
2. `FloorsvcApplyNotRecorded` means the platform changed but `apply_attempts`, `plans.status` and the audit row may not have. Find the attempt from the error log (`"msg":"apply finished but not recorded"`, with `plan_id` and `key`). The attempt stays `in_progress` until its lease expires; after that a retry with the same key reclaims it, finds the plan's own writes already on the platform, and records a stale result that says so. Compute a new plan to converge. Check database health (below) first: this almost always means Postgres was unreachable for longer than `RECORD_TIMEOUT` (10s).

## Stuck leases and takeover

An apply claims its plan with a row in `apply_attempts` (`status='in_progress'`, `started_at`, `attempt_token`). If the pod dies mid-apply the row stays `in_progress`. Nothing needs to be done by hand:

- **Same idempotency key:** once `APPLY_LEASE` (5m) has passed since `started_at`, a retry reclaims the attempt with a new token. The old worker, if it was only paused, is refused when it tries to record (`apply_lease_lost_total`).
- **Key lost:** a request with a new key marks the expired attempt `abandoned` (audit `plan.apply.abandoned`, event `plan.apply_abandoned`) and starts a new one. The old key then returns 409 "superseded".
- **Before the lease expires:** both return 409 "already in progress". That is the plan lock working.

To see what is in flight:

```sql
SELECT publisher_id, idempotency_key, plan_id, actor, started_at, now() - started_at AS age
FROM apply_attempts WHERE status = 'in_progress' ORDER BY started_at;
```

Rows older than the lease are waiting for a retry, not stuck. Do not delete or update `apply_attempts` rows by hand; the fencing token is what keeps two workers from both recording. If `apply_lease_lost_total` rises steadily, applies are taking longer than the lease: raise `APPLY_LEASE` (it must stay above `APPLY_TIMEOUT + RECORD_TIMEOUT`; the service refuses to start otherwise) and keep `terminationGracePeriodSeconds` covering the longest apply (`TestDeploymentGraceCoversLongestApply` checks it).

## Outbox lag

Alerts: `FloorsvcOutboxLag`, `FloorsvcOutboxDeadLetters`.

Every change writes its event to the `outbox` table in the same transaction. Each replica's relay claims batches (`FOR UPDATE SKIP LOCKED`, 2-minute claim), delivers, and marks a row sent only after every consumer succeeded. A failed delivery is retried with backoff (1s doubling to 5m); after 10 failures the row is dead-lettered (`dead_at` set) and left in the table.

1. Is the relay running? `outbox_published_total` should move whenever `outbox_pending` is above 0. If no pod's counter moves, check that pods are up and not crash-looping, and look for `"msg":"outbox relay"` warnings (store errors).
2. Is one event failing? Look for `"msg":"outbox delivery failed, will retry"` with the same `event_id`, or:

   ```sql
   SELECT id, topic, attempts, last_error, created_at, claimed_until
   FROM outbox WHERE sent_at IS NULL AND dead_at IS NULL ORDER BY seq LIMIT 20;
   ```

   One poisoned event does not block the others (each row retries on its own schedule), but it keeps `outbox_oldest_pending_seconds` high. Fix the consumer bug and the next retry delivers it.
3. Dead letters:

   ```sql
   SELECT id, topic, attempts, last_error, dead_at FROM outbox WHERE dead_at IS NOT NULL ORDER BY seq;
   ```

   After fixing the cause, requeue one row by hand. Consumers dedupe by event ID (`processed_events`), so a requeue can never apply an event twice:

   ```sql
   UPDATE outbox SET dead_at = NULL, attempts = 0, claimed_until = NULL WHERE id = '<event id>' AND sent_at IS NULL;
   ```

## Database failover and migration failure

**Failover.** Cloud SQL HA fails over in about a minute. During it, `/readyz` returns 503 (it pings the database and checks the schema version), so the load balancer stops sending traffic; liveness stays up, so pods are not restarted. Requests in flight fail with 500/503. An apply whose recording failed is covered by "Apply failures" above. The outbox catches up on its own once the database is back. Nothing to do unless readiness stays down after the instance reports healthy: then check the DSN secret and the server CA (`server-ca.pem`, mounted by the Secret Manager CSI driver), since connections require `sslmode=verify-ca` or `verify-full`.

**Migration failure.** Migrations run in the `floorsvc-migrate` Job (`floorsvc migrate`, as the migrator role), never at service start in the cluster. Each migration runs in its own transaction and is recorded in `schema_migrations`, so a failed one leaves the schema at the previous version, not half-applied.

1. `kubectl -n floorrules logs job/floorsvc-migrate` names the migration and the Postgres error, plus the ones applied before it.
2. The new Deployment's pods will not become ready: `/readyz` reports the schema is behind. The old ReplicaSet keeps serving, so nothing is down.
3. Fix forward: correct the problem (often data a constraint rejects, or a lock timeout), then rerun the Job. Never edit a migration that has been applied anywhere; add a new one.

## Rollback

The image is the unit of rollback: `kubectl -n floorrules rollout undo deployment/floorsvc`, or redeploy the previous digest. Schema changes are additive (new columns, wider checks), and an older binary checks only that its own latest migration is present, so it runs on the newer schema. Do not roll the schema back by hand.

One case to know: after a rollback past `0005_attempt_abandoned`, an older binary does not understand `status='abandoned'` attempts. A replay of an abandoned key on the old binary returns 409 "in progress" instead of "superseded". It is wrong in the safe direction (it never re-applies).

Binary Authorization admits only images attested by CI, so the rollback target must be a digest CI built and attested; an old digest built before attestation existed will be refused.
