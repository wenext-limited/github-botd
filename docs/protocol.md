# github-botd/v1 protocol

The implemented protocol is HTTP + JSON over a local Unix socket, or explicitly configured loopback TCP. There is no gRPC implementation. Go records live in `api/v1/types.go`; `api/v1/schema.json` provides JSON Schema 2020-12 definitions. Select an endpoint's `$defs` fragment for validation. The schema does not replace authorization, UTF-8 byte limits, event correlation, or action-specific semantic validation.

## Authority and identity

Every `/v1/` endpoint requires `Authorization: Bearer CLIENT_TOKEN`. Tokens identify one configured client; they are not GitHub credentials. Requests carrying `Origin` are rejected to avoid browser ambient authority. `/healthz` and `/readyz` expose minimal public health information. Readiness checks local store health and configuration, not live GitHub connectivity or permissions.

`repository` in IPC events, actions, and context requests is a configured **alias**. Only the daemon maps it to `owner/name` and an installation. Nested `Event.data` can still contain the real repository name and installation ID as context, never as permission to use a different repository.

Capabilities are explicit strings: `events.read`, `events.replay`, `context.read`, the seven action-kind strings below, and `actions.proactive`. Repository grants apply independently. `/v1/info` reports the authenticated identity, protocol string, allowed aliases, and capabilities. Routes additionally select repository, event type, action, actor allowlist, trigger, and recipient clients. Bot actors are excluded unless explicitly enabled. Routing is fan-out, not competing consumers across clients.

## Endpoint contract

- `GET /healthz`: 200 `Health`, status `ok`.
- `GET /readyz`: 200 or 503 `Health`, status `ready` or `not_ready`. Checks include `store` and `github`; the latter is `dry_run` or `configured_not_live_verified`.
- `GET /v1/info`: 200 `ClientInfo`.
- `GET /v1/status`: 200 client-scoped integer counters. Keys include `pending`, `inflight`, `deadletter`, `acked`, and `actions_pending`, `actions_running`, `actions_succeeded`, `actions_failed`, `actions_uncertain`.
- `GET /v1/events?limit=1&wait=25`: requires `events.read`. `limit` is 1..100; `wait` is 0..30 seconds. Defaults are 1 and 25. Returns 200 `{"deliveries": []}` or a batch of leased `Delivery` records. A GET here changes lease state.
- `POST /v1/events/{deliveryID}/ack`: requires `events.read`; body `AckRequest`. Success is 200 `{"status":"acked"}` or `{"status":"retrying"}` when `retry` is true.
- `GET /v1/deadletters?limit=100`: requires `events.replay` and current repository grants; limit 1..100. Returns 200 `{ "deliveries": [{ "id": "...", "event": {}, "attempts": 8, "error": "..." }] }` with `DeadLetter` records and no lease token. Use these IDs for recovery.
- `POST /v1/events/{deliveryID}/replay`: requires `events.replay`. Only the client's own dead-letter delivery is eligible. Success is 200 `{"status":"requeued"}`. No request body is required.
- `POST /v1/actions`: accepts `Action`; checks exact kind capability and repository grant. Returns 202 `Receipt`, including when returning an existing idempotent receipt.
- `GET /v1/actions/{receiptID}`: returns 200 `Receipt` only for the authenticated client's action.
- `POST /v1/context`: requires `context.read` and repository grant; body `ContextRequest`. Returns 200 adapter JSON (object or array), not an additional envelope. Supported kinds: `pull`, `files`, `issue_comments`, `review_comments`, `reviews`, `workflow_run`, `check_run`. Use PR/issue `number` for PR/issue reads and resource `id` for workflow/check reads. Offline dry-run without a GitHub adapter cannot perform these reads.

JSON request decoders reject unknown fields and trailing JSON. Successful protocol JSON responses use `Content-Type: application/json`, `Cache-Control: no-store`, and `X-Content-Type-Options: nosniff`. Unsupported routes/methods may use Go HTTP router responses rather than the application error envelope.

## Events, leases, and acknowledgement

`Event` contains stable normalized `id`, upstream `delivery_id`, `source`, GitHub `type`/`action`, alias, actor metadata, resource identifiers, body, mentions, requested reviewer/team, timestamp, and optional structured `data`. `Event.Data` is bounded, recursively sanitized GitHub JSON. Credential-like fields are redacted; this is not a guarantee that arbitrary user prose contains no secrets. Bodies, diffs, comments, and the entire data field remain untrusted input, never instructions or authority.

A `Delivery` wraps an event with a client-specific `id`, opaque `lease_token`, `lease_expires_at`, and `attempt`. Do not confuse the upstream delivery ID, normalized event ID, client delivery ID, and action receipt ID. ACK uses the client delivery ID and current lease token. Action correlation uses the normalized event ID.

The default lease is 120 seconds, configurable from 1 to 3600. Only a current, unexpired token can ACK or retry. Expired leases permit redelivery; attempts increment on claiming. Failed deliveries use retry delay and eventually dead-letter at the configured maximum attempt count (default 8). Replay resets an owned dead-letter delivery for processing; it does not grant new scope or create actions. There is no lease-renewal endpoint.

ACK only after completing or durably handing off the handler's work. Lost responses, crashes, or lease expiration can repeat delivery: handlers must tolerate **at-least-once** delivery. ACK is independent from outbox execution. ACK after receiving a durable pending action receipt confirms local handoff, not successful GitHub mutation.

## Actions and receipts

Exactly seven kinds exist:

1. `issue.comment.create`: add issue/PR conversation comment.
2. `issue.comment.update`: update a comment, with ownership checks in the adapter.
3. `review.reply`: reply to a delivered review thread.
4. `review.comment`: create one inline review comment.
5. `review.submit`: submit a **COMMENT-only** review, optionally with inline comments.
6. `check.create`: create a check run.
7. `check.update`: update an existing check run, with ownership checks.

The separate `pull.merge` capability supports only guarded Squash merges. There is no approval, request-changes verdict, arbitrary endpoint proxy, general repository mutation or script-execution action. PR review is one client use case, not the daemon's hard-coded purpose.

Actions require `version: "github-botd/v1"`, a 1..128-byte stable `idempotency_key`, kind, and repository target (canonical `owner/name` or a legacy alias). Without `actions.proactive`, an `event_id` correlated to that client's routed event is required. The event's repository and supplied issue/PR number must match; replies must target its delivered thread/comment. Authorization is checked again before queued execution.

Comment/review actions need positive `number` and content. Updates/replies need positive `comment_id`. Reviews need `head_sha`; the adapter checks the current PR head before posting. Inline comments need nonempty relative path, positive line, `LEFT`/`RIGHT` side, and body. At most 100 inline comments are accepted. Check creation needs `name` and `head_sha`; update needs positive `check_run_id`. Status is `queued`, `in_progress`, or `completed`; only completed checks accept a conclusion, and require one of `success`, `failure`, `neutral`, `cancelled`, `skipped`, `timed_out`, `action_required`.

The durable outbox scopes idempotency by authenticated client and key. An identical resubmission returns the prior receipt; changed content with the same key conflicts. Use event ID plus operation revision, never lease token or attempt count, to derive a stable key. Receipts have `pending`, `running`, `succeeded`, `failed`, or `uncertain` states plus attempts/timestamps and optional result/error. `succeeded` in dry-run has a `dry_run` result and does not indicate a GitHub write.

Ambiguous write transport failures and process interruption while running become `uncertain`. The daemon does not blindly retry them. Only proven nonexecuted transient failures, such as explicit pre-execution rate limits, are scheduled for bounded retry. Investigate GitHub before deciding whether a new action is needed. There is no exactly-once external write guarantee.

## Limits and errors

Webhook and action request bodies are limited by `limits.max_body_bytes` (default 2 MiB; configuration range 1 KiB..25 MiB). The GitHub adapter applies its own 4 MiB payload/response bound, so the effective accepted size can be lower. ACK and context JSON bodies are limited to 4096 bytes. ACK error text is at most 1000 bytes; action body/summary and each inline body are bounded to 60000 bytes. The SDK separately limits responses to 8 MiB and CLI JSON input to 1 MiB. Schema character counts do not replace server byte checks.

Application errors have `{"code":"...","message":"..."}`. Typical mappings: 400 `invalid_request`, 401 `unauthorized`, 403 `forbidden` or `origin_denied`, 404 `not_found`, 409 `lease_conflict` or `conflict`, 503 `capacity` or `store_unavailable`. State is not rolled back merely because a caller lost the response. Inspect receipts or retry the identical idempotent submission rather than inventing a new key.

## Separate webhook ingress

The webhook listener is distinct from local client IPC. `POST /hooks/{source}` requires JSON, valid `X-Hub-Signature-256` HMAC-SHA256 over the exact raw body, `X-GitHub-Delivery`, and `X-GitHub-Event`. Configured source installation restrictions and repository mappings are checked before durable routing. Valid `ping` returns 200 `{"status":"pong"}`; other accepted input returns 202 `{"accepted":true,"new":true|false}`. Unknown repositories are intentionally ignored. Deduplication is by source plus upstream delivery ID. Duplicate deliveries do not retroactively add route recipients.

## Canonical repositories and guarded merges

A client with `all_repositories: true` may address canonical `owner/name`
targets reachable by the daemon's machine-user credential without adding alias
entries. Capabilities remain independent. App installations require configured
repository/installation routing; ingress/polling also retains its explicit
repository configuration. Queued execution and receipt access resolve current
grants, so revocation is not bypassed by an older queued action.

Canonical writes also require `repository_id`, obtained from GitHub context
(for a PR, `base.repo.id`). The adapter compares this immutable ID before
writing, so replacing a repository at the same mutable name does not retarget
an older queued action. Existing alias-only clients remain compatible.

`POST /v1/merge-check` accepts a `pull.merge` Action and reads the live merge
conditions without writing. It returns `{"ready":true}` only on success. The
normal action/outbox path repeats verification before execution. The action's
`merge` evidence contains `requester_id`, `base_sha`, `review_digest` and an
optional native `build` attestation (`provider:"jenkins"`, `head_sha`, `source`,
`run`). It must come from a trusted native client with `pull.merge`; this is not
a schema for exposing identity or verdict fields to a model. iBot constructs
it from verified turn identity and observed reviewer/Jenkins results.

The daemon requires an open, non-draft PR at the assessed head/base, current
author/assignee identity, explicit clean mergeability, no outstanding requested
changes and successful trusted compilation at that head. Operator-configured
`merge_checks` bind a name to an app/creator ID. GitHub Actions additionally
requires a matching successful run/check suite and an unchanged trusted
`workflow_path` between base and head. Native Jenkins sources must be allowed
by `merge_jenkins_sources`. Missing/pending/failed/skipped/old proof refuses.

Execution sends `PUT /repos/{owner}/{repo}/pulls/{number}/merge` with exactly
`{"sha":"<reviewed head>","merge_method":"squash"}`. GitHub atomically checks
the head and mergeability; its endpoint provides no base/assignee CAS, so the
adapter rereads those just before writing. A response without `merged:true`
is not success. Ambiguous writes retain the existing uncertain/no-retry
contract; no admin bypass, force push, branch deletion or auto-merge is added.
