---
name: fetch-runner-logs
description: Retrieve a runs-fleet runner's own instance-side logs for a GitHub Actions job from S3. Use when GitHub has expired a job's logs, when `gh run view --job` redirects to the newest attempt, when diagnosing a failed or superseded attempt, or when a build failure's stated cause looks like an upstream outage but might be local to the runner.
---

# Fetch a job's runner logs

GitHub expires the logs of a superseded attempt within hours, and the agent
wipes `_diag` when the job ends. The agent uploads those logs to S3 first, so
they outlive both. This is how you read them.

Reach for this whenever the GitHub UI can no longer answer "what actually
happened on the box" — especially when an error names an external service. A
runner-local failure (a dead on-host proxy, a broken mount, an exhausted disk)
routinely surfaces as someone else's 5xx.

## 1. Resolve the bucket — never hardcode it

In order:

```bash
echo "${RUNS_FLEET_CACHE_BUCKET:-}"                     # set in the orchestrator's env
AWS_PROFILE=pub aws s3api list-buckets \
  --query "Buckets[?contains(Name, 'runs-fleet')].Name" --output text
```

If neither answers, ask the user and save it to memory. Do not write a bucket
name, account ID, or registry host into any file in this repo — it is public.

Everything below assumes `AWS_PROFILE=pub` and the bucket in `$BUCKET`.

## 2. Get run_id and job_id from the URL

A job URL is `.../actions/runs/<run_id>/job/<job_id>`. Both IDs come straight
out of it; no API call needed.

For a `.../attempts/<N>` URL, or when you only have a run, list that attempt's
jobs — each attempt has its own job_ids, which is exactly why re-runs don't
overwrite each other:

```bash
gh api repos/<owner>/<repo>/actions/runs/<run_id>/attempts/<N>/jobs \
  --jq '.jobs[] | "\(.id) \(.name) \(.conclusion)"'
```

## 3. Find the objects

```bash
aws s3 ls "s3://$BUCKET/runner-logs/$RUN_ID/$JOB_ID/" --recursive
```

Empty? The job may have run without a job_id, keyed under `unknown-job`. List
the whole run and pick by instance:

```bash
aws s3 ls "s3://$BUCKET/runner-logs/$RUN_ID/" --recursive
```

The key layout is `runner-logs/<run_id>/<job_id>/<instance_id>/<name>.log.gz`.
The instance segment matters: a spot-interrupted job is retried on a new
instance under the same job_id, so both attempts are present side by side.

## 4. Read them

Download into the session scratchpad — not the repo — then decompress:

```bash
aws s3 cp "s3://$BUCKET/<key>" "$SCRATCH/worker.log.gz" && gunzip "$SCRATCH/worker.log.gz"
```

Or stream without keeping a file:

```bash
aws s3 cp "s3://$BUCKET/<key>" - | gunzip | head -100
```

**Which file to read:**

- `Worker_*.log` — the job's step output: commands, their output, the failure.
- `Runner_*.log` — the runner service itself: registration, listener, JIT
  config, job acquisition. **Check this first when the job never started**, was
  never picked up, or the Worker log is absent entirely.

## 5. Handle secrets carefully

These logs are **not** secret-masked in every path, unlike what GitHub renders.
Treat them as sensitive:

- Keep them in the scratchpad; never copy one into the repo.
- Never paste log content into an issue, PR, commit message, or artifact.
- When quoting a line to the user, quote only the line you need, and do not
  echo anything token-shaped.

## 6. When nothing is there

Check these before concluding the logs were never written:

- **Past retention.** Objects expire 14 days after upload.
- **Upload denied.** The instance role needs `s3:PutObject` on the
  `runner-logs/` prefix. Check the job's `log_upload` outcome (telemetry) or
  the `RunnerLogUpload` metric — a fleet-wide `failed` means the grant is
  missing and no logs are being kept at all.
- **AMI predates the feature.** The agent binary is baked into the AMI, so an
  instance from an older image never uploaded anything.

## Fallbacks when the logs are genuinely gone

- The buildx `.dockerbuild` artifact holds a full build trace including the
  real first error, and survives independently of the job log. Fetch with
  `gh api repos/<o>/<r>/actions/artifacts/<id>/zip` — it arrives as gzipped
  tar, not a zip: `gunzip -c x.zip > y && tar -xf y`, then grep the NDJSON
  blob under `blobs/sha256/`.
- A live instance in the same pool can be inspected directly via
  `aws ssm send-command` + `aws ssm get-command-invocation`. A bug that
  reproduces fleet-wide (a misconfigured unit, a dead listener) is usually
  visible on any current runner, not just the one that failed.
