# shuck operator runbook

Day-2 operations for a deployed **self-hosted event router**: token and secret
operations, sweeps, the DLQ, gateway redeploys, image visibility, and an
incident-triage table. It applies to both deployment targets; where they differ
the Terraform and Helm procedures are called out side by side.

> **Portable mode is the default and needs none of this.** The `shuck` CLI /
> MCP server with a GitHub token deploys nothing. This runbook is for operators
> who have opted into the backend. See
> [`docs/ARCHITECTURE.md#two-modes`](ARCHITECTURE.md#two-modes).

**First-time setup is not here.** The end-to-end install walkthroughs
(GitHub App registration, apply/install, webhook + callback registration, token
mint, shim config, forced-CI-failure acceptance loop) live in
[`deploy/terraform/README.md`](../deploy/terraform/README.md) and
[`deploy/helm/shuck/README.md`](../deploy/helm/shuck/README.md). This runbook
assumes a running deployment.

## Token operations

- **Revoke one token.** The holder regenerates in the portal; the old token
  dies at its next reconnect. Any running session using it stops at the next
  reconnect with the shim's "fix the token and restart" error. There is no
  separate admin revoke UI in v1 — regeneration by the holder, or offboarding
  (below), are the revocation paths.
- **Offboard a person.** Remove them from the GitHub org (or revoke their App
  user-authorization). The **daily re-validation sweep** revokes their token on
  its next pass — GitHub org membership is the access-control plane. To revoke
  immediately rather than waiting for the sweep, run the sweep on demand
  (below).
- **Rotate the deliver secret** (no downtime — two values are accepted):
  - *Terraform*: the deliver secret is generated in-stack. `terraform taint
    random_password.deliver_secret` then `terraform apply` rolls it; workers and
    gateway pick up the new env on redeploy. For a true zero-gap rotation,
    stage the new value via the `deliver_secret_secondary` variable, apply
    (consumers now accept both), then promote it to primary and unset the
    secondary in a second apply.
  - *Helm*: update the `deliver-secret` (and optionally
    `deliver-secret-secondary`) in the source Secret / ESO backend and
    `helm upgrade`; roll workers and gateway.
- **Rotate the webhook secret.** Update it on both sides — the deployment
  (`webhook_secret` output / chart Secret) **and** the GitHub App's webhook
  config — within the same change window; a mismatch fails HMAC verification
  and drops deliveries. **GitHub does not retry failed deliveries on its own**
  — after fixing the mismatch, redeliver the missed events from the App's
  recent-deliveries panel (UI or API).
- **Rotate the GitHub App private key.** Generate a new key in the App
  settings, update the mounted secret, redeploy the worker and portal. Old and
  new keys are both valid on GitHub until you delete the old one.

## Sweeps

There are **two** independent sweeps; don't confuse them.

1. **Portal re-validation sweep** — re-checks every token holder's current org
   membership and revokes departed members. A validation **error is "unknown"
   and never revokes**, so a GitHub API blip cannot mass-revoke.
   - *Terraform*: an EventBridge-scheduled Lambda (daily).
   - *Helm*: the `portal-sweep` CronJob (default `17 3 * * *`,
     `concurrencyPolicy: Forbid`) running `shuck-portal sweep`.
   - **Run on demand**: invoke the sweep Lambda, or
     `kubectl create job --from=cronjob/<release>-portal-sweep manual-sweep-1`.
   - **Stuck sweep symptoms**: departed members still hold live tokens a day
     later → check the sweep's logs for GitHub API errors (all logged as
     "unknown", so the sweep is *not* revoking). Fix the API access (App
     installation, `members:read`) and re-run.
2. **Gateway grace-window sweep** — drops subscriptions and buffered events for
   sessions disconnected beyond the grace window (default 24h).
   - *Terraform*: an EventBridge-scheduled gateway Lambda (every 15 min).
   - *Helm*: in-process in the resident gateway.
   - An occasional failed pass is tolerable — buffered-event rows carry TTLs
     and expire regardless — but the sweep is the **only** cleanup for
     subscription and presence rows (they have no TTL), and it is what stops
     fan-out to long-gone sessions. A sweep that stays broken means those rows
     and their per-delivery buffer writes grow without bound: treat repeated
     sweep failures as an incident, not noise.

## DLQ handling and the deploy-order contract

- **Poison envelopes** land in the DLQ after 5 receives. **Alarm on the DLQ
  depth** — neither target provisions the alarm for you (Terraform surfaces
  `dlq_name`; add a CloudWatch alarm on `ApproximateNumberOfMessagesVisible`).
  After fixing the cause, **redrive** the DLQ back onto the main queue
  (`queue_name`). SQS redelivery (visibility timeout × maxReceiveCount) absorbs
  transient skew before anything reaches the DLQ.
- **The most common DLQ cause is upgrade order.** **`shuck-worker` must roll
  out before `shuck-ingest` serves** — an old worker treats a new envelope kind
  as poison and DLQs it.
  - *Terraform*: encoded as a `depends_on` edge in the graph.
  - *Helm*: enforced by a `wait-for-worker` initContainer on the ingest pod
    (`deployOrder.enforce`, default on). Every new ingest pod gates on
    `kubectl rollout status` of the worker Deployment (minimal RBAC:
    get/list/watch deployments in the release namespace). Rolling updates keep
    the old ingest pod serving while the gate holds, so webhook availability is
    preserved.
  - If you ever disable the gate, sequence it by hand: upgrade the worker,
    confirm its rollout, then the ingest.

## Gateway redeploys

- **Resident (Helm).** Single replica until JUS-95 (the live-connection
  registry is in-memory). On SIGTERM the gateway flips `/readyz` to 503; the
  preStop sleep (`gateway.preStopSleepSeconds`) lets endpoint removal propagate
  before every socket closes with `1001`; shims reconnect and buffered replay
  hides the gap. Keep `terminationGracePeriodSeconds` above preStop + the 10s
  drain timeout. **What you should observe on a rollout: a brief blip, then
  replay fills it — no lost events, no manual intervention.**
- **Serverless (Terraform).** Per-invocation Lambdas — there is no rollout
  drain to manage. Shims reconnect routinely anyway (API Gateway idles sockets
  at 10 minutes and hard-caps them at 2 hours; the shim sends a keepalive every
  5 minutes and backs off + replays on reconnect). This is normal, not an
  incident.

## GHCR image visibility

The four backend images are published by `docker.yml`:

```
ghcr.io/justanotherspy/shuck-ingest
ghcr.io/justanotherspy/shuck-worker
ghcr.io/justanotherspy/shuck-gateway
ghcr.io/justanotherspy/shuck-portal
```

- **First push creates each GHCR package PRIVATE.** A fresh cluster pulling
  them fails with `ImagePullBackOff` / unauthorized until you either make the
  packages public (one-time, in the org's package settings) **or** configure
  `image.pullSecrets` with a token that can read them.
- Tags: `edge` (main), `sha-<short>`, semver on releases. `ghcr-cleanup.yml`
  prunes only `sha-*` tags (keeping the 2 newest) and untagged orphans —
  `edge` / `latest` / semver are never touched.

## Retention and storage

Nothing here needs manual GC — event data is on a timer (authoritative table in
[`docs/ARCHITECTURE.md`](ARCHITECTURE.md#stores-and-retention-defaults)): raw
logs 24h (S3 lifecycle), buffered events 72h (DynamoDB TTL), disconnected
subscribers swept after 24h, dedupe rows 1h. Subscription and presence rows
have **no TTL** — the grace-window sweep is their only cleanup (see Sweeps
above). If storage grows unexpectedly, verify the S3 lifecycle rule and the
DynamoDB TTL attributes are enabled (a disabled TTL is the usual cause of
buffer growth), then check the gateway sweep is completing.

## Observability

The binaries emit structured `log/slog` logs and in-process atomic counters.
The counters are logged on a periodic snapshot always, and — opt-in —
exported two ways (JUS-96, both off by default):

- **Resident binaries (Helm target):** a Prometheus `/metrics` endpoint on a
  dedicated port, enabled per component by setting `SHUCK_METRICS_ADDR`
  (e.g. `:9090`). The Helm chart wires this from `observability.enabled`,
  adds the `metrics` Service port, opens it in the NetworkPolicy, and can
  ship a `ServiceMonitor` (`observability.serviceMonitor.enabled`, covers
  gateway/portal/ingest) and/or `PodMonitor`
  (`observability.podMonitor.enabled`, covers the worker too). Metric names
  are `shuck_<component>_<field>` — the same counters listed below.
- **Serverless target (Terraform):** the resident `/metrics` path has no home
  in Lambda; CloudWatch is the equivalent. `var.observability` turns on
  per-Lambda error alarms, a DLQ-depth alarm, a gateway-error alarm, a
  stack dashboard, and optional X-Ray tracing.

The periodic counter snapshots and `/healthz` listeners run in the
resident/server modes (the Helm shape); in Lambda mode rely on the per-event
structured logs and the CloudWatch alarms above. Watch:

- **ingest** (server mode): the periodic snapshot of received / verified /
  deduped / dropped / enqueued; a high dropped count is normal (most events
  aren't subscribed).
- **worker** (poll mode): the periodic snapshot — processed / delivered /
  deliver retries and errors / parse errors; S3 archive failures (best-effort,
  counted, never fatal).
- **gateway**: heartbeat failures, replayed events on reconnect.
- **portal / sweep**: mint and revoke audit log lines; each sweep pass logs
  its revoked count; per-row "unknown" (API error) warnings — rising
  "unknown" warnings mean the sweep can't validate and is (correctly) not
  revoking.
- **Health**: gateway `/healthz` + `/readyz` (readiness flips 503 on drain —
  that is the rollout signal), worker / ingest / portal `/healthz` (server
  modes).

## Incident triage

| Symptom | Likely cause | Action |
| --- | --- | --- |
| Events never arrive in a session | shim not subscribed, wrong gateway URL, or token rejected | Confirm `shuck_subscribe` ran; check `SHUCK_CHANNEL_GATEWAY_URL` / `SHUCK_CHANNEL_TOKEN`; look for the shim's token error |
| Shim reports a token error and stays disconnected (it stops permanently on `unauthorized` — no reconnect loop) | token revoked/regenerated or holder offboarded | Mint a fresh token in the portal, update the shim config, restart the session |
| DLQ depth climbing | poison envelopes, usually old worker + new envelope kind | Verify worker rolled out before ingest; fix worker; redrive DLQ → main queue |
| Webhooks not being received | HMAC mismatch (secret rotated on one side only) or wrong webhook URL | Re-check the App's webhook URL + secret against the deployment; GitHub's recent-deliveries panel shows the response code |
| Sweep revoking too many tokens | should not happen — errors are "unknown" | If it does, inspect sweep logs; confirm `members:read` and the App installation; a true mass-revoke implies real org changes |
| `ImagePullBackOff` on the backend pods | GHCR packages still private | Make packages public once, or set `image.pullSecrets` |
| Duplicate events in a session | at-least-once redelivery / reconnect replay | Expected; the shim dedupes by `event_id` — verify the shim isn't downgraded |

## See also

- [`docs/ARCHITECTURE.md`](ARCHITECTURE.md) — how it all fits together.
- [`docs/THREAT-MODEL.md`](THREAT-MODEL.md) — trust boundaries and what's
  defended.
- [`deploy/terraform/README.md`](../deploy/terraform/README.md) /
  [`deploy/helm/shuck/README.md`](../deploy/helm/shuck/README.md) — install
  walkthroughs and target-specific knobs.
