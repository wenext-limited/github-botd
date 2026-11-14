# Verification report

Validated locally on macOS/arm64 using Go 1.26.4. This report describes executed checks, not promises of production or live GitHub readiness.

## Executed checks

- `make check`: all package tests, race detector, vet, and gofmt checks pass.
- `make demo`: actual daemon/CLI binaries communicate through a private Unix socket while a signed synthetic webhook enters through an ephemeral loopback HTTP listener. Verified ingress deduplication, independent event/receipt IDs, durable ACK, repeated identical action submission, and dry-run receipt completion. No GitHub credentials are used.
- `go test -cover ./...`: statement coverage collected for core packages; entrypoint behavior also exercised by the standalone demo.
- Linux/amd64 daemon and CLI cross-builds validate platform compilation, not Linux runtime behavior.
- Strict sample configurations load successfully with synthetic test environment variables.
- JSON Schema syntax and field parity were checked against Go wire records. A separate full JSON Schema validator was not installed or run.

The local sandbox initially blocked temporary socket binding. Those network-free tests were rerun with approved local-listener permission and passed; they were not skipped.

## Live reload verification

`cmd/github-botd/reload_test.go` checks bearer/webhook-secret rotation, updated routing with retained deliveries, unchanged-config reuse, invalid JSON/config/credentials, immutable listener/state paths, alias identity rejection, concurrent HTTP requests, and prompt retirement of event long polls. A blocked fake GitHub write completes successfully through a reload while the next queued action fails after its grant is revoked. A loopback fake GitHub endpoint verifies forced reload of a machine-token file.

`cmd/github-botd/reload_process_test.go` starts the actual daemon entrypoint as a subprocess and uses the SDK over its private Unix socket. It verifies automatic file-only bearer rotation, invalid-edit rejection through a real SIGHUP, recovery after fixing the JSON, forced reload of unchanged JSON, and graceful SIGTERM without restarting the process during the checks. Gateway/store tests also cover live polling enablement/interval changes/disablement, retained polling cursors, live capacity/attempt bounds, and rollback when repository binding validation fails. These tests run in both the normal and race-detector suites.

Seven reload safeguards were separately ablated in temporary source copies: generation publication, fixed-resource validation, long-poll cancellation, durable alias validation, store-limit publication, fresh outbox snapshots, and fresh polling snapshots. Each targeted regression test failed when its safeguard was removed; all seven mutations compiled. No ablated source was retained in this checkout. The design checks show why a single startup snapshot, uncancelled long polls, or changing config without updating worker/store behavior do not satisfy live reload.

## Requested flows

`internal/transport/flows_test.go` uses the real webhook validator, routing, persistent store, HTTP IPC handler, outbox executor, and GitHub REST adapter. Only the external GitHub HTTP transport is replaced with controlled responses.

1. **PR mention:** signed `issue_comment.created` with a PR marker and configured mention routes to the client, which submits a commit-pinned COMMENT review and receives a succeeded receipt.
2. **Native review request:** signed `pull_request.review_requested` for the configured reviewer routes independently and produces a COMMENT-only review. This verifies event handling, not whether GitHub's UI will allow an arbitrary App bot to be requested.
3. **Inline follow-up:** signed `pull_request_review_comment.created` in a reply thread preserves the root comment ID and posts a reply to that root thread, not an unrelated top-level conversation.
4. **CI example:** signed `workflow_run` creates an App-authenticated check; a later signed `check_run` updates only that App-owned check. JWT/token exchange and mutation endpoints are exercised against fake responses.

The separate review example has tests for current-head extraction, root-thread choice, redelivery-stable keys, and rejecting regular issue comments/missing PR markers. It is a deterministic integration example, not an LLM code-review implementation.

## Failure/security coverage

- RSA JWT signing, installation-token caching/refresh, unknown installation rejection, credential-file permissions/ownership/no-follow/bounds.
- Signed/unsigned/tampered/oversized webhooks, wrong installation, strict JSON, client authentication, browser-Origin rejection, actor filters, default bot exclusion.
- Foreign client ACK/receipt access, expired/stale lease tokens, retry/backoff/dead letters, replay only from dead state, current repository grant filtering.
- Idempotency mismatch, concurrent repeated enqueue/claim, durable fan-out, transaction rollback, closed-store errors, retained-record capacity.
- Abrupt subprocess exit without graceful Close: committed records/cursors survive; previously running outbound actions become uncertain, pending actions remain pending.
- Durable repository-alias pins prevent old work being redirected after config changes; revoked proactive grants stop queued no-event work.
- Whole-token mention parsing, empty-child-body handling, large numeric ID preservation, bounded context pagination, cross-host/redirect rejection, credential-field redaction.
- COMMENT-only review payloads, stale-head rejection, own-author and exact-issue checks for comment edits, exact check-run/commit event correlation, App ownership checks for check updates.
- Rate-limit rejection versus ambiguous network/5xx outcomes; uncertain writes never silently replay.
- Poll bootstrap/cursor progression and bounded-history gap handling.

## Ablation study

Independent safeguards were temporarily removed in isolated copies, never in the delivered project. The relevant regression tests failed as expected when each was removed:

- Ingress delivery deduplication.
- Lease-token validation.
- Client receipt ownership.
- Current repository grant filtering.
- Immutable repository binding validation.
- Private state-directory mode validation.
- Dead-only replay restriction.
- Client redirect rejection (detected credential forwarding).
- Review current-head comparison.
- Comment exact-issue correlation.
- Check App-ownership comparison.

These checks establish that the tested safeguards are behaviorally necessary, not unused configuration switches. They are not a security audit certification or a claim that every vulnerability is excluded.

## Not run / remaining deployment checks

- No actual GitHub App/machine-user credentials read or configured.
- No live GitHub webhook delivery, comment, review, or check mutation.
- No GHES server compatibility test.
- No Docker image build/container startup or hosted GitHub Actions run.
- No public listener, relay, repository, PR, release, service installation, auto-start change, push, or deployment created.
- No load/soak benchmark, replicated storage, Windows daemon support, or exactly-once external action guarantee.

Before enabling writes, use a dedicated test repository and the least-privilege identity described in `github-setup.md`, inspect dry-run routing/receipts, then perform one deliberate live operation of each enabled kind.
