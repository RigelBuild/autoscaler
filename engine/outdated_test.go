package engine

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"go.woodpecker-ci.org/autoscaler/config"
	"go.woodpecker-ci.org/autoscaler/engine/types"
	mocks_provider "go.woodpecker-ci.org/autoscaler/engine/types/mocks"
	mocks_server "go.woodpecker-ci.org/autoscaler/server/mocks"
	"go.woodpecker-ci.org/woodpecker/v3/woodpecker-go/woodpecker"
)

// outdatedAgentTestProvider adds the optional OutdatedAgentLister to the mock
// provider, so the engine's type assertion takes the outdated path.
type outdatedAgentTestProvider struct {
	*mocks_provider.MockProvider
	names []string
	err   error
}

func (p *outdatedAgentTestProvider) ListOutdatedAgentNames(context.Context) ([]string, error) {
	return p.names, p.err
}

var _ types.OutdatedAgentLister = (*outdatedAgentTestProvider)(nil)

// agentsByName indexes agents the way the engine's agent map does.
func agentsByName(agents ...*woodpecker.Agent) map[string]*woodpecker.Agent {
	out := make(map[string]*woodpecker.Agent, len(agents))
	for _, a := range agents {
		out[a.Name] = a
	}
	return out
}

// Test_drainOutdatedAgents covers feature 2: outdated pool agents are drained
// before planning so a rollout replaces them instead of reusing them.
func Test_drainOutdatedAgents(t *testing.T) {
	t.Run("drains outdated schedulable pool agents", func(t *testing.T) {
		client := mocks_server.NewMockClient(t)
		provider := &outdatedAgentTestProvider{
			MockProvider: mocks_provider.NewMockProvider(t),
			names: []string{
				"pool-1-agent-outdated",
				"pool-1-agent-already-draining",
				"pool-1-agent-never-contacted",
				"pool-2-agent-non-pool",
			},
		}
		outdatedAgent := &woodpecker.Agent{ID: 1, Name: "pool-1-agent-outdated", LastContact: time.Now().Unix()}
		drainingAgent := &woodpecker.Agent{ID: 2, Name: "pool-1-agent-already-draining", NoSchedule: true, LastContact: time.Now().Unix()}
		neverContacted := &woodpecker.Agent{ID: 3, Name: "pool-1-agent-never-contacted"}
		current := &woodpecker.Agent{ID: 4, Name: "pool-1-agent-current", LastContact: time.Now().Unix()}
		nonPool := &woodpecker.Agent{ID: 5, Name: "pool-2-agent-non-pool", LastContact: time.Now().Unix()}
		autoscaler := Autoscaler{
			agents:    agentsByName(outdatedAgent, drainingAgent, neverContacted, current),
			allAgents: []*woodpecker.Agent{outdatedAgent, drainingAgent, neverContacted, current, nonPool},
			provider:  provider,
			client:    client,
		}
		client.On("AgentUpdate", mock.MatchedBy(func(agent *woodpecker.Agent) bool {
			return (agent.ID == 1 || agent.ID == 3) && agent.NoSchedule
		})).Return(nil, nil).Twice()

		outdated, err := autoscaler.outdatedAgentNames(t.Context())
		require.NoError(t, err)
		require.NoError(t, autoscaler.drainOutdatedAgents(outdated))
		assert.True(t, outdatedAgent.NoSchedule)
		assert.True(t, drainingAgent.NoSchedule)
		// A never-contacted outdated agent can hold no work, so it is retired too.
		assert.True(t, neverContacted.NoSchedule)
		assert.False(t, current.NoSchedule)
		assert.False(t, nonPool.NoSchedule)
		client.AssertNumberOfCalls(t, "AgentUpdate", 2)
	})

	t.Run("provider without outdated listing is a no-op", func(t *testing.T) {
		client := mocks_server.NewMockClient(t)
		provider := mocks_provider.NewMockProvider(t)
		autoscaler := Autoscaler{
			agents: agentsByName(&woodpecker.Agent{
				ID:          1,
				Name:        "pool-1-agent-outdated",
				LastContact: time.Now().Unix(),
			}),
			provider: provider,
			client:   client,
		}

		outdated, err := autoscaler.outdatedAgentNames(t.Context())
		require.NoError(t, err)
		require.Nil(t, outdated)
		require.NoError(t, autoscaler.drainOutdatedAgents(outdated))
		client.AssertNotCalled(t, "AgentUpdate", mock.Anything)
	})

	t.Run("agent update error is returned", func(t *testing.T) {
		client := mocks_server.NewMockClient(t)
		client.On("AgentUpdate", mock.Anything).Return(nil, fmt.Errorf("server unavailable"))
		autoscaler := Autoscaler{
			agents: agentsByName(&woodpecker.Agent{
				ID:          1,
				Name:        "pool-1-agent-outdated",
				LastContact: time.Now().Unix(),
			}),
			client: client,
		}

		err := autoscaler.drainOutdatedAgents(map[string]struct{}{"pool-1-agent-outdated": {}})
		require.ErrorContains(t, err, "server unavailable")
	})

	t.Run("lister error stops reconcile before scaling", func(t *testing.T) {
		client := mocks_server.NewMockClient(t)
		provider := &outdatedAgentTestProvider{
			MockProvider: mocks_provider.NewMockProvider(t),
			err:          fmt.Errorf("pods list denied"),
		}
		client.On("AgentListWithOpts", mock.Anything).Return([]*woodpecker.Agent{
			{ID: 1, Name: "pool-1-agent-outdated", LastContact: time.Now().Unix()},
		}, nil).Once()
		client.On("AgentListWithOpts", mock.Anything).Return([]*woodpecker.Agent{}, nil).Once()
		autoscaler := Autoscaler{
			provider: provider,
			client:   client,
			config:   &config.Config{PoolID: "1"},
		}

		err := autoscaler.Reconcile(t.Context())
		require.ErrorContains(t, err, "pods list denied")
		client.AssertNotCalled(t, "QueueInfo")
		client.AssertNotCalled(t, "AgentUpdate", mock.Anything)
		client.AssertNotCalled(t, "AgentCreate", mock.Anything)
	})
}

// Test_createAgents_outdated covers the rollout rules in createAgents: an
// outdated agent is never reactivated, and a busy outdated agent keeps its
// MaxAgents slot until removal.
func Test_createAgents_outdated(t *testing.T) {
	t.Run("replaces outdated agents instead of reactivating them", func(t *testing.T) {
		ctx := t.Context()
		client := mocks_server.NewMockClient(t)
		provider := mocks_provider.NewMockProvider(t)
		outdatedDrained := &woodpecker.Agent{
			ID: 1, Name: "pool-1-agent-outdated", NoSchedule: true,
			Platform: "linux/amd64", Backend: "docker", LastContact: time.Now().Unix(),
		}
		autoscaler := Autoscaler{
			agents:               agentsByName(outdatedDrained),
			client:               client,
			provider:             provider,
			config:               &config.Config{PoolID: "1", MaxAgents: 5},
			providerCapabilities: []types.Capability{dockerAmd64Cap},
			scope:                instanceAdminPoolScope(),
			pendingDeploys:       map[string]types.Capability{},
		}
		client.On("AgentCreate", mock.Anything).Return(&woodpecker.Agent{ID: 2, Name: "pool-1-agent-new"}, nil)
		provider.On("DeployAgent", mock.Anything, mock.Anything, dockerAmd64Cap).Return(nil)

		err := autoscaler.createAgents(ctx, agentBucket{Capability: dockerAmd64Cap}, 1,
			map[string]struct{}{"pool-1-agent-outdated": {}})
		require.NoError(t, err)
		client.AssertNotCalled(t, "AgentUpdate", mock.Anything)
		client.AssertCalled(t, "AgentCreate", mock.Anything)
		assert.True(t, outdatedDrained.NoSchedule, "the outdated agent stays drained")
	})

	t.Run("creates no more than MaxAgents allows", func(t *testing.T) {
		// The executor half of the surge fix: asked for 3 with one slot held
		// by a drained outdated agent and MaxAgents=2, only 1 agent is created.
		ctx := t.Context()
		client := mocks_server.NewMockClient(t)
		provider := mocks_provider.NewMockProvider(t)
		outdatedDrained := &woodpecker.Agent{
			ID: 1, Name: "pool-1-agent-outdated", NoSchedule: true,
			Platform: "linux/amd64", Backend: "docker", LastContact: time.Now().Unix(),
		}
		autoscaler := Autoscaler{
			agents:         agentsByName(outdatedDrained),
			client:         client,
			provider:       provider,
			config:         &config.Config{PoolID: "1", MaxAgents: 2},
			pendingDeploys: map[string]types.Capability{},
		}
		created := 0
		client.On("AgentCreate", mock.Anything).Return(func(*woodpecker.Agent) (*woodpecker.Agent, error) {
			created++
			return &woodpecker.Agent{ID: int64(10 + created), Name: fmt.Sprintf("pool-1-agent-new-%d", created)}, nil
		}).Maybe()
		provider.On("DeployAgent", mock.Anything, mock.Anything, dockerAmd64Cap).Return(nil).Maybe()

		require.NoError(t, autoscaler.createAgents(ctx, agentBucket{Capability: dockerAmd64Cap}, 3,
			map[string]struct{}{"pool-1-agent-outdated": {}}))
		assert.Equal(t, 1, created, "one free slot under MaxAgents=2, so one create")
		assert.Len(t, autoscaler.agents, 2)
	})

	t.Run("busy outdated agents hold their slots under MaxAgents", func(t *testing.T) {
		// Every tracked agent, draining or not, holds a MaxAgents slot. The
		// pool is full (footprint 1 = MaxAgents) with one busy drained outdated
		// agent, and a pending task routes to its bucket. The outdated agent
		// must not be counted as reusable, so no agent is created.
		client := mocks_server.NewMockClient(t)
		provider := &outdatedAgentTestProvider{
			MockProvider: mocks_provider.NewMockProvider(t),
			names:        []string{"pool-1-agent-outdated"},
		}
		busy := &woodpecker.Agent{
			ID: 1, Name: "pool-1-agent-outdated", NoSchedule: true,
			Platform: "linux/amd64", Backend: "docker",
			LastContact: time.Now().Unix(), LastWork: time.Now().Unix(),
		}
		a := Autoscaler{
			client:               client,
			provider:             provider,
			agents:               map[string]*woodpecker.Agent{},
			pendingDeploys:       map[string]types.Capability{},
			danglingSince:        map[string]time.Time{},
			providerCapabilities: []types.Capability{dockerAmd64Cap},
			scope:                instanceAdminPoolScope(),
			config: &config.Config{
				WorkflowsPerAgent: 1, MaxAgents: 1, MinAgents: 0, PoolID: "1",
				AgentInactivityTimeout: 10 * time.Minute, AgentIdleTimeout: 10 * time.Minute,
			},
		}
		routed := woodpecker.Task{Labels: map[string]string{"platform": "linux/amd64", "repo": "rigel/x", "org-id": "1"}}
		client.On("AgentListWithOpts", woodpecker.AgentListOptions{Page: 1}).Return([]*woodpecker.Agent{busy}, nil)
		client.On("AgentListWithOpts", woodpecker.AgentListOptions{Page: 2}).Return([]*woodpecker.Agent{}, nil).Maybe()
		client.On("QueueInfo").Return(&woodpecker.Info{Pending: []woodpecker.Task{routed}}, nil)
		client.On("AgentTasksList", int64(1)).Return([]*woodpecker.Task{{ID: "9"}}, nil).Maybe()
		client.On("AgentDelete", mock.Anything).Return(nil).Maybe()
		provider.On("ListDeployedAgentNames", mock.Anything).Return([]string{busy.Name}, nil)
		// Registered so a breach is reported by the assertions below, not a mock panic.
		client.On("AgentCreate", mock.Anything).Return(&woodpecker.Agent{ID: 2, Name: "pool-1-agent-new"}, nil).Maybe()
		provider.On("DeployAgent", mock.Anything, mock.Anything, mock.Anything).Return(nil).Maybe()

		require.NoError(t, a.Reconcile(t.Context()))
		client.AssertNotCalled(t, "AgentCreate", mock.Anything)
		provider.AssertNotCalled(t, "DeployAgent", mock.Anything, mock.Anything, mock.Anything)
		assert.Len(t, a.agents, 1, "the footprint stays at MaxAgents")
	})
}

// Test_removeDrainedAgents_outdated covers feature 2's removal rule: an outdated
// agent is removed as soon as it is task-free, without waiting out the idle
// timeout that would otherwise keep a drained agent warm.
func Test_removeDrainedAgents_outdated(t *testing.T) {
	t.Run("an outdated drained agent with recent work is removed once task-free", func(t *testing.T) {
		client := mocks_server.NewMockClient(t)
		provider := mocks_provider.NewMockProvider(t)
		outdatedAgent := &woodpecker.Agent{
			ID: 1, Name: "pool-1-agent-outdated", NoSchedule: true,
			LastWork: time.Now().Unix(), LastContact: time.Now().Unix(),
		}
		autoscaler := Autoscaler{
			agents:   agentsByName(outdatedAgent),
			client:   client,
			provider: provider,
			config:   &config.Config{AgentIdleTimeout: time.Hour, PoolID: "1"},
		}
		client.On("AgentTasksList", int64(1)).Return([]*woodpecker.Task{}, nil)
		provider.On("RemoveAgent", mock.Anything, outdatedAgent).Return(nil)
		client.On("AgentDelete", int64(1)).Return(nil)

		require.NoError(t, autoscaler.removeDrainedAgents(t.Context(), map[string]struct{}{"pool-1-agent-outdated": {}}))
		assert.NotContains(t, autoscaler.agents, "pool-1-agent-outdated")
	})

	t.Run("an outdated drained agent with a task in flight is kept", func(t *testing.T) {
		client := mocks_server.NewMockClient(t)
		provider := mocks_provider.NewMockProvider(t)
		outdatedAgent := &woodpecker.Agent{
			ID: 1, Name: "pool-1-agent-outdated", NoSchedule: true,
			LastWork: time.Now().Unix(), LastContact: time.Now().Unix(),
		}
		autoscaler := Autoscaler{
			agents:   agentsByName(outdatedAgent),
			client:   client,
			provider: provider,
			config:   &config.Config{AgentIdleTimeout: time.Hour, PoolID: "1"},
		}
		client.On("AgentTasksList", int64(1)).Return([]*woodpecker.Task{{ID: "9"}}, nil)

		require.NoError(t, autoscaler.removeDrainedAgents(t.Context(), map[string]struct{}{"pool-1-agent-outdated": {}}))
		assert.Contains(t, autoscaler.agents, "pool-1-agent-outdated", "a busy outdated agent is not removed")
		provider.AssertNotCalled(t, "RemoveAgent", mock.Anything, mock.Anything)
	})
}
