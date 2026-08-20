# Railway template definition

Notes for publishing the template in Railway's
[template composer](https://railway.com/workspace/templates). Deploying users
need only the [README](README.md).

Two services: the `autoscaler`, and the `github-runner` pool it scales. The
runner has a few settings that fail silently when wrong, so it ships in the
template rather than being hand-built by every user.

## Service 1: autoscaler

| Setting | Value |
| --- | --- |
| Service name | **`autoscaler`** |
| Source | `https://github.com/ggoggam/railway-github-runner-autoscaler` |
| Public networking | **HTTP, port 8080** — GitHub must reach `/webhook` |
| Builder, healthcheck | from [`railway.json`](railway.json) |

| Variable | Value | Description to show the user |
| --- | --- | --- |
| `RAILWAY_API_TOKEN` | *(user supplies)* | Railway **workspace** token from railway.com/account/tokens. Seal it. |
| `GITHUB_ACCESS_TOKEN` | *(user supplies)* | GitHub PAT. Classic: `repo`. Fine-grained: **Administration: read and write** + **Actions: read** (write is how the runner registers). Seal it. |
| `GITHUB_API_REPOSITORY` | *(user supplies)* | `owner/repo`, or a bare org name if `GITHUB_RUNNER_SCOPE` is `org`. |
| `GITHUB_RUNNER_SCOPE` | `repo` | `repo` or `org`. Both services read this one value. |
| `GITHUB_WEBHOOK_SECRET` | `${{secret(64, "abcdef0123456789")}}` | Generated for you. Copy into the GitHub webhook's Secret field. |
| `RAILWAY_RUNNER_SERVICE_NAME` | `github-runner` | Service to scale. Change only if you rename that service. |
| `GITHUB_RUNNER_LABELS` | `self-hosted,railway` | A job is served only if it requests every one of these labels. |
| `MAX_RUNNERS` | `3` | Upper bound on concurrent runners. |
| `MIN_REPLICAS` | `0` | Runners kept warm while idle. `0` costs nothing but adds a cold start. |

Do not add `RAILWAY_PROJECT_ID` or `RAILWAY_ENVIRONMENT_ID` — Railway injects
them, and an empty value shadows the real one. `RAILWAY_RUNNER_SERVICE_ID` is
omitted because service IDs do not exist until deploy; the autoscaler resolves
the runner by name instead.

## Service 2: github-runner

| Setting | Value |
| --- | --- |
| Service name | **`github-runner`** |
| Source | Docker image `myoung34/github-runner:latest` |
| Restart policy | **`ALWAYS`**, max retries **`1000`**, draining `300`s |
| Public networking | disabled |
| Initial replicas | `1` |

| Variable | Value |
| --- | --- |
| `ACCESS_TOKEN` | `${{autoscaler.GITHUB_ACCESS_TOKEN}}` |
| `RUNNER_SCOPE` | `${{autoscaler.GITHUB_RUNNER_SCOPE}}` |
| `REPO_URL` | `https://github.com/${{autoscaler.GITHUB_API_REPOSITORY}}` |
| `ORG_NAME` | `${{autoscaler.GITHUB_API_REPOSITORY}}` |
| `LABELS` | `${{autoscaler.GITHUB_RUNNER_LABELS}}` |
| `EPHEMERAL` | `true` |
| `RUNNER_NAME_PREFIX` | `railway` |
| `DISABLE_AUTO_UPDATE` | `true` |

The image reads only the pair its scope calls for, so `REPO_URL` and `ORG_NAME`
can both be set. Leave `RUNNER_NAME` and
`CONFIGURED_ACTIONS_RUNNER_FILES_DIR` unset.

The restart settings matter: an ephemeral runner exits `0` after every job, so
`ON_FAILURE` never restarts it and a low retry cap marks the deployment
`CRASHED`. [`railway.runner.json`](railway.runner.json) records them, but a
Docker-image service has no repo to read config-as-code from — set them in the
composer. Do **not** set `multiRegionConfig` on the runner; config-as-code
would reset the replica count on every deployment.

## Before publishing

- 1:1 transparent icon on the template and both services.
- The workspace name is shown publicly as the template author.
- Deploy once into a clean project and run a real workflow through it.

## Marketplace overview

Paste everything below the rule into the composer's overview field, promoting
each heading by one level (`##` becomes `#`). Follows Railway's
[required structure](https://docs.railway.com/templates/best-practices#overview).

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
- [autoscaler source and configuration reference](https://github.com/ggoggam/railway-github-runner-autoscaler)

#### Implementation Details

After deploying, three steps finish the setup. Two are tokens the template cannot mint for you; the third is a webhook only you can add.

Paste a **Railway workspace token** ([railway.com/account/tokens](https://railway.com/account/tokens)) and a **GitHub PAT** into the autoscaler service. The PAT needs `repo` scope, or fine-grained **Administration: read and write** plus **Actions: read** — write is required because the runner uses the same token to register itself.

Then add a webhook to the repository under **Settings → Webhooks → Add webhook**:

- **Payload URL**: `https://<your-autoscaler-domain>/webhook`
- **Content type**: `application/json`
- **Secret**: the `GITHUB_WEBHOOK_SECRET` value generated on the autoscaler service
- **Events**: *Let me select individual events* → **Workflow jobs** only

Finally, point a job at the pool:

```yaml
jobs:
  build:
    runs-on: [self-hosted, railway]
```

The labels must match `GITHUB_RUNNER_LABELS`. `GET /status` on the autoscaler reports current queued and in-progress counts alongside the live runner count.

Two things worth knowing. Run exactly one replica of the autoscaler — job state is held in memory, so a second replica would scale against its own partial view. And `MIN_REPLICAS` defaults to `0`, which costs nothing while idle at the price of a cold start on the first job; raise it to keep a runner warm.

The template deploys repo-scoped runners. To serve a whole organization instead, set `GITHUB_RUNNER_SCOPE=org` on the autoscaler and put the organization name in `GITHUB_API_REPOSITORY` (the github-runner service follows by reference), then add the webhook at the organization level. The [configuration reference](https://github.com/ggoggam/railway-github-runner-autoscaler#org-scoped-runners) covers the details.

### Why Deploy autoscaled-github-actions-runner on Railway?

Railway is a singular platform to deploy your infrastructure stack. Railway will host your infrastructure so you don't have to deal with configuration, while allowing you to vertically and horizontally scale it.

By deploying autoscaled-github-actions-runner on Railway, you are one step closer to supporting a complete full-stack application with minimal burden. Host your servers, databases, AI agents, and more on Railway.

---
