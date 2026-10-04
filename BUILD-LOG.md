# Build log

An honest record of how this repo was made. It exists because how AI-written code was checked matters as much as the code.

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

The first real commit through the hook (the docs commit) was then blocked with `gofmt: command not found`, because Go was not on that shell's PATH. That is the intended behaviour: the hook fails closed when a tool is missing, instead of skipping it. The commit was re-run with Go on PATH and passed all five stages.

## Caught by review, not by a tool

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
