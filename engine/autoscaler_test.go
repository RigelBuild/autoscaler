package engine

import (
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"

	"go.woodpecker-ci.org/autoscaler/config"
	"go.woodpecker-ci.org/autoscaler/engine/types"
	mocks_provider "go.woodpecker-ci.org/autoscaler/engine/types/mocks"
	mocks_server "go.woodpecker-ci.org/autoscaler/server/mocks"
	"go.woodpecker-ci.org/woodpecker/v3/woodpecker-go/woodpecker"
)

// MockClient serves a configurable queue Info (pending/running task lists with
// labels) for the label-aware calcAgents tests. It embeds woodpecker.Client so
// only QueueInfo needs an implementation.
type MockClient struct {
	pending []woodpecker.Task
	running []woodpecker.Task
	woodpecker.Client
}

func (m MockClient) QueueInfo() (*woodpecker.Info, error) {
	return &woodpecker.Info{
		Pending: m.pending,
		Running: m.running,
	}, nil
}

// linuxTask / heavyTask / macTask build tasks in the parent's routing
// vocabulary, with the server-stamped repo/org-id every real queued task
// carries (the labels the modeled pool filter must survive).
func linuxTask() woodpecker.Task {
	return woodpecker.Task{Labels: map[string]string{"type": "linux", "repo": "rigel/x", "org-id": "1"}}
}

func heavyTask() woodpecker.Task {
	return woodpecker.Task{Labels: map[string]string{"type": "linux", "size": "large", "repo": "rigel/x", "org-id": "1"}}
}

func macTask() woodpecker.Task {
	return woodpecker.Task{Labels: map[string]string{"type": "macos", "repo": "rigel/x", "org-id": "1"}}
}

// elasticLabels is the pool's WOODPECKER_AGENT_LABELS in the parent's model.
func elasticLabels() map[string]string {
	return map[string]string{"type": "linux", "pool": "elastic"}
}

func Test_calcAgents(t *testing.T) {
	zerolog.SetGlobalLevel(zerolog.ErrorLevel)

	t.Run("all size=large pending ⇒ no scale-up (the headline waste case)", func(t *testing.T) {
		autoscaler := Autoscaler{client: &MockClient{
			pending: []woodpecker.Task{heavyTask(), heavyTask(), heavyTask()},
		}, config: &config.Config{
			WorkflowsPerAgent: 1,
			MaxAgents:         8,
			MinAgents:         0,
			ExtraAgentLabels:  elasticLabels(),
		}}

		value, err := autoscaler.calcAgents(t.Context())
		assert.NoError(t, err)
		assert.Equal(t, float64(0), value, "heavy jobs the pool can't run must not scale it up")
	})

	t.Run("macOS pending ⇒ ignored", func(t *testing.T) {
		autoscaler := Autoscaler{client: &MockClient{
			pending: []woodpecker.Task{macTask(), macTask()},
		}, config: &config.Config{
			WorkflowsPerAgent: 1,
			MaxAgents:         8,
			ExtraAgentLabels:  elasticLabels(),
		}}

		value, _ := autoscaler.calcAgents(t.Context())
		assert.Equal(t, float64(0), value)
	})

	t.Run("bare linux pending ⇒ scales for eligible count", func(t *testing.T) {
		autoscaler := Autoscaler{client: &MockClient{
			pending: []woodpecker.Task{linuxTask(), linuxTask(), linuxTask()},
		}, config: &config.Config{
			WorkflowsPerAgent: 1,
			MaxAgents:         8,
			ExtraAgentLabels:  elasticLabels(),
		}}

		value, _ := autoscaler.calcAgents(t.Context())
		assert.Equal(t, float64(3), value)
	})

	t.Run("mixed queue ⇒ scales only for the eligible (linux) count", func(t *testing.T) {
		autoscaler := Autoscaler{client: &MockClient{
			pending: []woodpecker.Task{heavyTask(), linuxTask(), macTask(), linuxTask(), heavyTask()},
		}, config: &config.Config{
			WorkflowsPerAgent: 1,
			MaxAgents:         8,
			ExtraAgentLabels:  elasticLabels(),
		}}

		value, _ := autoscaler.calcAgents(t.Context())
		assert.Equal(t, float64(2), value, "only the two bare-linux tasks are eligible")
	})

	t.Run("WorkflowsPerAgent packs multiple eligible tasks per agent", func(t *testing.T) {
		autoscaler := Autoscaler{client: &MockClient{
			pending: []woodpecker.Task{linuxTask(), linuxTask(), linuxTask(), linuxTask(), linuxTask(), linuxTask()},
		}, config: &config.Config{
			WorkflowsPerAgent: 5,
			MaxAgents:         8,
			ExtraAgentLabels:  elasticLabels(),
		}}

		value, _ := autoscaler.calcAgents(t.Context())
		assert.Equal(t, float64(2), value, "ceil(6/5) = 2")
	})

	t.Run("MaxAgents clamps scale-up", func(t *testing.T) {
		autoscaler := Autoscaler{client: &MockClient{
			pending: []woodpecker.Task{linuxTask(), linuxTask(), linuxTask(), linuxTask()},
		}, config: &config.Config{
			WorkflowsPerAgent: 1,
			MaxAgents:         2,
			ExtraAgentLabels:  elasticLabels(),
		}}

		value, _ := autoscaler.calcAgents(t.Context())
		assert.Equal(t, float64(2), value, "4 eligible clamped to MaxAgents=2")
	})

	t.Run("MinAgents floor forces scale-up on an empty queue", func(t *testing.T) {
		autoscaler := Autoscaler{client: &MockClient{}, config: &config.Config{
			WorkflowsPerAgent: 1,
			MaxAgents:         2,
			MinAgents:         1,
			ExtraAgentLabels:  elasticLabels(),
		}}

		value, _ := autoscaler.calcAgents(t.Context())
		assert.Equal(t, float64(1), value, "empty queue but MinAgents=1 ⇒ +1")
	})

	t.Run("pool running tasks count toward required (no redundant scale-up)", func(t *testing.T) {
		// one pool agent already running one eligible task; queue empty ⇒ pool
		// is exactly staffed, delta 0.
		autoscaler := Autoscaler{
			client: &MockClient{
				running: []woodpecker.Task{{AgentID: 10, Labels: map[string]string{"type": "linux"}}},
			},
			agents:    []*woodpecker.Agent{{ID: 10, Name: "pool-1-agent-a"}},
			allAgents: []*woodpecker.Agent{{ID: 10, Name: "pool-1-agent-a"}},
			config: &config.Config{
				WorkflowsPerAgent: 1,
				MaxAgents:         8,
				PoolID:            "1",
				ExtraAgentLabels:  elasticLabels(),
			},
		}

		value, _ := autoscaler.calcAgents(t.Context())
		assert.Equal(t, float64(0), value, "required=1, one pool agent present ⇒ delta 0")
	})

	t.Run("shared-eligible pending netted out by a free static slot", func(t *testing.T) {
		// A free static agent (large, capacity 1, idle) can absorb the one
		// bare-linux pending task ⇒ pool does not scale.
		staticAgent := &woodpecker.Agent{
			ID: 99, Name: "mattserver", OrgID: -1, Capacity: 1,
			CustomLabels: map[string]string{"type": "linux", "size": "large"},
			LastContact:  time.Now().Unix(),
		}
		autoscaler := Autoscaler{
			client:    &MockClient{pending: []woodpecker.Task{linuxTask()}},
			allAgents: []*woodpecker.Agent{staticAgent},
			config: &config.Config{
				WorkflowsPerAgent:      1,
				MaxAgents:              8,
				PoolID:                 "1",
				ExtraAgentLabels:       elasticLabels(),
				AgentInactivityTimeout: 10 * time.Minute,
			},
		}

		value, _ := autoscaler.calcAgents(t.Context())
		assert.Equal(t, float64(0), value, "the free static slot absorbs the shared-eligible task")
	})

	t.Run("shared-eligible pending counted when static slot is busy", func(t *testing.T) {
		// Same static agent, but its one slot is already running a task ⇒ no
		// free capacity to net, so the pool scales for the pending linux task.
		staticAgent := &woodpecker.Agent{
			ID: 99, Name: "mattserver", OrgID: -1, Capacity: 1,
			CustomLabels: map[string]string{"type": "linux", "size": "large"},
			LastContact:  time.Now().Unix(),
		}
		autoscaler := Autoscaler{
			client: &MockClient{
				pending: []woodpecker.Task{linuxTask()},
				running: []woodpecker.Task{{AgentID: 99, Labels: map[string]string{"type": "linux", "size": "large"}}},
			},
			allAgents: []*woodpecker.Agent{staticAgent},
			config: &config.Config{
				WorkflowsPerAgent:      1,
				MaxAgents:              8,
				PoolID:                 "1",
				ExtraAgentLabels:       elasticLabels(),
				AgentInactivityTimeout: 10 * time.Minute,
			},
		}

		value, _ := autoscaler.calcAgents(t.Context())
		assert.Equal(t, float64(1), value, "busy static can't absorb ⇒ pool scales")
	})

	t.Run("stale non-pool agent is not credited ⇒ pool scales (offline-builder redundancy)", func(t *testing.T) {
		// A cleanly-offline static agent's row persists (capacity 1, idle,
		// NoSchedule=false) but its last contact is older than the inactivity
		// window ⇒ its phantom slot must NOT net out the pending task.
		staticAgent := &woodpecker.Agent{
			ID: 99, Name: "mattserver", OrgID: -1, Capacity: 1,
			CustomLabels: map[string]string{"type": "linux", "size": "large"},
			LastContact:  time.Now().Add(-20 * time.Minute).Unix(),
		}
		autoscaler := Autoscaler{
			client:    &MockClient{pending: []woodpecker.Task{linuxTask()}},
			allAgents: []*woodpecker.Agent{staticAgent},
			config: &config.Config{
				WorkflowsPerAgent:      1,
				MaxAgents:              8,
				PoolID:                 "1",
				ExtraAgentLabels:       elasticLabels(),
				AgentInactivityTimeout: 10 * time.Minute,
			},
		}

		value, _ := autoscaler.calcAgents(t.Context())
		assert.Equal(t, float64(1), value, "stale static agent's phantom slot must not be netted ⇒ pool scales")
	})

	t.Run("fresh non-pool agent still credited ⇒ no scale-up (healthy-primary path intact)", func(t *testing.T) {
		// Same static agent, contacted just now ⇒ live, so its free slot still
		// absorbs the pending task and the pool does not scale.
		staticAgent := &woodpecker.Agent{
			ID: 99, Name: "mattserver", OrgID: -1, Capacity: 1,
			CustomLabels: map[string]string{"type": "linux", "size": "large"},
			LastContact:  time.Now().Unix(),
		}
		autoscaler := Autoscaler{
			client:    &MockClient{pending: []woodpecker.Task{linuxTask()}},
			allAgents: []*woodpecker.Agent{staticAgent},
			config: &config.Config{
				WorkflowsPerAgent:      1,
				MaxAgents:              8,
				PoolID:                 "1",
				ExtraAgentLabels:       elasticLabels(),
				AgentInactivityTimeout: 10 * time.Minute,
			},
		}

		value, _ := autoscaler.calcAgents(t.Context())
		assert.Equal(t, float64(0), value, "live static slot still absorbs the shared-eligible task")
	})

	t.Run("static agent that can't run the task does not net it out", func(t *testing.T) {
		// A free macOS static agent cannot absorb a bare-linux task ⇒ counted.
		macStatic := &woodpecker.Agent{
			ID: 77, Name: "macmini", OrgID: -1, Capacity: 2,
			CustomLabels: map[string]string{"type": "macos"},
			LastContact:  time.Now().Unix(),
		}
		autoscaler := Autoscaler{
			client:    &MockClient{pending: []woodpecker.Task{linuxTask()}},
			allAgents: []*woodpecker.Agent{macStatic},
			config: &config.Config{
				WorkflowsPerAgent:      1,
				MaxAgents:              8,
				PoolID:                 "1",
				ExtraAgentLabels:       elasticLabels(),
				AgentInactivityTimeout: 10 * time.Minute,
			},
		}

		value, _ := autoscaler.calcAgents(t.Context())
		assert.Equal(t, float64(1), value, "macOS static can't run linux ⇒ not netted")
	})

	t.Run("draining pool agent finishing its last task ⇒ no scale-up (M1)", func(t *testing.T) {
		// A NoSchedule (draining) pool agent is running its final task with an
		// empty queue. The in-flight task must count as neither demand nor
		// capacity: it is on an agent we're tearing down, so re-justifying that
		// agent would defeat the whole drain window.
		autoscaler := Autoscaler{
			client: &MockClient{
				running: []woodpecker.Task{{AgentID: 10, Labels: map[string]string{"type": "linux"}}},
			},
			agents:    []*woodpecker.Agent{{ID: 10, Name: "pool-1-agent-a", NoSchedule: true}},
			allAgents: []*woodpecker.Agent{{ID: 10, Name: "pool-1-agent-a", NoSchedule: true}},
			config: &config.Config{
				WorkflowsPerAgent: 1,
				MaxAgents:         8,
				MinAgents:         0,
				PoolID:            "1",
				ExtraAgentLabels:  elasticLabels(),
			},
		}

		value, _ := autoscaler.calcAgents(t.Context())
		assert.Equal(t, float64(0), value, "draining pool agent's last task must not re-justify the agent (M1)")
	})

	t.Run("mixed drain+schedulable pool scales down by the schedulable count (M1b)", func(t *testing.T) {
		// Empty queue ⇒ required=0. Only the schedulable agent counts as an
		// available pool agent (the draining NoSchedule one is excluded via
		// getPoolAgents(true)) = 1, so reqPoolAgents = 0 - 1 = -1, clamped by
		// maxDown = 1-0 = 1 ⇒ -1. This pins the symmetric scale-DOWN side of M1:
		// if a refactor let the draining agent into availablePoolAgents (=2) the
		// result would be -2 — draining 2 when only 1 is schedulable.
		autoscaler := Autoscaler{
			client: &MockClient{},
			agents: []*woodpecker.Agent{
				{ID: 10, Name: "pool-1-agent-a"},
				{ID: 11, Name: "pool-1-agent-b", NoSchedule: true},
			},
			allAgents: []*woodpecker.Agent{
				{ID: 10, Name: "pool-1-agent-a"},
				{ID: 11, Name: "pool-1-agent-b", NoSchedule: true},
			},
			config: &config.Config{
				WorkflowsPerAgent: 1,
				MaxAgents:         8,
				MinAgents:         0,
				PoolID:            "1",
				ExtraAgentLabels:  elasticLabels(),
			},
		}

		value, _ := autoscaler.calcAgents(t.Context())
		assert.Equal(t, float64(-1), value, "draining agent excluded from availablePoolAgents; only the 1 schedulable idle agent drives scale-down (M1b)")
	})

	t.Run("free NoSchedule static agent does not net a task (M3a)", func(t *testing.T) {
		// A quarantined (NoSchedule) static agent has nominal free capacity but
		// cannot take new work, so it must not absorb the pending linux task —
		// the pool still has to scale.
		staticAgent := &woodpecker.Agent{
			ID: 99, Name: "mattserver", OrgID: -1, Capacity: 1, NoSchedule: true,
			CustomLabels: map[string]string{"type": "linux"},
			LastContact:  time.Now().Unix(),
		}
		autoscaler := Autoscaler{
			client:    &MockClient{pending: []woodpecker.Task{linuxTask()}},
			allAgents: []*woodpecker.Agent{staticAgent},
			config: &config.Config{
				WorkflowsPerAgent:      1,
				MaxAgents:              8,
				PoolID:                 "1",
				ExtraAgentLabels:       elasticLabels(),
				AgentInactivityTimeout: 10 * time.Minute,
			},
		}

		value, _ := autoscaler.calcAgents(t.Context())
		assert.Equal(t, float64(1), value, "a quarantined (NoSchedule) static agent cannot absorb work")
	})

	t.Run("Capacity:2 idle static absorbs two pending linux tasks (M3b)", func(t *testing.T) {
		// One idle static agent with two free slots nets out both eligible
		// pending tasks ⇒ the pool does not scale.
		staticAgent := &woodpecker.Agent{
			ID: 99, Name: "mattserver", OrgID: -1, Capacity: 2,
			CustomLabels: map[string]string{"type": "linux"},
			LastContact:  time.Now().Unix(),
		}
		autoscaler := Autoscaler{
			client:    &MockClient{pending: []woodpecker.Task{linuxTask(), linuxTask()}},
			allAgents: []*woodpecker.Agent{staticAgent},
			config: &config.Config{
				WorkflowsPerAgent:      1,
				MaxAgents:              8,
				PoolID:                 "1",
				ExtraAgentLabels:       elasticLabels(),
				AgentInactivityTimeout: 10 * time.Minute,
			},
		}

		value, _ := autoscaler.calcAgents(t.Context())
		assert.Equal(t, float64(0), value, "a capacity-2 idle static agent absorbs both pending linux tasks")
	})

	t.Run("greedy first-fit over-provisions in adversarial order (M3c)", func(t *testing.T) {
		// PINS the documented greedy first-fit netting approximation (see the
		// maximum-bipartite-matching comment in calcAgents) — this is the
		// accepted safe-direction over-provisioning, NOT a bug to fix here.
		//
		// broad (OrgID:-1 ⇒ org-id:*) matches org 1 and org 2; narrow (OrgID:1
		// ⇒ org-id:1) matches org 1 only. allAgents order matters:
		// nonPoolFreeSlots iterates allAgents, so broad is tried first. The
		// org-1 task greedily consumes the broad slot, leaving only the
		// org-1-scoped narrow slot for the org-2 task — which it can't use — so
		// the pool over-provisions by 1. An optimal matcher would send org-1 to
		// narrow and net both to 0; changing that is a deliberate behavior
		// change a future reader should review, and would flip this to 0.
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
		autoscaler := Autoscaler{
			client:    &MockClient{pending: []woodpecker.Task{linuxTask() /* org-id 1 */, taskOrg2}},
			allAgents: []*woodpecker.Agent{broad, narrow}, // ORDER MATTERS: broad tried first
			config: &config.Config{
				WorkflowsPerAgent:      1,
				MaxAgents:              8,
				PoolID:                 "1",
				ExtraAgentLabels:       elasticLabels(),
				AgentInactivityTimeout: 10 * time.Minute,
			},
		}

		value, _ := autoscaler.calcAgents(t.Context())
		assert.Equal(t, float64(1), value, "greedy first-fit: linux(org1) takes the broad slot, org2 can't use the org1-scoped narrow slot ⇒ over-provisions by 1 (accepted safe-direction approx; see calcAgents netting comment)")
	})
}

func Test_getQueueInfo(t *testing.T) {
	zerolog.SetGlobalLevel(zerolog.ErrorLevel)
	t.Run("returns the full queue info with task lists", func(t *testing.T) {
		autoscaler := Autoscaler{
			client: &MockClient{
				pending: []woodpecker.Task{linuxTask(), heavyTask()},
				running: []woodpecker.Task{linuxTask()},
			},
			config: &config.Config{},
		}

		info, err := autoscaler.getQueueInfo(t.Context())
		assert.NoError(t, err)
		assert.Len(t, info.Pending, 2)
		assert.Len(t, info.Running, 1)
	})
}

func Test_getPoolAgents(t *testing.T) {
	autoscaler := Autoscaler{
		agents: []*woodpecker.Agent{
			{ID: 1, Name: "pool-1-agent-1", NoSchedule: false},
			{ID: 2, Name: "pool-1-agent-2", NoSchedule: true},
			{ID: 3, Name: "pool-1-agent-3", NoSchedule: false},
		},
	}

	agents := autoscaler.getPoolAgents(false)
	assert.Equal(t, 3, len(agents))

	agents = autoscaler.getPoolAgents(true)
	assert.Equal(t, 2, len(agents))
}

func Test_createAgents(t *testing.T) {
	zerolog.SetGlobalLevel(zerolog.ErrorLevel)

	t.Run("should create a new agent", func(t *testing.T) {
		ctx := t.Context()
		client := mocks_server.NewMockClient(t)
		provider := mocks_provider.NewMockProvider(t)
		autoscaler := Autoscaler{
			client:   client,
			provider: provider,
			config: &config.Config{
				PoolID: "1",
			},
		}

		client.On("AgentCreate", mock.Anything).Return(&woodpecker.Agent{Name: "pool-1-agent-1"}, nil)
		provider.On("DeployAgent", ctx, mock.Anything).Return(nil)

		err := autoscaler.createAgents(ctx, 1)
		assert.NoError(t, err)
	})

	t.Run("should reuse an no-schedule agent first before creating a new one", func(t *testing.T) {
		ctx := t.Context()
		client := mocks_server.NewMockClient(t)
		provider := mocks_provider.NewMockProvider(t)
		autoscaler := Autoscaler{
			client:   client,
			provider: provider,
			agents: []*woodpecker.Agent{
				{
					ID:         1,
					NoSchedule: true,
				},
			},
			config: &config.Config{
				PoolID: "1",
			},
		}

		client.On("AgentUpdate", mock.MatchedBy(func(agent *woodpecker.Agent) bool {
			return agent.ID == 1 && agent.NoSchedule == false
		})).Return(nil, nil)
		client.On("AgentCreate", mock.Anything).Return(&woodpecker.Agent{Name: "pool-1-agent-1"}, nil)
		provider.On("DeployAgent", ctx, mock.Anything).Return(nil)

		err := autoscaler.createAgents(ctx, 2)
		assert.NoError(t, err)
	})
}

func Test_cleanupDanglingAgents(t *testing.T) {
	t.Run("should remove agent that is only present on woodpecker (not provider)", func(t *testing.T) {
		ctx := t.Context()
		client := mocks_server.NewMockClient(t)
		provider := mocks_provider.NewMockProvider(t)
		autoscaler := Autoscaler{
			agents: []*woodpecker.Agent{
				{ID: 1, Name: "pool-1-agent-1", NoSchedule: false},
			},
			provider: provider,
			client:   client,
		}

		provider.On("ListDeployedAgentNames", mock.Anything).Return(nil, nil)
		client.On("AgentDelete", int64(1)).Return(nil)

		err := autoscaler.cleanupDanglingAgents(ctx)
		assert.NoError(t, err)
	})

	t.Run("should remove agent that is only present on provider (not woodpecker) once grace has elapsed", func(t *testing.T) {
		ctx := t.Context()
		client := mocks_server.NewMockClient(t)
		provider := mocks_provider.NewMockProvider(t)
		autoscaler := Autoscaler{
			agents: []*woodpecker.Agent{
				{ID: 1, Name: "pool-1-agent-1", NoSchedule: false},
			},
			provider: provider,
			client:   client,
			config:   &config.Config{AgentInactivityTimeout: 10 * time.Minute},
			// Pre-seed the grace clock past the timeout so termination fires
			// on this pass (mirrors the two-pass grace behavior).
			danglingSince: map[string]time.Time{
				"pool-1-agent-2": time.Now().Add(-20 * time.Minute),
			},
		}

		provider.On("ListDeployedAgentNames", mock.Anything).Return([]string{"pool-1-agent-1", "pool-1-agent-2"}, nil)
		provider.On("RemoveAgent", mock.Anything, mock.MatchedBy(func(agent *woodpecker.Agent) bool {
			return agent.Name == "pool-1-agent-2"
		})).Return(nil)

		err := autoscaler.cleanupDanglingAgents(ctx)
		assert.NoError(t, err)
		_, ok := autoscaler.danglingSince["pool-1-agent-2"]
		assert.False(t, ok, "danglingSince entry should be cleared after termination")
	})

	t.Run("should not terminate a provider instance the first pass it is missing (grace)", func(t *testing.T) {
		ctx := t.Context()
		client := mocks_server.NewMockClient(t)
		provider := mocks_provider.NewMockProvider(t)
		autoscaler := Autoscaler{
			agents: []*woodpecker.Agent{
				{ID: 1, Name: "pool-1-agent-1", NoSchedule: false},
			},
			provider:      provider,
			client:        client,
			config:        &config.Config{AgentInactivityTimeout: 10 * time.Minute},
			danglingSince: map[string]time.Time{},
		}

		// No RemoveAgent expectation registered: mockery fails if it is called.
		provider.On("ListDeployedAgentNames", mock.Anything).Return([]string{"pool-1-agent-1", "pool-1-agent-2"}, nil)

		err := autoscaler.cleanupDanglingAgents(ctx)
		assert.NoError(t, err)
		_, ok := autoscaler.danglingSince["pool-1-agent-2"]
		assert.True(t, ok, "missing instance should get a danglingSince entry on first pass")
	})

	t.Run("should reset grace when the instance reappears on woodpecker", func(t *testing.T) {
		ctx := t.Context()
		client := mocks_server.NewMockClient(t)
		provider := mocks_provider.NewMockProvider(t)
		autoscaler := Autoscaler{
			agents: []*woodpecker.Agent{
				{ID: 1, Name: "pool-1-agent-1", NoSchedule: false},
				{ID: 2, Name: "pool-1-agent-2", NoSchedule: false},
			},
			provider: provider,
			client:   client,
			config:   &config.Config{AgentInactivityTimeout: 10 * time.Minute},
			danglingSince: map[string]time.Time{
				"pool-1-agent-2": time.Now().Add(-20 * time.Minute),
			},
		}

		// Both provider and woodpecker list the name: no RemoveAgent expected.
		provider.On("ListDeployedAgentNames", mock.Anything).Return([]string{"pool-1-agent-1", "pool-1-agent-2"}, nil)

		err := autoscaler.cleanupDanglingAgents(ctx)
		assert.NoError(t, err)
		_, ok := autoscaler.danglingSince["pool-1-agent-2"]
		assert.False(t, ok, "danglingSince entry should be cleared when instance reappears")
	})

	t.Run("should not terminate a still-within-grace instance on a later pass", func(t *testing.T) {
		ctx := t.Context()
		client := mocks_server.NewMockClient(t)
		provider := mocks_provider.NewMockProvider(t)
		// The clock started 5m ago under a 10m timeout: the instance is missing
		// but still within its grace window, so this pass must neither terminate
		// it nor disturb its clock. This guards the grace deferral itself — the
		// whole point of the grace deferral — which the record/terminate/reappear tests
		// leave uncovered.
		started := time.Now().Add(-5 * time.Minute)
		autoscaler := Autoscaler{
			agents: []*woodpecker.Agent{
				{ID: 1, Name: "pool-1-agent-1", NoSchedule: false},
			},
			provider:      provider,
			client:        client,
			config:        &config.Config{AgentInactivityTimeout: 10 * time.Minute},
			danglingSince: map[string]time.Time{"pool-1-agent-2": started},
		}

		// No RemoveAgent expectation registered: mockery fails if it is called.
		provider.On("ListDeployedAgentNames", mock.Anything).Return([]string{"pool-1-agent-1", "pool-1-agent-2"}, nil)

		err := autoscaler.cleanupDanglingAgents(ctx)
		assert.NoError(t, err)
		assert.Equal(t, started, autoscaler.danglingSince["pool-1-agent-2"],
			"within-grace clock must be preserved unchanged (not reset, not fired)")
	})

	t.Run("should garbage-collect the grace clock when the provider stops listing a still-dangling instance", func(t *testing.T) {
		ctx := t.Context()
		client := mocks_server.NewMockClient(t)
		provider := mocks_provider.NewMockProvider(t)
		// pool-1-agent-2 was recorded missing-from-woodpecker while still within
		// grace, then the provider instance itself vanished (out-of-band Spot
		// reclaim) before the window elapsed. The dangling loop never visits it
		// again, so its clock must be reaped by the post-loop GC or it leaks
		// forever.
		autoscaler := Autoscaler{
			agents: []*woodpecker.Agent{
				{ID: 1, Name: "pool-1-agent-1", NoSchedule: false},
			},
			provider:      provider,
			client:        client,
			config:        &config.Config{AgentInactivityTimeout: 10 * time.Minute},
			danglingSince: map[string]time.Time{"pool-1-agent-2": time.Now().Add(-5 * time.Minute)},
		}

		// Provider no longer lists pool-1-agent-2; no RemoveAgent expected.
		provider.On("ListDeployedAgentNames", mock.Anything).Return([]string{"pool-1-agent-1"}, nil)

		err := autoscaler.cleanupDanglingAgents(ctx)
		assert.NoError(t, err)
		_, ok := autoscaler.danglingSince["pool-1-agent-2"]
		assert.False(t, ok, "grace clock for a vanished provider instance must be garbage-collected, not leaked")
	})
}

func Test_cleanupStaleAgents(t *testing.T) {
	t.Run("should remove agent that never connected (last contact = 0) in over 15 minutes", func(t *testing.T) {
		ctx := t.Context()
		client := mocks_server.NewMockClient(t)
		provider := mocks_provider.NewMockProvider(t)
		autoscaler := Autoscaler{
			agents: []*woodpecker.Agent{
				{
					ID:          1,
					Name:        "active agent",
					NoSchedule:  false,
					Created:     time.Now().Add(-time.Minute * 20).Unix(), // created 20 minutes ago
					LastContact: time.Now().Add(-time.Minute * 5).Unix(),  // last contact 5 minutes ago
				},
				{
					ID:          2,
					Name:        "never contacted agent",
					NoSchedule:  false,
					Created:     time.Now().Add(-time.Minute * 20).Unix(), // created 20 minutes ago
					LastContact: 0,                                        // never contacted
				},
			},
			provider: provider,
			client:   client,
			config: &config.Config{
				AgentInactivityTimeout: time.Minute * 15,
			},
		}

		client.On("AgentTasksList", int64(2)).Return(nil, nil)
		client.On("AgentDelete", int64(2)).Return(nil)
		provider.On("RemoveAgent", mock.Anything, mock.MatchedBy(func(agent *woodpecker.Agent) bool {
			return agent.ID == 2
		})).Return(nil)

		err := autoscaler.cleanupStaleAgents(ctx)
		assert.NoError(t, err)
	})

	t.Run("should remove agent that has lost connection for more than 15 minutes", func(t *testing.T) {
		ctx := t.Context()
		client := mocks_server.NewMockClient(t)
		provider := mocks_provider.NewMockProvider(t)
		autoscaler := Autoscaler{
			agents: []*woodpecker.Agent{
				{
					ID:          1,
					Name:        "active agent",
					NoSchedule:  false,
					Created:     time.Now().Add(-time.Minute * 20).Unix(), // created 20 minutes ago
					LastContact: time.Now().Add(-time.Minute * 5).Unix(),  // last contact 5 minutes ago
				},
				{
					ID:          2,
					Name:        "stale agent",
					NoSchedule:  false,
					Created:     time.Now().Add(-time.Minute * 20).Unix(), // created 20 minutes ago
					LastContact: time.Now().Add(-time.Minute * 20).Unix(), // last contact 20 minutes ago
				},
			},
			provider: provider,
			client:   client,
			config: &config.Config{
				AgentInactivityTimeout: time.Minute * 15,
			},
		}

		client.On("AgentTasksList", int64(2)).Return(nil, nil)
		client.On("AgentDelete", int64(2)).Return(nil)
		provider.On("RemoveAgent", mock.Anything, mock.MatchedBy(func(agent *woodpecker.Agent) bool {
			return agent.ID == 2
		})).Return(nil)

		err := autoscaler.cleanupStaleAgents(ctx)
		assert.NoError(t, err)
	})
}

func Test_isAgentIdle(t *testing.T) {
	t.Run("should return false if agent has tasks", func(t *testing.T) {
		client := mocks_server.NewMockClient(t)
		autoscaler := Autoscaler{
			client: client,
			config: &config.Config{
				AgentIdleTimeout: time.Minute * 15,
			},
		}

		client.On("AgentTasksList", int64(1)).Return([]*woodpecker.Task{
			{ID: "1"},
		}, nil)

		idle, err := autoscaler.isAgentIdle(&woodpecker.Agent{
			ID:         1,
			Name:       "pool-1-agent-1",
			NoSchedule: false,
		})
		assert.NoError(t, err)
		assert.False(t, idle)
	})

	t.Run("should return false if agent has done work recently", func(t *testing.T) {
		client := mocks_server.NewMockClient(t)
		autoscaler := Autoscaler{
			client: client,
			config: &config.Config{
				AgentIdleTimeout: time.Minute * 15,
			},
		}

		client.On("AgentTasksList", int64(1)).Return(nil, nil)

		idle, err := autoscaler.isAgentIdle(&woodpecker.Agent{
			ID:         1,
			Name:       "pool-1-agent-1",
			NoSchedule: false,
			LastWork:   time.Now().Add(-time.Minute * 10).Unix(),
		})
		assert.NoError(t, err)
		assert.False(t, idle)
	})

	t.Run("should return true if agent is idle", func(t *testing.T) {
		client := mocks_server.NewMockClient(t)
		autoscaler := Autoscaler{
			client: client,
			config: &config.Config{
				AgentIdleTimeout: time.Minute * 15,
			},
		}

		client.On("AgentTasksList", int64(1)).Return(nil, nil) // no tasks

		idle, err := autoscaler.isAgentIdle(&woodpecker.Agent{
			ID:         1,
			Name:       "pool-1-agent-1",
			NoSchedule: false,
			LastWork:   time.Now().Add(-time.Minute * 20).Unix(), // last work 20 minutes ago
		})
		assert.NoError(t, err)
		assert.True(t, idle)
	})
}

func Test_drainAgents(t *testing.T) {
	t.Run("should drain agents and skip no-schedule ones", func(t *testing.T) {
		ctx := t.Context()
		client := mocks_server.NewMockClient(t)
		provider := mocks_provider.NewMockProvider(t)
		autoscaler := Autoscaler{
			agents: []*woodpecker.Agent{
				{ID: 1, Name: "pool-1-agent-1", NoSchedule: false, LastContact: time.Now().Add(-time.Minute * 2).Unix()},
				{ID: 2, Name: "pool-1-agent-2", NoSchedule: true, LastContact: time.Now().Add(-time.Minute * 2).Unix()},
				{ID: 3, Name: "pool-1-agent-3", NoSchedule: true, LastContact: time.Now().Add(-time.Minute * 2).Unix()},
				{ID: 4, Name: "pool-1-agent-4", NoSchedule: false, LastContact: time.Now().Add(-time.Minute * 2).Unix()},
			},
			provider: provider,
			client:   client,
			config: &config.Config{
				AgentIdleTimeout: time.Minute * 15,
			},
		}

		client.On("AgentUpdate", mock.MatchedBy(func(agent *woodpecker.Agent) bool {
			return (agent.ID == 1 || agent.ID == 4) && agent.NoSchedule == true
		})).Return(nil, nil)

		err := autoscaler.drainAgents(ctx, 2)
		assert.NoError(t, err)
		assert.True(t, autoscaler.agents[0].NoSchedule)
		assert.True(t, autoscaler.agents[3].NoSchedule)
	})

	t.Run("should not remove an agent that never connected", func(t *testing.T) {
		ctx := t.Context()
		client := mocks_server.NewMockClient(t)
		provider := mocks_provider.NewMockProvider(t)
		autoscaler := Autoscaler{
			agents: []*woodpecker.Agent{
				{ID: 1, Name: "pool-1-agent-1", NoSchedule: false, LastContact: 0},
			},
			provider: provider,
			client:   client,
			config: &config.Config{
				AgentIdleTimeout: time.Minute * 15,
			},
		}

		err := autoscaler.drainAgents(ctx, 1)
		assert.NoError(t, err)
		assert.False(t, autoscaler.agents[0].NoSchedule)
	})

	t.Run("should not remove an agent that has recently done some work", func(t *testing.T) {
		ctx := t.Context()
		client := mocks_server.NewMockClient(t)
		provider := mocks_provider.NewMockProvider(t)
		autoscaler := Autoscaler{
			agents: []*woodpecker.Agent{
				{
					ID:          1,
					Name:        "pool-1-agent-1",
					NoSchedule:  false,
					LastContact: time.Now().Add(-time.Minute * 2).Unix(), // last contact 2 minutes ago
					LastWork:    time.Now().Add(-time.Minute * 5).Unix(), // last work 5 minutes ago
				},
			},
			provider: provider,
			client:   client,
			config: &config.Config{
				AgentIdleTimeout: time.Minute * 15,
			},
		}

		err := autoscaler.drainAgents(ctx, 1)
		assert.NoError(t, err)
		assert.False(t, autoscaler.agents[0].NoSchedule)
	})
}

func Test_removeDrainedAgents(t *testing.T) {
	t.Run("should remove agent", func(t *testing.T) {
		ctx := t.Context()
		client := mocks_server.NewMockClient(t)
		provider := mocks_provider.NewMockProvider(t)
		autoscaler := Autoscaler{
			agents: []*woodpecker.Agent{
				{ID: 1, Name: "pool-1-agent-1", NoSchedule: false},
				{ID: 2, Name: "pool-1-agent-2", NoSchedule: true},
				{ID: 3, Name: "pool-1-agent-3", NoSchedule: false},
			},
			provider: provider,
			client:   client,
			config: &config.Config{
				AgentIdleTimeout: time.Minute * 15,
			},
		}

		client.On("AgentTasksList", int64(2)).Return(nil, nil)
		provider.On("RemoveAgent", mock.Anything, mock.MatchedBy(func(agent *woodpecker.Agent) bool {
			return agent.ID == 2
		})).Return(nil)
		client.On("AgentDelete", int64(2)).Return(nil)

		err := autoscaler.removeDrainedAgents(ctx)
		assert.NoError(t, err)
	})

	t.Run("should not remove agent with tasks", func(t *testing.T) {
		ctx := t.Context()
		client := mocks_server.NewMockClient(t)
		provider := mocks_provider.NewMockProvider(t)
		autoscaler := Autoscaler{
			agents: []*woodpecker.Agent{
				{ID: 1, Name: "pool-1-agent-1", NoSchedule: false},
				{ID: 2, Name: "pool-1-agent-2", NoSchedule: true},
				{ID: 3, Name: "pool-1-agent-3", NoSchedule: false},
			},
			provider: provider,
			client:   client,
			config:   &config.Config{},
		}

		client.On("AgentTasksList", int64(2)).Return([]*woodpecker.Task{
			{ID: "1"},
		}, nil)

		err := autoscaler.removeDrainedAgents(ctx)
		assert.NoError(t, err)
	})
}

func Test_inTeardownWindow(t *testing.T) {
	// margin 2m + reconciliation interval 1m => 3m window at the end of each hour
	cfg := &config.Config{
		AgentBillingTeardownMargin: 2 * time.Minute,
		ReconciliationInterval:     time.Minute,
	}
	autoscaler := Autoscaler{config: cfg}

	// Offsets are anchored at time.Now() and stay at least a minute clear of the
	// 57-minute window edge, so wall-clock drift during the test is irrelevant.
	createdAgo := func(d time.Duration) *woodpecker.Agent {
		return &woodpecker.Agent{Created: time.Now().Add(-d).Unix()}
	}

	t.Run("inside the window of the first paid hour", func(t *testing.T) {
		assert.True(t, autoscaler.inTeardownWindow(createdAgo(58*time.Minute)))
	})

	t.Run("just before the window opens", func(t *testing.T) {
		assert.False(t, autoscaler.inTeardownWindow(createdAgo(56*time.Minute)))
	})

	t.Run("early in the paid hour stays warm", func(t *testing.T) {
		assert.False(t, autoscaler.inTeardownWindow(createdAgo(5*time.Minute)))
	})

	t.Run("window recurs every hour", func(t *testing.T) {
		assert.True(t, autoscaler.inTeardownWindow(createdAgo(118*time.Minute)))
		assert.False(t, autoscaler.inTeardownWindow(createdAgo(90*time.Minute)))
	})

	t.Run("agent without a creation time is never in the window", func(t *testing.T) {
		assert.False(t, autoscaler.inTeardownWindow(&woodpecker.Agent{Created: 0}))
	})

	t.Run("creation time in the future (negative age) is never in the window", func(t *testing.T) {
		assert.False(t, autoscaler.inTeardownWindow(createdAgo(-5*time.Minute)))
	})
}

func Test_drainAgents_hourlyRoundUp(t *testing.T) {
	zerolog.SetGlobalLevel(zerolog.ErrorLevel)
	now := time.Now()

	t.Run("only drains agents inside their teardown window", func(t *testing.T) {
		ctx := t.Context()
		client := mocks_server.NewMockClient(t)
		provider := mocks_provider.NewMockProvider(t)
		autoscaler := Autoscaler{
			agents: []*woodpecker.Agent{
				// idle but mid-hour => kept warm, even though it has no recent work
				{ID: 1, Name: "pool-1-agent-1", LastContact: now.Add(-time.Minute).Unix(), LastWork: now.Add(-30 * time.Minute).Unix(), Created: now.Add(-30 * time.Minute).Unix()},
				// in the teardown window => eligible to drain
				{ID: 2, Name: "pool-1-agent-2", LastContact: now.Add(-time.Minute).Unix(), LastWork: now.Add(-time.Minute).Unix(), Created: now.Add(-58 * time.Minute).Unix()},
			},
			provider: provider,
			client:   client,
			config: &config.Config{
				BillingModel:               types.BillingHourlyRoundUp,
				AgentBillingTeardownMargin: 2 * time.Minute,
				ReconciliationInterval:     time.Minute,
			},
		}

		client.On("AgentUpdate", mock.MatchedBy(func(agent *woodpecker.Agent) bool {
			return agent.ID == 2 && agent.NoSchedule
		})).Return(nil, nil)

		err := autoscaler.drainAgents(ctx, 2)
		assert.NoError(t, err)
		assert.False(t, autoscaler.agents[0].NoSchedule)
		assert.True(t, autoscaler.agents[1].NoSchedule)
	})
}

func Test_removeDrainedAgents_hourlyRoundUp(t *testing.T) {
	zerolog.SetGlobalLevel(zerolog.ErrorLevel)
	now := time.Now()
	cfg := func() *config.Config {
		return &config.Config{
			BillingModel:               types.BillingHourlyRoundUp,
			AgentBillingTeardownMargin: 2 * time.Minute,
			ReconciliationInterval:     time.Minute,
		}
	}

	t.Run("removes a drained agent inside its teardown window even if it just worked", func(t *testing.T) {
		ctx := t.Context()
		client := mocks_server.NewMockClient(t)
		provider := mocks_provider.NewMockProvider(t)
		autoscaler := Autoscaler{
			agents: []*woodpecker.Agent{
				{ID: 2, Name: "pool-1-agent-2", NoSchedule: true, LastWork: now.Add(-time.Minute).Unix(), Created: now.Add(-58 * time.Minute).Unix()},
			},
			provider: provider,
			client:   client,
			config:   cfg(),
		}

		client.On("AgentTasksList", int64(2)).Return(nil, nil)
		provider.On("RemoveAgent", mock.Anything, mock.MatchedBy(func(agent *woodpecker.Agent) bool {
			return agent.ID == 2
		})).Return(nil)
		client.On("AgentDelete", int64(2)).Return(nil)

		err := autoscaler.removeDrainedAgents(ctx)
		assert.NoError(t, err)
	})

	t.Run("keeps a drained agent that rolled into a fresh paid hour", func(t *testing.T) {
		ctx := t.Context()
		client := mocks_server.NewMockClient(t)
		provider := mocks_provider.NewMockProvider(t)
		autoscaler := Autoscaler{
			agents: []*woodpecker.Agent{
				// drained while busy near the boundary; now idle 5m into a new paid hour
				{ID: 2, Name: "pool-1-agent-2", NoSchedule: true, LastWork: now.Add(-time.Minute).Unix(), Created: now.Add(-65 * time.Minute).Unix()},
			},
			provider: provider,
			client:   client,
			config:   cfg(),
		}

		err := autoscaler.removeDrainedAgents(ctx)
		assert.NoError(t, err)
	})
}

func Test_isAgentIdle_hourlyRoundUp(t *testing.T) {
	t.Run("recent work does not keep an hourly agent busy", func(t *testing.T) {
		client := mocks_server.NewMockClient(t)
		autoscaler := Autoscaler{
			client: client,
			config: &config.Config{
				BillingModel:     types.BillingHourlyRoundUp,
				AgentIdleTimeout: time.Minute * 15,
			},
		}

		client.On("AgentTasksList", int64(1)).Return(nil, nil) // no tasks

		idle, err := autoscaler.isAgentIdle(&woodpecker.Agent{
			ID:       1,
			Name:     "pool-1-agent-1",
			LastWork: time.Now().Add(-time.Minute).Unix(), // worked one minute ago
		})
		assert.NoError(t, err)
		assert.True(t, idle)
	})

	t.Run("an hourly agent with an in-flight task is still busy", func(t *testing.T) {
		client := mocks_server.NewMockClient(t)
		autoscaler := Autoscaler{
			client: client,
			config: &config.Config{BillingModel: types.BillingHourlyRoundUp},
		}

		client.On("AgentTasksList", int64(1)).Return([]*woodpecker.Task{{ID: "1"}}, nil)

		idle, err := autoscaler.isAgentIdle(&woodpecker.Agent{ID: 1, Name: "pool-1-agent-1"})
		assert.NoError(t, err)
		assert.False(t, idle)
	})
}
