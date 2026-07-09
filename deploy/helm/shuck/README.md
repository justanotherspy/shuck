# shuck self-hosted backend — Helm chart

Deploys shuck's opt-in self-hosted event router (GitHub App → webhook
ingest → SQS → worker → gateway → channel shim) on Kubernetes using the
**resident** binaries: the long-lived `shuck-gateway` WebSocket server,
the `shuck-worker` SQS poll loop, the `shuck-portal` HTTP server (plus a
daily `shuck-portal sweep` CronJob), and — by default — an in-cluster
`shuck-ingest` behind a dedicated public `/webhook` Ingress.

> **Portable mode is the default.** The `shuck` CLI / MCP server with a
> GitHub token needs none of this — installing this chart is the
> operator's opt-in to push-based delivery, and not installing it costs
> users nothing. See
> [`docs/ARCHITECTURE.md#two-modes`](../../../docs/ARCHITECTURE.md#two-modes)
> for the two-mode contract.

The AWS managed dependencies (SQS queue + DLQ, the four DynamoDB tables,
the raw-log S3 bucket) are **not** created by the chart. Provision them
with the [`deploy/terraform`](../../terraform/README.md) module or by
hand — the table schemas are frozen in `docs/V2.md § JUS-88`. The
serverless gateway's registry sort keys in the buffer table are harmless
here: the resident gateway never writes them and they are TTL-pruned.

## Images

The chart consumes the backend images published by the repo's `docker.yml`
matrix (multi-arch amd64+arm64, cosign-signed, SBOM + SLSA provenance):

```
ghcr.io/justanotherspy/shuck-ingest
ghcr.io/justanotherspy/shuck-worker
ghcr.io/justanotherspy/shuck-gateway
ghcr.io/justanotherspy/shuck-portal
```

Tags: `edge` (main), `sha-<short>`, and semver on releases. The chart's
`appVersion` pins the release the chart shipped with; `image.tag`
overrides. **GHCR packages are created private on first push** — the repo
owner makes them public once, or you configure `image.pullSecrets`.

## Install

```sh
# from a repo clone (a published OCI chart also exists per release):
helm install shuck deploy/helm/shuck -n shuck --create-namespace -f my-values.yaml

# or from the OCI registry:
helm install shuck oci://ghcr.io/justanotherspy/charts/shuck -n shuck --create-namespace -f my-values.yaml
```

### 1. Create the GitHub App (manual, once)

Identical to the Terraform target — on your org or user account:
**Settings → Developer settings → GitHub Apps → New GitHub App.**

- **Permissions**: Actions *read-only*, Pull requests *read-only*,
  Organization members *read-only* (org installs).
- **Webhook events**: *Workflow runs*, *Pull requests*,
  *Pull request reviews*, *Pull request review comments*.
- **Webhook URL**: `https://<ingest host>/webhook` (your
  `ingest.ingress.host`); secret = the chart's `webhook-secret`.
- **Identifying and authorizing users**: enable **Request user
  authorization (OAuth) during installation**; callback URL
  `<portal.baseUrl>/github/callback`.
- Generate a **private key** (`.pem`) and a **client secret**; install the
  App and note the **installation id** (org mode needs it).

### 2. Values

The minimum surface: `config.*` (queue URL + table names), `github.*`
(App id, client id, org **xor** accountId, installation id for org mode),
`portal.baseUrl`, the secrets (below), ingress hosts, `aws.region`, and
IRSA role annotations per component (`serviceAccounts.*.annotations`) —
the Terraform module's per-function IAM policies are the least-privilege
reference. Set `worker.ignoreAuthors` to the App's bot identity (e.g.
`"12345,my-shuck[bot]"`) or agent-authored review comments will loop.

### 3. Secrets — one contract, three sources

Every component reads one k8s Secret (keys: `webhook-secret`,
`deliver-secret`, optional `deliver-secret-secondary`, `session-secret`,
`github-client-secret`, optional `oidc-client-secret`,
`github-app-private-key.pem`). Pick a source:

- `secrets.create: true` + `secrets.values.*` — chart-rendered, dev/demo
  only (secrets in values files end up in state/history).
- `secrets.existingSecret: <name>` — bring your own.
- `externalSecrets.enabled: true` — an ExternalSecret produces it via the
  External Secrets Operator (`external-secrets.io/v1`; ESO is assumed
  installed) from whatever backend your SecretStore points at. Nothing
  secret lives in values.

The App private key is mounted as a file
(`SHUCK_GITHUB_APP_PRIVATE_KEY_FILE`) — never an env value.

### 4. Mint a token and wire a session

1. Open the portal, sign in (OIDC if configured), **Connect GitHub**, mint
   a token — shown exactly once.
2. Configure the channel shim (`plugins/shuck-channel/README.md`):
   `SHUCK_CHANNEL_GATEWAY_URL=wss://<gateway.ingress.host>/ws`,
   `SHUCK_CHANNEL_TOKEN=shk_…`.
3. In a Claude Code session with the shim installed: `shuck_subscribe` a
   PR, push a commit that fails CI, and the distilled failure arrives in
   the session seconds after the run completes.

## Ingress split & WebSocket timeouts

Two deliberately separate Ingresses:

- **Public**: `ingest.ingress` routes exactly `/webhook` on its own
  class/annotations, so WAF or rate limits attach to the public route
  independently. With `ingest.mode: lambda` the chart deploys no ingest at
  all — run the Terraform module's ingest Lambda into the same SQS queue
  and the cluster keeps **zero public surface**.
- **Private**: `gateway.ingress` routes exactly `/ws` (the deliver
  endpoint is never routed — cluster-internal only), and `portal.ingress`
  the portal UI. Keep both on an internal LB / VPN-routable class; the
  portal requires TLS (Secure session cookies).

The gateway pings every WebSocket every 30s (`SHUCK_HEARTBEAT`), which
keeps connections alive through nginx's default 60s `proxy-read-timeout`.
Still, set the timeouts deliberately for long-lived sockets, e.g. nginx:

```yaml
gateway:
  ingress:
    annotations:
      nginx.ingress.kubernetes.io/proxy-read-timeout: "3600"
      nginx.ingress.kubernetes.io/proxy-send-timeout: "3600"
```

## Deploy order (worker before ingest)

Upgrades that add envelope kinds poison old workers, so **shuck-worker
must roll out before shuck-ingest serves** (in Terraform this is a
`depends_on` edge). The chart enforces it with a `wait-for-worker`
initContainer on the ingest pod (`deployOrder.enforce`, default on): every
new ingest pod gates on `kubectl rollout status` of the worker Deployment,
with minimal RBAC (get/list/watch deployments in the release namespace).
Rolling updates keep the old ingest pod serving while the gate holds, so
webhook availability is preserved; SQS redelivery (visibility timeout ×
maxReceiveCount) absorbs any residual skew.

## Gateway rollouts

Single replica until JUS-95 (the live-connection registry is in-memory).
On SIGTERM the gateway flips `/readyz` to 503, and the preStop sleep
(`gateway.preStopSleepSeconds`) lets endpoint removal propagate before
every socket closes with 1001 — shims reconnect and the buffered replay
hides the gap. Keep `terminationGracePeriodSeconds` above preStop + the
10s drain timeout.

## Worker scaling modes

- `static` (default): fixed replicas — correct at typical volume.
- `hpa`: resource-metric HPA, no extra controllers. Caveat: CPU is a
  lagging signal for queue consumers — a worker blocked on GitHub I/O
  burns little CPU while the queue grows.
- `keda`: ScaledObject on SQS depth, scale-to-zero (KEDA assumed
  installed). `identityOwner: operator` polls with KEDA's own AWS
  identity; `pod` uses the worker's IRSA role.

## Traffic matrix (for NetworkPolicy / your own CNI policies)

| From | To | Port | Purpose |
| --- | --- | --- | --- |
| channel shims (users' machines) | gateway `/ws` via private ingress | 8080 | event delivery WebSocket |
| worker pods | gateway `/internal/deliver` | 8080 | distilled summaries (shared-secret header) |
| GitHub | ingest `/webhook` via public ingress | 8080 | webhook deliveries (HMAC-verified) |
| ingest | SQS, DynamoDB (dedupe + subscription tables) | 443 | enqueue + dedupe + pre-filter |
| ingest initContainer | kube-apiserver | 443/6443 | deploy-order gate |
| worker | SQS, S3, GitHub API | 443 | consume, archive, fetch |
| gateway | DynamoDB | 443 | tokens, subscriptions, buffer |
| portal / sweep | GitHub API, DynamoDB, OIDC issuer | 443 | auth, validation, token table |

`networkPolicy.enabled: true` renders vanilla NetworkPolicies implementing
this matrix (egress to the managed services is "443 anywhere" — their IPs
are not enumerable; use CNI FQDN policies via `extraManifests` to tighten).
NetworkPolicy is L4: peers allowed to the gateway port for `/ws` can also
*reach* `/internal/deliver`, where the shared secret still rejects them —
the policy blocks every other pod, the secret blocks everyone unauthorized
(defence in depth). Kubelet probe traffic is exempt on mainstream CNIs;
verify probes on anything unusual.

## Trust model & retention

Tokens are minted only after GitHub identity verification and stored as
SHA-256 hashes; the daily sweep revokes departed org members (GitHub org
membership is the access-control plane); a token grants event-summary
subscriptions only, no GitHub access. Retention defaults: raw logs 24h
(S3 lifecycle — provisioned with the bucket, never in worker code),
buffered events 72h, disconnected subscribers swept after 24h, dedupe
rows 1h. The full boundary-by-boundary analysis is
[`docs/THREAT-MODEL.md`](../../../docs/THREAT-MODEL.md); day-2 operations
(rotation, sweeps, DLQ, rollouts, image visibility) are in
[`docs/RUNBOOK.md`](../../../docs/RUNBOOK.md).

## Values

See the extensively commented [`values.yaml`](values.yaml) — every knob is
documented inline. `extraManifests` passes raw manifests through (strings
are templated) for anything the chart doesn't own: Cilium policies,
ServiceMonitors, KEDA TriggerAuthentications, ExternalSecret variants.
