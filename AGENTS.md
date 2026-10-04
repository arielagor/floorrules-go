# AGENTS.md

Instructions for any coding agent (Claude Code, Codex, Cursor, etc.) working in this repo. Humans: read it too, it is the contract.

## What this is

`floorrules`: a Go 1.27 service that validates ad floor-price rules, computes a plan against the ad platform's current state, and applies it with idempotency, retries and an audit trail. Read README.md for the flow. Stack: stdlib `net/http`, `log/slog`, `pgx/v5`, Postgres 16. One third-party module; adding a second needs a reason written in the commit message.

## Layering (do not cross it)

- `internal/domain` is pure: no I/O, no context, no logging. Planner and validation live here.
- `internal/service` holds use cases and is the only package that talks to both the store and the adapter.
- `internal/httpapi` does transport only: decode, authorise, call the service, map errors. No business rules.
- `internal/store` defines the `Store` interface. Any behaviour change goes into `storetest` first so both memstore and pgstore are held to it.

## Conventions

- Money is `int64` micros. Never `float64`.
- Errors: wrap with `%w`, compare with `errors.Is` / `errors.As` (errorlint enforces it). Internal errors never reach the client; the client gets a request ID.
- Every request path has a deadline. Outbound platform calls go through `adapter.RetryPolicy.Do`.
- Secrets come only from env or `*_FILE` mounts. Never log a secret, DSN or token; never echo one in an error.
- Logs are structured (`slog`), with `request_id` on every request line.
- Every exported identifier has a doc comment (revive enforces it).
- Schema changes are new numbered files in `internal/store/pgstore/migrations`. Never edit an applied migration.

## Patterns that passed every tool and were still wrong

The day-2 review (BUILD-LOG.md) found these in code that was formatted, vetted, linted, scanned and green. Write code that avoids them, and write the test that would catch them.

- **A deadline is not reusable after it is spent.** Work that must happen after a long step (recording a result, releasing a claim) gets its own budget from `context.WithoutCancel(ctx)` plus a fresh timeout, never the context the long step just exhausted. Test it with a fake that consumes the whole first budget.
- **A lease needs a fence.** Anything reclaimable after a timeout carries a token that changes on every claim, and every write that finishes the work checks the token in the same statement. Without it, a worker that paused past its lease overwrites its successor. Test with two workers and a forced expiry, against Postgres.
- **Check-then-act belongs in one transaction.** A status read followed by a write is a race unless the write is conditional (`UPDATE ... WHERE status = 'pending'`) or the row is locked (`SELECT ... FOR UPDATE`). Test the second actor arriving between the check and the act.
- **Scope every lookup by tenant.** Not only list endpoints: idempotency keys, IDs from a path, anything a client can name. A key or ID that is unique only globally lets one tenant probe or replay another's. Key the table by `(publisher_id, ...)` and test the cross-tenant replay.
- **At-least-once means the durable copy stays until the work is done.** Never mark an outbox row sent when it is handed to an in-memory buffer; mark it when the consumer (or the broker's ack) says it is done. Test a crash between hand-off and handling.
- **Shutdown grace must cover the longest operation.** `terminationGracePeriodSeconds` covers drain plus the longest apply plus recording, and the process waits for in-flight work before it exits. `TestDeploymentGraceCoversLongestApply` pins the arithmetic; change both together.

## Definition of done

A change is done only when all of these hold, and the evidence is in your summary:

1. `gofmt -l .` prints nothing.
2. `go vet -tags integration ./...` is clean.
3. `golangci-lint run ./...` reports 0 issues.
4. `go test -race ./...` passes. On Windows without gcc, run it in `golang:1.27` (see VERIFY.md).
5. If store code changed: `go test -race -tags integration ./internal/store/...` passes against Postgres.
6. `govulncheck ./...` reports no vulnerabilities.
7. A behaviour change has a test that failed before the change. Say which.
8. If deploy files changed: kubeconform passes exactly as CI runs it (`-strict`, with the CRDs-catalog schema location so PodMonitoring, Rules and SecretProviderClass are checked, not skipped), and `terraform validate` passes. If alert rules changed, `promtool check rules` passes on their groups and `TestAlertRulesUseExportedMetrics` passes.

The pre-commit hook (`git config core.hooksPath scripts`) runs 1, 2, 3, a non-race 4, and 6. A missing tool fails the hook; nothing is skipped.

## What an agent may NOT do

- Run `git push`, add a git remote, open a PR, or publish anything yourself. The owner decides about publishing. On the owner's machine a post-commit hook pushes each commit that passed the pre-commit gate, and CI re-runs every check on the push.
- Run `terraform plan`/`apply`, `kubectl apply`, `gcloud`, or anything else that creates or touches cloud resources or costs money.
- Commit secrets, `.env` files, keys, or real customer/publisher data. Use the dev token tool.
- Skip or weaken a check to get green: no `--no-verify`, no `//nolint` without a written justification on the same line, no deleting or `t.Skip`-ing a failing test, no lowering lint config.
- Edit an applied migration, or change the `Store` contract without updating `storetest`.
- Claim a check passed without showing its output. "Should pass" is not evidence. If a check could not run, say so and why.
- Touch Postgres instances other than the test one named in VERIFY.md.
