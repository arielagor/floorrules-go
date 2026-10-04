# VERIFY

Exact commands and their real output from the verification run on 2026-10-04, after the day-2 review fixes (BUILD-LOG.md), at commit `7e45ae3`. The commit that updates this file changes only Markdown. The machine was Windows 11 with Docker Desktop. Output is trimmed only where marked `...`.

The PATH for every command: `C:\Program Files\Go\bin`, `%USERPROFILE%\go\bin`.

## Summary

| Check | Result |
|---|---|
| Go | go1.27.0 windows/amd64 (host), go1.27.1 linux/amd64 (race/integration container) |
| gofmt | clean |
| go build, go vet (with and without the integration tag) | clean |
| golangci-lint 2.14.0 | 0 issues |
| govulncheck v1.8.0 | No vulnerabilities found |
| Unit tests (host) | 86 tests + 53 subtests passed, 0 failed, 0 skipped |
| Race + integration (Linux container, Postgres 16.15, `CI=true`) | 94 tests + 65 subtests passed, 0 failed, 0 skipped, 0 data races |
| Coverage | 86.4% of statements across `internal/...` (race + integration run, `-coverpkg=./internal/...`); CI's integration job reports the same 86.4% |
| kubeconform v0.8.0 `-strict`, CRD schemas from the datreeio CRDs-catalog | 14 resources valid, 0 invalid, 0 skipped |
| promtool 3.15.0 `check rules` on the alert rules' groups | SUCCESS: 4 rules found |
| terraform 1.16.4 `fmt -check` + `validate` | valid (no backend; never planned or applied) |
| docker build | ok; image 21.9 MB unpacked (`docker images`), 4.97 MB compressed content (`docker image inspect` `.Size` under Docker Desktop's containerd store); user 65532:65532 |
| syft v1.54.0 SBOM | SPDX-2.3, 14 packages |
| pre-commit hook | passes; rejects a misformatted file; fails when a tool is missing (BUILD-LOG L1) |
| Container smoke test | all assertions below hold |
| GitHub Actions | **passed**: run 37184234234 on commit 7e45ae3, https://github.com/arielagor/floorrules-go/actions/runs/37184234234. Jobs check, integration, manifests and image all succeeded. |
| cosign signing | **not run**: placeholder step only, needs a registry and GCP Workload Identity Federation. |

## Static checks

```
> go version
go version go1.27.0 windows/amd64

> gofmt -l .
(no output)

> go build ./...
exit=0

> go vet ./...
(no output)
> go vet -tags integration ./...
exit=0

> golangci-lint run ./...
0 issues.

> govulncheck ./...
Go: go1.27.0
Scanner: govulncheck@v1.8.0
DB: https://vuln.go.dev
...
No vulnerabilities found.
```

Lint config: `.golangci.yml`. It enables the standard set plus gosec, errorlint, bodyclose, noctx, misspell, revive, unconvert and copyloopvar, with the `integration` build tag on so integration files are linted too.

## Unit tests (host)

```
> go test -count=1 -cover ./...
	github.com/arielagor/floorrules-go/cmd/devtoken		coverage: 0.0% of statements
ok  	github.com/arielagor/floorrules-go/cmd/floorsvc	0.858s	coverage: 57.5% of statements
ok  	github.com/arielagor/floorrules-go/internal/adapter	1.275s	coverage: 92.4% of statements
ok  	github.com/arielagor/floorrules-go/internal/auth	0.845s	coverage: 90.8% of statements
ok  	github.com/arielagor/floorrules-go/internal/config	0.850s	coverage: 93.2% of statements
ok  	github.com/arielagor/floorrules-go/internal/domain	0.854s	coverage: 92.5% of statements
ok  	github.com/arielagor/floorrules-go/internal/events	1.871s	coverage: 89.3% of statements
ok  	github.com/arielagor/floorrules-go/internal/httpapi	1.723s	coverage: 93.1% of statements
	github.com/arielagor/floorrules-go/internal/id		coverage: 0.0% of statements
ok  	github.com/arielagor/floorrules-go/internal/metrics	1.333s	coverage: 95.6% of statements
ok  	github.com/arielagor/floorrules-go/internal/service	1.438s	coverage: 84.0% of statements
?   	github.com/arielagor/floorrules-go/internal/store	[no test files]
ok  	github.com/arielagor/floorrules-go/internal/store/memstore	1.281s	coverage: 89.8% of statements
	github.com/arielagor/floorrules-go/internal/store/pgstore		coverage: 0.0% of statements
	github.com/arielagor/floorrules-go/internal/store/storetest		coverage: 0.0% of statements
```

Counted from `go test -count=1 -json ./...`: **86 top-level tests and 53 subtests passed, 0 failed, 0 skipped.**

How to read coverage: a plain host run counts `pgstore` (it only runs under `-tags integration`), `storetest` (shared code, credited to its own package only with `-coverpkg`) and the `devtoken` tool as 0%, which is why CI's unit-job total is 56.3%. The whole-of-`internal` figure is the one below.

## Race detector + Postgres integration

Race needs cgo; this Windows host has no gcc (`cgo: C compiler "gcc" not found`), so it runs in the official Go image against the test Postgres container. `CI=true` is passed through, so a missing `TEST_DATABASE_URL` would fail the run instead of skipping (BUILD-LOG L2).

```
> docker run -d --name floorrules-pg -e POSTGRES_PASSWORD=test -p 5544:5432 postgres:16
> docker run --rm -v "$PWD:/src" -w /src -e CI=true \
    -e TEST_DATABASE_URL='postgres://postgres:test@host.docker.internal:5544/postgres?sslmode=disable' \
    golang:1.27 go test -race -count=1 -tags integration -json -coverpkg=./internal/... -coverprofile=c-int.out ./...

PostgreSQL 16.15 (Debian 16.15-1.pgdg13+2) on x86_64-pc-linux-gnu ...
go version go1.27.1 linux/amd64
exit=0
> go tool cover -func=c-int.out
total:	(statements)	86.4%
```

**94 tests and 65 subtests passed, 0 failed, 0 skipped. `WARNING: DATA RACE` occurrences: 0.** Every package passed. The tests that need Postgres, all passing:

```
pass TestContract/RuleLifecycle
pass TestContract/PlanScopedToPublisher
pass TestContract/ApplyIdempotency
pass TestContract/ApplyLeaseReclaim
pass TestContract/ConcurrentBeginApply            (16 goroutines, exactly one wins)
pass TestContract/ClaimRequiresPendingPlan
pass TestContract/IdempotencyKeysScopedToPublisher
pass TestContract/AbandonedAttemptFreesPlan
pass TestContract/AuditAndOutboxAtomicWithChange
pass TestContract/EventDedupe
pass TestContract/OutboxRedelivery
pass TestContract/OutboxFailureBackoffAndDeadLetter
pass TestMigrateIsIdempotent
pass TestMigrateCommand_BringsEmptyDatabaseToReady
pass TestPing_RequiresLatestSchema
pass TestRoles_RuntimeRoleHasNoDDL
pass TestSlowPlatformRecordsResultInPostgres
pass TestTwoWorkers_LeaseTakeoverFencesTheStaleWorker   (two Services, one Postgres)
```

## Deploy artifacts

```
> kubeconform -strict -summary -schema-location default \
    -schema-location 'https://raw.githubusercontent.com/datreeio/CRDs-catalog/main/{{.Group}}/{{.ResourceKind}}_{{.ResourceAPIVersion}}.json' \
    deploy/k8s/
Summary: 14 resources found in 9 files - Valid: 14, Invalid: 0, Errors: 0, Skipped: 0

> promtool check rules rules.yaml      # spec.groups of deploy/k8s/prometheus-rules.yaml, in prom/prometheus:latest
Checking /r/rules.yaml
  SUCCESS: 4 rules found

> cd infra/terraform
> terraform fmt -check -recursive        # exit 0
> terraform init -backend=false -input=false
- Using previously-installed hashicorp/google v6.50.0
Terraform has been successfully initialized!
> terraform validate
Success! The configuration is valid.
```

The PodMonitoring, Rules and SecretProviderClass resources are validated against their CRD schemas, not skipped. `TestAlertRulesUseExportedMetrics` (a unit test) checks that every metric an alert names is registered in `main.go` and emitted by the code. How to act on the alerts is in RUNBOOK.md.

`terraform plan` and `apply` were deliberately never run, and no cloud resources exist.

```
> docker build -t floorrules:dev .
exit=0
> docker images floorrules:dev
floorrules:dev 21.9MB                                  (unpacked)
> docker image inspect floorrules:dev
size=4973143 bytes user=65532:65532 entrypoint=[/floorsvc]   (compressed content size)
```

## SBOM

syft 1.54.0 on Windows fails to read image layers (`unable to place layer cache path ... The filename, directory name, or volume label syntax is incorrect`; its cache filenames contain `:`). The same version was run in Linux against a saved tarball:

```
> docker save floorrules:dev -o floorrules-dev.tar
> docker run --rm -v "$PWD:/x" golang:1.27 go run github.com/anchore/syft/cmd/syft@v1.54.0 -q \
    docker-archive:/x/floorrules-dev.tar -o spdx-json=/x/sbom.spdx.json
spdxVersion=SPDX-2.3 packages=14
  base-files 12.4+deb12u15          github.com/jackc/pgx/v5 v5.11.0
  ca-certificates 20250419~deb12u1  github.com/jackc/puddle/v2 v2.2.2
  media-types 10.0.0                golang.org/x/sync v0.23.0
  netbase 6.4                       golang.org/x/text v0.42.0   (patched; see BUILD-LOG)
  tzdata 2026b-0+deb12u1            stdlib go1.27.1
  github.com/jackc/pgpassfile v1.0.0, github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761,
  github.com/arielagor/floorrules-go (main module)
```

CI's image job uploads the same SBOM as the `sbom` artifact on every run.

## Pre-commit hook

```
> git config core.hooksPath scripts
> git commit ...          # every commit in the day-2 series went through it
...
pre-commit: govulncheck
No vulnerabilities found.
pre-commit: ok

# Negative check: stage a misformatted file and try to commit
> git commit -m "test: hook probe (must be rejected)"
pre-commit: gofmt
gofmt needed on:
internal\id\zz_hooktest.go
commit exit: 1            (probe file removed afterwards; HEAD unchanged)
```

With golangci-lint off PATH the hook now exits 1 instead of skipping it (BUILD-LOG L1, commit aa08d5c).

## Container smoke test

The built image was run hardened the way Kubernetes runs it: `docker run --read-only --cap-drop ALL --security-opt no-new-privileges -p 18080:8080 -p 19090:9090 ... -e STORE_BACKEND=memory floorrules:dev`. Tokens were minted with `go run ./cmd/devtoken -pubs acme-tv -scope "..."`.

```
healthz: 200
readyz: 200
no token: 401
read-only token POST: 403
other publisher: 403
rule created: geo=US demand_partner=* status=active          (normalised: "us" -> "US", blank -> "*")
invalid rule: 400  fields: floor_micros, segment.device, segment.geo   (all errors in one response)
plan: status=pending ops=1 op0=create to_micros=12500000
apply #1: 200 replayed= body={"plan_id":"...","status":"applied","results":[{... "outcome":"applied","attempts":1}]}
apply #2 same key: 200 replayed=true identical-body=True
apply new key on applied plan: 409
metrics on API port: 404
metrics (port 9090):
apply_total{status="applied"} 1
ssp_calls_total{op="create",outcome="ok"} 1
ssp_calls_total{op="list",outcome="ok"} 2
consumer log lines:
{"level":"INFO","msg":"rule change needs no plan: platform already matches","event_id":"...","publisher_id":"acme-tv"}
container exit code after SIGTERM: 0
{"level":"INFO","msg":"shutdown: draining","drain":5000000000}
{"level":"INFO","msg":"shutdown: complete"}
```

Before the fix described in BUILD-LOG item 4, the consumer line here was `"msg":"proposed plan from rule change", ... "ops":0`.

## Not verified

- Nothing ran on GKE or any real cluster. kubeconform checks schema only, not admission behaviour (for example, Pod Security Admission or Binary Authorization), NetworkPolicy semantics, or whether the Secret Manager CSI driver and Managed Prometheus behave as configured. The deploy tests in `cmd/floorsvc/deploy_test.go` check specific properties of the manifests, not a running cluster.
- The RUNBOOK's SQL and kubectl steps were checked against the code and schema, not exercised in an incident.
- CI's action versions are pinned by major tag, not commit SHA.
- No load or soak testing. There are no latency or throughput claims.
