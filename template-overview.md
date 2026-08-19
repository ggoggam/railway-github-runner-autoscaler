# Deploy and Host autoscaled-github-actions-runner on Railway

autoscaled-github-actions-runner is a self-hosted GitHub Actions runner pool that scales itself. An autoscaler service reads `workflow_job` webhooks, tracks how many jobs are queued or running, and drives the runner pool's replica count to match — growing the moment work arrives, shrinking only once it is safe. Each runner takes exactly one job, then exits.

## About Hosting autoscaled-github-actions-runner

Self-hosted runners are usually left always-on, which you pay for around the clock, or scaled by hand, which means someone has to notice the queue. Automating it is harder than it looks. The platform picks which replica to terminate and cannot know which one is mid-job. GitHub never retries a failed webhook, so one missed delivery can strand a job indefinitely. And a runner that exits without its container being rebuilt leaves behind a replica that serves nothing. This template handles all three: scale-down is gated on in-progress work, an idle cooldown, and a startup grace, and the autoscaler reconciles against the GitHub API every cycle, so a lost webhook or a dead pool recovers on its own.

## Common Use Cases

- Cutting CI spend by scaling runners to zero between jobs instead of paying for idle capacity
- Running jobs that need reach into a private network, internal registry, or database
- Escaping GitHub-hosted concurrency limits during release crunches
- Builds that want more CPU or memory than a standard GitHub-hosted runner offers
- Keeping build caches and toolchains on infrastructure you control

## Dependencies for autoscaled-github-actions-runner Hosting

- A GitHub repository with Actions enabled
- A GitHub personal access token, used to register runners and to reconcile job state
- A Railway workspace token, so the autoscaler can change the runner pool's replica count
- [`myoung34/github-runner`](https://github.com/myoung34/docker-github-actions-runner), which provides the runner container
- [Autoscaler source and configuration reference](https://github.com/ggoggam/railway-github-runner-autoscaler)

### Implementation Details

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

The labels must match `RUNNER_LABELS`. `GET /status` on the autoscaler reports current queued and in-progress counts alongside the live runner count.

Two things worth knowing. Run exactly one replica of the Autoscaler — job state is held in memory, so a second replica would scale against its own partial view. And `MIN_REPLICAS` defaults to `0`, which costs nothing while idle at the price of a cold start on the first job; raise it to keep a runner warm.

## Why Deploy autoscaled-github-actions-runner on Railway?

Railway is a singular platform to deploy your infrastructure stack. Railway will host your infrastructure so you don't have to deal with configuration, while allowing you to vertically and horizontally scale it.

By deploying autoscaled-github-actions-runner on Railway, you are one step closer to supporting a complete full-stack application with minimal burden. Host your servers, databases, AI agents, and more on Railway.
