# shuck self-hosted backend — serverless Terraform module

One `terraform apply` deploys shuck's opt-in self-hosted event router
(GitHub App → webhook ingest → SQS → worker → gateway → channel shim) to a
fresh AWS account, fully serverless: Lambda everywhere, an API Gateway
WebSocket API terminating the shim connections, DynamoDB on demand, and an
EventBridge-scheduled sweep. Idle cost is effectively zero.

> **Portable mode is the default.** The `shuck` CLI / MCP server with a
> GitHub token needs none of this — deploying this stack is the operator's
> opt-in to push-based delivery, and not deploying it costs users nothing.
> See `docs/V2.md` for the two-mode contract.

## Prerequisites

- An AWS account and credentials (`AWS_PROFILE` / `AWS_REGION` or the
  `region` variable).
- Terraform ≥ 1.9.
- A Go toolchain (any recent version; the build uses `GOTOOLCHAIN=auto`).
  Lambda artifacts are compiled from this clone at apply time — the repo is
  the distribution unit, so what you read is what you deploy. No Docker.

## Setup walkthrough

### 1. Create the GitHub App (manual, once)

On your org or user account: **Settings → Developer settings → GitHub Apps
→ New GitHub App.**

- **Permissions**: Actions *read-only*, Pull requests *read-only*,
  Organization members *read-only* (org installs).
- **Webhook events**: *Workflow runs*, *Pull requests*,
  *Pull request reviews*, *Pull request review comments*.
- **Webhook**: leave the URL empty for now (step 3 produces it); it can be
  filled in afterwards.
- **Identifying and authorizing users**: enable **Request user
  authorization (OAuth) during installation**; the callback URL also comes
  from step 3.
- Generate a **private key** (downloads a `.pem`) and a **client secret**.
- Install the App on your org or account and note the **installation id**
  (the number in the installation's URL) — org mode needs it.

### 2. Configure

```hcl
# terraform.tfvars (gitignored — never commit it)
github_app_id              = 123456
github_app_private_key     = file("~/secrets/shuck-app.pem")
github_client_id           = "Iv1.abc123"
github_client_secret       = "..."

# exactly one of:
github_org                 = "my-org"
github_app_installation_id = 7890123
# github_account_id        = 583231          # personal installs

# recommended: the App's bot identity, or agent-authored review comments loop
ignore_authors             = "123456,my-shuck[bot]"
```

Optional extras: an OIDC gate on the portal (`oidc_*`), GHES
(`github_api_url` / `github_url`), retention and schedule tuning — see
`variables.tf` for the full, commented surface.

### 3. Apply

```sh
cd deploy/terraform
terraform init
terraform apply
```

Then paste two values into the GitHub App settings:

```sh
terraform output webhook_url            # → the App's Webhook URL
terraform output -raw webhook_secret    # → the App's Webhook secret
terraform output portal_url             # → "<portal_url>/github/callback"
                                        #   as the App's OAuth callback URL
```

### 4. Mint a token and wire a session

1. Open `portal_url`, sign in (OIDC if configured), **Connect GitHub**,
   and mint a token — it is shown exactly once.
2. Configure the channel shim (see `plugins/shuck-channel/README.md`):

   ```sh
   export SHUCK_CHANNEL_GATEWAY_URL="$(terraform output -raw gateway_ws_url)"
   export SHUCK_CHANNEL_TOKEN="shk_..."
   ```

3. In a Claude Code session with the shim installed: subscribe to a PR
   (`shuck_subscribe`), push a commit that fails CI, and the distilled
   failure arrives in the session within seconds of the run completing.

`terraform destroy` removes everything but the GitHub App (and any raw-log
bucket contents are force-destroyed with it).

## What gets created

| Piece | Resources |
| --- | --- |
| Ingest | Lambda + public function URL (HMAC-gated), DynamoDB dedupe table (TTL) |
| Queue | SQS + DLQ (redrive after 5 receives), visibility timeout 300s |
| Worker | Lambda on an SQS event source mapping with `ReportBatchItemFailures`, S3 raw-log bucket with a 1-day lifecycle expiry |
| Gateway | API Gateway WebSocket API (`$connect`/`$default`/`$disconnect` → one Lambda), deliver Lambda + function URL, sweep Lambda on a 15-minute schedule, DynamoDB tokens/subscriptions/buffer tables |
| Portal | Lambda behind an API Gateway HTTP API (TLS), daily re-validation sweep Lambda |
| IAM | One least-privilege role per function |

Secrets are env-injected per the binaries' documented contracts — no
Secrets Manager reads in any binary. The deliver and session secrets are
generated in-stack and never leave it; the webhook secret is generated too
(or supplied) and surfaced once as a sensitive output for the App. Treat
the Terraform state as secret material and store it accordingly.

## Cost

Everything here is pay-per-use; an idle deployment rounds to **$0/month**
and a small active one to pocket change:

- **API Gateway WebSocket**: $1.14/million messages + $0.285/million
  connection-minutes (waived generously by the free tier at solo scale).
  One always-connected shim is ~43k connection-minutes/month ≈ $0.012.
- **Lambda / SQS / DynamoDB on-demand / EventBridge**: solo event volumes
  sit deep inside the perpetual free tiers.
- **S3 raw logs**: 24-hour retention keeps this at fractions of a cent.
- **CloudWatch Logs** is the likeliest first real charge ($0.50/GB
  ingested) — retention defaults to 30 days.

There is no fixed-cost component (no ALB, no App Runner, no NAT). The
trade: API Gateway hard-caps WebSocket connections at 2 hours and idles
them out at 10 minutes, so the shim reconnects routinely (buffered replay
makes this invisible) and sends a keepalive ping every 5 minutes.

## Trust model

- **Tokens** (`shk_…`) mint via the portal only after GitHub App
  user-authorization plus org-membership (or account-ownership) validation;
  only the token's SHA-256 is stored. A daily sweep re-validates every
  token holder and revokes departed members — GitHub org membership is the
  access-control plane, offboarding included.
- **Scope**: a token lets its holder's sessions subscribe to PR event
  summaries for repos the App is installed on — nothing else. It grants no
  GitHub access; workers mint their own short-lived App installation
  tokens server-side.
- **Ingress**: the webhook endpoint verifies GitHub's HMAC signature
  before parsing; the deliver endpoint requires the in-stack shared
  secret; the WebSocket API accepts connections but delivers nothing
  until a `hello` authenticates (unauthenticated sockets die at the
  10-minute idle timeout); the portal sits behind TLS with Secure,
  HMAC-signed session cookies.
- **Retention defaults**: raw job logs 24h (S3 lifecycle), buffered events
  72h (DynamoDB TTL), disconnected subscribers swept after 24h, webhook
  dedupe rows 1h.

## Operational notes

- **Deploy order** on upgrades is encoded in the graph: the worker updates
  before ingest (an old worker would DLQ new envelope kinds).
- **Poison envelopes** land in the DLQ after 5 receives — alarm on
  `dlq_name`'s depth; redrive back to `queue_name` after fixing.
- **Token rotation**: regenerate in the portal (old token dies at its next
  reconnect). Deliver-secret rotation: taint
  `random_password.deliver_secret` and apply.
- **Custom domains** are not wired in v1: the AWS-issued `execute-api`
  endpoints already terminate TLS. Fronting them with CloudFront + ACM is
  the upgrade path if you need branded URLs.
