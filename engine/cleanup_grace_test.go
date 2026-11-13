package engine

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"

	"go.woodpecker-ci.org/autoscaler/config"
	mocks_provider "go.woodpecker-ci.org/autoscaler/engine/types/mocks"
	mocks_server "go.woodpecker-ci.org/autoscaler/server/mocks"
	"go.woodpecker-ci.org/woodpecker/v3/woodpecker-go/woodpecker"
)

// Test_cleanupDanglingAgents_graceWindow covers the danglingSince grace window:
// a provider instance missing from woodpecker is only terminated once it has
// been absent past AgentInactivityTimeout, so a transient woodpecker restart
// does not tear down a busy machine. The clock is pre-seeded rather than slept,
// so each case is deterministic.
func Test_cleanupDanglingAgents_graceWindow(t *testing.T) {
	t.Run("should remove agent that is only present on woodpecker (not provider)", func(t *testing.T) {
		ctx := t.Context()
		client := mocks_server.NewMockClient(t)
		provider := mocks_provider.NewMockProvider(t)
		autoscaler := Autoscaler{
			agents: map[string]*woodpecker.Agent{
				"pool-1-agent-1": {ID: 1, Name: "pool-1-agent-1", NoSchedule: false},
			},
			provider:      provider,
			client:        client,
			danglingSince: map[string]time.Time{},
		}

		provider.On("ListDeployedAgentNames", mock.Anything).Return(nil, nil)
		client.On("AgentDelete", int64(1)).Return(nil)

		err := autoscaler.cleanupDanglingAgents(ctx)
		assert.NoError(t, err)
		assert.NotContains(t, autoscaler.agents, "pool-1-agent-1", "the woodpecker-only agent is deleted from memory")
	})

	t.Run("should keep the other agents in memory after removing one", func(t *testing.T) {
		ctx := t.Context()
		client := mocks_server.NewMockClient(t)
		provider := mocks_provider.NewMockProvider(t)
		kept := &woodpecker.Agent{ID: 2, Name: "pool-1-agent-2"}
		autoscaler := Autoscaler{
			agents: map[string]*woodpecker.Agent{
				"pool-1-agent-1": {ID: 1, Name: "pool-1-agent-1"},
				"pool-1-agent-2": kept,
			},
			provider:      provider,
			client:        client,
			danglingSince: map[string]time.Time{},
		}

		provider.On("ListDeployedAgentNames", mock.Anything).Return([]string{"pool-1-agent-2"}, nil)
		client.On("AgentDelete", int64(1)).Return(nil)

		err := autoscaler.cleanupDanglingAgents(ctx)
		assert.NoError(t, err)
		assert.Equal(t, map[string]*woodpecker.Agent{"pool-1-agent-2": kept}, autoscaler.agents)
	})

	t.Run("should remove agent that is only present on provider (not woodpecker) once grace has elapsed", func(t *testing.T) {
		ctx := t.Context()
		client := mocks_server.NewMockClient(t)
		provider := mocks_provider.NewMockProvider(t)
		autoscaler := Autoscaler{
			agents: map[string]*woodpecker.Agent{
				"pool-1-agent-1": {ID: 1, Name: "pool-1-agent-1", NoSchedule: false},
			},
			provider: provider,
			client:   client,
			config:   &config.Config{AgentInactivityTimeout: 10 * time.Minute},
			// Pre-seed the grace clock past the timeout so termination fires on
			// this pass.
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
			agents: map[string]*woodpecker.Agent{
				"pool-1-agent-1": {ID: 1, Name: "pool-1-agent-1", NoSchedule: false},
			},
			provider:      provider,
			client:        client,
			config:        &config.Config{AgentInactivityTimeout: 10 * time.Minute},
			danglingSince: map[string]time.Time{},
		}

		// No RemoveAgent expectation is registered: mockery fails if it is called.
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
			agents: map[string]*woodpecker.Agent{
				"pool-1-agent-1": {ID: 1, Name: "pool-1-agent-1", NoSchedule: false},
				"pool-1-agent-2": {ID: 2, Name: "pool-1-agent-2", NoSchedule: false},
			},
			provider: provider,
			client:   client,
			config:   &config.Config{AgentInactivityTimeout: 10 * time.Minute},
			danglingSince: map[string]time.Time{
				"pool-1-agent-2": time.Now().Add(-5 * time.Minute),
			},
		}

		provider.On("ListDeployedAgentNames", mock.Anything).Return([]string{"pool-1-agent-1", "pool-1-agent-2"}, nil)

		err := autoscaler.cleanupDanglingAgents(ctx)
		assert.NoError(t, err)
		_, ok := autoscaler.danglingSince["pool-1-agent-2"]
		assert.False(t, ok, "a reappeared instance must drop its grace clock")
	})

	t.Run("should not terminate a still-within-grace instance on a later pass", func(t *testing.T) {
		ctx := t.Context()
		client := mocks_server.NewMockClient(t)
		provider := mocks_provider.NewMockProvider(t)
		since := time.Now().Add(-3 * time.Minute)
		autoscaler := Autoscaler{
			agents: map[string]*woodpecker.Agent{
				"pool-1-agent-1": {ID: 1, Name: "pool-1-agent-1", NoSchedule: false},
			},
			provider: provider,
			client:   client,
			config:   &config.Config{AgentInactivityTimeout: 10 * time.Minute},
			danglingSince: map[string]time.Time{
				"pool-1-agent-2": since,
			},
		}

		provider.On("ListDeployedAgentNames", mock.Anything).Return([]string{"pool-1-agent-1", "pool-1-agent-2"}, nil)

		err := autoscaler.cleanupDanglingAgents(ctx)
		assert.NoError(t, err)
		assert.Equal(t, since, autoscaler.danglingSince["pool-1-agent-2"],
			"the original grace start is kept while the window is open")
	})

	t.Run("should garbage-collect the grace clock when the provider stops listing a still-dangling instance", func(t *testing.T) {
		ctx := t.Context()
		client := mocks_server.NewMockClient(t)
		provider := mocks_provider.NewMockProvider(t)
		autoscaler := Autoscaler{
			agents: map[string]*woodpecker.Agent{
				"pool-1-agent-1": {ID: 1, Name: "pool-1-agent-1", NoSchedule: false},
			},
			provider: provider,
			client:   client,
			config:   &config.Config{AgentInactivityTimeout: 10 * time.Minute},
			danglingSince: map[string]time.Time{
				"pool-1-agent-2": time.Now().Add(-3 * time.Minute),
			},
		}

		// The instance vanished from the provider mid-grace (out-of-band reclaim).
		provider.On("ListDeployedAgentNames", mock.Anything).Return([]string{"pool-1-agent-1"}, nil)

		err := autoscaler.cleanupDanglingAgents(ctx)
		assert.NoError(t, err)
		_, ok := autoscaler.danglingSince["pool-1-agent-2"]
		assert.False(t, ok, "a grace clock for an instance the provider no longer lists must not leak")
	})
}
