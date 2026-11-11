# Operation and recovery

## Processes and permissions

Run one daemon per state directory as an unprivileged user. The bbolt file is exclusively locked, owner-only, and opened without following the database leaf symlink. The actual state directory must also be owner-only and non-symlink; ancestor aliases such as macOS `/var` are allowed. The socket parent must be owned by the process user and mode 0700, with a 0600 socket. Existing paths are never silently chmodded. Regular files and active sockets are never removed to force startup.

Daemon flags: `-config PATH`, `-check`, `-version`. SIGINT/SIGTERM cancel listeners/workers and shut down gracefully. An interrupted outbound write may have succeeded upstream and is quarantined as uncertain on restart. Config reload is by restart; client grants and repository bindings are reevaluated. Rotating a bearer must retain the stable client ID to preserve its queue. A new client ID is a new queue and authority, not a continuation.

Repository aliases are pinned to repository+installation in state. Changing a pinned alias fails startup, even if temporarily removed from configuration. Use a new alias for a different target; never repurpose old queued work. Removed repository grants prevent pending deliveries, dead-letter reads/replay, and queued actions from escaping into the new scope. Revoking `actions.proactive` prevents old no-event actions executing after restart.

## Health and monitoring

`/healthz` proves process liveness; `/readyz` checks local state availability. Neither is a live GitHub authentication/permission probe. `/v1/status` exposes client-specific queue/action counters; `github-botctl status` is suitable for local monitoring. Monitor daemon error logs, disk space, dead-letter counts, and failed/uncertain receipts. Logs avoid tokens and webhook bodies. Client-provided body/error text remains sensitive operational data in the database and clients' own logs.

Delivery batches are leased, not destructively consumed. Keep handlers below the lease duration or durably hand work to another local queue before ACK. There is no lease extension; concurrent handlers must use stable idempotency. Default lease 120 seconds; configurable 1..3600 seconds. SIGKILL/power-loss tests cover committed local state, not physical storage failure guarantees.

## Dead-letter recovery

```sh
bin/github-botctl -endpoint unix:///absolute/run/github-botd.sock deadletters
bin/github-botctl -endpoint unix:///absolute/run/github-botd.sock replay DELIVERY_ID
```

Requires `events.replay` and current repository access. Only owned dead-letter deliveries can be replayed. Fix the handler/root cause first; replay resets attempts and creates a new lease on next claim. Already queued action idempotency keys still return the original receipt, avoiding duplicate action submission. Replaying an event does not retry a failed or uncertain GitHub action by itself.

For an `uncertain` receipt, inspect the exact GitHub target and timestamps. If the intended comment/review/check exists, do not resubmit it. If you have independently confirmed it did not execute, deliberately submit a new action with a new key. The daemon does not expose a blind uncertain-action retry or falsely mark unknown work successful. `failed` denotes a definite rejection/validation failure; use a new operation key after correcting the cause. Explicit rate-limit rejections are scheduled automatically with bounded backoff and attempt caps; 5xx mutation responses and transport ambiguity are not.

## Capacity and backups

`max_pending` currently bounds **retained** events, per-client deliveries, and actions separately. Completed rows remain because they are dedupe/receipt history. Hitting the cap rejects new unique work with 503 `capacity`; repeats of existing idempotency keys still resolve. There is no implicit pruning. Increase the configured cap with enough disk space or plan an explicit archival/migration window; do not delete the database as a routine recovery step.

To back up, stop the daemon gracefully, copy the closed `github-botd.db` to private storage, and retain configuration/repository bindings alongside it without including credentials in an insecure archive. Do not copy a live writable database as if it were a transactional snapshot. Restore only while stopped, with matching owner and private permissions. Restoring old state can replay GitHub operations performed after that backup; reconcile before enabling writes. Losing state loses dedupe and receipt guarantees.

## Polling

Polling is optional and off by default. It reads the repository activity feed, bootstraps the newest 100 events, and follows up to ten pages looking for its durable prior cursor. A missing cursor is a gap error and leaves state unchanged. It cannot recover arbitrary historical events, complete review-request transitions, or full workflow/check activity. GitHub can delay and truncate the feed. Webhooks are recommended for these features.

A whole batch is ingested before its cursor advances; individual inserts are transactional and deduplicated, so a restart can safely retry a partially persisted batch. Poll and webhook delivery IDs intentionally have different source namespaces: duplicate real-world activity can appear through both. Prefer one source per workload or use stable resource identity/revision in client idempotency keys, not only normalized event ID.

## Containers and service managers

The Dockerfile builds a CGO-free Linux binary and runs as UID/GID 65532. It is provided, not built or deployed as part of verification. Mount the config and secrets read-only, and private state/socket directories writable by that UID. App private keys and token files must be readable by the process and mode 0400/0600; file ownership is checked. Bind only the webhook interface to the container network if needed and set `allow_non_loopback` explicitly for that webhook listener. Keep IPC on the Unix socket shared only with permitted clients. Never publish an IPC TCP port.

For systemd/launchd use `bin/github-botd -config /absolute/config.json` as the process command, a stable working environment, appropriate user and private directories, and a restart policy. No service was installed, no auto-start setting changed, and no public webhook URL registered by this project creation.

## Untrusted input

GitHub comments, diffs, and workflow text are data from external actors. Actor allowlists default to requiring explicit entries; `"*"` is an intentional broad grant. Bot actors are excluded unless `include_bots` is enabled; CI routes often need the known GitHub Actions bot. Never send untrusted text to a shell. If you connect an LLM/agent, keep its execution permissions and prompt-injection defenses independent from this transport. The daemon's capabilities are an additional boundary, not a replacement for sandboxing.
