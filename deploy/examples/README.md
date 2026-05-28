# Reference deployments

The orchestrator (`cmd/server/`) is a stateless HTTP server that pulls
state from DynamoDB and reacts to SQS messages. How you run it is up to
you. Two starting points live here:

- `ecs/` — AWS Fargate task definition + service (HCL).
- `k8s/` — Kubernetes Deployment + ServiceAccount (raw YAML).

Pick one, copy it into your own infra repo, replace the placeholders,
and apply through your usual pipeline. Neither directory is consumed by
this repo's CI — these are documentation, not load-bearing.

## What's shared between the two

- **Same image** — `runs-fleet/orchestrator:<tag>` multi-arch manifest.
  Pin a `vX.Y.Z` tag, not `latest`, so deploys are reproducible.
- **Same IAM** — the task/pod role is the orchestrator role from
  [`../terraform/iam.tf`](../terraform/iam.tf).
- **Same env contract** — see `pkg/config/config.go` for the full
  surface. The minimum to boot is in each example, with comments.
- **Safe to scale horizontally** — the orchestrator uses DynamoDB-backed
  per-pool reconciliation locks, so 2+ replicas can run side by side
  without coordinating leader election out of band.
- **Webhook listener on `:8080`** — front it with whatever ingress you
  use (ALB, API Gateway, Service of type LoadBalancer, etc.).
