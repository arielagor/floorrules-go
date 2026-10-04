# Review checklist

For the human reviewing an agent's (or anyone's) change. Tools catch formatting, vet, lint and known CVEs. This list covers what they don't.

## Correctness
- [ ] Does a new test fail without the change? Ask for the failing run, or revert the fix locally and see.
- [ ] Do tests assert behaviour (status, stored state, emitted events), not just "no error"?
- [ ] For store changes: is the case in `storetest`, so memstore and Postgres are both held to it?
- [ ] Concurrency: what happens with two replicas doing this at once? With a pod killed halfway? Do not answer by reading the happy path: ask for the test that pauses or kills one worker mid-operation.
- [ ] Deadlines: is any context used again after the step that may have exhausted it? Recording and cleanup need their own budget (`context.WithoutCancel` plus a timeout).
- [ ] Leases: if work can be reclaimed after a timeout, is the final write fenced by a token that changes on every claim?
- [ ] Check-then-act: is every "read a status, then write" either one conditional statement or inside a transaction holding the row lock?

## Money and blast radius
- [ ] Any arithmetic on floors uses int64 micros, with no float conversion on the way.
- [ ] Can this change move more floors, or move them further, than before without the risky-ack path?
- [ ] Can it delete or overwrite a floor this service didn't create?

## Reliability
- [ ] Every outbound call has a deadline and goes through the retry policy; only retryable errors retry.
- [ ] Retried operations are idempotent on the platform side.
- [ ] State change, audit row and outbox event are in one transaction. No "write, then publish".
- [ ] Shutdown: does new background work stop on context cancel and get waited for? Is the pod's grace period at least drain + the longest operation + recording?
- [ ] At-least-once: is a durable record (outbox row, attempt row) marked done only after the work is done, never when it is handed to an in-memory queue?

## Security and tenancy
- [ ] Every new route goes through `authed(scope, ...)` with the narrowest scope.
- [ ] Every store query is scoped by publisher ID taken from the path *after* the grant check. That includes lookups by a client-supplied key or ID (idempotency keys, plan IDs), not only lists.
- [ ] Input is bounded (size, format) before it reaches storage or logs.
- [ ] No secret, token or DSN in logs, errors or test fixtures.
- [ ] 500s hide internals; the client gets a request ID.

## Operations
- [ ] New failure modes have a metric or a log line someone could alert on.
- [ ] Migrations are additive and safe to run while the old version is still serving.
- [ ] Manifest changes keep: non-root, read-only root FS, all caps dropped, resource requests, probes, NetworkPolicy.
- [ ] CI actions should be pinned to commit SHAs before this leaves sample status (they are major tags today).

## Agent-specific
- [ ] Did the agent touch anything outside the task? Check the diff stat.
- [ ] Any new `//nolint`, skipped test, loosened config or new dependency? Each needs a written reason.
- [ ] Is every "passes" claim backed by pasted output?
