package engine

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.woodpecker-ci.org/autoscaler/config"
	"go.woodpecker-ci.org/autoscaler/engine/types"
	"go.woodpecker-ci.org/woodpecker/v3/woodpecker-go/woodpecker"
)

// linuxTask / macTask build tasks in the parent's routing vocabulary, with
// the server-stamped repo/org-id every real queued task carries (the labels
// the modeled pool filter must survive).
func linuxTask() woodpecker.Task {
	return woodpecker.Task{Labels: map[string]string{"type": "linux", "repo": "rigel/x", "org-id": "1"}}
}

func macTask() woodpecker.Task {
	return woodpecker.Task{Labels: map[string]string{"type": "macos", "repo": "rigel/x", "org-id": "1"}}
}

// platformTask is a pending task keyed on the agent-self-reported platform
// label, as the taxonomy's pool selectors emit.
func platformTask() woodpecker.Task {
	return woodpecker.Task{Labels: map[string]string{"type": "linux", "platform": "linux/arm64", "repo": "rigel/x", "org-id": "1"}}
}

// elasticLabels is the pool's WOODPECKER_AGENT_LABELS in the parent's model.
func elasticLabels() map[string]string {
	return map[string]string{"type": "linux", "pool": "elastic"}
}

// pendingDelta returns the total scale-up the planner requests across buckets
// for the given queue, with the pool as configured. A bucket that is not
// planned for contributes nothing.
func pendingDelta(t *testing.T, a *Autoscaler, pending, running []woodpecker.Task) int {
	t.Helper()
	total := 0
	for _, d := range a.planScaling(pending, running, nil) {
		if d.Delta > 0 {
			total += d.Delta
		}
	}
	return total
}

// netTestAutoscaler builds an engine whose only pool bucket is an amd64
// docker capability with the given pool extra labels and pool platform.
func netTestAutoscaler(extra map[string]string, poolPlatform string, allAgents []*woodpecker.Agent) *Autoscaler {
	return &Autoscaler{
		agents:               map[string]*woodpecker.Agent{},
		allAgents:            allAgents,
		providerCapabilities: []types.Capability{dockerAmd64Cap},
		scope:                instanceAdminPoolScope(),
		config: &config.Config{
			WorkflowsPerAgent:      1,
			MaxAgents:              8,
			MinAgents:              0,
			PoolID:                 "1",
			ExtraAgentLabels:       extra,
			PoolPlatform:           poolPlatform,
			AgentInactivityTimeout: 10 * time.Minute,
		},
	}
}

func Test_netting(t *testing.T) {
	// linuxTask / platformTask come from autoscaler_test.go.

	t.Run("shared-eligible pending netted out by a free static slot", func(t *testing.T) {
		// A free static agent (large, capacity 1, idle) can absorb the one
		// bare-linux pending task, so the pool does not scale for it.
		staticAgent := &woodpecker.Agent{
			ID: 99, Name: "mattserver", OrgID: -1, Capacity: 1,
			CustomLabels: map[string]string{"type": "linux", "size": "large"},
			LastContact:  time.Now().Unix(),
		}
		a := netTestAutoscaler(elasticLabels(), "", []*woodpecker.Agent{staticAgent})
		assert.Equal(t, 0, pendingDelta(t, a, []woodpecker.Task{linuxTask()}, nil),
			"the free static slot absorbs the shared-eligible task")
	})

	t.Run("shared-eligible pending counted when static slot is busy", func(t *testing.T) {
		// Same static agent, but its one slot already runs a task, so there is
		// no free capacity to net and the pool scales for the pending task.
		staticAgent := &woodpecker.Agent{
			ID: 99, Name: "mattserver", OrgID: -1, Capacity: 1,
			CustomLabels: map[string]string{"type": "linux", "size": "large"},
			LastContact:  time.Now().Unix(),
		}
		running := []woodpecker.Task{{AgentID: 99, Labels: map[string]string{"type": "linux", "size": "large"}}}
		a := netTestAutoscaler(elasticLabels(), "", []*woodpecker.Agent{staticAgent})
		assert.Equal(t, 1, pendingDelta(t, a, []woodpecker.Task{linuxTask()}, running),
			"busy static can't absorb, so the pool scales")
	})

	t.Run("stale non-pool agent is not credited, so the pool scales (offline-builder redundancy)", func(t *testing.T) {
		// A cleanly-offline static agent's row persists with free capacity.
		// Its last contact is long past the inactivity timeout, so it must
		// not net out demand.
		stale := &woodpecker.Agent{
			ID: 99, Name: "mattserver", OrgID: -1, Capacity: 1,
			CustomLabels: map[string]string{"type": "linux", "size": "large"},
			LastContact:  time.Now().Add(-time.Hour).Unix(),
		}
		a := netTestAutoscaler(elasticLabels(), "", []*woodpecker.Agent{stale})
		assert.Equal(t, 1, pendingDelta(t, a, []woodpecker.Task{linuxTask()}, nil),
			"a stale static agent is not credited, so the pool scales")
	})

	t.Run("fresh non-pool agent still credited, so no scale-up (healthy-primary path intact)", func(t *testing.T) {
		fresh := &woodpecker.Agent{
			ID: 99, Name: "mattserver", OrgID: -1, Capacity: 1,
			CustomLabels: map[string]string{"type": "linux", "size": "large"},
			LastContact:  time.Now().Unix(),
		}
		a := netTestAutoscaler(elasticLabels(), "", []*woodpecker.Agent{fresh})
		assert.Equal(t, 0, pendingDelta(t, a, []woodpecker.Task{linuxTask()}, nil))
	})

	t.Run("never-contacted non-pool agent falls back to Created: recent is credited", func(t *testing.T) {
		recent := &woodpecker.Agent{
			ID: 99, Name: "mattserver", OrgID: -1, Capacity: 1,
			CustomLabels: map[string]string{"type": "linux", "size": "large"},
			Created:      time.Now().Unix(),
		}
		a := netTestAutoscaler(elasticLabels(), "", []*woodpecker.Agent{recent})
		assert.Equal(t, 0, pendingDelta(t, a, []woodpecker.Task{linuxTask()}, nil))
	})

	t.Run("never-contacted non-pool agent falls back to Created: stale is not credited", func(t *testing.T) {
		stale := &woodpecker.Agent{
			ID: 99, Name: "mattserver", OrgID: -1, Capacity: 1,
			CustomLabels: map[string]string{"type": "linux", "size": "large"},
			Created:      time.Now().Add(-time.Hour).Unix(),
		}
		a := netTestAutoscaler(elasticLabels(), "", []*woodpecker.Agent{stale})
		assert.Equal(t, 1, pendingDelta(t, a, []woodpecker.Task{linuxTask()}, nil))
	})

	t.Run("static agent that cannot run the task does not net it out", func(t *testing.T) {
		// A macOS static agent has free slots but cannot run a linux task.
		mac := &woodpecker.Agent{
			ID: 99, Name: "mattmini", OrgID: -1, Capacity: 2,
			CustomLabels: map[string]string{"type": "macos"},
			LastContact:  time.Now().Unix(),
		}
		a := netTestAutoscaler(elasticLabels(), "", []*woodpecker.Agent{mac})
		assert.Equal(t, 1, pendingDelta(t, a, []woodpecker.Task{linuxTask()}, nil))
	})

	t.Run("free NoSchedule static agent does not net a task", func(t *testing.T) {
		draining := &woodpecker.Agent{
			ID: 99, Name: "mattserver", OrgID: -1, Capacity: 1,
			CustomLabels: map[string]string{"type": "linux", "size": "large"},
			LastContact:  time.Now().Unix(),
			NoSchedule:   true,
		}
		a := netTestAutoscaler(elasticLabels(), "", []*woodpecker.Agent{draining})
		assert.Equal(t, 1, pendingDelta(t, a, []woodpecker.Task{linuxTask()}, nil))
	})

	t.Run("Capacity:2 idle static absorbs two pending linux tasks", func(t *testing.T) {
		static := &woodpecker.Agent{
			ID: 99, Name: "mattserver", OrgID: -1, Capacity: 2,
			CustomLabels: map[string]string{"type": "linux", "size": "large"},
			LastContact:  time.Now().Unix(),
		}
		a := netTestAutoscaler(elasticLabels(), "", []*woodpecker.Agent{static})
		assert.Equal(t, 0, pendingDelta(t, a, []woodpecker.Task{linuxTask(), linuxTask()}, nil))
	})

	t.Run("greedy first-fit over-provisions in adversarial order", func(t *testing.T) {
		// PINS the documented greedy first-fit approximation: accepted
		// safe-direction over-provisioning, not a bug. broad (OrgID:-1, so
		// org-id:*) matches org 1 and org 2; narrow (OrgID:1) matches org 1
		// only. broad is tried first, the org-1 task takes it, and the org-2
		// task cannot use the org-1-scoped narrow slot. An optimal matcher
		// would net both to 0.
		broad := &woodpecker.Agent{
			ID: 1, Name: "broadstatic", OrgID: -1, Capacity: 1,
			CustomLabels: map[string]string{"type": "linux"},
			LastContact:  time.Now().Unix(),
		}
		narrow := &woodpecker.Agent{
			ID: 2, Name: "narrowstatic", OrgID: 1, Capacity: 1,
			CustomLabels: map[string]string{"type": "linux"},
			LastContact:  time.Now().Unix(),
		}
		taskOrg2 := woodpecker.Task{Labels: map[string]string{"type": "linux", "repo": "rigel/x", "org-id": "2"}}
		a := netTestAutoscaler(elasticLabels(), "", []*woodpecker.Agent{broad, narrow})
		assert.Equal(t, 1, pendingDelta(t, a, []woodpecker.Task{linuxTask(), taskOrg2}, nil),
			"first-fit spends the broad slot on the org-1 task, leaving the org-2 task unabsorbed")
	})

	t.Run("pool running task on a draining agent is not counted as static capacity", func(t *testing.T) {
		pool := &woodpecker.Agent{ID: 5, Name: "pool-1-agent-abc", Capacity: 1, LastContact: time.Now().Unix()}
		a := netTestAutoscaler(elasticLabels(), "", []*woodpecker.Agent{pool})
		a.agents = map[string]*woodpecker.Agent{pool.Name: pool}
		assert.Equal(t, 1, pendingDelta(t, a, []woodpecker.Task{linuxTask()}, nil),
			"a pool agent is never a static slot, so it nets nothing")
	})

	t.Run("platform-keyed pending scales when PoolPlatform matches", func(t *testing.T) {
		caps := netTestAutoscaler(elasticLabels(), "linux/arm64", nil)
		caps.providerCapabilities = []types.Capability{{Platform: "linux/arm64", Backend: types.BackendDocker}}
		assert.Equal(t, 1, pendingDelta(t, caps, []woodpecker.Task{platformTask()}, nil))
	})

	t.Run("platform-keyed pending falls back to bucket routing when PoolPlatform is unset", func(t *testing.T) {
		a := netTestAutoscaler(elasticLabels(), "", nil)
		a.providerCapabilities = []types.Capability{{Platform: "linux/arm64", Backend: types.BackendDocker}}
		assert.Equal(t, 1, pendingDelta(t, a, []woodpecker.Task{platformTask()}, nil),
			"with no PoolPlatform the upstream bucket carries the capability platform")
	})

	t.Run("platform-keyed pending does not scale when PoolPlatform mismatches", func(t *testing.T) {
		a := netTestAutoscaler(elasticLabels(), "linux/amd64", nil)
		a.providerCapabilities = []types.Capability{{Platform: "linux/arm64", Backend: types.BackendDocker}}
		assert.Equal(t, 0, pendingDelta(t, a, []woodpecker.Task{platformTask()}, nil),
			"an amd64 pool must not scale for arm64 work")
	})

	t.Run("an idle static nets out platform-keyed work via its self-reported platform", func(t *testing.T) {
		builder := &woodpecker.Agent{
			ID: 7, Name: "mattmini", OrgID: -1, Capacity: 2,
			Platform:     "linux/arm64",
			CustomLabels: map[string]string{"builder": "image"},
			LastContact:  time.Now().Unix(),
		}
		buildTask := woodpecker.Task{Labels: map[string]string{"builder": "image", "platform": "linux/arm64", "repo": "rigel/x", "org-id": "1"}}
		a := netTestAutoscaler(map[string]string{"builder": "image"}, "linux/arm64", []*woodpecker.Agent{builder})
		a.providerCapabilities = []types.Capability{{Platform: "linux/arm64", Backend: types.BackendDocker}}
		assert.Equal(t, 0, pendingDelta(t, a, []woodpecker.Task{buildTask, buildTask}, nil),
			"two free slots on the idle arm64 builder absorb both arm64 builds")
	})

	t.Run("a pending task no bucket can run is not netted and not scaled for", func(t *testing.T) {
		// Netting only spends a static slot on a task the pool could have run.
		// A macos task is ineligible for this linux-only pool.
		static := &woodpecker.Agent{
			ID: 99, Name: "mattserver", OrgID: -1, Capacity: 1,
			CustomLabels: map[string]string{"type": "macos"},
			LastContact:  time.Now().Unix(),
		}
		a := netTestAutoscaler(elasticLabels(), "", []*woodpecker.Agent{static})
		require.Equal(t, 0, pendingDelta(t, a, []woodpecker.Task{macTask()}, nil))
	})
}

// Test_arm64PoolAgentOnAmd64Node mirrors the prod arm64 kubernetes pool: the
// agent pod runs on an amd64 node and self-reports linux/amd64, while its
// steps run on arm64 and the pool routes on PoolPlatform=linux/arm64 plus a
// platform custom label. A connected agent must stay in its bucket: it is
// not drained as unmatched, and it is counted as pool capacity.
func Test_arm64PoolAgentOnAmd64Node(t *testing.T) {
	labels := map[string]string{"builder": "image", "platform": "linux/arm64"}
	k8sCap := types.Capability{Backend: types.BackendKubernetes}
	agent := &woodpecker.Agent{
		ID: 1, Name: "pool-1-agent-arm", Platform: "linux/amd64", Backend: "kubernetes",
		CustomLabels: labels, LastContact: time.Now().Unix(),
	}
	a := netTestAutoscaler(labels, "linux/arm64", []*woodpecker.Agent{agent})
	a.providerCapabilities = []types.Capability{k8sCap}
	a.agents = map[string]*woodpecker.Agent{agent.Name: agent}

	buckets := a.agentBuckets()
	assert.Equal(t, 0, matchAgentToBucket(agent, buckets), "the agent belongs to the pool's bucket")
	require.NoError(t, a.drainUnmatchedAgents(buckets))
	assert.False(t, agent.NoSchedule, "a connected arm64-pool agent is not drained as unmatched")

	arm := woodpecker.Task{Labels: map[string]string{"builder": "image", "platform": "linux/arm64", "repo": "rigel/x", "org-id": "1"}}
	amd := woodpecker.Task{Labels: map[string]string{"builder": "image", "platform": "linux/amd64", "repo": "rigel/x", "org-id": "1"}}
	assert.Equal(t, 0, pendingDelta(t, a, []woodpecker.Task{arm}, nil),
		"the connected agent covers one arm64 task, so nothing is created")
	assert.Equal(t, 0, pendingDelta(t, a, []woodpecker.Task{amd}, nil),
		"amd64 work never routes to the arm64 pool")
}

// Test_poolPlatformRoutingFallback covers the amd64 kubernetes pools in prod:
// the capability has no platform and ExtraAgentLabels carry no platform key,
// so PoolPlatform is the only thing that makes a platform-keyed task routable.
func Test_poolPlatformRoutingFallback(t *testing.T) {
	labels := map[string]string{"cloud": "azure", "os": "linux"}
	a := netTestAutoscaler(labels, "linux/amd64", nil)
	a.providerCapabilities = []types.Capability{{Backend: types.BackendKubernetes}}
	task := woodpecker.Task{Labels: map[string]string{"os": "linux", "platform": "linux/amd64", "repo": "rigel/x", "org-id": "1"}}
	assert.Equal(t, 1, pendingDelta(t, a, []woodpecker.Task{task}, nil),
		"the bucket routes on PoolPlatform when the capability platform is unknown")
}
