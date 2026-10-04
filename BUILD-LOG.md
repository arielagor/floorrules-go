# Build log

How this repo was made, what the checks caught, and what a second review caught that the checks did not.

## Who did what

- **Ariel Agor** set the brief: the domain (ad-ops floor rules), the reliability and security bar, the deploy targets, and the guardrails. Those guardrails were: no remote and no push, no cloud resources or spend, use only the test database, author files with editor tools rather than shell heredocs, and record failures truthfully. This is Ariel's first Go codebase. He has not shipped Go in production.
- **Claude Code** (Claude Opus 5.5) wrote the code, tests, manifests and docs in one session on 2026-10-03. It ran every check locally and fixed what they found.
- Size at the end: about 5,200 lines of Go, about 1,700 of them tests.

## Timeline (from `git log`)

| Commit | Time | Step |
|---|---|---|
| 9218de8 | 19:34 | Domain model, planner, SSP adapter with retry/backoff |
| cdaed79 | 19:36 | `.gitattributes` pinning LF, done up front so `gofmt -l` is stable on Windows checkouts (preventive, not a caught failure) |
| 950511d | 19:56 | Store interface, memstore, pgstore + migrations, shared contract suite |
| 5e1c7f5 | 20:14 | Service layer, auth, outbox relay, consumer, metrics |
| e4f60d9 | 20:45 | HTTP API, config, entrypoint; lint and vulnerability fixes |
| 4ad330a | 20:57 | Dockerfile, GKE manifests, Terraform skeleton |
| 1f644bd | 21:15 | CI, pre-commit hook, fix found by the smoke run |

## What the checks caught

These are real, in the order they happened. The raw output of the first lint and govulncheck runs was kept and is quoted here.

### 1. golangci-lint, first run: 17 issues

```
17 issues:
* bodyclose: 3
* errcheck: 1
* errorlint: 2
* gofmt: 3
* gosec: 1
* revive: 7
```

- **errorlint x2, the ones that mattered.**
  - The HTTP recoverer compared `v == http.ErrAbortHandler`. With a wrapped abort error, that would have swallowed a deliberate abort and turned it into a 500.
  - An auth test used `err != ErrUnauthenticated`.
  - Both now use `errors.Is`.
- **bodyclose x3, errcheck x1.** The HTTP test helper returned `*http.Response` and left closing to each caller. It now reads and closes the body itself and returns a small value struct, so the leak can't be reintroduced.
- **gofmt x3.** Three files were written unformatted (`gofmt -l` also flagged a fourth). Fixed with `gofmt -w`.
- **gosec G304** in `cmd/devtoken`: a file path read from an env var. This is intended, because it is the operator's own secret mount, so it is suppressed with a written justification rather than "fixed".
- **revive x7.** Missing doc comments on exported constants and methods.
- Re-run: `0 issues.`

### 2. govulncheck: a reachable vulnerability in a transitive dependency

```
Vulnerability #1: GO-2026-5970
    Infinite loop on invalid input in golang.org/x/text
  Found in: golang.org/x/text@v0.29.0
  Fixed in: golang.org/x/text@v0.39.0
    #1: internal/store/pgstore/migrate.go:25:27: pgstore.Migrate calls pgxpool.Pool.Acquire, which eventually calls norm.Form.Properties
```

pgx pulled in `x/text` v0.29.0. The vulnerable code was reachable from our own call path, not just present in the module graph. Bumped to v0.42.0 (and `x/sync` to v0.23.0). Re-run: `No vulnerabilities found.` The syft SBOM of the final image confirms `golang.org/x/text v0.42.0` is what ships.

### 3. Race detector could not run on Windows

`go test -race` failed with `cgo: C compiler "gcc" not found`. The race tests were not skipped. They ran in the `golang:1.27` Linux container (go1.27.1) together with the Postgres integration tests, and passed. VERIFY.md has the command.

### 4. Container smoke run found a behaviour bug

Running the built image end to end showed this log line after the apply:

```
"msg":"proposed plan from rule change", ... "ops":0
```

The `rule.changed` event reached the consumer about a second after the rule had already been planned and applied. The consumer then created an empty "proposed" plan, which is review noise for operators. No unit test covered "event arrives after the change is already live".

The fix followed the order a reviewer would want. A regression test (`TestRuleChanged_NoDriftProposesNothing`) was added first and failed with `auto_plans_total = 1, want 0`. Then the consumer was changed to propose nothing when there is no drift, counted as `auto_plans_skipped_total{reason="no_drift"}`. The test now passes.

### 5. syft failed on Windows

`syft floorrules:dev` exited 1 with `unable to place layer cache path ... The filename, directory name, or volume label syntax is incorrect`. Its layer cache filenames contain `:`, which Windows rejects. The same syft version ran inside a Linux container against a `docker save` tarball instead. CI runs on Linux, where this does not apply.

### 6. The pre-commit hook was installed but never ran

I copied `scripts/pre-commit` into `.git/hooks/` and then told myself the next commit (1f644bd) was "gated by the hook". It wasn't. `git hook run pre-commit` said `cannot find a hook named pre-commit`, because a global `core.hooksPath` on this machine overrides `.git/hooks`.

Commit 1f644bd therefore went in on the strength of checks run by hand just before it (tests, lint), not the hook. The fix was `git config core.hooksPath scripts`. Then the hook was proven to work in both directions:
- `git hook run pre-commit` passes.
- A deliberately misformatted file was rejected (`gofmt needed on: internal\id\zz_hooktest.go`, commit exit 1). The probe was removed afterwards.

The install instructions now say why a copy is not enough.

The first real commit through the hook (the docs commit) was then blocked with `gofmt: command not found`, because Go was not on that shell's PATH. The commit was re-run with Go on PATH and passed all five stages. This section originally said the hook "fails closed when a tool is missing". That was true for Go and govulncheck but not for golangci-lint, which the hook skipped with "CI will run it". The day-2 review caught the contradiction (L1 below); since aa08d5c a missing golangci-lint fails the commit too.

## Caught by review, not by a tool (day 1)

Found while re-reading the code before the checks ran. Listed separately so the tools don't get credit for them.

- **Lease reclaim was unreachable.** Apply looked up the idempotency key first and returned "in progress" for any `in_progress` attempt. So an attempt abandoned by a crashed pod could never be taken over, even though the store supported it. Apply now falls through to the store's reclaim when the lease has expired. `TestApply_InProgressAndLeaseReclaim` covers it.
- **Rule listing order could tie** on equal timestamps, which made output nondeterministic. Added a `seq bigserial` (and an insertion counter in memstore).
- **Plan `created_at` differed** between what the service returned and what Postgres stored. Fixed with `COALESCE($7, now())` so the service's clock wins when set.
- Two leftover junk lines in tests (a stray expression and an undefined variable) were caught by the compiler.

## What went right the first time

- Most unit tests passed on their first run. The failures were compile errors from the leftover lines above, not wrong behaviour.
- The Postgres contract suite passed on its first run against real Postgres 16. It was then repeated with `-count=10` to look for flakiness in the concurrency tests: clean.
- kubeconform and `terraform validate` passed on first run.

None of that proves the design is right. It proves the code does what its tests say. The review checklist covers the rest.

## Day-2 review: caught by review, not by a tool

On 2026-10-03/04 a reviewer read the repo cold from a hiring manager's seat (a separate Claude session; `backend-dev/reviews/hm-critic.md` in the application packet). The fixes were then made by Claude Code in this repo, as before. Every finding below was in code that was formatted, vetted, linted with 0 issues, vulnerability-scanned, green under `-race` against Postgres 16, and green in CI. Each was then reproduced as a test that failed, fixed, and committed on its own, in the order below. The failing output is quoted after the table and, in full, in each commit message.

| # | What it was | Why the tool chain missed it | Test that pins it | Commit |
|---|---|---|---|---|
| L1 | The pre-commit hook skipped golangci-lint when it was missing ("CI will run it") and passed, while this log said it fails closed. | The hook was only ever run where the tool was installed. | Ran the hook with golangci-lint off PATH: exit 0 before, exit 1 after. | aa08d5c |
| L2 | With `TEST_DATABASE_URL` unset, the Postgres suite called `t.Skip` and reported `ok`, so a CI env typo would pass with zero integration tests. | A skipped test is a pass to `go test` and to CI. | `pgtest.TestDSN_FailsInCISkipsLocally`; the suite fails when `CI` is set and the URL is not. | 7a849da |
| H1 | `FinishApply` ran on the apply's own 2-minute context. A slow platform used all of it, so the result write failed with `context deadline exceeded`: floors changed, no audit row, no event, attempt stuck `in_progress`. | The in-memory store ignored its context, so writing on an expired context succeeded in every unit test. memstore now returns `ctx.Err()` like a driver. | `TestApply_SlowPlatformStillRecordsResult`, `TestSlowPlatformRecordsResultInPostgres` | 65ac5ca |
| H2 | On SIGTERM the process stopped the HTTP server and exited while detached applies (up to 2m) were still running; the pod grace period was 40s and its comment said it covered them. | No test exercised shutdown; `main` had no tests. | `TestShutdownWaitsForInFlightApply`, `TestApply_RefusedOnceShutdownBegins`, `TestDeploymentGraceCoversLongestApply` | e3c037f |
| M1 | Lease reclaim had no fence. A worker that stalled past its lease and resumed wrote its result over its successor's: plan status flipped, two audit rows, two events. Lease expiry also mixed the app's clock with the database's. | Every test ran one worker. The contract suite checked that reclaim happens, never what the reclaimed-from worker can still do. | `TestTwoWorkers_LeaseTakeoverFencesTheStaleWorker` (two `Service`s, one Postgres, `-race`); storetest `ApplyLeaseReclaim` | 6c2ebe8 |
| M2 | "Plan is pending" was checked outside the claim transaction, so a second key could apply an already-applied plan and overwrite its status with `stale`. | The pre-check made every sequential test pass; nothing put one apply inside another's check-then-act window. | `TestApply_SecondKeyAfterFirstFinishedIsRefused`; storetest `ClaimRequiresPendingPlan` (memstore and Postgres) | fa850b5 |
| M3 | Idempotency keys were global. Publisher B reusing A's key got A's stored result replayed, and A's key blocked B. | Tenant tests covered routes and lists, not lookups by a client-supplied key. Nothing to lint. | `TestApply_IdempotencyKeysAreTenantScoped`; storetest `IdempotencyKeysScopedToPublisher` | d1d8fc3 |
| M4 | The relay marked outbox rows sent when it handed them to an in-memory queue, so a restart, a full buffer or a short outage lost events for good. | Tests ran in one process that never stopped. | `TestRelay_EventSurvivesRestartBeforeHandling` | f9a2486 |
| M5 | The manifests promised a migrate Job that did not exist; readiness only pinged the pool, so pods went Ready on an unmigrated database; `RUN_MIGRATIONS` defaulted to true, so the runtime role needed DDL. | kubeconform validates the manifests that exist, not the ones a comment promises. | `TestLoad_MigrationsOffByDefault`, `TestPing_RequiresLatestSchema` (fail-first); `TestRoles_RuntimeRoleHasNoDDL`, `TestMigrateCommand_BringsEmptyDatabaseToReady` (new, written with the fix) | 5591ac5 |
| M6 | The NetworkPolicy admitted ingress only from a `gateway` namespace, but a GKE Gateway reaches pods from Google's proxy and health-check ranges: every request and probe would be dropped. `/metrics` was on the public API port and no scraper was admitted. | kubeconform checks schema, not NetworkPolicy semantics; nothing has run on a cluster. | `TestNetworkPolicyAdmitsLoadBalancerAndScraper`, `TestMetricsAreNotOnTheAPIListener` | a199e4b |
| M7 | Terraform declared controls nothing enforced: Binary Authorization with no policy (admit all), a public control plane, an unused `cloudsql.client` grant, Secret Manager IAM to a GSA while pods read a plain Kubernetes Secret, and a DSN with no `sslmode`. | `terraform validate` checks syntax, not that a control has a policy behind it. | `TestTerraformControlsAreReal`, `TestPodsReadSecretsThroughSecretManagerCSI`, `TestLoad_DatabaseMustVerifyServerCertificate` | a8df0a9 |
| M8 | The staleness check hashed every floor the publisher had, so any change anywhere made a plan stale; on a busy publisher apply would almost never succeed. | A design trade-off, not a defect a tool can see. Tests used quiet publishers. | `TestApply_UnrelatedChangeDoesNotMakePlanStale`, `TestFootprint_CoversOnlyThePlansSegments` | f317a05 |
| L3 | A lost key left its `in_progress` attempt blocking the plan for every other key forever, and a crashed worker's platform writes left no audit row. | Same as M1: no test lost a key or killed a worker. | storetest `AbandonedAttemptFreesPlan`, `TestApply_AfterAnInterruptedAttempt` | 74fceee |
| L4 | The runtime base image followed a moving tag while the builder was pinned by digest. | No linter for Dockerfiles in the chain. | `TestDockerfilePinsEveryBaseImageByDigest` | 28879ff |
| L5 | `ErrUnauthenticated`'s comment said the rejection reason "is logged server-side only"; nothing logged it. | A comment is not checked. | `TestVerify_Rejects` (reason per case), `TestRejectedTokenReasonIsLoggedNotReturned` | bbc821a |
| L6 | The recoverer sat outside the access log, so a panicking request got no access line, no 5xx count, and no request ID in the panic log. | The existing test called the recoverer alone, not the assembled chain. | `TestPanicStillCountsAndLogs` | 11d50f5 |
| L7 | After a reclaim, the interrupted attempt's own writes were reported as someone else's drift. | Same as L3. | `TestApply_AfterAnInterruptedAttempt/same_key_reclaims`, `TestSplitDrift_OwnWritesVersusForeignChanges` | 74fceee (with L3) |
| L8 | A Secret volume's `0400` mode worked only because of `fsGroup`. | Moot: M7 replaced the Kubernetes Secret volume with the Secret Manager CSI driver. | `TestPodsReadSecretsThroughSecretManagerCSI` (no Secret volume remains) | a8df0a9 |
| L9 | The metadata-server egress rule used the non-Dataplane-V2 address (`169.254.169.252:988`) on Autopilot, which is Dataplane V2; and the pod needs no metadata access at all. | Same as M6. | `TestPodsHaveNoMetadataServerEgress` | ad25cc4 |
| Nits | 4xx from the platform mapped to 502; case-sensitive `Bearer`; `New` dropped a caller's `RiskyChangePct`; nil-unsafe `Get`/`Describe`; a capitalised error string; `scanRule` order. | staticcheck's ST1005 skips a first word with an inner capital (`Idempotency-Key`); the rest are behaviour no linter models. | `TestApplyStatusCodes`, `TestBearerSchemeIsCaseInsensitive`, `TestNew_FillsLimitsFieldByField`, `TestRegistry_NilIsNoop`, `TestErrorStringsStartLowerCase` (scanRule has no observable change and no test) | 00ba428 |
| Ops | No runbook and no alerts. Writing the alert rules then caught `apply_record_failures_total` being emitted with no HELP registration. | Nothing checked that an alert's metric exists. | `TestAlertRulesUseExportedMetrics`; `promtool check rules` | 98eeff3 |

Where the process deviated from "test first, then fix":
- H2 also needed a separate, bounded wait for applies after `srv.Shutdown`, not just a longer grace period.
- M4's test helper wired `MemQueue` before the fix and `Dispatcher` after; the scenario and assertion did not change.
- M5's `migrate` subcommand and roles tests are new tests written with the fix, not fail-first.
- L3 and L7 share one commit because both live in `BeginApply` and the stale reason.
- M8 changed what the earlier drift test means: it now drifts the planned segment, because an unrelated segment no longer makes a plan stale.

### Failing output before each fix

```
H1  service_test.go:325: apply returned context deadline exceeded; the outcome must be recorded, not lost
H2  main_test.go:136: attempt status when run returned = in_progress, want succeeded (apply was cut off by shutdown)
    --- FAIL: TestShutdownWaitsForInFlightApply (0.02s)
M1  service_pg_integration_test.go:113: worker A's late result was accepted after its lease was taken over; it must be fenced off
    service_pg_integration_test.go:122: plan status = failed, want applied
    service_pg_integration_test.go:129: plan.apply audit rows = 2, want 1
    service_pg_integration_test.go:136: plan.applied events = 2, want 1
    --- FAIL: TestTwoWorkers_LeaseTakeoverFencesTheStaleWorker (1.03s)
M2  service_test.go:364: second click claimed a plan that was already applied (result stale)
    service_test.go:367: plan status = stale, want applied: the second click overwrote it
    suite.go:295: fresh key on an applied plan: created=true err=<nil>; want refused   (memstore and Postgres)
M3  service_test.go:349: cross-publisher replay: want ErrNotFound, got err=<nil> replayed=true result={PlanID:5daeef0b-... Status:applied ...}
    --- FAIL: TestApply_IdempotencyKeysAreTenantScoped (0.00s)
M4  events_test.go:163: event handled 0 times after the restart, want 1: it was marked sent before its consumer ran and was lost with the process
M5  config_test.go:40: RUN_MIGRATIONS defaults to true: the runtime role would need DDL rights
    pgstore_integration_test.go:43: Ping succeeded on a database with no schema; pods would go Ready and fail every request
M6  server_test.go:384: GET /metrics on the API handler = 200, want 404
    deploy_test.go:41: floorsvc-allow does not admit "cidr: 35.191.0.0/16"
M7  infra_test.go:37: main.tf: missing resource "google_binary_authorization_policy"
    infra_test.go:41: main.tf grants roles/cloudsql.client, which nothing uses
    config_test.go:73: DSN "postgres://app@10.20.0.3/floorrules" accepted without server verification (err=<nil>)
M8  service_test.go:302: apply = stale, <nil>; want applied: only untouched segments changed
L1  pre-commit: golangci-lint not installed, CI will run it ... pre-commit: ok  (exit=0)
L2  --- SKIP: TestContract (0.00s) ... ok github.com/arielagor/floorrules-go/internal/store/pgstore  (exit=0, with CI=true)
L3  suite.go:286: audit after the claim = [plan.create]; want plan.apply.started written with it
L4  infra_test.go:65: FROM gcr.io/distroless/static-debian12:nonroot is not pinned by digest
L5  auth_test.go:126: error "unauthenticated" does not name the reason "expired"
    server_test.go:438: no log line with the request id and the reason
L6  server_test.go:459: http_requests_total{code="500"} = 0, want 1
    server_test.go:474: panic log line has no request id
L7  service_test.go:395: reason = "a segment this plan changes was modified on the platform ..."; want it to say an earlier attempt was interrupted
L9  deploy_test.go:67: floorsvc-allow allows egress to a link-local metadata address: cidr: 169.254.169.252/32
Ops deploy_test.go:111: alert uses apply_record_failures_total, which main.go does not register
```

### What changes in the process

Checklist item "two replicas, or a pod killed halfway" existed on day 1 and did not catch M1, M2, H2 or L3, because it was answered by reading the happy path. The checklist now asks for the test that pauses or kills a worker, and AGENTS.md lists the six patterns above (deadline reuse, lease fencing, check-then-act, tenant-scoped lookups, at-least-once hand-off, shutdown grace) as things to write a test for, not to reason about.
