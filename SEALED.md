# sealed fork of `woodpecker-ci/autoscaler`

This is a sealed-owned fork of the upstream Woodpecker autoscaler
([`woodpecker-ci/autoscaler`](https://github.com/woodpecker-ci/autoscaler)),
carrying a **label-aware scaling patch** (SEA-1122) on top of an otherwise
unmodified upstream. It is Apache-2.0, same as upstream; `LICENSE` is retained
and the diff is kept minimal and attributable so the patch can be offered
upstream later.

## What the fork changes

The upstream autoscaler's `calcAgents` is **label-blind**: it counts every
pending task fleet-wide (`Info.Stats.Pending`) and every free slot fleet-wide
(`Stats.Workers`), so a job only the static hosts can run (e.g. `size=large`, or
`type=macos`) still spins up an elastic Spot agent that can't run it, idles, and
tears down — paid boot + paid idle, zero work.

This fork makes the scaler count only **label-satisfiable** demand per pool:

- `engine/labelfilter/` — a self-contained package modeling, per pool, which
  queued tasks the pool's advertised label set can actually run. Its match logic
  is a **verbatim transliteration** of the Woodpecker server scheduler's own
  rule (`server/scheduler/filter.go` in `go.woodpecker-ci.org/woodpecker/v3`)
  and is defended by a test table transliterated from the scheduler's
  `filter_test.go`. Any divergence there would silently over/under-scale.
- `engine/autoscaler.go` — `getQueueInfo` now returns the full `*woodpecker.Info`
  (the task lists were always in the API payload, only discarded), and
  `calcAgents` scales on `eligiblePending` (pool-satisfiable pending, after
  netting free non-pool capacity greedy-FIFO) + `poolRunning` (running tasks
  assigned to this pool's agents), over `WorkflowsPerAgent`. The `Min`/`Max`
  clamps and the entire create/drain/cleanup lifecycle are inherited unchanged.

No Woodpecker **server** change is needed — the per-task labels are already in
`/api/queue/info`. The design record lives in the sealed repo at
`docs/designs/platform/label-aware-autoscaler.md` (SEA-1122).

## Base ref & rebase policy

- **Base:** upstream `woodpecker-ci/autoscaler` release `1.5.0` (pinned).
- The functional diff is confined to `engine/autoscaler.go`,
  `engine/labelfilter/` (new), and their tests. Everything else tracks upstream
  so rebases stay cheap.
- **Rebase opportunistically** onto upstream releases; the minimal-diff
  discipline keeps that a small, low-conflict operation. Do not accumulate
  sealed-isms outside the two engine touchpoints above.

## Build & publish

`.github/workflows/publish-image.yml` publishes the multi-arch
`ghcr.io/rigelbuild/autoscaler:<version>` image on a `v*.*.*-rigel.*` tag,
or by manual `workflow_dispatch` with an explicit version.
No `:latest` is deployed. The elastic-runner pool's IaC pins that image by
digest. Only the scaler process changes; the pool's VPC/IAM, cloud-init,
labels and admin token stay the same.
