package engine

import (
	"context"
	"fmt"
	"testing"
	"testing/synctest"
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
// before planning so a rollout replaces them instead of reusing them. A rollout
// advances one agent at a time and waits while another outdated agent drains.
func Test_drainOutdatedAgents(t *testing.T) {
	t.Run("waits while an outdated agent is draining", func(t *testing.T) {
		client := mocks_server.NewMockClient(t)
		outdatedAgent := &woodpecker.Agent{ID: 1, Name: "pool-1-agent-outdated", LastContact: time.Now().Unix()}
		drainingAgent := &woodpecker.Agent{ID: 2, Name: "pool-1-agent-already-draining", NoSchedule: true, LastContact: time.Now().Unix()}
		neverContacted := &woodpecker.Agent{ID: 3, Name: "pool-1-agent-never-contacted"}
		provider := &outdatedAgentTestProvider{MockProvider: mocks_provider.NewMockProvider(t), names: []string{outdatedAgent.Name, drainingAgent.Name, neverContacted.Name}}
		autoscaler := Autoscaler{
			agents:   agentsByName(outdatedAgent, drainingAgent, neverContacted),
			provider: provider,
			client:   client,
			config:   &config.Config{AgentInactivityTimeout: time.Hour},
		}
		outdated, err := autoscaler.outdatedAgentNames(t.Context())
		require.NoError(t, err)
		require.NoError(t, autoscaler.drainOutdatedAgents(outdated, true))
		assert.False(t, outdatedAgent.NoSchedule)
		assert.True(t, drainingAgent.NoSchedule)
		assert.False(t, neverContacted.NoSchedule)
		client.AssertNotCalled(t, "AgentUpdate", mock.Anything)
	})

	t.Run("drains exactly one outdated agent and picks never-contacted agents by name", func(t *testing.T) {
		client := mocks_server.NewMockClient(t)
		zNeverContacted := &woodpecker.Agent{ID: 1, Name: "pool-1-agent-z"}
		aNeverContacted := &woodpecker.Agent{ID: 2, Name: "pool-1-agent-a"}
		busy := &woodpecker.Agent{ID: 3, Name: "pool-1-agent-busy", LastContact: time.Now().Unix()}
		a := Autoscaler{agents: agentsByName(busy, zNeverContacted, aNeverContacted), client: client, config: &config.Config{PoolID: "1"}}
		client.On("AgentTasksList", busy.ID).Return([]*woodpecker.Task{{ID: "running-work"}}, nil).Once()
		client.On("AgentUpdate", mock.MatchedBy(func(agent *woodpecker.Agent) bool {
			return agent.ID == aNeverContacted.ID && agent.NoSchedule
		})).Return(nil, nil).Once()
		require.NoError(t, a.drainOutdatedAgents(map[string]struct{}{zNeverContacted.Name: {}, aNeverContacted.Name: {}, busy.Name: {}}, true))
		assert.True(t, aNeverContacted.NoSchedule)
		assert.False(t, zNeverContacted.NoSchedule)
		assert.False(t, busy.NoSchedule)
		client.AssertNumberOfCalls(t, "AgentUpdate", 1)
	})
	t.Run("prefers oldest idle agent before a busy agent", func(t *testing.T) {
		client := mocks_server.NewMockClient(t)
		oldIdle := &woodpecker.Agent{ID: 1, Name: "pool-1-agent-idle-old", LastContact: time.Now().Unix(), LastWork: time.Now().Add(-2 * time.Hour).Unix()}
		newIdle := &woodpecker.Agent{ID: 2, Name: "pool-1-agent-idle-new", LastContact: time.Now().Unix(), LastWork: time.Now().Add(-time.Hour).Unix()}
		busy := &woodpecker.Agent{ID: 3, Name: "pool-1-agent-busy", LastContact: time.Now().Unix()}
		a := Autoscaler{agents: agentsByName(busy, newIdle, oldIdle), client: client, config: &config.Config{PoolID: "1", AgentIdleTimeout: 30 * time.Minute}}
		client.On("AgentTasksList", oldIdle.ID).Return([]*woodpecker.Task{}, nil).Once()
		client.On("AgentTasksList", newIdle.ID).Return([]*woodpecker.Task{}, nil).Once()
		client.On("AgentTasksList", busy.ID).Return([]*woodpecker.Task{{ID: "running-work"}}, nil).Once()
		client.On("AgentUpdate", mock.MatchedBy(func(agent *woodpecker.Agent) bool {
			return agent.ID == oldIdle.ID && agent.NoSchedule
		})).Return(nil, nil).Once()
		require.NoError(t, a.drainOutdatedAgents(map[string]struct{}{oldIdle.Name: {}, newIdle.Name: {}, busy.Name: {}}, true))
		assert.True(t, oldIdle.NoSchedule)
		assert.False(t, newIdle.NoSchedule)
		assert.False(t, busy.NoSchedule)
		client.AssertNumberOfCalls(t, "AgentUpdate", 1)
	})

	t.Run("non-outdated draining agent does not block outdated drain", func(t *testing.T) {
		client := mocks_server.NewMockClient(t)
		currentDraining := &woodpecker.Agent{ID: 1, Name: "pool-1-agent-current", NoSchedule: true, LastContact: time.Now().Unix()}
		outdated := &woodpecker.Agent{ID: 2, Name: "pool-1-agent-outdated"}
		a := Autoscaler{agents: agentsByName(currentDraining, outdated), client: client, config: &config.Config{PoolID: "1"}}
		client.On("AgentUpdate", mock.MatchedBy(func(agent *woodpecker.Agent) bool {
			return agent.ID == outdated.ID && agent.NoSchedule
		})).Return(nil, nil).Once()
		require.NoError(t, a.drainOutdatedAgents(map[string]struct{}{outdated.Name: {}}, true))
		assert.True(t, currentDraining.NoSchedule)
		assert.True(t, outdated.NoSchedule)
		client.AssertNumberOfCalls(t, "AgentUpdate", 1)
	})

	t.Run("waits for replacement with no contact", func(t *testing.T) {
		client := mocks_server.NewMockClient(t)
		outdated := &woodpecker.Agent{ID: 1, Name: "pool-1-agent-outdated", LastContact: time.Now().Unix()}
		replacement := &woodpecker.Agent{ID: 2, Name: "pool-1-agent-new", Created: time.Now().Unix()}
		a := Autoscaler{
			agents:                      agentsByName(replacement),
			client:                      client,
			outdatedDrainAgent:          outdated.Name,
			outdatedDrainExistingAgents: map[string]int64{outdated.Name: outdated.ID},
			pendingDeploys:              map[string]types.Capability{},
			config:                      &config.Config{AgentCreationTimeout: time.Minute},
		}

		require.NoError(t, a.drainOutdatedAgents(map[string]struct{}{outdated.Name: {}}, true))

		client.AssertNotCalled(t, "AgentUpdate", mock.Anything)
	})
	t.Run("waits for a current replacement to connect", func(t *testing.T) {
		client := mocks_server.NewMockClient(t)
		outdated := &woodpecker.Agent{ID: 1, Name: "pool-1-agent-outdated", LastContact: time.Now().Unix()}
		replacement := &woodpecker.Agent{ID: 2, Name: "pool-1-agent-new", Created: time.Now().Unix()}
		a := Autoscaler{
			agents:                      agentsByName(replacement),
			client:                      client,
			outdatedDrainAgent:          outdated.Name,
			outdatedDrainExistingAgents: map[string]int64{outdated.Name: outdated.ID},
			pendingDeploys: map[string]types.Capability{
				replacement.Name: dockerAmd64Cap,
			},
			config: &config.Config{AgentCreationTimeout: time.Minute},
		}

		require.NoError(t, a.drainOutdatedAgents(map[string]struct{}{outdated.Name: {}}, true))

		client.AssertNotCalled(t, "AgentUpdate", mock.Anything)
	})
	t.Run("warns when drain waits past inactivity timeout", func(t *testing.T) {
		client := mocks_server.NewMockClient(t)
		drained := &woodpecker.Agent{ID: 1, Name: "pool-1-agent-outdated", NoSchedule: true, LastContact: time.Now().Unix()}
		a := Autoscaler{
			agents:                      agentsByName(drained),
			client:                      client,
			outdatedDrainAgent:          drained.Name,
			outdatedDrainExistingAgents: map[string]int64{drained.Name: drained.ID},
			outdatedDrainSince:          time.Now().Add(-time.Hour),
			config:                      &config.Config{AgentInactivityTimeout: time.Minute},
		}
		require.NoError(t, a.drainOutdatedAgents(map[string]struct{}{drained.Name: {}}, true))
		assert.True(t, a.outdatedDrainWarned)
		client.AssertNotCalled(t, "AgentUpdate", mock.Anything)
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
		require.NoError(t, autoscaler.drainOutdatedAgents(outdated, true))
		client.AssertNotCalled(t, "AgentUpdate", mock.Anything)
	})

	t.Run("agent update error is returned", func(t *testing.T) {
		client := mocks_server.NewMockClient(t)
		client.On("AgentUpdate", mock.Anything).Return(nil, fmt.Errorf("server unavailable"))
		client.On("AgentTasksList", int64(1)).Return([]*woodpecker.Task{}, nil).Once()
		autoscaler := Autoscaler{
			agents: agentsByName(&woodpecker.Agent{
				ID:          1,
				Name:        "pool-1-agent-outdated",
				LastContact: time.Now().Unix(),
			}),
			client: client,
		}

		err := autoscaler.drainOutdatedAgents(map[string]struct{}{"pool-1-agent-outdated": {}}, true)
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

func Test_reconcile_drainsOutdatedAgentsGradually(t *testing.T) {
	client, a := newOutdatedRolloutAutoscaler(t, 4, nil)
	require.NoError(t, a.Reconcile(t.Context()))
	assert.Equal(t, 1, countDrainCalls(client.Calls))
	assert.Contains(t, a.pendingDeploys, "pool-1-agent-new")
	assert.Equal(t, 0, countNoSchedule(a.agents))
	assert.Len(t, a.agents, a.config.MaxAgents)

	// A booting replacement blocks the next outdated agent.
	require.NoError(t, a.Reconcile(t.Context()))
	assert.Equal(t, 1, countDrainCalls(client.Calls))

	serverReplacement := a.agents["pool-1-agent-new"]
	serverReplacement.LastContact = time.Now().Unix()
	serverReplacement.Platform = "linux/amd64"
	serverReplacement.Backend = "docker"
	_, err := client.AgentUpdate(serverReplacement)
	require.NoError(t, err)
	require.NoError(t, a.Reconcile(t.Context()))
	assert.Equal(t, 2, countDrainCalls(client.Calls), "the next outdated agent drains after replacement connects")
}

func Test_reconcile_failedReplacementKeepsDrainGate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client, a := newOutdatedRolloutAutoscaler(t, 4, nil)
		a.config.AgentCreationTimeout = time.Minute
		require.NoError(t, a.Reconcile(t.Context()))
		for i := 1; i <= 3; i++ {
			synctest.Sleep(2 * time.Minute)
			require.NoError(t, a.Reconcile(t.Context()))
			assert.Equal(t, 1, countDrainCalls(client.Calls))
			assert.Equal(t, i, a.outdatedDrainFailures)
			assert.Len(t, a.agents, a.config.MaxAgents)
		}
		client.AssertNumberOfCalls(t, "AgentCreate", 4)
	})
}

func Test_reconcile_quietPoolContinuesRollout(t *testing.T) {
	client, a := newOutdatedRolloutAutoscaler(t, 2, &woodpecker.Info{})
	a.config.MinAgents = 1
	require.NoError(t, a.Reconcile(t.Context()))
	require.NoError(t, a.Reconcile(t.Context()))
	assert.Equal(t, 2, countDrainCalls(client.Calls))
	assert.Zero(t, a.outdatedDrainFailures)
}

func Test_reconcile_unmatchedPendingContinuesRollout(t *testing.T) {
	queue := &woodpecker.Info{Pending: []woodpecker.Task{{Labels: map[string]string{"platform": "linux/arm64"}}}}
	client, a := newOutdatedRolloutAutoscaler(t, 2, queue)
	a.config.MinAgents = 1
	require.NoError(t, a.Reconcile(t.Context()))
	require.NoError(t, a.Reconcile(t.Context()))
	assert.Equal(t, 2, countDrainCalls(client.Calls))
}

func Test_reconcile_deployFailureKeepsDrainGate(t *testing.T) {
	client, a := newOutdatedRolloutAutoscaler(t, 4, nil)
	a.config.MinAgents = 1
	provider := a.provider.(*outdatedAgentTestProvider)
	for _, call := range provider.ExpectedCalls {
		if call.Method == "DeployAgent" {
			call.ReturnArguments = mock.Arguments{fmt.Errorf("deploy failed")}
		}
	}
	for range 3 {
		require.ErrorContains(t, a.Reconcile(t.Context()), "deploy failed")
		assert.Equal(t, 1, countDrainCalls(client.Calls))
	}
}

func Test_replacementBootBound(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		creation, inactivity time.Duration
		bounded              bool
	}{
		{name: "creation", creation: time.Minute, inactivity: 2 * time.Minute, bounded: true},
		{name: "fallback", inactivity: time.Minute, bounded: true},
		{name: "disabled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				_, a := newOutdatedRolloutAutoscaler(t, 4, nil)
				a.config.AgentCreationTimeout = tc.creation
				a.config.AgentInactivityTimeout = tc.inactivity
				require.NoError(t, a.Reconcile(t.Context()))
				synctest.Sleep(59 * time.Second)
				require.NoError(t, a.Reconcile(t.Context()))
				if tc.bounded {
					assert.Zero(t, a.outdatedDrainFailures)
				} else {
					assert.NotEmpty(t, a.outdatedDrainAgent)
					assert.True(t, a.outdatedNoBoundWarned)
				}
				synctest.Sleep(2 * time.Second)
				require.NoError(t, a.Reconcile(t.Context()))
				if tc.bounded {
					assert.Equal(t, 1, a.outdatedDrainFailures)
				} else {
					assert.NotEmpty(t, a.outdatedDrainAgent)
				}
			})
		})
	}
}

func Test_reconcile_busyDrainKeepsFreshBoot(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client := mocks_server.NewMockClient(t)
		provider := &outdatedAgentTestProvider{MockProvider: mocks_provider.NewMockProvider(t), names: []string{"pool-1-agent-old"}}
		old := &woodpecker.Agent{ID: 1, Name: "pool-1-agent-old", NoSchedule: true, LastContact: time.Now().Unix()}
		fresh := &woodpecker.Agent{ID: 2, Name: "pool-1-agent-fresh", Created: time.Now().Unix()}
		a := Autoscaler{client: client, provider: provider, config: &config.Config{PoolID: "1", AgentCreationTimeout: time.Minute},
			outdatedDrainAgent: old.Name, outdatedDrainExistingAgents: map[string]int64{old.Name: old.ID},
			outdatedDrainSince: time.Now().Add(-time.Hour)}
		client.On("AgentListWithOpts", mock.Anything).Return([]*woodpecker.Agent{old, fresh}, nil).Once()
		client.On("AgentListWithOpts", mock.Anything).Return([]*woodpecker.Agent{}, nil).Maybe()
		client.On("QueueInfo").Return(&woodpecker.Info{}, nil)
		client.On("AgentTasksList", old.ID).Return([]*woodpecker.Task{{ID: "busy"}}, nil).Maybe()
		provider.On("ListDeployedAgentNames", mock.Anything).Return([]string{old.Name, fresh.Name}, nil)
		require.NoError(t, a.Reconcile(t.Context()))
		assert.Contains(t, a.agents, fresh.Name)
		client.AssertNotCalled(t, "AgentDelete", fresh.ID)
	})
}

func Test_drainOutdatedAgents_reusedName(t *testing.T) {
	client := mocks_server.NewMockClient(t)
	replacement := &woodpecker.Agent{ID: 2, Name: "pool-1-agent-old", LastContact: time.Now().Unix()}
	a := Autoscaler{
		agents: agentsByName(replacement), client: client, config: &config.Config{},
		outdatedDrainAgent:          replacement.Name,
		outdatedDrainExistingAgents: map[string]int64{replacement.Name: 1},
	}
	require.NoError(t, a.drainOutdatedAgents(nil, true))
	assert.Empty(t, a.outdatedDrainAgent)
}

func Test_reconcile_replacesIdleOutdatedAgentAtMaxAgentsOne(t *testing.T) {
	client, a := newOutdatedRolloutAutoscaler(t, 1, nil)
	oldAgent := a.agents["pool-1-agent-1"]
	oldAgent.LastContact = time.Now().Unix()
	oldAgent.NoSchedule = false
	a.config.MinAgents = 1
	require.NoError(t, a.Reconcile(t.Context()))
	assert.NotContains(t, a.agents, oldAgent.Name)
	assert.Equal(t, 1, len(a.pendingDeploys), "pending work is retained while the replacement boots")
	replacement := a.agents["pool-1-agent-new"]
	require.NotNil(t, replacement)
	assert.Len(t, a.agents, 1)
	assert.Contains(t, a.pendingDeploys, replacement.Name)
	client.AssertNumberOfCalls(t, "AgentCreate", 1)

	// The replacement becomes usable only after it reports first contact.
	require.NoError(t, a.Reconcile(t.Context()))
	replacement = a.agents[replacement.Name]
	replacement.LastContact = time.Now().Unix()
	replacement.Platform = "linux/amd64"
	replacement.Backend = "docker"
	_, err := client.AgentUpdate(replacement)
	require.NoError(t, err)
	require.NoError(t, a.Reconcile(t.Context()))
	assert.Empty(t, a.pendingDeploys)
	assert.False(t, replacement.NoSchedule)
	assert.Len(t, a.agents, 1)
}
func newOutdatedRolloutAutoscaler(t *testing.T, agentCount int, queue *woodpecker.Info) (*mocks_server.MockClient, *Autoscaler) {
	t.Helper()
	client := mocks_server.NewMockClient(t)
	provider := &outdatedAgentTestProvider{MockProvider: mocks_provider.NewMockProvider(t)}
	agents := []*woodpecker.Agent{
		{ID: 1, Name: "pool-1-agent-1", Platform: "linux/amd64", Backend: "docker", LastContact: time.Now().Unix()},
		{ID: 2, Name: "pool-1-agent-2", Platform: "linux/amd64", Backend: "docker", LastContact: time.Now().Unix()},
		{ID: 3, Name: "pool-1-agent-3", Platform: "linux/amd64", Backend: "docker", LastContact: time.Now().Unix()},
		{ID: 4, Name: "pool-1-agent-4", Platform: "linux/amd64", Backend: "docker", LastContact: time.Now().Unix()},
	}
	serverAgents := agentsByName(agents[:agentCount]...)
	provider.names = make([]string, 0, agentCount)
	for _, agent := range agents[:agentCount] {
		provider.names = append(provider.names, agent.Name)
	}
	a := Autoscaler{
		client:   client,
		provider: provider,
		config: &config.Config{
			WorkflowsPerAgent:      1,
			MaxAgents:              agentCount,
			MinAgents:              agentCount,
			AgentInactivityTimeout: 10 * time.Minute,
			AgentIdleTimeout:       10 * time.Minute,
			PoolID:                 "1",
		},
		agents:               agentsByName(agents[:agentCount]...),
		pendingDeploys:       map[string]types.Capability{},
		danglingSince:        map[string]time.Time{},
		providerCapabilities: []types.Capability{dockerAmd64Cap},
		scope:                instanceAdminPoolScope(),
	}
	client.On("AgentListWithOpts", woodpecker.AgentListOptions{Page: 1}).Return(func(woodpecker.AgentListOptions) ([]*woodpecker.Agent, error) {
		out := make([]*woodpecker.Agent, 0, len(serverAgents))
		for _, agent := range serverAgents {
			copy := *agent
			out = append(out, &copy)
		}
		return out, nil
	}).Maybe()
	client.On("AgentListWithOpts", woodpecker.AgentListOptions{Page: 2}).Return([]*woodpecker.Agent{}, nil).Maybe()
	client.On("AgentDelete", mock.Anything).Return(func(id int64) error {
		for name, agent := range serverAgents {
			if agent.ID == id {
				delete(serverAgents, name)
			}
		}
		return nil
	}).Maybe()
	if queue == nil {
		queue = &woodpecker.Info{Pending: []woodpecker.Task{
			{Labels: map[string]string{"platform": "linux/amd64", "repo": "rigel/x", "org-id": "1"}},
			{Labels: map[string]string{"platform": "linux/amd64", "repo": "rigel/x", "org-id": "1"}},
			{Labels: map[string]string{"platform": "linux/amd64", "repo": "rigel/x", "org-id": "1"}},
			{Labels: map[string]string{"platform": "linux/amd64", "repo": "rigel/x", "org-id": "1"}},
		}}
	}
	client.On("QueueInfo").Return(queue, nil)
	client.On("AgentUpdate", mock.Anything).Return(func(agent *woodpecker.Agent) (*woodpecker.Agent, error) {
		if existing, ok := serverAgents[agent.Name]; ok && existing.ID == agent.ID {
			copy := *agent
			serverAgents[agent.Name] = &copy
		}
		return agent, nil
	}).Maybe()
	client.On("AgentTasksList", mock.Anything).Return([]*woodpecker.Task{}, nil).Maybe()
	created := 0
	client.On("AgentCreate", mock.Anything).Return(func(agent *woodpecker.Agent) (*woodpecker.Agent, error) {
		created++
		agent.ID = int64(98 + created)
		agent.Name = "pool-1-agent-new"
		if created > 1 {
			agent.Name = fmt.Sprintf("pool-1-agent-new-%d", created)
		}
		agent.Created = time.Now().Unix()
		serverAgent := *agent
		serverAgents[agent.Name] = &serverAgent
		return agent, nil
	}).Maybe()
	provider.On("ListDeployedAgentNames", mock.Anything).Return(func(context.Context) ([]string, error) {
		names := make([]string, 0, len(serverAgents))
		for name := range serverAgents {
			names = append(names, name)
		}
		return names, nil
	}).Maybe()
	provider.On("DeployAgent", mock.Anything, mock.Anything, dockerAmd64Cap).Return(nil).Maybe()
	provider.On("RemoveAgent", mock.Anything, mock.Anything).Return(nil).Maybe()

	return client, &a
}
func countNoSchedule(agents map[string]*woodpecker.Agent) int {
	count := 0
	for _, agent := range agents {
		if agent.NoSchedule {
			count++
		}
	}
	return count
}

func countDrainCalls(calls []mock.Call) int {
	count := 0
	for _, call := range calls {
		if call.Method == "AgentUpdate" && call.Arguments.Get(0).(*woodpecker.Agent).NoSchedule {
			count++
		}
	}
	return count
}
