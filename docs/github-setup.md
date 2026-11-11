# GitHub setup and identities

The implementation keeps platform identity separate from local client identity. A local client bearer grants only configured daemon capabilities. It never becomes a GitHub token. Select one authentication mode per daemon; multiple App configurations/installations are supported within App mode. Machine-user and App auth are not mixed in one daemon.

## GitHub App (recommended)

Create/install the App yourself, selecting only needed repositories and permissions. This project does not create accounts, install Apps, register public webhooks, or rotate existing credentials automatically.

Suggested repository permissions by enabled feature:

- Metadata: read (GitHub's required baseline).
- Pull requests: read for context; write for PR comments/inline comments/reviews.
- Issues: read for issue comment context; write for issue comments.
- Checks: read for check context; write for publishing/updating checks.
- Actions: read for workflow-run context/events.
- Contents: read if required for your selected events/context; no write permission is used.

Subscribe only to events your routes need: `issue_comment`, `pull_request`, `pull_request_review`, `pull_request_review_comment`, `workflow_run`, `check_run`, `check_suite`, and any general events you intend to relay (such as push/issues/releases). GitHub's permissions and event availability still apply.

Use `config/github-app.example.json`. Set numeric App ID, installation IDs, private-key PEM path, and the App bot login (for owned-comment checks). An installation must belong to exactly one configured App. Every repository alias and webhook source must identify permitted installation IDs. Store PEM files with mode 0600 or 0400, outside version control. The daemon signs an RSA JWT, requests an installation token, caches it only in memory, and refreshes before expiry. Installation tokens ordinarily expire after one hour. [GitHub installation authentication](https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/authenticating-as-a-github-app-installation)

Check publishing requires App auth. Check updates verify App ownership; the CI client does not overwrite unrelated integrations' checks. [GitHub Checks API](https://docs.github.com/en/rest/checks/runs)

## Machine-user token

A machine user is a real GitHub user account, not an App's `[bot]` identity. Use a narrowly scoped token owned by the intended account and repository access that permits the desired operations. Choose `token_env` or `token_file`; do not put token values in JSON. In this mode configured repositories use installation ID 0. Token refresh/rotation is the operator's responsibility; restart after changing it. The daemon verifies actual authenticated-user ownership before editing a comment.

For native `pull_request.review_requested` routing, GitHub must actually allow the requested user/team to review that repository. Configure the exact `requested_reviewer.login` or team slug in route `names`. An ordinary GitHub App bot must **not** be assumed to appear as an eligible user in the reviewer picker. App-based automation should use an explicit mention/command route or another subscribed event. Installing an App does not create a separate personal account, grant team membership, or make native reviewer requests universally available. The gateway faithfully relays a real review-request event when GitHub emits it; it does not synthesize one to imply a native request happened.

For mentions, configure the exact logical names users will type, such as `your-app` and `your-app[bot]`. Routing parses the body with whole-token boundaries; GitHub notification behavior for an App is separate from this parser. Team mention names can be `org/team`; requested-team webhook names use the team's slug.

## Webhook ingress

Generate a unique random secret of at least 32 bytes. Configure the same secret in GitHub and the source's `secret_env`/`secret_file`. The route is `POST /hooks/{source}`, with raw JSON delivery, `X-GitHub-Event`, `X-GitHub-Delivery`, and `X-Hub-Signature-256`. Never use a local client token as the webhook secret. The receiver checks the exact-body HMAC before parsing or routing. [GitHub webhook validation](https://docs.github.com/en/webhooks/using-webhooks/validating-webhook-deliveries)

A laptop is not directly reachable from GitHub by default. Use an operator-configured HTTPS reverse proxy/tunnel forwarding only the webhook listener; do not expose the Unix IPC socket or loopback IPC port. This project does not set up or deploy a public relay. Return path/API egress is ordinary authenticated HTTPS to `https://api.github.com`.

For GHES, `github.base_url` can specify a trusted HTTPS API root, commonly `https://HOST/api/v3`. Never take that URL from an event or client. Real GHES compatibility has not been tested here. The API version header is pinned to `2022-11-28` for stable contracts; endpoint behavior is tested against fixtures and should be smoke-tested on your target server.

## Enabling writes deliberately

Examples default to `dry_run: true`. In that mode receipts explicitly report `dry_run` and no mutation request is made. Configured credentials can still be used for context reads/polling. If you want fully offline operation, omit GitHub auth and keep polling disabled, as `make demo` does.

After verifying scopes and routing, setting `dry_run: false` authorizes configured client actions in your deployment. The service does not implement human approval prompts. Use a separate restricted client/daemon for stronger operational separation. All reviews remain COMMENT-only; there is no approval or merge API. GitHub's own permissions may reject a requested action even when the local grant permits it.

[GitHub review-comment endpoints](https://docs.github.com/en/rest/pulls/comments) and [review endpoints](https://docs.github.com/en/rest/pulls/reviews) define the upstream semantics. Inline replies target a root review-comment ID. Review writes carry a specific commit SHA and fail if the current PR head differs when checked.
