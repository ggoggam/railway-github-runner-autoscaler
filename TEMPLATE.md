# Railway template definition

This file is for whoever *publishes* the template, not for people deploying it.
It records the exact two-service configuration to enter in Railway's
[template composer](https://railway.com/workspace/templates), because a Railway
template is an object in Railway — not a file in this repo — and there is no
supported way to commit one.

Deploying users need only the [README](README.md).

## Why two services

The autoscaler is half the system. On its own it scales nothing, and the service
it scales has three settings that are easy to get wrong and silent when wrong
(see [Runner](#service-2-runner)). A template that shipped only the autoscaler
would leave every user hand-building the one service whose misconfiguration is
the most common failure in this whole design. Both services belong in the
template.

## What the user must still do by hand

The template cannot be one click, and the overview should not pretend otherwise:

1. **Create a GitHub PAT** and paste it at deploy time. Nothing can mint it.
2. **Create a Railway token** and paste it. Same.
3. **Add the webhook in GitHub** after deploy, using the generated domain and
   the generated webhook secret.

Everything else — service wiring, replica plumbing, restart policies, the
webhook secret itself — the template does.

## Service 1: Autoscaler

| Setting | Value |
| --- | --- |
| Source | `https://github.com/ggoggam/railway-github-runner-autoscaler` |
| Builder | Dockerfile (set by [`railway.json`](railway.json)) |
| Healthcheck path | `/health` (set by `railway.json`) |
| Public networking | **HTTP, port 8080** — required; GitHub must be able to reach `/webhook` |
| Volume | none |
| Icon | 1:1, transparent background |

### Variables

| Variable | Value | Description to show the user |
| --- | --- | --- |
| `RAILWAY_API_TOKEN` | *(empty — user supplies)* | A Railway **workspace** token from railway.com/account/tokens. Scoped to one workspace, unlike an account token. Seal it. |
| `GITHUB_ACCESS_TOKEN` | *(empty — user supplies)* | GitHub PAT. Classic: `repo`. Fine-grained: **Administration: read and write** + **Actions: read**. Write is needed because the runner uses this same token to register. Seal it. |
| `GITHUB_API_REPOSITORY` | *(empty — user supplies)* | What the runners serve: the repository as `owner/repo`, or a bare organization name if `GITHUB_RUNNER_SCOPE` is `org`. |
| `GITHUB_RUNNER_SCOPE` | `repo` | `repo` or `org`. Both services read this one value, so switching scope is a single edit. |
| `GITHUB_WEBHOOK_SECRET` | `${{secret(64, "abcdef0123456789")}}` | Generated for you. Copy it into the GitHub webhook's Secret field. |
| `RAILWAY_RUNNER_SERVICE_NAME` | `github-runner` | Name of the service to scale. Change it only if you rename the Runner service. |
| `GITHUB_RUNNER_LABELS` | `self-hosted,railway` | A job is served only if it requests every one of these labels. |
| `MAX_RUNNERS` | `3` | Upper bound on concurrent runners. |
| `MIN_REPLICAS` | `0` | Runners kept warm while idle. `0` costs nothing when idle; the first job then waits for a cold start. |

`RAILWAY_PROJECT_ID` and `RAILWAY_ENVIRONMENT_ID` are injected by Railway. Do
not add them — an explicit empty value shadows the real one.

`RAILWAY_RUNNER_SERVICE_ID` is deliberately absent. A service ID does not exist
until the template is deployed, so the autoscaler resolves the runner by name
against the project instead. It stays available as an override.

## Service 2: Runner

| Setting | Value |
| --- | --- |
| Source | Docker image `myoung34/github-runner:latest` |
| Service name | **`github-runner`** — this is what `RAILWAY_RUNNER_SERVICE_NAME` resolves |
| Restart policy | **`ALWAYS`** |
| Restart max retries | **`1000`** |
| Draining seconds | `300` |
| Healthcheck | none — a runner serves no HTTP |
| Public networking | **disabled** |
| Volume | none |
| Initial replicas | `1` (the autoscaler drops it to `MIN_REPLICAS` once `STARTUP_GRACE` elapses) |

The restart settings are not cosmetic. An ephemeral runner exits `0` after every
job. Under `ON_FAILURE` Railway reads that as a clean finish and never restarts
it, so the pool drains to nothing; under a low retry cap the restarts — which
are routine here, one per job — exhaust it and the deployment is marked
`CRASHED`. [`railway.runner.json`](railway.runner.json) records the same
settings, but a Docker-image service has no repo to read config-as-code from, so
they must be set on the service in the composer.

Do **not** set `multiRegionConfig` anywhere in the runner's configuration.
Config-as-code overrides service settings on every deployment, which would reset
the replica count the autoscaler just set.

### Variables

| Variable | Value | Description to show the user |
| --- | --- | --- |
| `ACCESS_TOKEN` | `${{Autoscaler.GITHUB_ACCESS_TOKEN}}` | Reference — the PAT is entered once, on the Autoscaler. |
| `RUNNER_SCOPE` | `${{Autoscaler.GITHUB_RUNNER_SCOPE}}` | Reference — `repo` or `org`, decided once on the Autoscaler. |
| `REPO_URL` | `https://github.com/${{Autoscaler.GITHUB_API_REPOSITORY}}` | Reference — read only at `repo` scope; the image ignores it at `org` scope. |
| `ORG_NAME` | `${{Autoscaler.GITHUB_API_REPOSITORY}}` | Reference — read only at `org` scope, where that variable holds a bare org name; ignored at `repo` scope. |
| `LABELS` | `${{Autoscaler.GITHUB_RUNNER_LABELS}}` | Reference — the labels must match or the autoscaler serves jobs no runner picks up. |
| `EPHEMERAL` | `true` | One job per container, so no state leaks between jobs. |
| `RUNNER_NAME_PREFIX` | `railway` | Each replica appends a random suffix, so replicas never collide. |
| `DISABLE_AUTO_UPDATE` | `true` | The image is the update mechanism; in-place updates fight it. |

Leave `RUNNER_NAME` unset — a fixed name would collide across replicas. Leave
`CONFIGURED_ACTIONS_RUNNER_FILES_DIR` unset too: it exists to avoid
re-registering, which is the opposite of what an ephemeral runner wants.

## Switching to org-scoped runners

The template deploys repo-scoped, which is the common case. Because both
services read the scope and the target from the Autoscaler, converting a
deployed project to serve a whole organization touches one service:

| Service | Change |
| --- | --- |
| Autoscaler | Set `GITHUB_RUNNER_SCOPE` to `org`, and replace `GITHUB_API_REPOSITORY`'s `owner/repo` with the bare org name. |
| Runner | Nothing — `RUNNER_SCOPE`, `ORG_NAME`, and `REPO_URL` all follow by reference, and the image reads only the pair its scope calls for. |

The PAT then needs org-runner permissions instead: classic `admin:org`, or
fine-grained organization **Self-hosted runners: read and write** (write is the
runner's registration; the autoscaler alone needs only read). The webhook must
be added at the **organization** level so every repo's `workflow_job` events
arrive.

GitHub has no org-level jobs API, so at org scope the autoscaler reconciles
runner liveness (dead pools are still detected and recycled) while job counts
stay webhook-driven. The [README](README.md#org-scoped-runners) covers the
consequences.

## Before publishing

- Set a 1:1 transparent icon on the template and on both services.
- Check the workspace name — it is shown publicly as the template author. Use
  your own professional name; neither GitHub nor Railway is affiliated with this
  template.
- Deploy the template once into a clean project and run a real workflow through
  it before publishing.

## Marketplace overview

Everything below the rule is the template's overview copy, following Railway's
[required structure](https://docs.railway.com/templates/best-practices#overview).
Paste it verbatim into the composer's overview field, promoting each heading by
one level (`##` becomes `#`).

---

## Deploy and Host autoscaled-github-actions-runner on Railway

autoscaled-github-actions-runner is a self-hosted GitHub Actions runner pool that scales itself. An autoscaler service reads `workflow_job` webhooks, tracks how many jobs are queued or running, and drives the runner pool's replica count to match — growing the moment work arrives, shrinking only once it is safe. Each runner takes exactly one job, then exits.

### About Hosting autoscaled-github-actions-runner

Self-hosted runners are usually left always-on, which you pay for around the clock, or scaled by hand, which means someone has to notice the queue. Automating it is harder than it looks. The platform picks which replica to terminate and cannot know which one is mid-job. GitHub never retries a failed webhook, so one missed delivery can strand a job indefinitely. And a runner that exits without its container being rebuilt leaves behind a replica that serves nothing. This template handles all three: scale-down is gated on in-progress work, an idle cooldown, and a startup grace, and the autoscaler reconciles against the GitHub API every cycle, so a lost webhook or a dead pool recovers on its own.

### Common Use Cases

- Cutting CI spend by scaling runners to zero between jobs instead of paying for idle capacity
- Running jobs that need reach into a private network, internal registry, or database
- Escaping GitHub-hosted concurrency limits during release crunches
- Builds that want more CPU or memory than a standard GitHub-hosted runner offers
- Keeping build caches and toolchains on infrastructure you control

### Dependencies for autoscaled-github-actions-runner Hosting

- A GitHub repository with Actions enabled
- A GitHub personal access token, used to register runners and to reconcile job state
- A Railway workspace token, so the autoscaler can change the runner pool's replica count
- [`myoung34/github-runner`](https://github.com/myoung34/docker-github-actions-runner), which provides the runner container
- [Autoscaler source and configuration reference](https://github.com/ggoggam/railway-github-runner-autoscaler)

#### Implementation Details

After deploying, three steps finish the setup. Two are tokens the template cannot mint for you; the third is a webhook only you can add.

Paste a **Railway workspace token** ([railway.com/account/tokens](https://railway.com/account/tokens)) and a **GitHub PAT** into the Autoscaler service. The PAT needs `repo` scope, or fine-grained **Administration: read and write** plus **Actions: read** — write is required because the runner uses the same token to register itself.

Then add a webhook to the repository under **Settings → Webhooks → Add webhook**:

- **Payload URL**: `https://<your-autoscaler-domain>/webhook`
- **Content type**: `application/json`
- **Secret**: the `GITHUB_WEBHOOK_SECRET` value generated on the Autoscaler service
- **Events**: *Let me select individual events* → **Workflow jobs** only

Finally, point a job at the pool:

```yaml
jobs:
  build:
    runs-on: [self-hosted, railway]
```

The labels must match `GITHUB_RUNNER_LABELS`. `GET /status` on the autoscaler reports current queued and in-progress counts alongside the live runner count.

Two things worth knowing. Run exactly one replica of the Autoscaler — job state is held in memory, so a second replica would scale against its own partial view. And `MIN_REPLICAS` defaults to `0`, which costs nothing while idle at the price of a cold start on the first job; raise it to keep a runner warm.

The template deploys repo-scoped runners. To serve a whole organization instead, set `GITHUB_RUNNER_SCOPE=org` on the Autoscaler and put the organization name in `GITHUB_API_REPOSITORY` (the Runner follows by reference), then add the webhook at the organization level. The [configuration reference](https://github.com/ggoggam/railway-github-runner-autoscaler#org-scoped-runners) covers the details.

### Why Deploy autoscaled-github-actions-runner on Railway?

Railway is a singular platform to deploy your infrastructure stack. Railway will host your infrastructure so you don't have to deal with configuration, while allowing you to vertically and horizontally scale it.

By deploying autoscaled-github-actions-runner on Railway, you are one step closer to supporting a complete full-stack application with minimal burden. Host your servers, databases, AI agents, and more on Railway.

---
