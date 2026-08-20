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
| `GITHUB_API_TOKEN` | *(empty — user supplies)* | GitHub PAT. Classic: `repo`. Fine-grained: **Administration: read and write** + **Actions: read**. Write is needed because the runner uses this same token to register. Seal it. |
| `GITHUB_API_REPOSITORY` | *(empty — user supplies)* | The repository whose jobs to serve, as `owner/repo`. |
| `GITHUB_WEBHOOK_SECRET` | `${{secret(64, "abcdef0123456789")}}` | Generated for you. Copy it into the GitHub webhook's Secret field. |
| `RAILWAY_RUNNER_SERVICE_NAME` | `Runner` | Name of the service to scale. Change it only if you rename the Runner service. |
| `RUNNER_LABELS` | `self-hosted,railway` | A job is served only if it requests every one of these labels. |
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
| Service name | **`Runner`** — this is what `RAILWAY_RUNNER_SERVICE_NAME` resolves |
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
| `ACCESS_TOKEN` | `${{Autoscaler.GITHUB_API_TOKEN}}` | Reference — the PAT is entered once, on the Autoscaler. |
| `REPO_URL` | `https://github.com/${{Autoscaler.GITHUB_API_REPOSITORY}}` | Reference — derived from the repo entered on the Autoscaler. |
| `LABELS` | `${{Autoscaler.RUNNER_LABELS}}` | Reference — the labels must match or the autoscaler serves jobs no runner picks up. |
| `EPHEMERAL` | `true` | One job per container, so no state leaks between jobs. |
| `RUNNER_SCOPE` | `repo` | Register against a single repository. |
| `RUNNER_NAME_PREFIX` | `railway` | Each replica appends a random suffix, so replicas never collide. |
| `DISABLE_AUTO_UPDATE` | `true` | The image is the update mechanism; in-place updates fight it. |

Leave `RUNNER_NAME` unset — a fixed name would collide across replicas. Leave
`CONFIGURED_ACTIONS_RUNNER_FILES_DIR` unset too: it exists to avoid
re-registering, which is the opposite of what an ephemeral runner wants.

## Switching to org-scoped runners

The template deploys repo-scoped, which is the common case and the one whose
variables can be wired by reference. A deployed project is converted to serve a
whole organization by changing variables only:

| Service | Change |
| --- | --- |
| Autoscaler | Remove `GITHUB_API_REPOSITORY`; set `GITHUB_API_ORGANIZATION` to the org name. |
| Runner | Remove `REPO_URL`; set `RUNNER_SCOPE` to `org` and `ORG_NAME` to the org name. |

The PAT then needs org-runner permissions instead: classic `admin:org`, or
fine-grained organization **Self-hosted runners: read and write** (write is the
runner's registration; the autoscaler alone needs only read). The webhook must
be added at the **organization** level so every repo's `workflow_job` events
arrive.

GitHub has no org-level jobs API, so at org scope the autoscaler reconciles
runner liveness (dead pools are still detected and recycled) while job counts
stay webhook-driven. The [README](README.md#org-scoped-runners) covers the
consequences.

## Marketplace overview

Paste [template-overview.md](template-overview.md) as the template overview. It
follows Railway's [required structure](https://docs.railway.com/templates/best-practices#overview).

## Before publishing

- Set a 1:1 transparent icon on the template and on both services.
- Check the workspace name — it is shown publicly as the template author. Use
  your own professional name; neither GitHub nor Railway is affiliated with this
  template.
- Deploy the template once into a clean project and run a real workflow through
  it before publishing.
