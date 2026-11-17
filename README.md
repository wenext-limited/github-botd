# github-botd

**A local, bidirectional gateway between GitHub and your clients.**

One daemon owns GitHub authentication, signed event ingress, routing, durable delivery, and outbound API actions. Local agents, command-line tools, CI bridges, and other services connect through a versioned capability-scoped protocol. A PR reviewer is one possible client, not the product boundary.

Inspired by [feishu-botd](https://github.com/wenext-limited/feishu-botd)'s separation of platform credentials and local providers. This is an independent implementation: the current transport is JSON/HTTP over Unix sockets, not Feishu's protobuf/gRPC contract.

## What is implemented

- Multiple GitHub App installations/apps, or a machine-user token, with canonical repository targets, optional legacy aliases and daemon-only credentials.
- HMAC-SHA256 webhook validation, installation/repository allowlists, structured event normalization, and durable deduplication.
- Explicit routing by repository, event/action, actor, mention, requested reviewer/team, and client; independent multi-client fan-out.
- Unix-domain IPC with private filesystem permissions and distinct client bearers; optional loopback-only TCP.
- Durable bbolt inbox/outbox, leases, ACK/retry, bounded attempts, discoverable dead letters and replay, scoped idempotent receipts, and crash recovery.
- Issue/PR conversation comments and owned-comment edits, inline review comments, root-thread replies, COMMENT-only reviews, and GitHub App check-run creation/update.
- Authorized, paginated PR/files/comment/review/workflow/check context reads.
- Optional best-effort repository-event polling with durable cursors and explicit gap errors.
- Go SDK, `github-botctl`, mock client, transparent review integration example, and CI consumer/publisher.
- Versioned [wire contract](docs/protocol.md), [JSON Schema](api/v1/schema.json), tests, local demo, Dockerfile, and CI workflow.

A separate `pull.merge` capability permits guarded exact-head Squash merges, with native clean-review evidence, a verified author/assignee requester and trusted compilation proof. Approval/request-changes verdicts, arbitrary GitHub endpoint proxies and shell execution are not exposed. The daemon does not contain an LLM or execute PR code.

## Run the offline demo

Requires Go 1.24+ and macOS or Linux. The demo additionally uses Python 3. It creates temporary local secrets and state, starts loopback webhook ingress plus Unix IPC, injects a signed synthetic mention, consumes it, submits an idempotent dry-run response, ACKs, verifies the receipt, and cleans up.

```sh
make demo
```

**No GitHub account, credentials, or network access to GitHub is required.** The demo prints the verified event/action path. It is not a live GitHub smoke test.

Build and verify everything:

```sh
make build
make check
```

## Configure a real connection

1. Choose [GitHub App or machine-user authentication](docs/github-setup.md). App auth is recommended for installation-scoped access and check publishing. An ordinary App bot must not be assumed to behave as a selectable user reviewer.
2. Copy the appropriate example to a private, ignored deployment config and replace repository, installation, actor, and identity placeholders:

   ```sh
   cp config/github-app.example.json config/github-botd.json
   # Or start from config/github-botd.example.json for a machine-user token.
   ```

3. Provide a distinct random bearer (at least 32 bytes) for each client and webhook source through the configured environment variable or private file. Keep GitHub keys/tokens in daemon-only files/environment. Do not paste them into clients or commit them.
4. Keep `dry_run: true` initially. Validate and start:

   ```sh
   bin/github-botd -config config/github-botd.json -check
   bin/github-botd -config config/github-botd.json
   ```

5. Configure a reachable HTTPS webhook endpoint forwarding **only** `/hooks/main` to the webhook listener. The daemon itself binds loopback by default. [Deployment guidance](docs/operations.md) explains listener isolation and secret handling. Never publish local IPC.
6. Supply the appropriate client bearer via `GITHUB_BOTD_TOKEN`, then connect:

   ```sh
   bin/github-botctl -endpoint "unix://$PWD/run/github-botd.sock" info
   bin/github-botctl -endpoint "unix://$PWD/run/github-botd.sock" events
   go run ./examples/mock-client -endpoint "unix://$PWD/run/github-botd.sock"
   ```

7. Inspect dry-run receipts and grants before deliberately setting `dry_run: false`. That enables allowed clients to write to GitHub under their configured capabilities. No additional interactive approval UI is part of this daemon.

Paths in configuration resolve relative to the configuration file. The daemon creates new state/socket directories as owner-only; existing permissive or symlink leaf directories are rejected, not modified. Repository aliases are durably pinned to their repository+installation identity and cannot silently be reused for another target.

Configuration changes reload automatically within about one second. Invalid edits keep the last valid configuration running. `SIGHUP` forces an immediate reload and reopens GitHub credential files. State-directory and listener-address changes require a restart; see [live reload behavior](docs/operations.md#live-configuration-reload).

## Clients, not a single agent

- [`mock-client`](examples/mock-client): logs and ACKs normalized events.
- [`review-agent`](examples/review-agent): handles configured mentions/review requests, reads current PR context, and optionally sends caller-supplied findings as a COMMENT review or inline-thread reply. It defaults to log-only and explicitly states that no automated AI review was performed.
- [`ci-client`](examples/ci-client): consumes workflow/check events or, when explicitly requested, publishes a check using supplied local CI results. It does not execute arbitrary jobs.
- [`pkg/client`](pkg/client): Go SDK. Any language supporting HTTP over Unix sockets can use the JSON contract. No shared language runtime is required.

[Client examples and commands →](docs/clients.md)

## Delivery guarantees and honest limits

Events are **at least once**, independently for each client. Stable idempotency keys prevent duplicate local action submission. GitHub does not offer a transaction spanning its write and the local database: ambiguous responses and interrupted writes become **uncertain**, never silently retried. Only explicit pre-execution rate-limit rejections are automatically retried within configured bounds.

Polling uses GitHub's delayed, bounded repository activity feed. It is not a complete webhook replacement, especially for native review requests and CI activity. Enabling polling and webhooks for the same activity may produce separate source events; use resource-based client idempotency or choose one ingress source. Webhooks remain the primary path.

State retention is bounded by `limits.max_pending` (a retained-record cap, not only live backlog). Completed records are not silently pruned because they carry dedupe guarantees. There is no automatic retention/purge, lease renewal, multi-host replication, or Windows daemon support in this release. Plan capacity and backups; see [operations](docs/operations.md).

Local tests cover real Unix IPC and fake GitHub HTTP behavior. Live App permissions, actual GitHub delivery, GHES compatibility, container startup, and hosted CI execution have **not** been verified against a deployed account.

## Documentation

- [GitHub setup and identity constraints](docs/github-setup.md)
- [Protocol and schema](docs/protocol.md)
- [Architecture and trust boundaries](docs/architecture.md)
- [SDK, CLI, and clients](docs/clients.md)
- [Operation, recovery, and deployment](docs/operations.md)
- [Verification and ablation report](docs/verification.md)

MIT licensed. No code or credentials from the reference project were copied.
