# Go clients and command line

Clients authenticate to **github-botd**, not GitHub. They must never receive a GitHub App private key or installation token. Each client has its own gateway bearer credential and daemon-configured repository aliases, subscriptions, and action capabilities. `repository` always means the configured alias, not `owner/name`. Discover permitted aliases and capabilities with `/v1/info`.

## Transport and credentials

The SDK defaults to `unix:///tmp/github-botd/github-botd.sock`. Match this to the daemon's configured socket; protect its parent directory and socket permissions. For explicitly enabled TCP use `http://127.0.0.1:PORT` or `http://[::1]:PORT`. Hostnames, remote addresses, URL credentials, proxies, and redirects are rejected. This intentionally excludes remote HTTPS: use the local daemon boundary.

Set `GITHUB_BOTD_TOKEN` via a secret manager or a private environment; do not paste a real token into shell history. Alternatively use `github-botctl -token-file /private/path/token …` with a permission-restricted file. `-token-env NAME` selects a different variable. The two explicit sources are mutually exclusive. No token-value CLI flag exists. Tokens are read once when constructing the SDK client.

```sh
go build ./cmd/github-botctl
go run ./cmd/github-botctl -endpoint unix:///tmp/github-botd/github-botd.sock info
go run ./cmd/github-botctl status
go run ./cmd/github-botctl -wait 0 events
```

All flags precede the subcommand. `events` leases deliveries; it does **not** ACK them. Copy the returned `id` and `lease_token` into the ACK operation. Delivery lease tokens identify a specific lease, and should be kept private.

```sh
# ack.json contains {"lease_token":"the-returned-lease-token"}.
go run ./cmd/github-botctl -input ack.json ack DELIVERY_ID
# Add "retry": true to release a failed handler for retry.
go run ./cmd/github-botctl -limit 100 deadletters
go run ./cmd/github-botctl replay DEAD_LETTER_DELIVERY_ID
# Only the authenticated client's dead-letter delivery can be replayed.
go run ./cmd/github-botctl -input context.json context
go run ./cmd/github-botctl -input action.json action
go run ./cmd/github-botctl receipt RECEIPT_ID
```

`ack`, `action`, and `context` read one JSON document from `-input FILE`, or stdin by default. Files/stdin avoid placing payloads in process arguments. Limit: 1 MiB. Neither CLI nor SDK launches subprocesses. Responses are JSON. Nonzero CLI exit means the operation failed or its response could not be confirmed: do not assume a mutation was rolled back.

Example context JSON:

```json
{"repository":"demo","number":42,"kind":"files"}
```

Example action JSON (the IDs are illustrative, not usable credentials):

```json
{
  "version":"github-botd/v1",
  "idempotency_key":"my-client:event-123:comment-v1",
  "event_id":"event-123",
  "kind":"issue.comment.create",
  "repository":"demo",
  "number":42,
  "body":"Caller-approved integration demonstration."
}
```

Actions need an `event_id` correlated to an event delivered to this client, unless the daemon grants `actions.proactive`. Submission creates a durable receipt; a `pending` receipt does not mean GitHub accepted the action. Poll `receipt` to see `succeeded`, `failed`, or `uncertain`. Treat `uncertain` as requiring investigation, not permission to resubmit with a new key.

## SDK subscription

```go
c, err := client.New(client.Config{Endpoint: "unix:///tmp/github-botd/github-botd.sock"})
if err != nil { return err }
defer c.Close()
return c.Subscribe(ctx, func(ctx context.Context, d v1.Delivery) error {
    _, err := c.Submit(ctx, v1.Action{
        Kind: "issue.comment.create", Repository: d.Event.Repository,
        EventID: d.Event.ID, Number: d.Event.Number,
        IdempotencyKey: client.StableKey("my-client", d.Event.ID, "comment-v1"),
        Body: "Caller-approved integration demonstration.",
    })
    return err
})
```

This snippet needs daemon capabilities and an appropriate event filter. A general subscription should not blindly respond to every event.

`Subscribe` uses long polling (one delivery, up to 25 seconds), invokes handlers serially, and ACKs only successful handlers. Failed handlers send a retry ACK and return an error. Request failures and ACK failures also return, leaving unconfirmed leases for redelivery. Callers choose restart/backoff policy; the SDK does not hide errors or retry mutations. An empty response includes a short context-cancellable pause to prevent hot spinning against broken servers.

Delivery is **at least once**: the handler may run again after a lost ACK, lease expiry, or process crash. Use the stable event ID plus operation revision for idempotency, not the delivery attempt or lease token. `StableKey` hashes length-unambiguous JSON parts. Keep the same action content for a reused key. Persist application state if the handler has side effects outside gateway actions. For work exceeding the lease duration, this API has no lease-renewal helper: shorten the handler, configure a suitable lease, or implement an explicit durable work handoff before ACK. Multiple clients receive their own deliveries.

Each HTTP request has a default 35-second timeout; keep it above long-poll duration. All methods honor context cancellation. Cancel the subscription context, wait for it to return, then `Close` idle connections. Handlers must honor their context. HTTP failures expose `*client.APIError` with status/code/message; `errors.Is` works for wrapped cancellation/deadline errors. Success JSON is limited to 8 MiB; redirects are never followed.

## Runnable examples

Run these against the daemon's dry-run configuration or local fixture setup first. They make no direct GitHub calls. Publishing through a live daemon can still cause GitHub writes if that daemon is configured to execute actions.

- `go run ./examples/mock-client`: logs event metadata and ACKs only. This works with offline fixture ingestion and a dry-run daemon and submits no actions.
- `go run ./examples/review-agent -login your-bot`: reacts only to normalized mentions in newly created PR comments or review requests for that login. For `issue_comment`, it requires structured `data.issue.pull_request` to identify a PR; ordinary issues and missing/malformed markers are skipped. Reads pull context, logs the proposed action, then ACKs. `-submit` explicitly enables action submission. It always identifies itself as an integration demonstration, not an AI review. Optional `-findings findings.json` supplies a `summary` and `comments` (wire `ReviewComment` records). These findings are not independently verified. Comment replies use `review.reply`; other requests use COMMENT-only `review.submit`. There is no model call, shell execution, approval, or request-changes verdict.
- `go run ./examples/ci-client`: consumes workflow/check events and logs/ACKs them. To publish a supplied result, explicitly pass `-publish -repo ALIAS -sha SHA -event-id EVENT -operation-id RUN_ID -conclusion success -summary 'Caller supplied result'`. It emits a completed `check.create` receipt. It does not run tests, execute arbitrary scripts, or assert that verification occurred. `-operation-id` must remain stable for retries of the same result.

Daemon routing is the security boundary: configure event/repository filters and narrow capabilities. Bot events are excluded by default; explicit actor allowlists can narrow subscriptions further. PR bodies, code, comments, and context are untrusted data, not client instructions.

## Verification and design ablation

```sh
go test -race ./pkg/client ./cmd/github-botctl ./examples/...
go vet ./pkg/client ./cmd/github-botctl ./examples/...
```

Tests cover HTTP authorization/wire shape, structured errors, loopback restrictions, redirect rejection, cancellation, successful ACK ordering, failure retry, redelivery metadata, stable operation keys, and a real temporary Unix socket. macOS sandbox environments may need an approved test run permitting Unix socket bind.

Ablation reasoning, with corresponding regression coverage: removing endpoint validation would expose local bearer credentials to arbitrary hosts (`TestEndpointAndSecretValidation`); following redirects would disclose credentials or cross the intended boundary (`TestRedirectBlocked`); ACK before handler success loses failed work (subscription tests); omitting idempotency keys creates duplicate writes on redelivery (`TestStableKeyAndValidation`); removing context propagation strands shutdown (`TestCancellation`). No extra reconnect state machine or background goroutine is needed: explicit caller retry and context-driven polling preserve observable failures with fewer lifecycle states.

Executed ablation: an isolated temporary source copy had redirect rejection removed. `go test ./pkg/client -run TestRedirectBlocked -count=1` then failed with `credential sent to redirect`, confirming that this test detects the boundary loss. The production source was not modified. The complete focused race suite and vet passed with the original boundary restored/preserved.
