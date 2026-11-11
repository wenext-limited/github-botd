# Architecture and trust boundaries

`github-botd` is a local GitHub capability gateway. It centralizes credentials, ingress, routing, durable deliveries, and restricted outbound actions. It is not an AI reviewer or an arbitrary GitHub API proxy.

## Components

- `internal/config`: strict configuration, repository aliases, installation assignments, client credentials, explicit grants, routes, listener and resource bounds. Secrets come from environment variables or private files.
- `internal/github`: GitHub App JWT/installation-token or machine-user authentication, webhook normalization, bounded context reads, polling, and seven allowlisted outbound action adapters. App private keys and GitHub access tokens stay on this side of the boundary.
- `internal/transport`: separate signed webhook ingress and local bearer-authenticated HTTP IPC. JSON is the implemented wire format; no gRPC server is implemented.
- `internal/gateway`: identity checks, deterministic routing, event/action correlation, current authorization checks, polling loop, and outbox execution.
- `internal/store`: bbolt inbox, deduplication, independent client deliveries, leases, durable outbox, idempotency indexes, receipts, polling metadata, and repository-binding pins.
- `pkg/client`, `github-botctl`, and examples: unprivileged gateway consumers using per-client credentials. Clients select their own behavior within daemon grants. PR review and CI are examples, not privileged built-in products.

## Data flow

Signed webhook input, or an optional configured polling adapter, becomes a normalized `v1.Event`. The gateway checks installation/repository identity, replaces the public repository name with a configured alias, assigns a stable normalized event ID, and computes authorized recipients. One transactional ingest commits deduplication, event, and all fan-out delivery records together. Duplicate ingress does not retroactively create deliveries for changed routes.

A client long-polls its queue, receives a leased delivery, reads authorized context if needed, and submits explicitly typed actions. The gateway validates scope and correlation before atomically writing the outbox entry, receipt, and idempotency association. The worker rechecks grants, maps the pinned alias to its GitHub identity, invokes the restricted adapter, and durably records the result. Event ACK is separate from action completion.

## Security boundaries

1. **Internet to webhook listener:** an exact-body HMAC, known source, configured installation IDs, bounded JSON, and known repository are required. A valid GitHub signature establishes delivery authenticity, not authority to follow instructions embedded in comments.
2. **Local process to IPC:** a private Unix socket directory and 0600 socket are preferred. Optional TCP binds literal loopback only. Every `/v1/` request requires a distinct client bearer; browser Origin requests are rejected. Health endpoints reveal only minimal operational information. Do not expose the IPC listener through a public reverse proxy.
3. **Client to gateway authority:** separate repository grants and capability strings constrain reads/writes. Route actor allowlists and default bot exclusion prevent unintended fan-out and feedback loops. Clients cannot provide arbitrary GitHub URLs, installations, approval verdicts, merge operations, or executable scripts.
4. **Alias to GitHub identity:** a durable binding pins each alias to canonical repository plus installation before listeners or workers start. Changing the meaning of an existing alias is rejected atomically; removed aliases remain pinned. This prevents old queued work from being redirected to another repository after configuration changes. A new target needs a new alias and explicit grants, not silent reuse.
5. **GitHub data to client reasoning:** `Event.Data` and context responses retain structured data with recursive credential-field sanitization. Repository IDs and installation IDs are context, not credentials. Sanitization cannot make arbitrary prose safe or truthful: bodies, diffs, and comments remain untrusted. Never execute them or treat them as instructions overriding the user's intent.

Review submissions are COMMENT-only. Inline review operations check commit identity, and comment/check updates enforce adapter ownership constraints. Rechecking grants at execution prevents a revoked queued action from retaining its old permissions. The client examples never receive GitHub credentials or execute external scripts. The review example is a transparent deterministic local integration placeholder with optional caller-supplied findings, not an integrated LLM and not a claim of completed AI code review.

## Durability and failure model

The store uses bbolt's transactional, fsync-backed commits in an owner-only database file. Successful store calls indicate a committed transaction, subject to the filesystem and device's durability guarantees. bbolt provides a single local database writer; this design is not a replicated distributed queue. Protect and back up state as sensitive operational data. Do not run competing daemons against copied, divergent state and assume they share deduplication.

Deliveries are at least once. A handler crash or missing ACK leaves a lease that can expire and be claimed again. Retry ACKs apply backoff; bounded attempts lead to dead-letter state. Explicit replay resets an eligible client-owned delivery. There is no exactly-once handler guarantee, lease renewal endpoint, or transaction joining client side effects to ACK.

Outbox idempotency prevents repeated *local submissions* under the same client/key/content from creating new actions. It cannot atomically commit both a GitHub write and the local result record. A process can die after GitHub accepts a write but before bbolt records success. Consequently, startup converts previously running actions to `uncertain`, and ambiguous network/server outcomes are quarantined rather than blindly retried. Only failures proving the mutation did not execute receive bounded automatic retry. Reconcile uncertain receipts with GitHub before deciding on a new action. No exactly-once external promise is made.

Dry-run consumes the durable outbox and records a result explicitly marked `dry_run`; it does not write GitHub. If no GitHub authentication/adapter is configured, context reads and polling are unavailable even though local event handling can be exercised with fixtures.

## Capacity and operation

`limits.max_pending` is a **retained-record cap**, despite its name, not merely a live backlog limit. Current checks separately bound retained event, delivery, and action records. ACKed deliveries and completed receipts still count. There is no automatic pruning/retention or operator purge endpoint. Capacity exhaustion fails closed rather than dropping history or weakening idempotency, and requires deliberate operator state/retention planning. Monitor `/v1/status` for client queue/action states and disk usage separately; local health is not proof of GitHub access.

Default lease duration is 120 seconds; default attempt cap is 8; default retained-record limit is 100000. Limits are configurable. Polling cursors persist only after successful ingestion of the batch. HTTP requests, context reads, and outbound writes are bounded; shutdown cancels worker contexts and leaves durable state for restart. Clients should cancel subscription contexts and let handlers return before closing idle connections.

For endpoint details and limitations see `protocol.md`; SDK and examples are covered by `clients.md`.
