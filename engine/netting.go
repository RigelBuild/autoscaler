package engine

import (
	"time"

	"go.woodpecker-ci.org/autoscaler/engine/labelfilter"
	"go.woodpecker-ci.org/woodpecker/v3/woodpecker-go/woodpecker"
)

// This file nets pending demand against capacity this pool does not own.
//
// A static agent outside the pool (a builder, a dev box) often has free slots
// that can run a queued task. Scaling the pool for that task would boot a paid
// machine beside an idle one. So before the planner sees the queue, each
// pending task that a free non-pool slot can run is absorbed by that slot, and
// only the remainder is counted as demand for the pool's buckets.

// freeStaticSlots models the currently-free capacity on one non-pool agent as
// a label filter plus a remaining-slot count. Slots are Agent.Capacity minus
// the running tasks already assigned to that agent.
type freeStaticSlots struct {
	filter labelfilter.Filter
	free   int
}

// nonPoolFreeSlots models the free capacity on every non-pool agent that can
// take new work now. Pool agents are excluded: the netting step only credits
// demand that capacity outside this pool can absorb.
func (a *Autoscaler) nonPoolFreeSlots(running []woodpecker.Task) []*freeStaticSlots {
	poolIDs := make(map[int64]struct{}, len(a.agents))
	for _, agent := range a.agents {
		poolIDs[agent.ID] = struct{}{}
	}

	assigned := make(map[int64]int)
	for _, task := range running {
		assigned[task.AgentID]++
	}

	slots := make([]*freeStaticSlots, 0, len(a.allAgents))
	for _, agent := range a.allAgents {
		if _, ok := poolIDs[agent.ID]; ok {
			continue // pool agent — not "other" capacity
		}
		if agent.NoSchedule {
			continue // draining or quarantined — cannot take new work
		}
		// A cleanly-offline agent's server row persists (Capacity>0, zero
		// running, NoSchedule=false) and would otherwise credit phantom free
		// slots that net out real pending demand, so the pool never scales to
		// cover a dead static agent. Skip an agent whose last contact is
		// stale, mirroring the liveness window cleanupStaleAgents enforces.
		lastContact := agent.LastContact
		if lastContact == 0 {
			lastContact = agent.Created
		}
		if time.Since(time.Unix(lastContact, 0)) > a.config.AgentInactivityTimeout {
			continue
		}
		free := int(agent.Capacity) - assigned[agent.ID]
		if free <= 0 {
			continue
		}
		slots = append(slots, &freeStaticSlots{filter: labelfilter.AgentFilter(agent), free: free})
	}
	return slots
}

// unabsorbedPending returns the pending tasks the pool must still scale for,
// after free non-pool slots have absorbed what they can run.
//
// Each task is matched first-fit against the free slots, in queue order. This
// is a first-fit approximation of maximum bipartite matching. It can under-net
// when an early task takes a broad-filter slot that a later, more-constrained
// task also needed, so the pool over-provisions by up to the number of such
// collisions. That error is always the safe direction (extra capacity, never
// starvation), is bounded by MaxAgents, and self-corrects on the next
// reconcile. Static agents are few and mostly homogeneous, so exact matching
// is not worth the complexity here.
func (a *Autoscaler) unabsorbedPending(pending, running []woodpecker.Task) []woodpecker.Task {
	slots := a.nonPoolFreeSlots(running)
	if len(slots) == 0 {
		return pending
	}

	remaining := make([]woodpecker.Task, 0, len(pending))
	for _, task := range pending {
		absorbed := false
		for _, slot := range slots {
			if slot.free > 0 && slot.filter.Satisfiable(task) {
				slot.free--
				absorbed = true
				break
			}
		}
		if !absorbed {
			remaining = append(remaining, task)
		}
	}
	return remaining
}

// poolEligiblePending keeps the pending tasks this pool can run at all. It is
// the PoolPlatform synthesis: a platform-keyed task counts as pool demand only
// when the platform the pool's agents will self-report matches it, and the
// modeled pool filter stands in for those agents' labels.
//
// It narrows eligibility only. It never creates a bucket: a bucket still needs
// a provider capability, and a pair the provider cannot deploy must not be
// planned for. With PoolPlatform empty the step is skipped: upstream's buckets
// already carry each capability's platform, so bucket routing decides.
func (a *Autoscaler) poolEligiblePending(pending []woodpecker.Task) []woodpecker.Task {
	if a.config.PoolPlatform == "" {
		return pending
	}
	poolFilter := labelfilter.NewPoolFilter(a.config.ExtraAgentLabels, a.config.PoolPlatform)
	eligible := make([]woodpecker.Task, 0, len(pending))
	for _, task := range pending {
		if poolFilter.Satisfiable(task) {
			eligible = append(eligible, task)
		}
	}
	return eligible
}
