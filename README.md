# railway-github-runner-autoscaler

Scales a pool of self-hosted GitHub Actions runners on [Railway](https://railway.com) in response to `workflow_job` webhooks.

A small Go service that listens for GitHub webhooks, tracks how many jobs are waiting or running, and drives the runner service's replica count to match — scaling up the moment work arrives and back down once it is safe.

This is a modernized fork of [shaezzy/railway-github-runner-autoscaler](https://github.com/shaezzy/railway-github-runner-autoscaler). See [What changed](#what-changed-from-upstream).

---

## How it works

```
GitHub ──workflow_job webhook──▶ autoscaler ──GraphQL──▶ Railway
                                     │                      │
                                     │                      ▼
                                     └── replica count ─▶ runner service
                                                          (N ephemeral runners)
```

Each runner replica is an **ephemeral** runner: it registers, takes exactly one job, then exits. Railway restarts it, and it registers again. The autoscaler only decides *how many* of those replicas should exist.

## Layout

```
cmd/autoscaler/      entrypoint: config loading, wiring, graceful shutdown
internal/config/     environment parsing and validation
internal/scaler/     job tracking and the reconcile loop (the scaling logic)
internal/railway/    Railway GraphQL client: replica reads/writes, retries
internal/webhook/    GitHub webhook auth, parsing, and HTTP routes
internal/bounded/    fixed-capacity set used for de-duplication
```

`scaler` and `webhook` each define their own `Options` struct and depend on
narrow interfaces (`Backend`, `Tracker`) rather than on `config`, so both are
testable in isolation.

## Prerequisites: the runner service

This autoscaler is only half the system. The runner service it scales **must** be configured with:

| Setting | Value | Why |
| --- | --- | --- |
| `restartPolicyType` | `ALWAYS` | An ephemeral runner exits 0 after each job. Under `ON_FAILURE` Railway treats that as a clean finish and never restarts it, so the pool silently drains to nothing. |
| `restartPolicyMaxRetries` | high (e.g. `1000`) | One exit per job means restarts are routine, not failures. A low cap gets exhausted and the deployment is marked `CRASHED`. |
| `EPHEMERAL` | `true` | One job per container. Without it a runner persists across jobs and state leaks between them. |

A `railway.runner.json` is included as a starting point. Getting `restartPolicyType` wrong is the single most common cause of a runner pool that "keeps crashing".

## Setup

### 1. Deploy this service

Point a Railway service at this repo. It builds from the included `Dockerfile`.

### 2. Configure it

Required:

| Variable | Description |
| --- | --- |
| `GITHUB_WEBHOOK_SECRET` | Shared secret from the GitHub webhook config. |
| `RAILWAY_API_TOKEN` | Account or workspace token from [railway.com/account/tokens](https://railway.com/account/tokens). |
| `RAILWAY_RUNNER_SERVICE_ID` | Service ID of the **runner** service to scale. |
| `RAILWAY_ENVIRONMENT_ID` | Injected automatically by Railway. |

Optional:

| Variable | Default | Description |
| --- | --- | --- |
| `RUNNER_LABELS` | `self-hosted,railway` | A job is counted only if it requests **every** one of these labels. |
| `MAX_RUNNERS` | `3` | Upper bound on replicas. |
| `MIN_REPLICAS` | `1` | Replicas kept warm while idle. `0` scales to zero (cheaper, slower first job). |
| `IDLE_COOLDOWN` | `60s` | Quiet period required before shrinking the pool. |
| `STARTUP_GRACE` | `5m` | After a restart, hold existing replicas this long before any scale-down. |
| `JOB_TTL` | `6h` | Forget jobs whose completion webhook never arrived. |
| `RESYNC_PERIOD` | `30s` | How often to retry failed scales and expire stale jobs. |
| `RAILWAY_RUNNER_REGION` | discovered | Pin the region instead of reading it from the service. |
| `RAILWAY_API_URL` | Railway public API | Override the GraphQL endpoint (tests, proxies). |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error`. |
| `PORT` | `8080` | Listen port. |

> **Run exactly one replica of the autoscaler.** Job state is in memory; a second replica would scale against its own partial view.

### 3. Add the GitHub webhook

In the repo or org: **Settings → Webhooks → Add webhook**

- **Payload URL**: `https://<your-autoscaler>.up.railway.app/webhook`
- **Content type**: `application/json`
- **Secret**: the same value as `GITHUB_WEBHOOK_SECRET`
- **Events**: *Let me select individual events* → **Workflow jobs** only

### 4. Target the runners

```yaml
jobs:
  build:
    runs-on: [self-hosted, railway]
```

The labels must match `RUNNER_LABELS`.

## Endpoints

| Path | Purpose |
| --- | --- |
| `POST /webhook` | GitHub `workflow_job` events. Rejects anything without a valid HMAC signature. |
| `GET /health` | Liveness probe. |
| `GET /status` | Current queued/in-progress counts and replica state. |

## Scaling behaviour

Desired replicas = `clamp(queued + inProgress, MIN_REPLICAS, MAX_RUNNERS)`.

**Scaling up** is applied immediately.

**Scaling down** is deliberately conservative, because Railway chooses *which* replica to terminate — it cannot know which one is mid-job. A shrink is deferred while any of these hold:

- a job is still in progress;
- fewer than `IDLE_COOLDOWN` has passed since the last job event (absorbs webhook lag, where a runner picks up a job before its `in_progress` event lands);
- the process started less than `STARTUP_GRACE` ago (state is in memory, so a fresh start has not yet heard about jobs already running).

All Railway mutations happen on a single reconciler goroutine. Webhook handlers only touch in-memory counters, so concurrent deliveries cannot interleave a read-modify-write against the API.

Failed scales are not lost: the reconcile loop retries every `RESYNC_PERIOD` until the desired state is reached.

## What changed from upstream

### Correctness

- **Fixed a replica-count race.** Upstream computed the new replica count, released the lock, *then* called Railway. Concurrent webhooks raced and issued conflicting writes, producing `Problem processing request` errors and wrong counts. Scaling now runs on a single reconciler goroutine.
- **Stopped killing running jobs.** Upstream could shrink the pool while runners were still executing, and Railway would terminate an arbitrary replica — surfacing as `Runner … is currently running a job and cannot be deleted`. Scale-down is now gated on in-progress count, an idle cooldown, and a startup grace.
- **Fixed a counter leak.** A lost `completed` webhook pinned replicas up forever. Jobs now expire after `JOB_TTL`.
- **Handle out-of-order webhooks.** A late `in_progress` for a finished job used to resurrect it permanently. Terminal jobs are now remembered.
- **Dedupe redelivered webhooks** via `X-GitHub-Delivery`; GitHub retries deliveries.
- **Retry failed scales.** Upstream returned 500 and relied on GitHub redelivering. Reconciliation is now asynchronous and self-healing.

### Railway API

- **`backboard.railway.com`** — upstream used the legacy `.app` host.
- **`multiRegionConfig`** instead of the legacy bare `numReplicas`, which is how current Railway represents horizontal scaling. Regions are discovered from the service, with a documented fallback.
- **Rate-limit and transient-fault handling**: honours `Retry-After` on 429, backs off on 5xx, and retries the 200-with-errors responses Railway returns for internal faults. Every mutation is idempotent, so retrying is safe.
- **Correct restart policy for the runner service** documented and shipped as `railway.runner.json`.

### Build and operations

- Go 1.22 → **1.26**; `log` → structured `log/slog`.
- **Multi-arch Dockerfile** (upstream hardcoded `GOARCH=amd64`), build cache mounts, and a distroless non-root base instead of `scratch`.
- **Graceful shutdown**, HTTP server timeouts, and a `/status` endpoint.
- **Tests**: 69 covering the scaling state machine, webhook auth, retry behaviour, and concurrency under `-race` — 94% coverage on `scaler`, 100% on `config`.
- **Package layout** split out of a flat root into `cmd/` plus focused `internal/` packages.

## Development

```bash
go test -race ./...      # unit tests
go vet ./...
docker build -t autoscaler .
```

To exercise it end to end without touching Railway, point `RAILWAY_API_URL` at a local mock and post signed webhooks to `/webhook`.

## License

MIT — see [LICENSE](LICENSE). Derived from [shaezzy/railway-github-runner-autoscaler](https://github.com/shaezzy/railway-github-runner-autoscaler), MIT.
