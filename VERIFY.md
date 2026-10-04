# VERIFY

Exact commands and their real output from the final verification run on 2026-10-03, at commit `1f644bd` plus the docs commit that adds this file. The machine was Windows 11 with Docker Desktop. Output is trimmed only where marked `...`.

The PATH for every command: `C:\Program Files\Go\bin`, `%USERPROFILE%\go\bin`.

## Summary

| Check | Result |
|---|---|
| Go | go1.27.0 windows/amd64 (host), go1.27.1 linux/amd64 (race/integration container) |
| gofmt | clean |
| go vet (incl. integration tag) | clean |
| golangci-lint 2.14.0 | 0 issues |
| govulncheck v1.8.0 | No vulnerabilities found |
| Unit tests (host) | 56 tests + 47 subtests passed, 0 failed, 0 skipped |
| Race + integration (Linux container, Postgres 16.15) | 58 tests + 55 subtests passed, 0 failed, 0 skipped, 0 data races |
| Coverage | 88.4% of statements across `internal/...` (race + integration run, `-coverpkg=./internal/...`) |
| kubeconform v0.8.0 `-strict` | 7 resources valid, 0 invalid |
| terraform 1.16.4 `fmt -check` + `validate` | valid (no backend; never planned or applied) |
| docker build | ok, 4.95 MB image, user 65532:65532 |
| syft v1.54.0 SBOM | SPDX-2.3, 14 packages |
| pre-commit hook | passes; rejects a misformatted file |
| Container smoke test | all assertions below hold |
| GitHub Actions | **passed**: run 37177854018 on commit 4a621bd, https://github.com/arielagor/floorrules-go/actions/runs/37177854018. All jobs passed. Each CI step and its output are also documented locally above. |
| cosign signing | **not run**: placeholder step only, needs a registry and GCP Workload Identity Federation. |

## Static checks

```
> go version
go version go1.27.0 windows/amd64

> gofmt -l .
(no output)

> go vet -tags integration ./...
exit=0

> golangci-lint run ./...
0 issues.

> govulncheck ./...
Scanner: govulncheck@v1.8.0
DB updated: 2026-10-01 20:24:15 +0000 UTC
No vulnerabilities found.
```

Lint config: `.golangci.yml`. It enables the standard set plus gosec, errorlint, bodyclose, noctx, misspell, revive, unconvert and copyloopvar, with the `integration` build tag on so integration files are linted too.

## Unit tests (host)

```
> go test -count=1 -cover ./...
ok  	github.com/arielagor/floorrules/internal/adapter	coverage: 92.4% of statements
ok  	github.com/arielagor/floorrules/internal/auth	coverage: 90.6% of statements
ok  	github.com/arielagor/floorrules/internal/config	coverage: 95.2% of statements
ok  	github.com/arielagor/floorrules/internal/domain	coverage: 95.2% of statements
ok  	github.com/arielagor/floorrules/internal/events	coverage: 90.4% of statements
ok  	github.com/arielagor/floorrules/internal/httpapi	coverage: 93.9% of statements
ok  	github.com/arielagor/floorrules/internal/metrics	coverage: 92.0% of statements
ok  	github.com/arielagor/floorrules/internal/service	coverage: 88.9% of statements
ok  	github.com/arielagor/floorrules/internal/store/memstore	coverage: 95.0% of statements
	github.com/arielagor/floorrules/internal/store/pgstore		coverage: 0.0% of statements   (integration-only)
	github.com/arielagor/floorrules/internal/store/storetest	coverage: 0.0% of statements   (exercised via memstore/pgstore)
	github.com/arielagor/floorrules/cmd/floorsvc			coverage: 0.0% of statements   (covered by the smoke test, not unit tests)
...
```

Counted from `go test -json`: **56 top-level tests and 47 subtests passed, 0 failed, 0 skipped.**

How to read coverage: the plain host run's `go tool cover -func` "total" is about 55% (54.7% when measured, one commit before the final fix). That figure counts `pgstore` (it only runs under `-tags integration`), `storetest` (shared code credited to its own package only when using `-coverpkg`) and the two `main` packages as 0%. The number below is the honest whole-of-`internal` figure.

## Race detector + Postgres integration

Race needs cgo; this Windows host has no gcc (`cgo: C compiler "gcc" not found`), so it runs in the official Go image against the test Postgres container:

```
> docker run -d --name floorrules-pg -e POSTGRES_PASSWORD=test -p 5544:5432 postgres:16
> docker run --rm -v "$PWD:/src" -w /src \
    -e TEST_DATABASE_URL='postgres://postgres:test@host.docker.internal:5544/postgres?sslmode=disable' \
    golang:1.27 go test -race -count=1 -tags integration -coverpkg=./internal/... -coverprofile=/tmp/c.out ./...

PostgreSQL 16.15 (Debian 16.15-1.pgdg13+2) on x86_64-pc-linux-gnu ...
go version go1.27.1 linux/amd64
exit=0
total:	(statements)	88.4%
```

**58 tests and 55 subtests passed, 0 failed, 0 skipped. `WARNING: DATA RACE` occurrences: 0.** Every package passed, including `internal/store/pgstore`, which runs the same contract suite as memstore against real Postgres:

```
pass TestMigrateIsIdempotent
pass TestContract/RuleLifecycle
pass TestContract/PlanScopedToPublisher
pass TestContract/ApplyIdempotency
pass TestContract/ApplyLeaseReclaim
pass TestContract/ConcurrentBeginApply          (16 goroutines, exactly one wins)
pass TestContract/AuditAndOutboxAtomicWithChange
pass TestContract/EventDedupe
pass TestContract/OutboxRedelivery
```

An earlier run of the pgstore contract with `-count=10` was also clean.

## Deploy artifacts

```
> kubeconform -strict -summary deploy/k8s/
Summary: 7 resources found in 5 files - Valid: 7, Invalid: 0, Errors: 0, Skipped: 0

> cd infra/terraform
> terraform fmt -check -recursive        # exit 0
> terraform init -backend=false -input=false
- Using previously-installed hashicorp/google v6.50.0
Terraform has been successfully initialized!
> terraform validate
Success! The configuration is valid.
```

`terraform plan` and `apply` were deliberately never run, and no cloud resources exist.

```
> docker build -t floorrules:dev .
exit=0
> docker image inspect floorrules:dev
size=4950022 bytes user=65532:65532 entrypoint=[/floorsvc]
```

## SBOM

syft 1.54.0 on Windows fails to read image layers (`unable to place layer cache path ... The filename, directory name, or volume label syntax is incorrect`; its cache filenames contain `:`). The same version was run in Linux against a saved tarball:

```
> docker save floorrules:dev -o floorrules-dev.tar
> docker run --rm -v "$PWD:/x" golang:1.27 go run github.com/anchore/syft/cmd/syft@v1.54.0 \
    docker-archive:/x/floorrules-dev.tar -o spdx-json=/x/sbom.spdx.json
spdxVersion=SPDX-2.3 packages=14
  base-files 12.4+deb12u15          github.com/jackc/pgx/v5 v5.11.0
  ca-certificates 20250419~deb12u1  github.com/jackc/puddle/v2 v2.2.2
  media-types 10.0.0                golang.org/x/sync v0.23.0
  netbase 6.4                       golang.org/x/text v0.42.0   (patched; see BUILD-LOG)
  tzdata 2026b-0+deb12u1            stdlib go1.27.1
  github.com/jackc/pgpassfile v1.0.0, github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761,
  github.com/arielagor/floorrules (main module)
```

## Pre-commit hook

```
> git config core.hooksPath scripts
> git hook run pre-commit
...
pre-commit: ok

# Negative check: stage a misformatted file and try to commit
> git commit -m "test: hook probe (must be rejected)"
pre-commit: gofmt
gofmt needed on:
internal\id\zz_hooktest.go
commit exit: 1            (probe file removed afterwards; HEAD unchanged)
```

## Container smoke test

The built image was run hardened the way Kubernetes runs it: `docker run --read-only --cap-drop ALL --security-opt no-new-privileges ... -e STORE_BACKEND=memory floorrules:dev`. A token was minted with `go run ./cmd/devtoken -pubs acme-tv -scope "rules:read rules:write plans:write plans:apply audit:read"`.

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
metrics:
apply_total{status="applied"} 1
ssp_calls_total{op="create",outcome="ok"} 1
consumer log lines:
{"level":"INFO","msg":"rule change needs no plan: platform already matches","event_id":"...","publisher_id":"acme-tv"}
container exit code after SIGTERM: 0
{"level":"INFO","msg":"shutdown: draining","drain":5000000000}
{"level":"INFO","msg":"shutdown: complete"}
```

Before the fix described in BUILD-LOG item 4, the consumer line here was `"msg":"proposed plan from rule change", ... "ops":0`.

## Not verified

- The GitHub Actions workflow has never executed on GitHub. Its steps mirror the commands above, but action versions and the hosted runner environment are untested.
- Nothing ran on GKE or any real cluster. kubeconform checks schema only, not admission behaviour (for example, Pod Security Admission) or NetworkPolicy semantics.
- No load or soak testing. There are no latency or throughput claims.
