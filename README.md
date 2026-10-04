# floorrules

> **Self-made work sample, not production code, built Oct 2026 by Ariel Agor directing Claude Code. Ariel's first Go codebase.**
>
> No employer, client or real ad platform is involved. The SSP is a mock. Nothing here has run in production. How it was built, including what the checks caught, is in [BUILD-LOG.md](BUILD-LOG.md); every claim about tests and tooling is backed by commands and output in [VERIFY.md](VERIFY.md).

A small Go service for one ad-ops job: operators define **floor-price rules** per publisher segment (device, geo, genre, demand partner), and the service turns them into a **reviewed plan** and **applies it safely** to an ad platform.

The shape is plan, review, apply. Floors move money, so nothing touches the platform without a stored plan, and anything risky needs explicit acknowledgement.

## What it does

1. `POST /v1/publishers/{pub}/rules` validates and stores a rule. All field errors come back at once, one active rule per segment.
2. `POST /v1/publishers/{pub}/plans` reads the platform's current floors and diffs them against the active rules. The plan stores the ops (deletes, then updates, then creates, in a deterministic order) and a fingerprint of the platform state it saw.
   - Guardrails: at most 200 ops per plan. An update that moves a floor by more than 50% is flagged risky, and so is taking over a floor this service did not set. It never deletes a floor it did not create.
3. `POST /v1/publishers/{pub}/plans/{id}/apply` with an `Idempotency-Key` header applies the plan.
   - It refuses if the platform drifted since planning (`409 stale`) or if risky ops are not acknowledged (`422`).
   - Each op is retried with full-jitter exponential backoff, but only on 429, 5xx and timeouts, honouring a capped `Retry-After`.
   - It runs detached from the HTTP request, so a client disconnect cannot leave a half-applied plan with no record.
   - The same key returns the stored result with `Idempotent-Replayed: true`. The same key with a different request is rejected.
4. Every change writes its audit row and a `rule.changed` / `plan.applied` event in the **same transaction** (transactional outbox). A relay publishes the events (at least once), and a consumer turns `rule.changed` into a proposed plan, deduplicated by event ID so redelivery creates no second plan.

Also: `GET /healthz` (liveness), `GET /readyz` (pings the store; goes false during shutdown drain), `GET /metrics` (Prometheus text format), and `GET .../audit`.

## Layout

```
cmd/floorsvc         entrypoint: config, wiring, graceful shutdown
cmd/devtoken         mints a short-lived dev JWT (local use only)
internal/domain      rules, validation, planner, fingerprint (pure, no I/O)
internal/service     use cases: plan, apply (idempotency, retries), consumer
internal/adapter     SSP interface, retry policy, mock SSP with fault injection
internal/store       Store interface + memstore, pgstore (pgx, SQL migrations), shared contract suite
internal/events      outbox relay, bounded in-memory queue with redelivery and dead-letter
internal/httpapi     net/http (Go 1.22+ routing), auth middleware, error mapping
internal/auth        HS256 JWT verification, scopes, per-publisher grants
internal/config      env + *_FILE secret loading
deploy/k8s           GKE manifests (restricted PSA, NetworkPolicy, PDB, Workload Identity)
infra/terraform      skeleton: Autopilot, Artifact Registry, Cloud SQL, Secret Manager (validate only)
```

## Run it locally

Requires Go 1.27+. The memory backend needs no database:

```sh
export AUTH_HMAC_SECRET=$(openssl rand -hex 32) AUTH_ISSUER=https://auth.example.internal
STORE_BACKEND=memory go run ./cmd/floorsvc &

TOKEN=$(go run ./cmd/devtoken -pubs acme-tv -scope "rules:read rules:write plans:write plans:apply audit:read")
curl -s -X POST localhost:8080/v1/publishers/acme-tv/rules \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"segment":{"device":"ctv","geo":"US","genre":"sports"},"floor_micros":12500000}'
PLAN=$(curl -s -X POST localhost:8080/v1/publishers/acme-tv/plans \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' -d '{}' | jq -r .id)
curl -s -X POST localhost:8080/v1/publishers/acme-tv/plans/$PLAN/apply \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: demo-apply-0001' -d '{"acknowledge_risky":false}'
```

With Postgres: `STORE_BACKEND=postgres DATABASE_URL=postgres://... RUN_MIGRATIONS=true`. Prefer `DATABASE_URL_FILE` / `AUTH_HMAC_SECRET_FILE` pointing at mounted secrets; a `_FILE` variable wins over the plain one.

Tests:

```sh
go test -race ./...                                             # unit + contract tests on fakes
TEST_DATABASE_URL=postgres://postgres:test@localhost:5544/postgres?sslmode=disable \
  go test -race -tags integration ./internal/store/...          # same contract against real Postgres
git config core.hooksPath scripts                               # enable the pre-commit gate
```

## Design choices worth questioning

- **Money is int64 micros.** No floats anywhere near a price.
- **One Store interface, two implementations, one contract suite.** Unit tests run the memstore through the same suite the integration build runs against Postgres. That keeps the fake honest.
- **Idempotency in the database, not in memory.**
  - `apply_attempts` is keyed by the client's key and also has a partial unique index allowing one in-flight attempt per plan.
  - An attempt abandoned by a crashed pod is reclaimable once its lease expires.
- **Outbox over dual writes.** The event cannot exist without the change, nor the change without the event. Claims use `FOR UPDATE SKIP LOCKED` with a lease, so several replicas can relay without double-claiming.
- **Hand-rolled HS256 and Prometheus text output** keep the dependency list at one module (pgx). In production I'd expect an RS256/JWKS verifier from the identity provider behind the same `Verifier` interface, and the Prometheus client library.

## Out of scope (stated, not hidden)

- A real SSP / ad-server client. The mock implements the same interface with fault injection.
- A real broker (Pub/Sub). The relay talks to a `Publisher` interface, and the in-memory queue stands in.
- Multi-currency, rule versioning/edit-in-place (rules are disabled and replaced), and a UI.
- The migrations run at startup behind an advisory lock; a real rollout would use a separate Job.
- The repo is public and CI runs on GitHub Actions. The first run passed all jobs (run 37177854018 on commit 4a621bd, https://github.com/arielagor/floorrules-go/actions/runs/37177854018). Each step and its output are also documented locally in VERIFY.md.
